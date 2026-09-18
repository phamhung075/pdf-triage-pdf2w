// Manual-decision routes #19-#22, ported from web-server.ts:544-632.
//
// GET    /api/manual-decisions       list the human-decision audit log
// PUT    /api/manual-decisions/:id   edit a decision (forbidden-subcategory + generic-category
//
//	guards via app/guards, then DECISIONS_UPDATED)
//
// DELETE /api/manual-decisions/:id   remove one decision
// DELETE /api/manual-decisions       clear every decision
//
// A decision never touches a document or file: it is a learning record that feeds the NEXT
// classification prompt immediately.
package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/app/guards"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

func (s *server) registerManualDecisions(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/manual-decisions", s.listManualDecisionsHandler)
	mux.HandleFunc("PUT /api/manual-decisions/{id}", s.updateManualDecisionHandler)
	mux.HandleFunc("DELETE /api/manual-decisions/{id}", s.deleteManualDecisionHandler)
	mux.HandleFunc("DELETE /api/manual-decisions", s.clearManualDecisionsHandler)
}

func (s *server) listManualDecisionsHandler(w http.ResponseWriter, r *http.Request) {
	decisions := s.deps.ManualDecisions.GetManualDecisions()
	if decisions == nil {
		decisions = []manualdecisions.Record{}
	}
	writeJSON(w, 200, map[string]any{"total": len(decisions), "decisions": decisions})
}

func (s *server) updateManualDecisionHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := parseJSInt(r.PathValue("id"))
	if !ok {
		writeError(w, 400, "Invalid decision id")
		return
	}

	patch := buildDecisionPatch(bodyBytes(r))

	// Golden Rule #4: an explicit forbidden target subcategory is rejected before any write.
	// TS's `patch.new_subcategory && ...` means an empty string bypasses the guard.
	if patch.NewSubcategory != nil && *patch.NewSubcategory != "" {
		if violation := guards.ForbiddenSubcategoryViolation(*patch.NewSubcategory); violation != nil {
			writeError(w, 400, violation.ShortMessage)
			return
		}
	}
	if patch.NewCategory != nil {
		if violation := guards.GenericCategoryViolation(*patch.NewCategory); violation != nil {
			writeError(w, 400, violation.Message)
			return
		}
	}

	updated, err := s.deps.ManualDecisions.UpdateManualDecision(id, patch)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if updated == nil {
		writeError(w, 404, "Decision not found")
		return
	}
	s.hub.Broadcast(map[string]any{"type": "DECISIONS_UPDATED", "action": "UPDATE", "decisionId": id})
	writeJSON(w, 200, map[string]any{
		"success":  true,
		"message":  "Decision updated — it now teaches the AI its new values.",
		"decision": updated,
	})
}

func (s *server) deleteManualDecisionHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := parseJSInt(r.PathValue("id"))
	if !ok {
		writeError(w, 400, "Invalid decision id")
		return
	}
	deleted, err := s.deps.ManualDecisions.DeleteManualDecision(id)
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}
	if !deleted {
		writeError(w, 404, "Decision not found")
		return
	}
	s.hub.Broadcast(map[string]any{"type": "DECISIONS_UPDATED", "action": "DELETE", "decisionId": id})
	writeJSON(w, 200, map[string]any{
		"success": true,
		"message": "Decision removed — it no longer teaches the AI.",
	})
}

func (s *server) clearManualDecisionsHandler(w http.ResponseWriter, r *http.Request) {
	if err := s.deps.ManualDecisions.ClearManualDecisions(); err != nil {
		writeError(w, 500, err.Error())
		return
	}
	s.hub.Broadcast(map[string]any{"type": "DECISIONS_UPDATED", "action": "CLEAR"})
	writeJSON(w, 200, map[string]any{
		"success": true,
		"message": "All human decisions cleared — nothing teaches the AI anymore.",
	})
}

// buildDecisionPatch is the body-normalization block at web-server.ts:564-584.
func buildDecisionPatch(raw []byte) manualdecisions.Patch {
	var patch manualdecisions.Patch
	if len(bytes.TrimSpace(raw)) == 0 {
		return patch
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(raw, &body); err != nil {
		return patch
	}

	if value, ok := stringField(body, "new_category"); ok {
		lower := strings.ToLower(value)
		patch.NewCategory = &lower
	}
	if value, ok := stringField(body, "new_subcategory"); ok {
		lower := strings.ToLower(value)
		patch.NewSubcategory = &lower
	}
	if value, ok := stringField(body, "user_feedback_reason"); ok {
		patch.UserFeedbackReason = &value
	}
	if rawKeywords, present := body["rule_keywords"]; present {
		keywords := normalizeRuleKeywords(rawKeywords)
		patch.RuleKeywords = &keywords
	}
	if rawEnabled, present := body["enabled"]; present {
		var value any
		if err := decodeAny(rawEnabled, &value); err == nil {
			enabled := 0
			if jsTruthy(value) {
				enabled = 1
			}
			patch.Enabled = &enabled
		}
	}
	return patch
}

// normalizeRuleKeywords ports web-server.ts:575-583:
//
//	Array.isArray(x) ? x : String(x || '').split(',')  -> trim, drop empties, dedupe, cap 10.
func normalizeRuleKeywords(raw json.RawMessage) []string {
	var value any
	if err := decodeAny(raw, &value); err != nil {
		return []string{}
	}
	var rawList []any
	if array, ok := value.([]any); ok {
		rawList = array
	} else {
		text := ""
		if value != nil {
			text = jsStringValue(value)
		}
		parts := strings.Split(text, ",")
		rawList = make([]any, len(parts))
		for i, part := range parts {
			rawList[i] = part
		}
	}

	seen := map[string]bool{}
	keywords := make([]string, 0, len(rawList))
	for _, item := range rawList {
		keyword := strings.TrimSpace(jsStringValue(item))
		if keyword == "" || seen[keyword] {
			continue
		}
		seen[keyword] = true
		keywords = append(keywords, keyword)
		if len(keywords) == 10 {
			break
		}
	}
	if keywords == nil {
		keywords = []string{}
	}
	return keywords
}

// stringField returns (value, true) only when the JSON field is a string (typeof === 'string').
func stringField(body map[string]json.RawMessage, key string) (string, bool) {
	raw, present := body[key]
	if !present {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", false
	}
	return value, true
}

// decodeAny decodes with json.Number so number formatting can mirror JS String(number).
func decodeAny(raw json.RawMessage, out *any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	return dec.Decode(out)
}

// jsStringValue is JavaScript String(value) for the JSON scalar types that can appear in
// rule_keywords: null -> "null", booleans lowercased, numbers without a trailing .0.
func jsStringValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case json.Number:
		return v.String()
	case float64:
		text := strconv.FormatFloat(v, 'f', -1, 64)
		return text
	default:
		return ""
	}
}

// jsTruthy is JavaScript truthiness for `body.enabled ? 1 : 0`.
func jsTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case json.Number:
		f, err := v.Float64()
		return err == nil && f != 0
	case float64:
		return v != 0
	default:
		return true
	}
}

// parseJSInt ports parseInt(value, 10): leading whitespace, an optional sign, then leading digits.
// A value with no digits (NaN) returns false, matching `!Number.isFinite(id)`.
func parseJSInt(value string) (int64, bool) {
	s := strings.TrimSpace(value)
	if s == "" {
		return 0, false
	}
	i := 0
	negative := false
	if s[0] == '+' || s[0] == '-' {
		negative = s[0] == '-'
		i++
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	n, err := strconv.ParseInt(s[start:i], 10, 64)
	if err != nil {
		return 0, false
	}
	if negative {
		n = -n
	}
	return n, true
}
