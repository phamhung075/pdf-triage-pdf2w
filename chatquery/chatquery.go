// Package chatquery is a Go port of pdf-triage's src/domain/chat-query.ts (166 lines). It ports,
// function-for-function: the StructuredQuerySchema Zod schema (as Parse), the StructuredQuery
// result type, buildFtsMatchExpression, planQueryHeuristic and relaxQuery, plus the STOPWORDS set.
//
// The deterministic, zero-I/O planner is the fallback that keeps the chat usable when Ollama is
// stopped or returns unparseable JSON, and the fast path for the eval harness's --no-llm mode. It
// is deliberately dumber than the model: stopwords out, years into a date range, tokens that match
// a known tag promoted to entities, everything else a keyword.
//
// The TypeScript source is the behavioral source of truth. Deviations, all resolved in favor of
// matching TS:
//
//  1. JS whitespace. The tokenizer's `\s`, and every `.trim()` below, use JavaScript's
//     WhiteSpace+LineTerminator set (NBSP, the Unicode space separators, U+2028/U+2029, U+FEFF).
//     Go's regexp `\s` is ASCII-only and strings.TrimSpace follows unicode.White_Space, so
//     jsWhitespaceClass is injected into the splitter and jsTrim replaces strings.TrimSpace.
//  2. UTF-16 string length. planQueryHeuristic's `token.length <= 2` counts UTF-16 code units, not
//     runes; utf16Len reproduces that count.
//  3. JS parseInt. optionalLimit's string coercion is parseInt(v, 10), not Go's strconv.Atoi:
//     leading JS whitespace is skipped, an optional sign is consumed, parsing stops at the first
//     non-digit ("3.9" -> 3, "0x10" -> 0, "1e3" -> 1), and no digits means NaN -> undefined.
//     jsParseInt10 reproduces it.
//  4. Zod collects every issue into one "Invalid input" union error; this port returns the first
//     error. No ported test asserts on an error message.
//  5. Optional-without-default fields (category, subcategory, dateFrom, dateTo, limit) are
//     pointers, because TS distinguishes the absent value (undefined) from a present one.
package chatquery

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

// StructuredQuery mirrors z.infer<typeof StructuredQuerySchema>: the four term arrays are always
// present (default []), the rest are optional pointers where nil is the TS undefined.
type StructuredQuery struct {
	DocTypes    []string `json:"docTypes"`
	Entities    []string `json:"entities"`
	Keywords    []string `json:"keywords"`
	NotTerms    []string `json:"notTerms"`
	Category    *string  `json:"category,omitempty"`
	Subcategory *string  `json:"subcategory,omitempty"`
	DateFrom    *string  `json:"dateFrom,omitempty"`
	DateTo      *string  `json:"dateTo,omitempty"`
	Limit       *int     `json:"limit,omitempty"`
}

var (
	yearRe = regexp.MustCompile(`^\d{4}$`)
	// hasContentRe is `[\p{L}\p{N}]` with the u flag.
	hasContentRe = regexp.MustCompile(`[\p{L}\p{N}]`)
	// tokenSplitRe is `/[\s,.;:!?/\\'"()[\]]+/` with JS's `\s` set injected.
	tokenSplitRe = regexp.MustCompile(`[` + jsWhitespaceClass + `,.;:!?/\\'"()\[\]]+`)
)

// jsWhitespaceClass is the exact character set matched by JavaScript's `\s`: WhiteSpace plus
// LineTerminator. Go's `\s` would silently drop NBSP, U+2028/29, the Unicode spaces and U+FEFF.
const jsWhitespaceClass = `\t\n\v\f\r \x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

// STOPWORDS ports the TS STOPWORDS set: French and English function words plus the conversational
// filler people put in a chat box ("j'ai besoin", "peux-tu"). The old scorer had no stopword list
// at all, so `besoin` was a search term with the same standing as `rib`.
var stopwords = map[string]struct{}{
	"ai": {}, "aux": {}, "avec": {}, "avoir": {}, "besoin": {}, "cette": {}, "ces": {}, "dans": {},
	"des": {}, "donne": {}, "donner": {}, "elle": {}, "est": {}, "et": {}, "eux": {}, "faire": {},
	"fait": {}, "iel": {}, "ils": {}, "les": {}, "leur": {}, "mais": {}, "merci": {}, "mes": {},
	"moi": {}, "mon": {}, "nos": {}, "notre": {}, "nous": {}, "ont": {}, "ou": {}, "par": {},
	"pas": {}, "peux": {}, "peut": {}, "plus": {}, "pour": {}, "pouvez": {}, "quel": {}, "quelle": {},
	"quels": {}, "quelles": {}, "que": {}, "qui": {}, "sur": {}, "ses": {}, "son": {}, "sont": {},
	"tous": {}, "tout": {}, "toute": {}, "toutes": {}, "trouve": {}, "trouver": {}, "une": {},
	"veux": {}, "voir": {}, "vos": {}, "votre": {}, "vous": {},
	"a": {}, "about": {}, "all": {}, "and": {}, "any": {}, "are": {}, "can": {}, "find": {},
	"for": {}, "from": {}, "get": {}, "give": {}, "have": {}, "i": {}, "is": {}, "me": {},
	"my": {}, "need": {}, "of": {}, "please": {}, "show": {}, "some": {}, "the": {}, "to": {},
	"want": {}, "with": {}, "you": {}, "your": {},
}

// Parse ports StructuredQuerySchema.parse.
func Parse(rawJSON []byte) (StructuredQuery, error) {
	out := StructuredQuery{DocTypes: []string{}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{}}
	m, err := parseObject(rawJSON)
	if err != nil {
		return out, err
	}
	termFields := []struct {
		name string
		dst  *[]string
	}{
		{"docTypes", &out.DocTypes},
		{"entities", &out.Entities},
		{"keywords", &out.Keywords},
		{"notTerms", &out.NotTerms},
	}
	for _, f := range termFields {
		terms, err := parseTermArray(m, f.name)
		if err != nil {
			return out, err
		}
		*f.dst = terms
	}
	stringFields := []struct {
		name string
		dst  **string
	}{
		{"category", &out.Category},
		{"subcategory", &out.Subcategory},
		{"dateFrom", &out.DateFrom},
		{"dateTo", &out.DateTo},
	}
	for _, f := range stringFields {
		v, err := parseNullableOptionalString(m, f.name)
		if err != nil {
			return out, err
		}
		*f.dst = v
	}
	if out.Limit, err = parseOptionalLimit(m, "limit"); err != nil {
		return out, err
	}
	return out, nil
}

// BuildFtsMatchExpression ports buildFtsMatchExpression.
//
// Compiles a StructuredQuery into an FTS5 MATCH expression. Facets are ANDed, terms within a facet
// are ORed: "what kind of document" AND "whose" AND "about what". Returns nil when no positive
// facet survives — the caller must treat that as "no FTS query is possible" rather than running an
// empty MATCH.
func BuildFtsMatchExpression(q StructuredQuery) *string {
	facets := []string{}
	for _, facet := range [][]string{q.DocTypes, q.Entities, q.Keywords} {
		terms := normaliseFacet(facet)
		if len(terms) == 0 {
			continue
		}
		facets = append(facets, "("+strings.Join(toFtsPhrases(terms), " OR ")+")")
	}

	if len(facets) == 0 {
		return nil
	}

	positive := strings.Join(facets, " AND ")
	nots := normaliseFacet(q.NotTerms)
	if len(nots) == 0 {
		return &positive
	}

	// FTS5 precedence is NOT > AND > OR, so `A AND B NOT C` would parse as `A AND (B NOT C)` and
	// apply the exclusion to one facet only. Parenthesise the whole positive side.
	result := "(" + positive + ") NOT (" + strings.Join(toFtsPhrases(nots), " OR ") + ")"
	return &result
}

// PlanQueryHeuristic ports planQueryHeuristic. knownTags is variadic to preserve the TS default of
// an empty array.
func PlanQueryHeuristic(userMessage string, knownTags ...string) StructuredQuery {
	tagSet := make(map[string]struct{}, len(knownTags))
	for _, t := range knownTags {
		tagSet[strings.ToLower(t)] = struct{}{}
	}
	entities := []string{}
	keywords := []string{}
	var dateFrom, dateTo *string

	tokens := splitTokens(userMessage)
	for _, token := range tokens {
		if yearRe.MatchString(token) {
			year, _ := strconv.Atoi(token)
			if isPlausibleDocumentYear(year) {
				df := fmt.Sprintf("%d-01-01", year)
				dt := fmt.Sprintf("%d-12-31", year)
				dateFrom = &df
				dateTo = &dt
				continue
			}
		}
		if utf16Len(token) <= 2 {
			continue
		}
		if _, ok := stopwords[token]; ok {
			continue
		}
		if _, ok := tagSet[token]; ok {
			entities = append(entities, token)
		} else {
			keywords = append(keywords, token)
		}
	}

	return StructuredQuery{
		DocTypes: []string{},
		Entities: entities,
		Keywords: keywords,
		NotTerms: []string{},
		DateFrom: dateFrom,
		DateTo:   dateTo,
	}
}

// RelaxQuery ports relaxQuery: one rung down the relaxation ladder — keywords, then notTerms, then
// entities. Returns nil when only docTypes (and the filters) remain — the document type is what the
// user is least willing to compromise on, so it is never dropped, and the ladder must terminate.
func RelaxQuery(q StructuredQuery) *StructuredQuery {
	if len(q.Keywords) > 0 {
		next := q
		next.Keywords = []string{}
		return &next
	}
	if len(q.NotTerms) > 0 {
		next := q
		next.NotTerms = []string{}
		return &next
	}
	if len(q.Entities) > 0 {
		next := q
		next.Entities = []string{}
		return &next
	}
	return nil
}

// parseTermArray ports
// z.union([z.array(z.unknown()), z.null()]).optional().transform(arr => (arr ?? []).filter(t => typeof t === 'string')).
// A single stray element or null field must not cost us the whole plan.
func parseTermArray(m map[string]json.RawMessage, name string) ([]string, error) {
	raw, ok := m[name]
	if !ok || isJSONNull(raw) {
		return []string{}, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil, fmt.Errorf("%s: Invalid input", name)
	}
	out := []string{}
	for _, el := range arr {
		if isJSONString(el) {
			var s string
			if err := json.Unmarshal(el, &s); err == nil {
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// parseNullableOptionalString ports
// z.union([z.string(), z.null()]).optional().transform(v => typeof v === 'string' && v.trim().length > 0 ? v.trim() : undefined).
func parseNullableOptionalString(m map[string]json.RawMessage, name string) (*string, error) {
	raw, ok := m[name]
	if !ok || isJSONNull(raw) {
		return nil, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("%s: Invalid input", name)
	}
	t := jsTrim(s)
	if t == "" {
		return nil, nil
	}
	return &t, nil
}

// parseOptionalLimit ports
// z.union([z.number(), z.string(), z.null()]).optional().transform(v => { const n = typeof v === 'string' ? parseInt(v, 10) : typeof v === 'number' ? v : NaN; return Number.isInteger(n) && n > 0 && n <= 50 ? n : undefined; }).
func parseOptionalLimit(m map[string]json.RawMessage, name string) (*int, error) {
	raw, ok := m[name]
	if !ok || isJSONNull(raw) {
		return nil, nil
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, fmt.Errorf("%s: Invalid input", name)
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, fmt.Errorf("%s: Invalid input", name)
		}
		n, has := jsParseInt10(s)
		if !has || n <= 0 || n > 50 {
			return nil, nil
		}
		return &n, nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		// booleans, objects and arrays fail the union exactly as they do in Zod.
		return nil, fmt.Errorf("%s: Invalid input", name)
	}
	if f == math.Trunc(f) && f > 0 && f <= 50 {
		n := int(f)
		return &n, nil
	}
	return nil, nil
}

// hasSearchableContent ports `[\p{L}\p{N}]`. Terms that tokenise to nothing make FTS5 raise
// "fts5: syntax error near ..." on the MATCH.
func hasSearchableContent(term string) bool {
	return hasContentRe.MatchString(term)
}

// toFtsPhrase ports toFtsPhrase. Quoting is not cosmetic: FTS5 reads `-`, `*`, `:`, `(`, `)`, `^`,
// `NEAR`, `AND`, `OR` and `NOT` as query syntax, and these terms are produced by a model that has
// read untrusted document text. A phrase literal is the one form that cannot be reinterpreted as an
// operator. Internal double quotes are escaped by doubling, per SQLite.
func toFtsPhrase(term string) string {
	return `"` + strings.ReplaceAll(term, `"`, `""`) + `"`
}

func toFtsPhrases(terms []string) []string {
	out := make([]string, len(terms))
	for i, t := range terms {
		out[i] = toFtsPhrase(t)
	}
	return out
}

func normaliseFacet(terms []string) []string {
	seen := make(map[string]struct{}, len(terms))
	out := []string{}
	for _, raw := range terms {
		term := jsTrim(raw)
		if !hasSearchableContent(term) {
			continue
		}
		key := strings.ToLower(term)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, term)
	}
	return out
}

// isPlausibleDocumentYear bounds a bare 4-digit number to something that could plausibly be a
// document year.
func isPlausibleDocumentYear(n int) bool {
	return n >= 1950 && n <= 2100
}

// splitTokens ports
// `userMessage.toLowerCase().split(/[\s,.;:!?/\\'"()[\]]+/).map(t => t.trim()).filter(Boolean)`.
func splitTokens(userMessage string) []string {
	parts := tokenSplitRe.Split(strings.ToLower(userMessage), -1)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := jsTrim(p); t != "" {
			out = append(out, t)
		}
	}
	return out
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

func isJSONNull(raw []byte) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

func isJSONString(raw []byte) bool {
	s := bytes.TrimSpace(raw)
	return len(s) > 0 && s[0] == '"'
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

// jsParseInt10 reproduces parseInt(value, 10): skip leading JS whitespace, consume an optional
// sign, then take the longest run of ASCII digits; no digits means "not a number".
func jsParseInt10(s string) (int, bool) {
	i := 0
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if !isJSWhitespace(r) {
			break
		}
		i += size
	}
	sign := 1
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		if s[i] == '-' {
			sign = -1
		}
		i++
	}
	start := i
	n := 0
	overflow := false
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		if !overflow {
			n = n*10 + int(s[i]-'0')
			if n > 1<<30 {
				overflow = true
			}
		}
		i++
	}
	if i == start {
		return 0, false
	}
	if overflow {
		// Any value beyond the 1..50 window collapses to "undefined" at the call site, so the
		// exact magnitude is irrelevant.
		return sign * (1 << 30), true
	}
	return sign * n, true
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

// utf16Len is JavaScript's `str.length`: the number of UTF-16 code units, not runes.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}
