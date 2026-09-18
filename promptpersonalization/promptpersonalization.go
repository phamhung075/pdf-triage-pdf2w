// Package promptpersonalization is a Go port of pdf-triage's src/domain/prompt-personalization.ts
// (154 lines). It ports, function-for-function: the PriorityRule and PromptPersonalization Zod
// schemas (as Parse/ParsePriorityRule), the EMPTY_PROMPT_PERSONALIZATION constant, the
// matchPriorityRules deterministic overlay matcher, and the renderPriorityRulesBlock /
// renderKnownEntitiesBlock prompt renderers.
//
// Personal prompt personalization — the private counterpart to the committed, generic prompts/
// templates. The files in prompts/ are PUBLIC and committed and must stay free of any real
// employer, bank product code, school, clinic or filename prefix belonging to the person running
// this instance. Those signals live in a gitignored `.prompts.private.json` and are rendered into
// two explicit placeholders at prompt-build time:
//
//	{{USER_PRIORITY_RULES}}  in prompts/classification_rules.md  <- renderPriorityRulesBlock
//	{{USER_KNOWN_ENTITIES}}  in prompts/micro_prompt_entity.md   <- renderKnownEntitiesBlock
//
// PriorityRule is consumed directly by the sibling decisionrule package, which imports this type
// rather than defining its own; there is a single Go definition, so no data migration is needed.
//
// The TypeScript source is the behavioral source of truth. Deviations, all resolved in favor of
// matching TS:
//
//  1. JS whitespace. Every `.trim()` here uses JavaScript's WhiteSpace+LineTerminator set, which
//     includes NBSP (U+00A0), the Unicode space separators (U+1680, U+2000-U+200A, U+202F, U+205F,
//     U+3000), U+2028/U+2029 and the BOM (U+FEFF). Go's strings.TrimSpace follows
//     unicode.White_Space and would disagree (it strips U+0085 NEL; it leaves U+FEFF). jsTrim
//     reproduces String.prototype.trim() exactly.
//  2. Letter boundaries without lookaround. matchPriorityRules builds `(?<!\p{L})…(?!\p{L})` in TS.
//     Go's regexp is RE2 and supports neither lookahead nor lookbehind, so keywordMatches finds
//     every occurrence of the literal keyword with a plain `(?i)` regex and then checks the
//     adjacent runes with unicode.IsLetter — the same `\p{L}` boundary semantics.
//  3. `filename?: string` becomes a variadic `filename ...string`; omitting it keeps the TS
//     undefined behavior (filename-scoped rules are skipped).
//  4. Zod collects all issues; this port returns the first error, with custom messages preserved.
//  5. JS truthiness for the string fields (rule.subcategory, rule.note, extra_rules_text) is the
//     Go non-empty-string test: "" is the only falsy string, so the two agree exactly.
package promptpersonalization

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// PriorityRule is the Go equivalent of the TS `PriorityRule` shape (z.infer of
// PriorityRuleSchema), and it is the single Go definition consumed directly by the sibling
// decisionrule package. The empty Subcategory / Note strings are the Go equivalent of TS
// undefined, and Scope is "all" or "filename".
type PriorityRule struct {
	Keywords    []string `json:"keywords"`
	Category    string   `json:"category"`
	Subcategory string   `json:"subcategory,omitempty"`
	Note        string   `json:"note,omitempty"`
	Scope       string   `json:"scope,omitempty"`
}

// PromptPersonalization mirrors PromptPersonalizationSchema: all three collections are always
// non-nil/defaulted after Parse.
type PromptPersonalization struct {
	// Real issuing entities from this user's own documents — employers, clinics, schools, small
	// companies — that a generic entity dictionary cannot know about. Fed to Step A (entity
	// extraction) as recognition hints.
	KnownEntities []string `json:"known_entities"`
	// High-priority keyword -> category/subcategory overrides, evaluated by the model BEFORE the
	// generic STEP 1..13 decision flow.
	PriorityRules []PriorityRule `json:"priority_rules"`
	// Escape hatch for rules that don't fit the keyword/category shape. Injected verbatim (as
	// Markdown) at the end of the priority-rules block.
	ExtraRulesText string `json:"extra_rules_text"`
}

// EMPTY_PROMPT_PERSONALIZATION ports the TS constant of the same name, produced there by
// PromptPersonalizationSchema.parse({}).
var EMPTY_PROMPT_PERSONALIZATION = PromptPersonalization{
	KnownEntities:  []string{},
	PriorityRules:  []PriorityRule{},
	ExtraRulesText: "",
}

// MatchResult mirrors the TS matchPriorityRules return shape
// `{ categorie: string; subcategorie: string; keyword: string } | null`; a nil *MatchResult is the
// TS null.
type MatchResult struct {
	Categorie    string `json:"categorie"`
	Subcategorie string `json:"subcategorie"`
	Keyword      string `json:"keyword"`
}

// Parse ports PromptPersonalizationSchema.parse.
func Parse(rawJSON []byte) (PromptPersonalization, error) {
	out := PromptPersonalization{KnownEntities: []string{}, PriorityRules: []PriorityRule{}, ExtraRulesText: ""}
	m, err := parseObject(rawJSON)
	if err != nil {
		return out, err
	}
	// known_entities: z.array(z.string()).optional().default([])
	if raw, ok := m["known_entities"]; ok {
		if isJSONNull(raw) {
			return out, errors.New("known_entities: Expected array, received null")
		}
		var a []string
		if err := json.Unmarshal(raw, &a); err != nil {
			return out, fmt.Errorf("known_entities: Expected array of strings, received %s", jsonTypeName(raw))
		}
		if a == nil {
			a = []string{}
		}
		out.KnownEntities = a
	}
	// priority_rules: z.array(PriorityRuleSchema).optional().default([])
	if raw, ok := m["priority_rules"]; ok {
		if isJSONNull(raw) {
			return out, errors.New("priority_rules: Expected array, received null")
		}
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil {
			return out, fmt.Errorf("priority_rules: Expected array, received %s", jsonTypeName(raw))
		}
		for _, item := range arr {
			im, err := parseObject(item)
			if err != nil {
				return out, fmt.Errorf("priority_rules[]: %w", err)
			}
			rule, err := parsePriorityRuleFields(im)
			if err != nil {
				return out, fmt.Errorf("priority_rules[]: %w", err)
			}
			out.PriorityRules = append(out.PriorityRules, rule)
		}
	}
	// extra_rules_text: z.string().optional().default('')
	if raw, ok := m["extra_rules_text"]; ok {
		s, err := decodeString(raw)
		if err != nil {
			return out, fmt.Errorf("extra_rules_text: %w", err)
		}
		out.ExtraRulesText = s
	}
	return out, nil
}

// ParsePriorityRule ports PriorityRuleSchema.parse.
func ParsePriorityRule(rawJSON []byte) (PriorityRule, error) {
	m, err := parseObject(rawJSON)
	if err != nil {
		return PriorityRule{}, err
	}
	return parsePriorityRuleFields(m)
}

func parsePriorityRuleFields(m map[string]json.RawMessage) (PriorityRule, error) {
	var out PriorityRule

	// keywords: z.array(z.string()).min(1)
	raw, ok := m["keywords"]
	if !ok {
		return out, errors.New("keywords: Required")
	}
	if isJSONNull(raw) {
		return out, errors.New("keywords: Expected array, received null")
	}
	var keywords []string
	if err := json.Unmarshal(raw, &keywords); err != nil {
		return out, fmt.Errorf("keywords: Expected array of strings, received %s", jsonTypeName(raw))
	}
	if len(keywords) < 1 {
		return out, errors.New("keywords: Array must contain at least 1 element(s)")
	}
	out.Keywords = keywords

	// category: z.string().min(1)
	raw, ok = m["category"]
	if !ok {
		return out, errors.New("category: Required")
	}
	s, err := decodeString(raw)
	if err != nil {
		return out, fmt.Errorf("category: %w", err)
	}
	if s == "" {
		return out, errors.New("category: String must contain at least 1 character(s)")
	}
	out.Category = s

	// subcategory / note: z.string().optional()
	if out.Subcategory, err = optionalStringValue(m, "subcategory"); err != nil {
		return out, err
	}
	if out.Note, err = optionalStringValue(m, "note"); err != nil {
		return out, err
	}

	// scope: z.enum(['all','filename']).optional().default('all')
	out.Scope = "all"
	if raw, ok := m["scope"]; ok {
		s, err := decodeString(raw)
		if err != nil {
			return out, fmt.Errorf("scope: %w", err)
		}
		switch s {
		case "all", "filename":
			out.Scope = s
		default:
			return out, fmt.Errorf("scope: Invalid enum value. Expected 'all' | 'filename', received %q", s)
		}
	}
	return out, nil
}

// RenderPriorityRulesBlock ports renderPriorityRulesBlock.
//
// Renders the {{USER_PRIORITY_RULES}} block for prompts/classification_rules.md.
//
// Returns "" when there is nothing to inject — the placeholder must vanish cleanly rather than
// leave an empty, confusing "STEP 0" heading in the prompt.
func RenderPriorityRulesBlock(p PromptPersonalization) string {
	rules := make([]PriorityRule, 0, len(p.PriorityRules))
	for _, r := range p.PriorityRules {
		for _, k := range r.Keywords {
			if jsTrim(k) != "" {
				rules = append(rules, r)
				break
			}
		}
	}
	extra := jsTrim(p.ExtraRulesText)
	if len(rules) == 0 && extra == "" {
		return ""
	}

	lines := []string{
		"",
		"STEP 0: USER-SPECIFIC HIGH-PRIORITY OVERRIDES (EVALUATE BEFORE STEP 1):",
		"- These keyword sets come from this archive's own documents. If one matches, apply it and SKIP the remaining steps.",
		"- ⚠️ EXCEPTION — STEPS 1-3 STILL WIN: if the document is itself a bank statement (STEP 1), a tax notice (STEP 2) or a pay slip (STEP 3), classify it under that STEP and treat any name matched here as content noise. A non-matching STEP 0 override never beats those three semantic anchors.",
		"- A rule marked FILENAME-ONLY matches the document FILENAME, never the body text: a word appearing inside the text is not evidence that the rule applies.",
	}

	for _, rule := range rules {
		nonBlank := make([]string, 0, len(rule.Keywords))
		for _, k := range rule.Keywords {
			if jsTrim(k) != "" {
				nonBlank = append(nonBlank, k)
			}
		}
		keywords := quoteList(nonBlank)
		var target string
		if rule.Subcategory != "" {
			target = "Category = '" + rule.Category + "', Subcategory = '" + rule.Subcategory + "'"
		} else {
			target = "Category = '" + rule.Category + "' (resolve the Subcategory from the issuing entity as usual)"
		}
		where := "the document text or filename contains"
		if rule.Scope == "filename" {
			where = "the document FILENAME contains"
		}
		line := "- IF " + where + " " + keywords + " -> " + target + "."
		if rule.Note != "" {
			line += " " + jsTrim(rule.Note)
		}
		lines = append(lines, line)
	}

	if extra != "" {
		lines = append(lines, extra)
	}

	return strings.Join(lines, "\n") + "\n"
}

// MatchPriorityRules ports matchPriorityRules.
//
// Deterministic counterpart to the rendered STEP 0 block: matches the same overlay rules against a
// document so `ruleBasedClassify` (the Ollama-down fallback) honours them too. Golden Rule #6
// requires the prompt and the fallback to stay logically aligned — without this, the fallback would
// silently keep classifying by signals the prompt no longer carries.
//
// Only rules with an explicit `subcategory` are returned: a rule that defers subcategory
// resolution to the issuing entity has nothing for a regex-based classifier to act on, and guessing
// one would manufacture a subcategory the document never supported.
//
// `combined` is the caller's lowercased filename + text haystack; `filename` is the bare
// (lowercased) filename. Rules with `scope: 'filename'` are matched against the filename ONLY —
// never against body text — so an auto-learned rule derived from one moved file's name cannot
// re-fire on an unrelated document that merely mentions the same generic word.
func MatchPriorityRules(combined string, p PromptPersonalization, filename ...string) *MatchResult {
	hasFilename := len(filename) > 0
	filenameArg := ""
	if hasFilename {
		filenameArg = filename[0]
	}
	for _, rule := range p.PriorityRules {
		if rule.Subcategory == "" {
			continue
		}
		haystack := combined
		if rule.Scope == "filename" {
			if !hasFilename || filenameArg == "" {
				continue
			}
			haystack = strings.ToLower(filenameArg)
		}
		for _, raw := range rule.Keywords {
			keyword := strings.ToLower(jsTrim(raw))
			if keyword == "" {
				continue
			}
			// Boundaries exclude adjacent LETTERS, not digits. A keyword must not match inside a
			// longer word — "gan" may not fire on "organization" — but the codes these rules exist
			// for are routinely glued to a date or account number ("recXX20240424",
			// "STMT_CHK_101"), and a digit-excluding boundary would never match those. A separator
			// ("stmt_", "c/c ") needs no trailing guard at all.
			if keywordMatches(haystack, keyword) {
				return &MatchResult{Categorie: rule.Category, Subcategorie: rule.Subcategory, Keyword: keyword}
			}
		}
	}
	return nil
}

// RenderKnownEntitiesBlock ports renderKnownEntitiesBlock.
//
// Renders the {{USER_KNOWN_ENTITIES}} block for prompts/micro_prompt_entity.md.
//
// Returns "" when no entities are configured, so the committed generic prompt reads correctly on a
// fresh clone.
func RenderKnownEntitiesBlock(p PromptPersonalization) string {
	entities := make([]string, 0, len(p.KnownEntities))
	for _, e := range p.KnownEntities {
		if t := jsTrim(e); t != "" {
			entities = append(entities, t)
		}
	}
	if len(entities) == 0 {
		return ""
	}
	return "\nKNOWN ISSUING ENTITIES IN THIS ARCHIVE (prefer an exact match from this list when the document text supports it — but never force one that the text does not actually contain): " + quoteList(entities) + ".\n"
}

// keywordMatches reproduces the TS `new RegExp(leading + escaped + trailing, 'iu').test(haystack)`
// without lookaround (unsupported by RE2): find every occurrence of the literal keyword and accept
// the first whose adjacent runes satisfy the `\p{L}` boundary guards. The regexp is compiled per
// call, exactly as the TS `new RegExp(...)` is, so there is no shared mutable state.
func keywordMatches(haystack, keyword string) bool {
	re := regexp.MustCompile("(?i)" + regexp.QuoteMeta(keyword))
	leadingGuard := startsWithLetter(keyword)
	trailingGuard := endsWithLetter(keyword)

	searchStart := 0
	for searchStart <= len(haystack) {
		loc := re.FindStringIndex(haystack[searchStart:])
		if loc == nil {
			return false
		}
		start := searchStart + loc[0]
		end := searchStart + loc[1]

		accepted := true
		if leadingGuard && start > 0 {
			if r, _ := utf8.DecodeLastRuneInString(haystack[:start]); unicode.IsLetter(r) {
				accepted = false
			}
		}
		if accepted && trailingGuard && end < len(haystack) {
			if r, _ := utf8.DecodeRuneInString(haystack[end:]); unicode.IsLetter(r) {
				accepted = false
			}
		}
		if accepted {
			return true
		}

		// Advance by one rune so overlapping occurrences are still considered (RE2 FindAll would
		// skip them, diverging from the JS lookaround scan).
		_, size := utf8.DecodeRuneInString(haystack[start:])
		if size == 0 {
			return false
		}
		searchStart = start + size
	}
	return false
}

func startsWithLetter(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsLetter(r)
}

func endsWithLetter(s string) bool {
	r, _ := utf8.DecodeLastRuneInString(s)
	return unicode.IsLetter(r)
}

// quoteList ports `values.map(v => \`"${v.trim()}"\`).join(', ')`.
func quoteList(values []string) string {
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = `"` + jsTrim(v) + `"`
	}
	return strings.Join(parts, ", ")
}

// ---- Zod-primitive helpers ----

func parseObject(rawJSON []byte) (map[string]json.RawMessage, error) {
	if isJSONNull(rawJSON) {
		return nil, errors.New("Expected object, received null")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(rawJSON, &m); err != nil {
		return nil, fmt.Errorf("Expected object, received %s", jsonTypeName(rawJSON))
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	return m, nil
}

// decodeString ports z.string(): null and every non-string JSON value fail.
func decodeString(raw json.RawMessage) (string, error) {
	if isJSONNull(raw) {
		return "", errors.New("Expected string, received null")
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("Expected string, received %s", jsonTypeName(raw))
	}
	return s, nil
}

// optionalStringValue ports z.string().optional() for a string struct field: absent -> "".
func optionalStringValue(m map[string]json.RawMessage, name string) (string, error) {
	raw, ok := m[name]
	if !ok {
		return "", nil
	}
	return decodeString(raw)
}

func isJSONNull(raw []byte) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func jsonTypeName(raw []byte) string {
	s := bytes.TrimSpace(raw)
	if len(s) == 0 {
		return "undefined"
	}
	switch s[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	default:
		return "number"
	}
}

// jsTrim is String.prototype.trim(): it strips exactly the JavaScript whitespace set below. Go's
// strings.TrimSpace would additionally strip U+0085 NEL and would leave U+FEFF.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSWhitespace)
}

// isJSWhitespace reports whether r is in JavaScript's WhiteSpace+LineTerminator set.
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}
