// Package classification is a Go port of the rule-based fallback classifier and entity matcher in
// pdf-triage's src/domain/classification.ts (724 lines): ALL_ENTITY_DOMAINS, matchEntityDictionary,
// buildEntityHintLine, normalizeSlug, preprocessRawText, isGroundedSubcategorySlug,
// cleanAndParseJSON (and its repairTruncatedJSON helper), ruleBasedClassify, extractRuleBasedContact,
// formatLocalDate, reconcileDocumentDate and buildCategoriesDescriptionStr.
//
// It reuses, rather than reimplements, the already-ported sibling packages the TypeScript imports
// map to: documentschema (CategoryItem, EntityDictionary, EntityItem, CategoriesConfig) and
// promptpersonalization (PromptPersonalization, EMPTY_PROMPT_PERSONALIZATION, MatchPriorityRules).
//
// The TypeScript source is the behavioral source of truth. The upstream TypeScript suite is GREEN
// at port time (`npx vitest run src/domain/classification.test.ts` -> 77 passed), so no upstream
// case is pinned red. The deviations below are pure language mechanics, each resolved in favor of
// matching TS exactly:
//
//  1. Lookaround. JS uses `(?<!…)` / `(?!…)` (entity word boundaries, the `cni` guard, the
//     preprocessRawText `(?![a-z])` guards, countSlugOccurrences). Go's regexp is RE2 and supports
//     neither, so each one is evaluated explicitly: literalBoundaryMatch finds every occurrence of
//     the literal and accepts the first whose adjacent runes satisfy the \p{L}\p{N} / \p{L} guard
//     (the same technique promptpersonalization.MatchPriorityRules uses for `(?<!\p{L})`); the
//     `cni` alternative is lifted out of the identity alternations and guarded separately; and the
//     `(?![a-z])` guards are applied by replaceUnlessFollowedByASCIILetter, which leaves a match
//     untouched when the next rune is an ASCII letter — the `i` flag makes `[a-z]` match both cases.
//  2. JS whitespace and dot classes. `\s`/`\S` and `String.prototype.trim()` use JavaScript's
//     WhiteSpace+LineTerminator set (NBSP U+00A0, the Unicode space separators, U+2028/U+2029, the
//     BOM U+FEFF, …). Go's regexp `\s` is ASCII-only and strings.TrimSpace follows
//     unicode.White_Space, so jsWhitespaceClass is injected into every regex and jsTrim replaces
//     strings.TrimSpace. For the same reason `.` is the explicit jsDotClass wherever TS uses `.*`:
//     JS `.` also excludes U+2028/U+2029, while Go's `.` excludes only '\n' and would match '\r'.
//  3. NFD folding without x/text. JS `String.prototype.normalize('NFD').replace(/[\u0300-\u036f]/g,
//     ”)` decomposes a precomposed letter and strips the combining mark. Go's standard library has
//     no Unicode normalizer and go.mod must stay dependency-free, so stripAccents uses a
//     hand-written precomposed-Latin fold table plus explicit combining-mark stripping — the same
//     approach and comment as taxonomyconflicts.normalizeSlugForComparison. The table covers
//     Latin-1 Supplement and Latin Extended-A (the scripts this archive's French slugs use); letters
//     with no canonical decomposition (œ, æ, ø, ß, …) are deliberately absent, matching TS, which
//     leaves them to the `[^a-z0-9_-]` filter.
//  4. UTF-16 string length and substring. JS `str.length` counts UTF-16 code units and
//     `rawText.substring(0, 4000)` cuts on a code unit. utf16Len / utf16Slice reproduce both. The
//     archive corpus is BMP (French/European text), where a code unit is a rune; utf16Slice
//     additionally matches astral text for whole code points and diverges from JS only for a
//     surrogate pair straddling the cut, which JS would leave as a lone surrogate — unrepresentable
//     in a Go UTF-8 string and unreachable for this corpus.
//  5. Truthiness and optional fields. TS `personalization`, `documentText`, `now` and the variadic
//     `filename` use undefined defaults; the Go ports use variadic parameters and fall back to the
//     TS default. `payment_status` / `invoice_type` are optional in the TS return type but always
//     assigned, so the Go struct always sets them. The TS `{ date, corrected, reason? }` maps to a
//     struct whose empty Reason is the undefined case. TS `string | null` returns (the entity
//     match) map to a nil *EntityMatch.
//  6. The WeakMap entity-candidate memo is intentionally NOT reproduced. It existed only to avoid
//     rebuilding ~2,700 RegExp objects per call (V8 compiles lazily); this port does a literal
//     substring scan with explicit boundary guards and needs no per-dictionary cache, so every
//     observable result (including the "does not leak between dictionaries" behaviour) is identical
//     without it.
//  7. The truncated-JSON repair's `console.warn` is a logging side effect, not a result; this
//     package is pure and does not emit it. The WHY comment is preserved verbatim on
//     cleanAndParseJSON.
//  8. Dates. ruleBasedClassify's default date is `new Date().toISOString().split('T')[0]` — UTC
//     today — so it is time.Now().UTC(). reconcileDocumentDate's `new Date(y, m, d)` is LOCAL, so it
//     builds time.Date(..., time.Local) and compares instants; formatLocalDate uses the time's own
//     location, exactly as JS uses the Date's local calendar fields.
//  9. Math.round and Number.prototype.toFixed do not appear anywhere in this file, so the
//     half-up-vs-half-away-from-zero and tie-rounding gaps are not reachable here.
//
// The original WHY comments (bank-statement traps, Golden Rule 4/6/7/15 history, the filename-echo
// guard, the deliberately loud JSON repair) are preserved verbatim at their functions.
package classification

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
)

// domainCategoryMap mirrors the TS DOMAIN_CATEGORY_MAP, and AllEntityDomains mirrors
// ALL_ENTITY_DOMAINS (Object.keys preserves insertion order, so the slice order is the map's).
var domainCategoryMap = map[string]string{
	"banks":     "bank",
	"energy":    "invoices",
	"telecom":   "invoices",
	"insurance": "insurance",
	"gov":       "administrative",
	"health":    "health",
}

// AllEntityDomains ports the exported TS `ALL_ENTITY_DOMAINS`.
var AllEntityDomains = []string{"banks", "energy", "telecom", "insurance", "gov", "health"}

// EntityMatch mirrors the TS `{ categorie: string; subcategorie: string } | null`; nil is null.
type EntityMatch struct {
	Categorie    string
	Subcategorie string
}

// RuleBasedClassifyResult mirrors the TS ruleBasedClassify return shape. PaymentStatus / InvoiceType
// are optional in TS but always assigned, so they are always populated here too.
type RuleBasedClassifyResult struct {
	Categorie     string
	Subcategorie  string
	Title         string
	Date          string
	Reason        string
	PaymentStatus string
	InvoiceType   string
}

// RuleBasedContact mirrors the TS extractRuleBasedContact return shape.
type RuleBasedContact struct {
	ContactName    string
	ContactEmail   string
	ContactPhone   string
	ContactAddress string
	ContactWebsite string
}

// DateReconciliation mirrors the TS `{ date: string; corrected: boolean; reason?: string }`; an
// empty Reason is the TS `reason` undefined.
type DateReconciliation struct {
	Date      string
	Corrected bool
	Reason    string
}

// matchEntityDictionary's candidate construction comment, preserved verbatim from the TS source:
//
// The dictionary ships ~1,044 entities and each contributes a name plus every alias, so matching
// used to build a RegExp per candidate on every call — roughly 2,700 of them, each with a unicode
// lookbehind. V8 compiles a regex lazily on its first execution, so the first full-dictionary call
// measured 2,092ms against 23ms for an empty dictionary, and that 2s made
// classification-resolution.test.ts flake against vitest's 5s timeout. Subsequent calls only looked
// cheap because V8's regex compilation cache still held the sources — a bounded, evictable cache.
//
// Two changes fix it. First, a plain lowercase `includes()` pre-filter: a boundary-anchored pattern
// can only match if its literal text occurs at all, so the overwhelming majority of candidates are
// rejected by a substring scan and never become a RegExp. Second, the survivors are compiled once
// and memoized. The pre-filter is the part that matters — it turns ~2,700 regex compilations into
// the handful of entities a document actually mentions.
//
// Keyed by the dictionary OBJECT via a WeakMap: entity-dictionary-store.ts caches parsed
// dictionaries, so an unchanged file yields the same identity and this memo hits; edit the file and
// the store hands back a fresh object, which misses here and rebuilds exactly as it should. WeakMap
// means a replaced dictionary's candidates become collectable rather than leaking.
//
// MatchEntityDictionary ports matchEntityDictionary. The TS WeakMap memo is not reproduced: the
// boundary scan below is a literal substring scan, so there is no per-call RegExp compilation to
// amortize (see package comment deviation 6). The `haystack` lowercasing keeps the cheap pre-filter
// equivalent to the regexes' 'i' flag, exactly as in TS.
func MatchEntityDictionary(combined string, domains []string, dictionary documentschema.EntityDictionary) *EntityMatch {
	haystack := strings.ToLower(combined)

	for _, domain := range domains {
		categorie, ok := domainCategoryMap[domain]
		if !ok {
			continue
		}
		for _, entry := range entriesForDomain(&dictionary, domain) {
			if entry == nil {
				continue
			}
			for _, candidate := range entityCandidates(entry) {
				// The regexes carry the 'i' flag, so lowercasing once here keeps the pre-filter
				// equivalent to what they would match, while letting every candidate be tested with
				// a plain substring scan.
				needle := strings.ToLower(candidate)
				if needle == "" {
					continue // cannot match; skip compiling anything
				}
				// Unicode-aware word boundaries, so accented characters delimit correctly.
				if literalBoundaryMatch(haystack, needle) {
					return &EntityMatch{Categorie: categorie, Subcategorie: entry.Slug}
				}
			}
		}
	}
	return nil
}

// entityCandidates mirrors `[entry.name, ...entry.aliases]`.
func entityCandidates(entry *documentschema.EntityItem) []string {
	out := make([]string, 0, 1+len(entry.Aliases))
	out = append(out, entry.Name)
	out = append(out, entry.Aliases...)
	return out
}

// entriesForDomain mirrors `dictionary[domain]`.
func entriesForDomain(dictionary *documentschema.EntityDictionary, domain string) []*documentschema.EntityItem {
	switch domain {
	case "banks":
		return dictionary.Banks
	case "energy":
		return dictionary.Energy
	case "telecom":
		return dictionary.Telecom
	case "insurance":
		return dictionary.Insurance
	case "gov":
		return dictionary.Gov
	case "health":
		return dictionary.Health
	}
	return nil
}

// BuildEntityHintLine ports buildEntityHintLine.
//
// @param documentText When given, only entities this document actually mentions are listed.
//
// Without it every one of the dictionary's ~1,000 entities is emitted for every category on every
// classification. Measured on this corpus that was 44,873 of the 50,859-char category description
// (88%), pushing the full system prompt to ~67,500 chars — roughly 19k tokens against the
// num_ctx of 8192 pinned in ollama-client.ts. More than half the prompt, including the tail of the
// STEP 1..13 decision flow, was being truncated away before the model read it, and it grew with
// every auto-created subcategory. This is a correctness problem before it is a speed one.
//
// Dropping the list entirely would be safe — matchEntityDictionary applies the same dictionary
// deterministically in ruleBasedClassify, and Step A already extracts the issuer — but keeping the
// handful of entities the text mentions preserves the naming nudge at a fraction of the cost.
func BuildEntityHintLine(categoryID string, dictionary documentschema.EntityDictionary, documentText ...string) string {
	entries := []*documentschema.EntityItem{}
	for _, domain := range AllEntityDomains {
		if domainCategoryMap[domain] == categoryID {
			entries = append(entries, entriesForDomain(&dictionary, domain)...)
		}
	}

	if len(documentText) > 0 {
		haystack := strings.ToLower(documentText[0])
		filtered := make([]*documentschema.EntityItem, 0, len(entries))
		for _, entry := range entries {
			if entry == nil {
				continue
			}
			for _, candidate := range entityCandidates(entry) {
				needle := jsTrim(strings.ToLower(candidate))
				if needle != "" && strings.Contains(haystack, needle) {
					filtered = append(filtered, entry)
					break
				}
			}
		}
		entries = filtered
	}

	if len(entries) == 0 {
		return ""
	}
	parts := make([]string, 0, len(entries))
	for _, e := range entries {
		parts = append(parts, e.Slug+" ("+e.Name+")")
	}
	return " Known real-world entities: " + strings.Join(parts, ", ") + "."
}

// NormalizeSlug ports normalizeSlug:
//
//	str.normalize('NFD').replace(/[\u0300-\u036f]/g, '')
//	  .toLowerCase().trim()
//	  .replace(/[^a-z0-9_-]+/g, '_').replace(/^_+|_+$/g, '')
func NormalizeSlug(str string) string {
	// strip accents (é -> e) before collapsing, same as taxonomy.ts's cleanTitle
	folded := strings.ToLower(jsTrim(stripAccents(str)))
	folded = nonSlugCharsRe.ReplaceAllString(folded, "_")
	return strings.Trim(folded, "_")
}

// PreprocessRawText ports preprocessRawText.
//
// Preprocesses and normalizes raw extracted text by inserting spaces between fused/concatenated
// words, numbers, and currency symbols (commonly caused by PDF text stripping or OCR).
func PreprocessRawText(text string) string {
	if text == "" {
		return ""
	}
	// Separate colon boundaries (e.g. name:DUPOND -> name: DUPOND, Invoice#INV -> Invoice # INV)
	out := preColonRe.ReplaceAllString(text, "$1: $2")
	out = preHashRe.ReplaceAllString(out, "$1 # $2")
	// Separate camelCase boundaries (e.g. InvoiceDetails -> Invoice Details)
	out = preCamelRe.ReplaceAllString(out, "$1 $2")
	// Separate common fused document field keywords (e.g. Customersname -> Customers name,
	// Totalpayable -> Total payable). Deliberately narrow: this runs on the text Step A extracts
	// the issuer from and Step D writes titre/summary from, so a false split corrupts the value
	// that is then fed back to Step D as "GROUND TRUTH". The short, substring-prone tokens
	// (date, code, rate, item, info, vat, tax, price, total, amount, wrap) were removed because
	// they split ordinary words — "private" -> "pri vate", "corporate" -> "corpo rate" — and the
	// prefix now needs 5+ letters plus a non-letter tail so "surname"/"username" survive intact.
	out = replaceUnlessFollowedByASCIILetter(out, preKeywordRe, func(m []int) string {
		return out[m[2]:m[3]] + " " + out[m[4]:m[5]]
	})
	// 'date' needs a known field prefix rather than the generic {5,} rule: plenty of ordinary
	// words end in it and would be mangled ('candidate' -> 'candi date', 'consolidate',
	// 'liquidate'), while the real fusions all come from a small, closed set of field labels.
	out = replaceUnlessFollowedByASCIILetter(out, preDateRe, func(m []int) string {
		return out[m[2]:m[3]] + " " + out[m[4]:m[5]]
	})
	// Separate letter-number boundaries (e.g. Deliverydate02 -> Deliverydate 02)
	out = preLetterDigitRe.ReplaceAllString(out, "$1 $2")
	out = preDigitLetterRe.ReplaceAllString(out, "$1 $2")
	// Separate currency boundaries (e.g. Totalpayable€12.98 -> Totalpayable € 12.98)
	out = preCurrencyBeforeRe.ReplaceAllString(out, "$1 $2")
	out = preCurrencyAfterRe.ReplaceAllString(out, "$1 $2")
	out = preSpacesRe.ReplaceAllString(out, " ")
	return jsTrim(out)
}

// --- Ungrounded subcategory slug guard --------------------------------------------------
// When neither the curated regex list nor the entity dictionary recognizes a real entity,
// both the Qwen prompt (classifyPDFText) and ruleBasedClassify's last-resort fallback are
// tempted to invent a subcategory slug from the filename itself — e.g.
// "DcyJXe9MT9i7Un7tOlhU_StanW.pdf" -> "dcyjxe9mt9i7un7tolhu", "Page de confirmation.pdf"
// -> "page". That slug then gets permanently auto-created in categories.json (Golden Rule
// #5) even though it names nothing real. A "specific"-looking slug is only accepted here if
// it is actually grounded in the document's own text — not merely echoed from the filename
// or a generic/structural word.

var genericSlugDenylist = stringSet(
	"general", "other", "divers", "autre", "autres", "various", "misc", "note", "notes",
	"info", "page", "bon", "export", "scan", "copie", "copy", "document", "doc", "fichier",
	"file", "image", "confirmation", "recu", "releve", "extrait", "titre",
	"contrat", "facture", "attestation", "lettre", "avis", "bulletin", "certificat",
	"anyscanner", "camscanner", "geniusscan", "adobescan", "tinyscanner", "simplescan", "docscanner",
	"jpg", "jpeg", "png", "webp", "bmp", "tiff", "pdf", "txt", "docx", "xlsx",
)

// minGroundedSlugLength mirrors MIN_GROUNDED_SLUG_LENGTH.
const minGroundedSlugLength = 3

// filenameSlugTokens ports filenameSlugTokens.
func filenameSlugTokens(filename string) []string {
	cleanName := pdfSuffixRe.ReplaceAllString(filename, "")
	cleanName = filenameSeparatorsRe.ReplaceAllString(cleanName, "_")
	cleanName = strings.ToLower(cleanName)
	var out []string
	for _, w := range strings.Split(cleanName, "_") {
		if utf16Len(w) >= 3 && !allDigitsRe.MatchString(w) {
			out = append(out, w)
		}
	}
	return out
}

// isFilenameEchoedSlug ports isFilenameEchoedSlug.
func isFilenameEchoedSlug(slug, filename string) bool {
	wholeFilenameSlug := NormalizeSlug(pdfSuffixRe.ReplaceAllString(filename, ""))
	if slug == wholeFilenameSlug {
		return true
	}
	for _, t := range filenameSlugTokens(filename) {
		if t == slug || strings.Contains(slug, t) || strings.Contains(t, slug) {
			return true
		}
	}
	return false
}

// countSlugOccurrences ports countSlugOccurrences.
//
// Slugs are snake_case but real document text uses spaces/hyphens between words (e.g.
// slug "france_travail" must still match body text "France Travail"), so underscores
// become a flexible separator instead of a literal character. Also match French
// connecting words like "de", "d'", "du", "des", "demande".
func countSlugOccurrences(slug, text string) int {
	normText := stripAccents(text)
	normSlug := stripAccents(slug)
	re := slugOccurrenceRegexp(normSlug)
	if re == nil {
		return 0
	}
	return countBoundaryMatches(re, normText)
}

// IsGroundedSubcategorySlug ports isGroundedSubcategorySlug.
//
// True only if `slug` looks like a real-world entity name grounded in the document's own
// text, as opposed to a generic/structural word, gibberish, or an echo of the filename.
// Used to gate the dynamic subcategory auto-create path in both classifyPDFText and
// ruleBasedClassify. Exported for testing.
func IsGroundedSubcategorySlug(slug, rawText, filename string, personalNameDenylist []string) bool {
	if slug == "" || utf16Len(slug) < minGroundedSlugLength {
		return false
	}
	if _, ok := genericSlugDenylist[slug]; ok {
		return false
	}
	// The document owner's own name appears in nearly every header/footer (postal address,
	// "cher Monsieur/Madame", etc.), so a naive grounding check would mistake the owner for
	// the actual issuer/entity. personalNameDenylist filters the owner's own name out so it
	// is never mistaken for a grounding match.
	denylistSet := make(map[string]struct{}, len(personalNameDenylist))
	for _, n := range personalNameDenylist {
		denylistSet[strings.ToLower(jsTrim(n))] = struct{}{}
	}
	for _, part := range strings.Split(slug, "_") {
		if _, ok := denylistSet[part]; ok {
			return false
		}
	}

	occurrences := countSlugOccurrences(slug, rawText)
	if occurrences == 0 {
		// If body text OCR extracted little/no text (e.g. scanned image "Scanned with AnyScanner"),
		// but the slug's key compound words are explicitly present in the filename (e.g. 'vente_vehicule' in 'vehicule-Belleville-vente.pdf',
		// or 'declaration_vol' in 'declaration-de-vol.pdf'), accept it if it's a compound slug!
		fnTokens := filenameSlugTokens(filename)
		var slugParts []string
		for _, p := range strings.Split(slug, "_") {
			if utf16Len(p) >= 3 {
				slugParts = append(slugParts, p)
			}
		}
		matchesFilename := len(slugParts) >= 2
		if matchesFilename {
			for _, sp := range slugParts {
				found := false
				for _, ft := range fnTokens {
					if ft == sp || strings.Contains(ft, sp) || strings.Contains(sp, ft) {
						found = true
						break
					}
				}
				if !found {
					matchesFilename = false
					break
				}
			}
		}
		return matchesFilename
	}

	if isFilenameEchoedSlug(slug, filename) {
		// A slug that's also present in the filename is exactly what a hallucinating model
		// falls back to — require it to show up more than once in the body (letterhead,
		// footer, reference line, ...) rather than a single incidental mention.
		return occurrences >= 2
	}

	return true
}

// repairTruncatedJSON ports repairTruncatedJSON: close an unterminated string and any brackets
// left open, respecting string boundaries.
func repairTruncatedJSON(text string) string {
	result := text
	inString := false
	escaped := false
	var stack []rune

	for _, ch := range result {
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if ch == '{' || ch == '[' {
			stack = append(stack, ch)
		} else if ch == '}' && len(stack) > 0 && stack[len(stack)-1] == '{' {
			stack = stack[:len(stack)-1]
		} else if ch == ']' && len(stack) > 0 && stack[len(stack)-1] == '[' {
			stack = stack[:len(stack)-1]
		}
	}

	if inString {
		result += `"`
	}
	for len(stack) > 0 {
		open := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if open == '{' {
			result += "}"
		} else {
			result += "]"
		}
	}
	return result
}

// CleanAndParseJSON ports cleanAndParseJSON. It returns the decoded JSON value (the TS `any`) and a
// non-nil error where TS throws.
func CleanAndParseJSON(rawStr string) (any, error) {
	text := jsTrim(rawStr)
	text = fenceJSONRe.ReplaceAllString(text, "")
	text = fenceRe.ReplaceAllString(text, "")
	text = jsTrim(text)

	start := strings.Index(text, "{")
	if start == -1 {
		return nil, errors.New("No JSON object found in AI response")
	}
	text = text[start:]

	end := strings.LastIndex(text, "}")
	candidate := text
	if end != -1 {
		candidate = text[:end+1]
	}
	cleaned := trailingCommaRe.ReplaceAllString(candidate, "$1")

	var parsed any
	if err := json.Unmarshal([]byte(cleaned), &parsed); err == nil {
		return parsed, nil
	}

	// Response was truncated mid-generation — the context window ran out before the model
	// finished. Repair by closing any unterminated string and any brackets left open,
	// respecting string boundaries, then retry.
	//
	// The repair is deliberately loud. Silently patching the JSON is how a real bug hid for the
	// life of this corpus: at num_ctx 8192 the reply was cut off after `tags`, this closed the
	// object, the schema filled the missing keys with '', and every downstream layer saw a valid
	// result — so total_amount, vat_amount, siren, iban, expiry_date and all five contact_*
	// fields were empty on all 774 documents with nothing anywhere reporting a problem.
	//
	// The TS console.warn is a logging side effect and is not reproduced (package comment
	// deviation 7); the repair itself is identical.
	repaired := trailingCommaRe.ReplaceAllString(repairTruncatedJSON(text), "$1")
	if err := json.Unmarshal([]byte(repaired), &parsed); err != nil {
		return nil, err
	}
	return parsed, nil
}

// RuleBasedClassify ports ruleBasedClassify. The optional personalization defaults to
// EMPTY_PROMPT_PERSONALIZATION exactly as the TS default parameter does.
func RuleBasedClassify(rawText, filename string, dictionary documentschema.EntityDictionary, personalNameDenylist []string, personalization ...promptpersonalization.PromptPersonalization) RuleBasedClassifyResult {
	p := promptpersonalization.EMPTY_PROMPT_PERSONALIZATION
	if len(personalization) > 0 {
		p = personalization[0]
	}

	combined := strings.ToLower(filename + " " + utf16Slice(rawText, 4000))

	// Generic bank-statement signal phrases (same signals as the Qwen prompt's STEP 1)
	// used to guard the gov (7b) and insurance-dictionary (8) branches so a
	// Crédit Mutuel / Chase / Barclays relevé isn't misfiled via a transaction-row mention of
	// CAF / AXA / etc. (Golden Rule #6 "archetypal trap").
	//
	// Bank-specific statement filename codes and account-product names are personal, so they are
	// not literals here — they come from the gitignored .prompts.private.json overlay, the same
	// source that feeds the prompt's STEP 0. A statement whose only signal is such a code is
	// recognized through `priorityMatch` below.
	priorityMatch := promptpersonalization.MatchPriorityRules(combined, p, filename)
	looksLikeBankStatement := bankStatementRe.MatchString(combined) ||
		(priorityMatch != nil && priorityMatch.Categorie == "bank")

	// Semantic anchors mirroring the prompt's STEP 2/3, the same way looksLikeBankStatement mirrors
	// STEP 1: a tax notice or a pay slip is never outranked by a STEP 0 overlay rule whose target
	// category disagrees. This is what the 2026-08-31 regression needed: 'paiement'/'échéance'
	// (auto-learned from "calendrier de paiement.PDF" -> invoices/cdiscount) fired on the body text
	// of an income-tax notice and a property-tax notice, filing both under the wrong category.
	//
	// Deliberately notice-level discrimination for property tax: 'taxe foncière' ALONE is not
	// enough — Foncia quittances list it among the recoverable charges — so a taxe foncière /
	// taxe d'habitation notice needs a second tax-authority signal (somme à payer, montant de
	// votre impôt, date limite de paiement, taux d'imposition, impots.gouv.fr) to count.
	looksLikeTaxNotice := taxNoticeMainRe.MatchString(combined) ||
		(taxNoticePropertyRe.MatchString(combined) && taxNoticeCorroborationRe.MatchString(combined))
	looksLikePayslip := payslipSignalRe.MatchString(combined)

	categorie := "administrative"
	subcategorie := "general"
	reason := "Default administrative fallback"

	// 0a. User overlay overrides — the deterministic mirror of the prompt's STEP 0
	// (.prompts.private.json). Golden Rule #6 still wins: a non-bank override never beats a bank
	// statement, so a landlord / vendor / employer name appearing only inside a statement's
	// transaction rows cannot pull the document out of 'bank'. The same anchor applies to tax
	// notices (STEP 2) and pay slips (STEP 3): a generic-keyword overlay whose target disagrees
	// is rejected so the document falls through to its real branch below.
	if priorityMatch != nil &&
		!(looksLikeBankStatement && priorityMatch.Categorie != "bank") &&
		!(looksLikeTaxNotice && priorityMatch.Categorie != "administrative") &&
		!(looksLikePayslip && priorityMatch.Categorie != "bulletin_salaire") {
		categorie = priorityMatch.Categorie
		subcategorie = priorityMatch.Subcategorie
		reason = fmt.Sprintf("Matched user overlay priority rule '%s' -> %s/%s", priorityMatch.Keyword, categorie, subcategorie)
	} else if looksLikeBankStatement {
		// 0b. Bank Statements, Check Statements & Savings Summaries (High Priority Override - Golden Rule #6)
		categorie = "bank"
		if bnpRe.MatchString(combined) {
			subcategorie = "bnp_paribas"
			reason = "Matched bank statement pattern -> BNP Paribas"
		} else if creditMutuelRe.MatchString(combined) {
			subcategorie = "credit_mutuel"
			reason = "Matched bank statement pattern -> Crédit Mutuel"
		} else if societeGeneraleRe.MatchString(combined) {
			subcategorie = "societe_generale"
			reason = "Matched bank statement pattern -> Société Générale"
		} else if boursoRe.MatchString(combined) {
			subcategorie = "boursobank"
			reason = "Matched bank statement pattern -> BoursoBank"
		} else if lclRe.MatchString(combined) {
			subcategorie = "lcl"
			reason = "Matched bank statement pattern -> LCL"
		} else if banquePostaleRe.MatchString(combined) {
			subcategorie = "la_banque_postale"
			reason = "Matched bank statement pattern -> La Banque Postale"
		} else {
			dictBank := MatchEntityDictionary(combined, []string{"banks"}, dictionary)
			if dictBank != nil {
				subcategorie = dictBank.Subcategorie
				reason = fmt.Sprintf("Matched bank statement pattern -> %s (via dictionary)", dictBank.Subcategorie)
			} else {
				subcategorie = "releve_bancaire"
				reason = "Matched generic bank statement pattern"
			}
		}
	} else if finesRe.MatchString(combined) {
		// 0c. Amendes / Traffic Fines & Penalty Receipts.
		// Deliberately AFTER the bank-statement branch, mirroring prompts/classification_rules.md where
		// STEP 1 (bank) precedes STEP 1B (fines). When it ran first, a Crédit Mutuel relevé listing a
		// single ANTAI debit row returned administrative/amende — the archetypal Golden Rule #6 trap the
		// rest of this function is built to avoid, in the one branch that sat above the guard.
		categorie = "administrative"
		subcategorie = "amende"
		reason = "Matched traffic fine / penalty pattern (amendes.gouv.fr/antai)"
	} else if payslipBranchRe.MatchString(combined) {
		// Specific Bulletin de Salaire / Pay Slips Category (Universal for all employers/companies)
		categorie = "bulletin_salaire"
		dictEmployer := MatchEntityDictionary(combined, AllEntityDomains, dictionary)
		if dictEmployer != nil {
			subcategorie = dictEmployer.Subcategorie
		} else {
			cleanName := strings.ToLower(filenameSeparatorsRe.ReplaceAllString(pdfSuffixRe.ReplaceAllString(filename, ""), "_"))
			words := filenameWords(cleanName, 3, bulletinFilenameStopWords)
			if len(words) > 0 {
				candidateSlug := NormalizeSlug(words[0])
				if IsGroundedSubcategorySlug(candidateSlug, rawText, filename, personalNameDenylist) {
					subcategorie = candidateSlug
				} else {
					subcategorie = "employeur"
				}
			} else {
				subcategorie = "employeur"
			}
		}
	} else if internshipRe.MatchString(combined) {
		// Internship Attestations (Universal for all educational institutions & companies)
		categorie = "education"
		subcategorie = "attestation_stage"
	} else if twoDdocRe.MatchString(combined) {
		// 2DDoc Domicile Proof Attestations
		categorie = "housing"
		subcategorie = "justificatif_domicile"
	} else if contractsRe.MatchString(combined) {
		// 1. Contracts, Commercial Mandates & General Conditions & Company Incorporation
		categorie = "contracts"
		if contractConditionsRe.MatchString(combined) {
			subcategorie = "conditions_generales"
		} else if contractAttestationEmployeurRe.MatchString(combined) {
			subcategorie = "attestation_employeur"
		} else if contractStatutsRe.MatchString(combined) {
			subcategorie = "statuts_societe"
		} else if contractSepaRe.MatchString(combined) {
			subcategorie = "mandat_sepa"
		} else {
			subcategorie = "cdi_cdd"
		}
	} else if identityMainRe.MatchString(combined) || letterBoundaryContains(combined, "cni") {
		// 2. Identity & Passports & Civil Records & Vehicle Cession
		categorie = "identity"
		if identityPasseportRe.MatchString(combined) {
			subcategorie = "passeport"
		} else if identityRecipisseRe.MatchString(combined) {
			subcategorie = "recipisse_sejour"
		} else if identityTitreSejourRe.MatchString(combined) {
			subcategorie = "titre_sejour"
		} else if identityCarteVitaleRe.MatchString(combined) {
			subcategorie = "carte_vitale"
		} else if identityPermisRe.MatchString(combined) {
			subcategorie = "permis_conduire"
		} else if identityCarteGriseRe.MatchString(combined) {
			subcategorie = "carte_grise"
		} else if identityCarteIdentiteRe.MatchString(combined) || letterBoundaryContains(combined, "cni") {
			subcategorie = "carte_identite"
		} else if identityActeMariageRe.MatchString(combined) {
			subcategorie = "acte_mariage"
		}
	} else if healthRe.MatchString(combined) {
		// 3. Health / Medical & Work Stoppages
		categorie = "health"
		if healthArretRe.MatchString(combined) {
			subcategorie = "arret_travail"
		} else if healthAmeliRe.MatchString(combined) {
			subcategorie = "ameli"
		} else {
			dictHealth := MatchEntityDictionary(combined, []string{"health"}, dictionary)
			if dictHealth != nil {
				subcategorie = dictHealth.Subcategorie
			}
		}
	} else if !looksLikeBankStatement && housingRe.MatchString(combined) {
		// 4. Housing & Domicile Proof & Transport Schedules
		if housingTransportRe.MatchString(combined) {
			categorie = "administrative"
			subcategorie = "navigo"
		} else {
			categorie = "housing"
			subcategorie = "justificatif_domicile"
		}
	} else if educationRe.MatchString(combined) {
		// 5. Education & Academic Diplomas & Transcripts
		categorie = "education"
		if educationAlternanceRe.MatchString(combined) {
			subcategorie = "alternance"
		} else if educationReleveNotesRe.MatchString(combined) {
			subcategorie = "releve_notes"
		} else if educationDiplomesRe.MatchString(combined) {
			subcategorie = "diplomes"
		} else {
			// A school / training-provider name is personal, so it is not a literal here. The
			// overlay's STEP 0 rules catch a known one earlier; the dictionary is the generic path.
			dictSchool := MatchEntityDictionary(combined, AllEntityDomains, dictionary)
			if dictSchool != nil {
				subcategorie = dictSchool.Subcategorie
			}
		}
	} else if invoicesRe.MatchString(combined) {
		// 6. Enterprise Invoices (Client Sales vs Supplier Purchases)
		isClientInvoice := clientInvoiceRe.MatchString(combined)
		if isClientInvoice {
			categorie = "factures_clients"
		} else {
			categorie = "invoices"
		}

		if invoiceSfrRe.MatchString(combined) {
			subcategorie = "sfr"
		} else if invoiceEdfRe.MatchString(combined) {
			subcategorie = "edf"
		} else if invoiceEngieRe.MatchString(combined) {
			subcategorie = "engie"
		} else if invoiceCdiscountRe.MatchString(combined) {
			subcategorie = "cdiscount"
		} else if invoiceAmazonRe.MatchString(combined) {
			subcategorie = "amazon"
		} else {
			dictVendor := MatchEntityDictionary(combined, []string{"telecom", "energy"}, dictionary)
			if dictVendor != nil {
				subcategorie = dictVendor.Subcategorie
			} else {
				dictInsuranceViaFacture := MatchEntityDictionary(combined, []string{"insurance"}, dictionary)
				if dictInsuranceViaFacture != nil {
					categorie = dictInsuranceViaFacture.Categorie
					subcategorie = dictInsuranceViaFacture.Subcategorie
				} else {
					// Dynamic Client or Vendor company name extraction from filename grounding
					cleanName := strings.ToLower(filenameSeparatorsRe.ReplaceAllString(pdfSuffixRe.ReplaceAllString(filename, ""), "_"))
					words := filenameWords(cleanName, 3, invoiceFilenameStopWords)
					if len(words) > 0 {
						candidateSlug := NormalizeSlug(words[0])
						if IsGroundedSubcategorySlug(candidateSlug, rawText, filename, personalNameDenylist) {
							subcategorie = candidateSlug
						}
					}
				}
			}
		}
	} else if !looksLikeBankStatement && taxesRe.MatchString(combined) {
		// 7. Taxes & Kbis & Company Registration Statements
		categorie = "administrative"
		if taxesKbisRe.MatchString(combined) {
			subcategorie = "kbis"
		} else if taxesDossierRe.MatchString(combined) {
			subcategorie = "dossier_administratif"
		} else {
			subcategorie = "impot"
		}
	} else if !looksLikeBankStatement && MatchEntityDictionary(combined, []string{"gov"}, dictionary) != nil {
		// 7b. Government & Social Agencies
		dictGov := MatchEntityDictionary(combined, []string{"gov"}, dictionary)
		categorie = dictGov.Categorie
		subcategorie = dictGov.Subcategorie
	} else if insuranceRe.MatchString(combined) || (!looksLikeBankStatement && MatchEntityDictionary(combined, []string{"insurance"}, dictionary) != nil) {
		// 8. Insurance / Assurances & Theft Claims
		categorie = "insurance"
		if insuranceTheftRe.MatchString(combined) {
			subcategorie = "declaration_vol"
		} else if insuranceAllianzRe.MatchString(combined) {
			subcategorie = "allianz"
		} else {
			dictInsurance := MatchEntityDictionary(combined, []string{"insurance"}, dictionary)
			if dictInsurance != nil {
				subcategorie = dictInsurance.Subcategorie
			}
		}
	} else if MatchEntityDictionary(combined, []string{"banks"}, dictionary) != nil {
		// 9. Banks / Finance (Fallback if not caught by high-priority looksLikeBankStatement above)
		dictBank := MatchEntityDictionary(combined, []string{"banks"}, dictionary)
		categorie = dictBank.Categorie
		subcategorie = dictBank.Subcategorie
	} else if recruitmentRe.MatchString(combined) {
		// 10. Recruitment
		categorie = "recruitment"
	} else if correspondenceRe.MatchString(combined) {
		// 11. Correspondence
		categorie = "correspondence"
	} else if technicalRe.MatchString(combined) {
		// 12. Technical
		categorie = "technical"
	} else if reportsRe.MatchString(combined) {
		// 13. Reports
		categorie = "reports"
	}

	// Exact Subcategory Fallbacks & Dynamic Subcategory Generation from Filename Keywords
	if subcategorie == "general" {
		if carrefourRe.MatchString(combined) {
			subcategorie = "carrefour"
		} else if kairosRe.MatchString(combined) {
			subcategorie = "kairos"
		} else if generalAllianzRe.MatchString(combined) {
			subcategorie = "allianz"
		} else if generalSfrRe.MatchString(combined) {
			subcategorie = "sfr"
		} else if generalEdfRe.MatchString(combined) {
			subcategorie = "edf"
		} else if generalEngieRe.MatchString(combined) {
			subcategorie = "engie"
		} else if generalBouyguesRe.MatchString(combined) {
			subcategorie = "bouygues"
		} else if generalFreeRe.MatchString(combined) {
			subcategorie = "free"
		} else if generalAmeliRe.MatchString(combined) {
			subcategorie = "ameli"
		} else if generalNavigoRe.MatchString(combined) {
			subcategorie = "navigo"
		} else if generalCdiscountRe.MatchString(combined) {
			subcategorie = "cdiscount"
		} else if generalAmazonRe.MatchString(combined) {
			subcategorie = "amazon"
		} else if generalFnacRe.MatchString(combined) {
			subcategorie = "fnac"
		} else if generalPageConfirmationRe.MatchString(combined) {
			categorie = "administrative"
			subcategorie = "attestation_confirmation"
		} else if generalCpfRe.MatchString(combined) {
			categorie = "education"
			subcategorie = "cpf"
		} else if generalBulletinFallbackRe.MatchString(combined) {
			categorie = "bulletin_salaire"
			subcategorie = "bulletin_salaire"
		} else if generalFactureFallbackRe.MatchString(combined) {
			if categorie != "factures_clients" {
				categorie = "invoices"
			}
			subcategorie = "facture"
		} else if dictAny := MatchEntityDictionary(combined, AllEntityDomains, dictionary); dictAny != nil {
			categorie = dictAny.Categorie
			subcategorie = dictAny.Subcategorie
		} else {
			// Dynamic Subcategory Extraction from Filename Words — ONLY accepted if the
			// resulting slug is actually grounded in the document text (isGroundedSubcategorySlug
			// above). Previously this unconditionally promoted a filename fragment (or a fully
			// random filename) to a permanent subcategory; now an ungrounded candidate is left as
			// 'general' so the caller's strict fail guard (Golden Rule #4) can BLOCK it instead.
			cleanName := strings.ToLower(filenameSeparatorsRe.ReplaceAllString(pdfSuffixRe.ReplaceAllString(filename, ""), "_"))
			words := filenameWords(cleanName, 3, generalFilenameStopWords)
			if len(words) > 0 {
				candidate := ""
				for _, w := range words {
					if _, ok := generalCandidateStopWords[w]; !ok {
						candidate = w
						break
					}
				}
				if candidate == "" {
					candidate = words[0]
				}
				if utf16Len(candidate) >= 3 {
					candidateSlug := NormalizeSlug(candidate)
					if IsGroundedSubcategorySlug(candidateSlug, rawText, filename, personalNameDenylist) {
						subcategorie = candidateSlug
					}
				}
			}
		}
	}

	date := time.Now().UTC().Format("2006-01-02")
	if m := compactDateRe.FindStringSubmatch(combined); m != nil {
		date = m[1] + "-" + m[2] + "-" + m[3]
	} else if m := dateYMDRe.FindStringSubmatch(combined); m != nil {
		date = m[1] + "-" + m[2] + "-" + m[3]
	} else if m := dateDMYRe.FindStringSubmatch(combined); m != nil {
		date = m[3] + "-" + m[2] + "-" + m[1]
	}

	title := jsTrim(titleSeparatorRe.ReplaceAllString(pdfSuffixRe.ReplaceAllString(filename, ""), " "))

	isPaid := paidRe.MatchString(combined)
	isUnpaid := unpaidRe.MatchString(combined)
	paymentStatus := "UNKNOWN"
	if isPaid {
		paymentStatus = "PAID"
	} else if isUnpaid {
		paymentStatus = "UNPAID"
	}
	invoiceType := "NONE"
	if categorie == "factures_clients" {
		invoiceType = "CLIENT"
	} else if categorie == "invoices" {
		invoiceType = "SUPPLIER"
	}

	return RuleBasedClassifyResult{
		Categorie:     categorie,
		Subcategorie:  subcategorie,
		Title:         title,
		Date:          date,
		Reason:        reason,
		PaymentStatus: paymentStatus,
		InvoiceType:   invoiceType,
	}
}

// ExtractRuleBasedContact ports extractRuleBasedContact.
//
// Regex guessing disabled by user request — contact fields are populated solely via AI model JSON output
func ExtractRuleBasedContact(rawText string) RuleBasedContact {
	// Regex guessing disabled by user request — contact fields are populated solely via AI model JSON output
	_ = rawText
	return RuleBasedContact{}
}

// FormatLocalDate ports formatLocalDate.
//
// Formats a Date using its LOCAL calendar fields (not toISOString, which converts to UTC and
// shifts "today" by a day for any timezone ahead of UTC — e.g. Europe/Paris at local midnight).
func FormatLocalDate(d time.Time) string {
	return d.Format("2006-01-02")
}

// parseDateParts ports parseDateParts.
func parseDateParts(dateStr string) *dateParts {
	str := jsTrim(dateStr)

	if iso := isoDateRe.FindStringSubmatch(str); iso != nil {
		return &dateParts{Year: atoi(iso[1]), Month: atoi(iso[2]), Day: atoi(iso[3])}
	}

	if frLong := frLongDateRe.FindStringSubmatch(str); frLong != nil {
		return &dateParts{Year: atoi(frLong[3]), Month: atoi(frLong[2]), Day: atoi(frLong[1])}
	}

	if frShort := frShortDateRe.FindStringSubmatch(str); frShort != nil {
		return &dateParts{Year: 2000 + atoi(frShort[3]), Month: atoi(frShort[2]), Day: atoi(frShort[1])}
	}

	return nil
}

// dateParts mirrors the TS parseDateParts return object.
type dateParts struct {
	Year  int
	Month int
	Day   int
}

// ReconcileDocumentDate ports reconcileDocumentDate.
//
// Defense-in-depth guard for Step D's "date" field: the classification LLM occasionally
// mis-derives a two-digit-year date from OCR-garbled source text (e.g. "30/11/26" read from a
// printed "30/11/25"), producing a "date" later than today for an otherwise-past document. When
// that happens AND the document's own titre states a different, non-future year (payslip titles
// always carry the pay period's year — "Bulletin de salaire - Novembre 2025"), trust the titre's
// year over the ambiguous digit. Declines to touch anything it isn't confident about (no titre
// year found, titre year matches already, or the "corrected" date would itself land in the future).
func ReconcileDocumentDate(rawDate, titre string, now ...time.Time) DateReconciliation {
	nowTime := time.Now()
	if len(now) > 0 {
		nowTime = now[0]
	}

	parsed := parseDateParts(rawDate)
	if parsed == nil {
		return DateReconciliation{Date: rawDate, Corrected: false}
	}

	// new Date(y, m, d) uses the LOCAL calendar, so build local midnights regardless of the
	// representation of nowTime (package comment deviation 8).
	parsedTimestamp := time.Date(parsed.Year, time.Month(parsed.Month), parsed.Day, 0, 0, 0, 0, time.Local)
	if !parsedTimestamp.After(nowTime) {
		return DateReconciliation{Date: rawDate, Corrected: false}
	}

	titleYearMatch := titleYearRe.FindStringSubmatch(titre)
	if titleYearMatch == nil {
		return DateReconciliation{Date: rawDate, Corrected: false}
	}

	titleYear := atoi(titleYearMatch[1])
	if titleYear == parsed.Year {
		return DateReconciliation{Date: rawDate, Corrected: false}
	}

	correctedTimestamp := time.Date(titleYear, time.Month(parsed.Month), parsed.Day, 0, 0, 0, 0, time.Local)
	if correctedTimestamp.After(nowTime) {
		return DateReconciliation{Date: rawDate, Corrected: false}
	}

	correctedDate := fmt.Sprintf("%d-%02d-%02d", titleYear, parsed.Month, parsed.Day)
	return DateReconciliation{
		Date:      correctedDate,
		Corrected: true,
		Reason: fmt.Sprintf(
			"\"%s\" is later than today (%s); corrected year to %d to match the titre's stated period",
			rawDate, FormatLocalDate(nowTime), titleYear,
		),
	}
}

// BuildCategoriesDescriptionStr ports buildCategoriesDescriptionStr. The optional documentText is
// forwarded to BuildEntityHintLine exactly as in TS.
func BuildCategoriesDescriptionStr(categoriesConfig documentschema.CategoriesConfig, dictionary documentschema.EntityDictionary, documentText ...string) string {
	lines := make([]string, 0, len(categoriesConfig.Categories))
	for _, c := range categoriesConfig.Categories {
		if c == nil {
			continue
		}
		subsStr := "none"
		if c.Subcategories != nil {
			ids := make([]string, 0, len(c.Subcategories))
			for _, s := range c.Subcategories {
				if s == nil {
					continue
				}
				ids = append(ids, s.ID)
			}
			subsStr = strings.Join(ids, ", ")
		}
		var entityHint string
		if len(documentText) > 0 {
			entityHint = BuildEntityHintLine(c.ID, dictionary, documentText[0])
		} else {
			entityHint = BuildEntityHintLine(c.ID, dictionary)
		}
		lines = append(lines, fmt.Sprintf("- Category '%s' (%s): %s. Existing subcategories: [%s].%s", c.ID, c.Name, c.Description, subsStr, entityHint))
	}
	return strings.Join(lines, "\n")
}

// ---- Shared helpers ----

// jsWhitespaceClass is the exact character set matched by JavaScript's `\s`: WhiteSpace plus
// LineTerminator, inlined into every regex below (see package comment deviation 2).
const jsWhitespaceClass = `\t\n\v\f\r \x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

// jsWs is the JS `\s` class.
const jsWs = "[" + jsWhitespaceClass + "]"

// jsDotClass is JavaScript's `.`: any character except a line terminator.
const jsDotClass = `[^\n\r\x{2028}\x{2029}]`

var (
	nonSlugCharsRe       = regexp.MustCompile(`[^a-z0-9_-]+`)
	allDigitsRe          = regexp.MustCompile(`^\d+$`)
	pdfSuffixRe          = regexp.MustCompile(`(?i)\.pdf$`)
	filenameSeparatorsRe = regexp.MustCompile("[-_" + jsWhitespaceClass + "]+")
	titleSeparatorRe     = regexp.MustCompile(`[-_]+`)

	fenceJSONRe     = regexp.MustCompile("(?i)```(?:json)?")
	fenceRe         = regexp.MustCompile("```")
	trailingCommaRe = regexp.MustCompile("," + jsWs + `*([}\]])`)

	isoDateRe     = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)
	frLongDateRe  = regexp.MustCompile(`^(\d{2})[/.\-](\d{2})[/.\-](\d{4})$`)
	frShortDateRe = regexp.MustCompile(`^(\d{2})[/.\-](\d{2})[/.\-](\d{2})$`)
	titleYearRe   = regexp.MustCompile(`\b(20\d{2})\b`)
)

// accentFold maps precomposed Latin-1/Latin Extended-A letters to the ASCII base letter that JS
// NFD normalization plus combining-mark stripping would produce. See the package comment for why
// this table exists instead of a Unicode normalizer. Letters with no canonical decomposition
// (œ, æ, ø, ß, đ, ł, …) are deliberately absent so they are dropped by the alphanumeric filter,
// exactly as they are by the TS `[^a-z0-9]+` removal.
var accentFold = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'ç': 'c',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'ñ': 'n',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u',
	'ý': 'y', 'ÿ': 'y',
	'ā': 'a', 'ă': 'a', 'ą': 'a',
	'ć': 'c', 'ĉ': 'c', 'ċ': 'c', 'č': 'c',
	'ď': 'd',
	'ē': 'e', 'ĕ': 'e', 'ė': 'e', 'ę': 'e', 'ě': 'e',
	'ĝ': 'g', 'ğ': 'g', 'ġ': 'g', 'ģ': 'g',
	'ĥ': 'h',
	'ĩ': 'i', 'ī': 'i', 'ĭ': 'i', 'į': 'i',
	'ĵ': 'j',
	'ķ': 'k',
	'ĺ': 'l', 'ļ': 'l', 'ľ': 'l',
	'ń': 'n', 'ņ': 'n', 'ň': 'n',
	'ō': 'o', 'ŏ': 'o', 'ő': 'o',
	'ŕ': 'r', 'ŗ': 'r', 'ř': 'r',
	'ś': 's', 'ŝ': 's', 'ş': 's', 'š': 's',
	'ţ': 't', 'ť': 't',
	'ũ': 'u', 'ū': 'u', 'ŭ': 'u', 'ů': 'u', 'ű': 'u', 'ų': 'u',
	'ŵ': 'w',
	'ŷ': 'y',
	'ź': 'z', 'ż': 'z', 'ž': 'z',
}

// stripAccents ports `str.normalize('NFD').replace(/[\u0300-\u036f]/g, ”)` (with a lowercasing
// convenience; every caller is either about to lowercase or is case-insensitive). See package
// comment deviation 3.
func stripAccents(s string) string {
	var b strings.Builder
	for _, r := range s {
		// Strip combining diacritics so already-decomposed input folds the same way NFD would.
		if r >= 0x0300 && r <= 0x036F {
			continue
		}
		r = unicode.ToLower(r)
		if base, ok := accentFold[r]; ok {
			r = base
		}
		b.WriteRune(r)
	}
	return b.String()
}

// jsTrim is `String.prototype.trim()`: it strips exactly the JavaScript whitespace set. Go's
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

// utf16Slice reproduces `str.substring(0, n)`: the first n UTF-16 code units. A rune whose code
// units would straddle the cut is dropped; JS would keep a lone surrogate there, which a Go UTF-8
// string cannot represent.
func utf16Slice(s string, n int) string {
	if n <= 0 {
		return ""
	}
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// literalBoundaryMatch ports `(?<![\p{L}\p{N}])needle(?![\p{L}\p{N}])` with the `iu` flags for a
// literal needle, without RE2 lookaround (package comment deviation 1). It finds every occurrence
// and accepts the first whose adjacent runes are not letters or numbers.
func literalBoundaryMatch(haystack, needle string) bool {
	pos := 0
	for {
		i := strings.Index(haystack[pos:], needle)
		if i < 0 {
			return false
		}
		start := pos + i
		end := start + len(needle)
		if !runeBeforeIsLetterOrNumber(haystack, start) && !runeAtIsLetterOrNumber(haystack, end) {
			return true
		}
		_, size := utf8.DecodeRuneInString(haystack[start:])
		if size == 0 {
			return false
		}
		pos = start + size
	}
}

// letterBoundaryContains ports `(?<!\p{L})needle(?!\p{L})` (letters only), used for the `cni`
// alternative lifted out of the identity regexes.
func letterBoundaryContains(haystack, needle string) bool {
	h := strings.ToLower(haystack)
	n := strings.ToLower(needle)
	pos := 0
	for {
		i := strings.Index(h[pos:], n)
		if i < 0 {
			return false
		}
		start := pos + i
		end := start + len(n)
		if !runeBeforeIsLetter(h, start) && !runeAtIsLetter(h, end) {
			return true
		}
		_, size := utf8.DecodeRuneInString(h[start:])
		if size == 0 {
			return false
		}
		pos = start + size
	}
}

func runeBeforeIsLetterOrNumber(s string, i int) bool {
	if i <= 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func runeAtIsLetterOrNumber(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func runeBeforeIsLetter(s string, i int) bool {
	if i <= 0 {
		return false
	}
	r, _ := utf8.DecodeLastRuneInString(s[:i])
	return unicode.IsLetter(r)
}

func runeAtIsLetter(s string, i int) bool {
	if i >= len(s) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(s[i:])
	return unicode.IsLetter(r)
}

// replaceUnlessFollowedByASCIILetter ports a global regex replace whose pattern ends with the
// lookahead `(?![a-z])`: a match is replaced only when the next rune is not an ASCII letter
// (the `i` flag makes `[a-z]` case-insensitive). matches come from a regex compiled without the
// lookahead; a match whose guard fails is left in place by not advancing `last`.
func replaceUnlessFollowedByASCIILetter(s string, re *regexp.Regexp, build func(m []int) string) string {
	matches := re.FindAllStringSubmatchIndex(s, -1)
	if matches == nil {
		return s
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		start, end := m[0], m[1]
		if end < len(s) {
			r, _ := utf8.DecodeRuneInString(s[end:])
			if isASCIILetter(r) {
				continue
			}
		}
		b.WriteString(s[last:start])
		b.WriteString(build(m))
		last = end
	}
	b.WriteString(s[last:])
	return b.String()
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// slugOccurrenceRegexp builds the countSlugOccurrences pattern from a deaccented slug, replacing
// each `_` with the flexible separator alternation and escaping every other rune literally. Returns
// nil when the slug is empty (the TS `if (!escaped) return 0`).
func slugOccurrenceRegexp(normSlug string) *regexp.Regexp {
	if normSlug == "" {
		return nil
	}
	var b strings.Builder
	b.WriteString("(?i)")
	for _, r := range normSlug {
		if r == '_' {
			b.WriteString(`(?:[` + jsWhitespaceClass + `_-]+(?:de[` + jsWhitespaceClass + `]+|d['’]|du[` + jsWhitespaceClass + `]+|des[` + jsWhitespaceClass + `]+|demande[` + jsWhitespaceClass + `]+)?|[` + jsWhitespaceClass + `_-]*)`)
			continue
		}
		b.WriteString(regexp.QuoteMeta(string(r)))
	}
	re, err := regexp.Compile(b.String())
	if err != nil {
		return nil
	}
	return re
}

// countBoundaryMatches performs a global regex scan with the `(?<![\p{L}\p{N}])…(?![\p{L}\p{N}])`
// boundary guards that RE2 cannot express. Accepted matches advance past their end; a match rejected
// by the guards advances one rune so a later valid start is still considered, mirroring the JS
// engine's scan.
func countBoundaryMatches(re *regexp.Regexp, text string) int {
	count := 0
	pos := 0
	for pos <= len(text) {
		loc := re.FindStringIndex(text[pos:])
		if loc == nil {
			break
		}
		start := pos + loc[0]
		end := pos + loc[1]
		if !runeBeforeIsLetterOrNumber(text, start) && !runeAtIsLetterOrNumber(text, end) {
			count++
			if end == start {
				_, size := utf8.DecodeRuneInString(text[end:])
				if size == 0 {
					break
				}
				pos = end + size
			} else {
				pos = end
			}
		} else {
			_, size := utf8.DecodeRuneInString(text[start:])
			if size == 0 {
				break
			}
			pos = start + size
		}
	}
	return count
}

// filenameWords ports `cleanName.split('_').filter(w => w.length >= minLen && !/^\d+$/.test(w) &&
// !stop.includes(w))`. minLen 3 covers both the `>= 3` and `> 2` call sites.
func filenameWords(cleanName string, minLen int, stop map[string]struct{}) []string {
	var out []string
	for _, w := range strings.Split(cleanName, "_") {
		if utf16Len(w) < minLen {
			continue
		}
		if allDigitsRe.MatchString(w) {
			continue
		}
		if _, ok := stop[w]; ok {
			continue
		}
		out = append(out, w)
	}
	return out
}

// stringSet mirrors a TS `new Set([...])` membership test.
func stringSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, v := range values {
		out[v] = struct{}{}
	}
	return out
}

// atoi is a best-effort integer parse for the regex-captured date fields, matching JS unary `+`
// for the digits these patterns can capture.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// Filename stop-word sets, ported from the three inline arrays in ruleBasedClassify.
var (
	bulletinFilenameStopWords = stringSet("bulletin", "salaire", "paie", "fiche", "payslip", "paystub", "pdf", "doc", "copy", "scan", "de", "du", "des", "le", "la", "les")
	invoiceFilenameStopWords  = stringSet("facture", "invoice", "bill", "receipt", "client", "fournisseur", "supplier", "pdf", "doc", "copy", "scan", "n°", "no")
	generalFilenameStopWords  = stringSet("pdf", "doc", "document", "copy", "scan", "the", "and", "for", "mon", "mes", "une", "des", "sur", "les", "par")
	generalCandidateStopWords = stringSet("contrat", "facture", "attestation", "lettre", "avis", "bulletin", "certificat")
)

// ---- Regexes ported from classification.ts ----

var (
	// preprocessRawText regexes.
	preColonRe          = regexp.MustCompile(`([a-zA-Z0-9]):([a-zA-Z0-9])`)
	preHashRe           = regexp.MustCompile(`([a-zA-Z0-9])#([a-zA-Z0-9])`)
	preCamelRe          = regexp.MustCompile(`([a-z])([A-Z])`)
	preKeywordRe        = regexp.MustCompile(`(?i)([a-zA-Z]{5,})(name|address|number|payable|details|subtotal|charges|promotions|invoice|seller|buyer)`)
	preDateRe           = regexp.MustCompile(`(?i)(invoice|delivery|order|due|issue|payment|start|end|birth|expiry)(date)`)
	preLetterDigitRe    = regexp.MustCompile(`([a-zA-Z])([0-9])`)
	preDigitLetterRe    = regexp.MustCompile(`([0-9])([a-zA-Z])`)
	preCurrencyBeforeRe = regexp.MustCompile(`([a-zA-Z0-9])([€$£])`)
	preCurrencyAfterRe  = regexp.MustCompile(`([€$£])([0-9])`)
	preSpacesRe         = regexp.MustCompile(`[ \t]+`)

	// ruleBasedClassify signal regexes.
	bankStatementRe = regexp.MustCompile(`(?i)(relev[ée]` + jsWs + `*de` + jsWs + `*compte|relev[ée]` + jsWs + `*de` + jsWs + `*ch[èe]ques|synth[èe]se` + jsWs + `*(d'|de` + jsWs + `*)?[ée]pargne|solde` + jsWs + `*cr[ée]diteur|relev[ée]` + jsWs + `*bancaire|extrait` + jsWs + `*de` + jsWs + `*compte|releve` + jsWs + `*de` + jsWs + `*cheques|bank` + jsWs + `*statement|account` + jsWs + `*statement|checking` + jsWs + `*account|savings` + jsWs + `*account|statement` + jsWs + `*of` + jsWs + `*account|credit` + jsWs + `*card` + jsWs + `*statement|opening` + jsWs + `*balance|closing` + jsWs + `*balance|bank` + jsWs + `*summary)`)

	taxNoticeMainRe          = regexp.MustCompile(`(?i)(avis[ _-]?d[ _-]?impot|avis[ _-]?d'imposition|impot[ _-]?sur[ _-]?les[ _-]?revenus|impot[ _-]?sur[ _-]?le[ _-]?revenu|centre[ _-]?des[ _-]?finances[ _-]?publiques|finances[ _-]?publiques|dgfip|impots\.gouv\.fr|prelevements[ _-]?sociaux)`)
	taxNoticePropertyRe      = regexp.MustCompile(`(?i)(taxe[ _-]?fonciere|taxe[ _-]?d'habitation)`)
	taxNoticeCorroborationRe = regexp.MustCompile(`(?i)(somme[ _-]?a[ _-]?payer|montant[ _-]?de[ _-]?votre[ _-]?impot|date[ _-]?limite[ _-]?de[ _-]?paiement|taux[ _-]?d'imposition|prelevement[ _-]?a[ _-]?l'echeance|impots\.gouv\.fr)`)
	payslipSignalRe          = regexp.MustCompile(`(?i)(bulletin[ _-]?de[ _-]?salaire|bulletin[ _-]?de[ _-]?paie|fiche[ _-]?de[ _-]?paie|salaire[ _-]?brut|net[ _-]?a[ _-]?payer|net[ _-]?à[ _-]?payer|payslip|pay[ _-]?slip|paystub|pay[ _-]?stub)`)

	bnpRe                          = regexp.MustCompile(`(?i)bnp[-_ ]?paribas|bnpparibas|\bbnp\b`)
	creditMutuelRe                 = regexp.MustCompile(`(?i)\b(caisse de credit mutuel|crédit mutuel|credit mutuel|creditmutuel)\b`)
	societeGeneraleRe              = regexp.MustCompile(`(?i)\b(société générale|societe generale)\b`)
	boursoRe                       = regexp.MustCompile(`(?i)\b(boursorama|boursobank)\b`)
	lclRe                          = regexp.MustCompile(`(?i)\b(lcl|crédit lyonnais|credit lyonnais)\b`)
	banquePostaleRe                = regexp.MustCompile(`(?i)\b(la banque postale|banque postale)\b`)
	finesRe                        = regexp.MustCompile(`(?i)\b(justificatif` + jsDotClass + `*règlement` + jsDotClass + `*amende|règlement` + jsDotClass + `*amende|reglement` + jsDotClass + `*amende|amende|amendes|amendes\.gouv\.fr|antai|avis de contravention|procès-verbal|proces-verbal|pv d'amende|traffic` + jsWs + `*fine|parking` + jsWs + `*ticket|penalty` + jsWs + `*charge)\b`)
	payslipBranchRe                = regexp.MustCompile(`(?i)bulletindesalaire|bulletin de salaire|bulletin de paie|fiche de paie|payslip|pay` + jsWs + `*slip|paystub|pay` + jsWs + `*stub|salary` + jsWs + `*statement|wage` + jsWs + `*statement|gross` + jsWs + `*pay|net` + jsWs + `*pay`)
	internshipRe                   = regexp.MustCompile(`(?i)(attestation|convention|certificat)` + jsWs + `*(de` + jsWs + `*)?stage|internship`)
	twoDdocRe                      = regexp.MustCompile(`(?i)attestationtitulairecontrat2ddoc|2ddoc`)
	contractsRe                    = regexp.MustCompile(`(?i)\b(contrat de travail|cdi|cdd|avenant au contrat|mandat d'agent commercial|mandat d'agent|mandat de prélèvement|mandat de prelevement|mandat sepa|sepa mandate|droit à l'image|droit a l'image|cg de mon contrat|conditions générales|notice-attestation-employeur|attestation-employeur|attestation employeur|engagement|convention collective|acte de société|acte de societe|dépôt d'entreprise|depot d'entreprise|employment` + jsWs + `*contract|employment` + jsWs + `*agreement|terms` + jsWs + `*and` + jsWs + `*conditions|non-disclosure` + jsWs + `*agreement|nda|service` + jsWs + `*agreement|lease` + jsWs + `*agreement|tenancy` + jsWs + `*agreement)\b`)
	contractConditionsRe           = regexp.MustCompile(`(?i)\bcg|conditions générales|terms` + jsWs + `*and` + jsWs + `*conditions\b`)
	contractAttestationEmployeurRe = regexp.MustCompile(`(?i)\battestation[ _-]employeur\b`)
	contractStatutsRe              = regexp.MustCompile(`(?i)\bacte de société|acte de societe|dépôt d'entreprise|depot d'entreprise\b`)
	contractSepaRe                 = regexp.MustCompile(`(?i)\bmandat de prélèvement|mandat de prelevement|mandat sepa|sepa mandate\b`)

	// `(?<!\p{L})cni(?!\p{L})` is lifted out of these alternations and guarded by
	// letterBoundaryContains (package comment deviation 1).
	identityMainRe          = regexp.MustCompile(`(?i)(passeport|passport|carte d'identité|piece_identite|pièce d'identité|piece d'identite|cancuoccongdan|giaypheplaixe|giay phep lai xe|permis de conduire|carte[-_ ]?de[-_ ]?séjour|carte[-_ ]?sejour|titre[-_ ]?de[-_ ]?séjour|titre[-_ ]?sejour|\btitre[-_]\p{L}|récépissé|recipisse|carte vitale|cartevitale|acte de mariage|actemariage|acte de naissance|livret de famille|cession` + jsDotClass + `*véhicule|identity` + jsWs + `*card|id` + jsWs + `*card|driver'?s?` + jsWs + `*license|residence` + jsWs + `*permit|visa` + jsWs + `*(de` + jsWs + `*)?(long|court|s[ée]jour|schengen)|birth` + jsWs + `*certificate|marriage` + jsWs + `*certificate)`)
	identityPasseportRe     = regexp.MustCompile(`(?i)(passeport|passport)`)
	identityRecipisseRe     = regexp.MustCompile(`(?i)(récépissé|recipisse)`)
	identityTitreSejourRe   = regexp.MustCompile(`(?i)(carte[-_ ]?de[-_ ]?séjour|carte[-_ ]?sejour|titre[-_ ]?de[-_ ]?séjour|titre[-_ ]?sejour|residence` + jsWs + `*permit|visa` + jsWs + `*(de` + jsWs + `*)?(long|court|s[ée]jour|schengen)|\btitre[-_]\p{L})`)
	identityCarteVitaleRe   = regexp.MustCompile(`(?i)(carte vitale|cartevitale)`)
	identityPermisRe        = regexp.MustCompile(`(?i)(giaypheplaixe|giay phep lai xe|permis de conduire|permis|driver'?s?` + jsWs + `*license)`)
	identityCarteGriseRe    = regexp.MustCompile(`(?i)(cession` + jsDotClass + `*véhicule|carte[-_ ]?grise)`)
	identityCarteIdentiteRe = regexp.MustCompile(`(?i)(cancuoccongdan|carte d'identité|piece_identite|pièce d'identité|piece d'identite|identity` + jsWs + `*card|id` + jsWs + `*card)`)
	identityActeMariageRe   = regexp.MustCompile(`(?i)(actemariage|acte de mariage|acte de naissance|birth` + jsWs + `*certificate|marriage` + jsWs + `*certificate)`)

	healthRe               = regexp.MustCompile(`(?i)\b(santé|sante|médical|medical|soins|dentaire|pharmacie|attestation de droits|attestationam|ameli|sécurité sociale|securite sociale|cpam|mutuelle|hospitalisation|arrêt de travail|arret de travail|avis d'arrêt|health` + jsWs + `*insurance|medical` + jsWs + `*bill|medical` + jsWs + `*claim|doctor'?s?` + jsWs + `*note|sick` + jsWs + `*leave|medical` + jsWs + `*statement|health` + jsWs + `*claim)\b`)
	healthArretRe          = regexp.MustCompile(`(?i)\barrêt de travail|arret de travail|avis d'arrêt|sick` + jsWs + `*leave|doctor'?s?` + jsWs + `*note\b`)
	healthAmeliRe          = regexp.MustCompile(`(?i)\bameli|assurance maladie|cpam|attestationam\b`)
	housingRe              = regexp.MustCompile(`(?i)\b(justificatif de domicile|attestation d'hébergement|attestation hebergement|declarationhonneur|quittance de loyer|logement|bus|navigo|proof` + jsWs + `*of` + jsWs + `*address|utility` + jsWs + `*bill|rent` + jsWs + `*receipt)\b`)
	housingTransportRe     = regexp.MustCompile(`(?i)\b(bus|navigo)\b`)
	educationRe            = regexp.MustCompile(`(?i)\b(formation|bachelor|étudiant|scolarité|inscription|école|université|diplôme|diplome|bulletinscolaire|certificat|alternance|relevé de notes|releve de notes|relevés de notes|bulletin de notes|academic` + jsWs + `*transcript|grade` + jsWs + `*report|certificate` + jsWs + `*of` + jsWs + `*enrollment|diploma|degree` + jsWs + `*certificate|tuition` + jsWs + `*fee)\b`)
	educationAlternanceRe  = regexp.MustCompile(`(?i)\balternance\b`)
	educationReleveNotesRe = regexp.MustCompile(`(?i)\brelevé de notes|releve de notes|relevés de notes|bulletin de notes|academic` + jsWs + `*transcript|grade` + jsWs + `*report\b`)
	educationDiplomesRe    = regexp.MustCompile(`(?i)\bdiplome|diplôme|bulletinscolaire|certificat|diploma|degree\b`)
	invoicesRe             = regexp.MustCompile(`(?i)\b(facture n°|facture no|facture|invoice|quittance|montant à payer|total ttc|bill|receipt|tax` + jsWs + `*invoice|amount` + jsWs + `*due|balance` + jsWs + `*due|total` + jsWs + `*due|payment` + jsWs + `*receipt)\b`)
	clientInvoiceRe        = regexp.MustCompile(`(?i)\b(facture client|facture de vente|facture émise|facturé à|destinataire|client_)\b`)
	invoiceSfrRe           = regexp.MustCompile(`(?i)\bsfr\b`)
	invoiceEdfRe           = regexp.MustCompile(`(?i)\bedf\b`)
	invoiceEngieRe         = regexp.MustCompile(`(?i)\bengie\b`)
	invoiceCdiscountRe     = regexp.MustCompile(`(?i)\bcdiscount\b`)
	invoiceAmazonRe        = regexp.MustCompile(`(?i)\bamazon\b`)
	taxesRe                = regexp.MustCompile(`(?i)\b(kbis|extrait kbis|avis[ _-]d[ _-]impot|avis[ _-]d'impot|avis[ _-]impot|déclaration[ _-]d'impôt|taxe[ _-]fonciere|taxe[ _-]foncière|taxe[ _-]d'habitation|revenus[ _-]et[ _-]prelev|prélèvement[ _-]sociaux|prelev[ _-]sociaux|finances[ _-]publiques|dgfip|impôt|impots|dossier-rempli|tax` + jsWs + `*return|tax` + jsWs + `*assessment|w-?2|form` + jsWs + `*1040|tax` + jsWs + `*notice|hmrc|property` + jsWs + `*tax|inland` + jsWs + `*revenue)\b`)
	taxesKbisRe            = regexp.MustCompile(`(?i)\bkbis|extrait kbis\b`)
	taxesDossierRe         = regexp.MustCompile(`(?i)\bdossier[-_]rempli\b`)
	insuranceRe            = regexp.MustCompile(`(?i)\b(assurance auto|assurance habitation|prévoyance|prevoyance|responsabilité civile|allianz|macif|maaf|déclaration de vol|declaration de vol|découverte de vol|decourverte de vol|dépôt de plainte|depot de plainte|plainte|car` + jsWs + `*insurance|auto` + jsWs + `*insurance|home` + jsWs + `*insurance|renters?` + jsWs + `*insurance|liability` + jsWs + `*insurance|policy` + jsWs + `*schedule|insurance` + jsWs + `*certificate)\b`)
	insuranceTheftRe       = regexp.MustCompile(`(?i)\bdéclaration de vol|declaration de vol|découverte de vol|decourverte de vol|dépôt de plainte|depot de plainte|plainte\b`)
	insuranceAllianzRe     = regexp.MustCompile(`(?i)\ballianz\b`)
	recruitmentRe          = regexp.MustCompile(`(?i)\b(lettre de motivation|candidature|recrutement|curriculum|cv|postuler|entretien|recommandation|cover` + jsWs + `*letter|job` + jsWs + `*application|resume)\b`)
	correspondenceRe       = regexp.MustCompile(`(?i)\b(courrier|lettre|email|mail|recommandé|notification|letter|notice)\b`)
	technicalRe            = regexp.MustCompile(`(?i)\b(manuel|guide|spécification|notice|documentation|technique|schema)\b`)
	reportsRe              = regexp.MustCompile(`(?i)\b(rapport|compte-rendu|projet|livrable|synthèse)\b`)

	// Subcategory fallback regexes.
	carrefourRe               = regexp.MustCompile(`(?i)\bcarrefour\b`)
	kairosRe                  = regexp.MustCompile(`(?i)\bkairos\b`)
	generalAllianzRe          = regexp.MustCompile(`(?i)\ballianz\b`)
	generalSfrRe              = regexp.MustCompile(`(?i)\b(sfr|red by sfr)\b`)
	generalEdfRe              = regexp.MustCompile(`(?i)\bedf\b`)
	generalEngieRe            = regexp.MustCompile(`(?i)\bengie\b`)
	generalBouyguesRe         = regexp.MustCompile(`(?i)\bbouygues\b`)
	generalFreeRe             = regexp.MustCompile(`(?i)\bfree` + jsWs + `*(mobile|telecom|haut` + jsWs + `*débit)\b|free\.\bfr\b`)
	generalAmeliRe            = regexp.MustCompile(`(?i)\b(ameli|assurance maladie|cpam)\b`)
	generalNavigoRe           = regexp.MustCompile(`(?i)\b(navigo|ile-de-france mobilités|ratp)\b`)
	generalCdiscountRe        = regexp.MustCompile(`(?i)\bcdiscount\b`)
	generalAmazonRe           = regexp.MustCompile(`(?i)\bamazon\b`)
	generalFnacRe             = regexp.MustCompile(`(?i)\bfnac\b`)
	generalPageConfirmationRe = regexp.MustCompile(`(?i)\b(page[ _-]de[ _-]confirmation|confirmation)\b`)
	generalCpfRe              = regexp.MustCompile(`(?i)\b(summary|cpf|docusign)\b`)
	generalBulletinFallbackRe = regexp.MustCompile(`(?i)\b(bulletindesalaire|bulletin[ _-]de[ _-]salaire|fiche[ _-]de[ _-]paie)\b`)
	generalFactureFallbackRe  = regexp.MustCompile(`(?i)\b(facture|invoice|bill|receipt)\b`)

	// Date and payment-status regexes.
	compactDateRe = regexp.MustCompile(`\b(20\d{2})(0[1-9]|1[0-2])(0[1-9]|[12]\d|3[01])\b`)
	dateYMDRe     = regexp.MustCompile(`\b(20\d{2})[-/](0[1-9]|1[0-2])[-/](0[1-9]|[12]\d|3[01])\b`)
	dateDMYRe     = regexp.MustCompile(`\b(0[1-9]|[12]\d|3[01])[-/](0[1-9]|1[0-2])[-/](20\d{2})\b`)
	paidRe        = regexp.MustCompile(`(?i)\b(payé|payee|acquittée|acquittee|réglé|regle|virement|solde 0|déjà réglé|paye)\b`)
	unpaidRe      = regexp.MustCompile(`(?i)\b(à payer|a payer|en attente|reste à régler|reste a regler|échéance|echeance|solde à payer)\b`)
)
