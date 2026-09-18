// Package classificationresolution is a Go port of pdf-triage's
// src/domain/classification-resolution.ts (265 lines): applyEntityPriorityOverride,
// refineClassification, resolveCategory and resolveSubcategory — the refine/resolve category and
// subcategory logic and the entity-priority override that carry Golden Rules 4 (strict
// no-subcategory fail guard), 6 (deep semantic reading over keywords) and 7 (company-level
// separation).
//
// It reuses, rather than reimplements, the already-ported sibling packages the TypeScript imports
// map to: documentschema (CategoryItem, SubcategoryItem, DocumentMetadata, EntityDictionary),
// classification (ruleBasedClassify, isGroundedSubcategorySlug, normalizeSlug,
// matchEntityDictionary, ALL_ENTITY_DOMAINS), taxonomyconflicts (findCategoryConflict,
// findSubcategoryConflict, TaxonomyConflict) and, transitively, taxonomy and promptpersonalization.
//
// Dependency injection. The TS module reaches into the infrastructure layer for two pure values:
// getEntityDictionary() (infrastructure/entity-dictionary-store.ts) and getPromptPersonalization()
// (infrastructure/prompt-personalization-store.ts). This package performs no I/O of its own. Callers
// pass a Deps value carrying exactly those two values:
//
//	type Deps struct {
//	    EntityDictionary      documentschema.EntityDictionary
//	    PromptPersonalization promptpersonalization.PromptPersonalization
//	}
//
// RefineClassification reads Deps.PromptPersonalization (its dictionary is already an explicit TS
// parameter); ResolveSubcategory reads both Deps fields inside the ungrounded-slug fallback branch.
// ApplyEntityPriorityOverride and ResolveCategory pull nothing from the stores (their dictionary /
// taxonomy are explicit arguments), so they take no Deps. The later store ports satisfy this
// surface by building a Deps from their cached reads; the tests supply fakes. The TS reads are lazy
// (inside the branch); taking a value struct reads them eagerly, which is unobservable for a pure
// cached store and is the only difference.
//
// The TypeScript source is the behavioral source of truth. The upstream TypeScript suite is GREEN
// at port time, so no upstream case is pinned red. The deviations below are pure language mechanics,
// each resolved in favor of matching TS exactly:
//
//  1. JS String.prototype.trim(). applyEntityPriorityOverride's `!extractedEntity.trim()` guard uses
//     JavaScript's WhiteSpace+LineTerminator set, which includes U+FEFF and excludes U+0085 NEL.
//     Go's strings.TrimSpace (unicode.White_Space) disagrees, so jsTrim/isJSWhitespace below
//     reproduce the exact JS set, the same approach and set classification.go uses in its unexported
//     copy (which this package cannot import).
//  2. Regex classes and anchors. The only patterns here are `impot|tax` (i), `_\d{4,8}$`,
//     `\d{4,8}$` and `^\d{4}$`. JS `\d` is ASCII [0-9] without the `u` flag and Go's RE2 `\d` is
//     ASCII too; `$` and `^` are unanchored-by-multiline in both, so each pattern ports literally.
//     None of these uses lookaround (RE2 has none) or `\s`.
//  3. Case folding. `extractedEntity.toLowerCase()` is Unicode-aware in both JS and Go
//     strings.ToLower. `normalizeSlug` already handles NFD accent folding inside the classification
//     package, so this module never folds accents itself.
//  4. Object spread `{ ...validated }` is a shallow copy: Go's struct value assignment copies the
//     scalar fields and shares the Tags slice / Other map headers, exactly as the JS spread does.
//     Only scalar Categorie/Subcategorie are then written, so the caller's input is never mutated.
//  5. Optional parameters. TS `rawSubcategorie?: string` becomes `rawSubcategorie ...string`
//     (omitted == undefined) and TS `categoriesConfig?` becomes a nil-able
//     *documentschema.CategoriesConfig. TS `newSubcategory?` / `conflict?` / `reason?` become
//     pointers / an empty string.
//  6. taxonomyconflicts type bridge. TS hands findCategoryConflict / findSubcategoryConflict the
//     same arbitrary `{ categories }` object it mutates. The Go port of that guard is typed to
//     *taxonomy.TaxonomyConfig, while this module works on the richer documentschema.CategoryItem
//     (name/description/aliases). toTaxonomyConfig builds a read-only taxonomy.TaxonomyConfig VIEW
//     for the guard; all mutation (append of the new category/subcategory) still happens on the
//     documentschema config the caller owns and persists. The guard returns string IDs, so no
//     pointer identity crosses the bridge.
//  7. New taxonomy entries. TS literally writes `subcategories: []` on a new category (Zod then
//     defaults the richer shape); this port sets the empty slice so the in-memory object matches
//     the Zod-defaulted shape. A rule-based-fallback subcategory is built without a subcategories
//     field in TS (undefined), so its Go Subcategories stays nil.
//  8. FORBIDDEN_SUBCATEGORIES is the module's OWN local set, deliberately narrower than
//     taxonomy.IsForbiddenSubcategory (no empty/year/numeric/extension heuristics). It is
//     reproduced here verbatim rather than substituted, because the TS module uses the narrow set
//     for its pre-creation sentinel check and swapping in the wider predicate would change results.
//  9. Math.round does not appear anywhere in this file, so the half-up-vs-half-away-from-zero
//     rounding gap is not reachable; slug similarity rounding lives in taxonomyconflicts, which
//     documents it.
//
// The original WHY comments (the weak-fallback reasoning, the Step A priority rationale, the
// unconditional bank override, the Golden Rule #5 duplicate guards) are preserved verbatim at their
// functions.
package classificationresolution

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/phamhung075/pdf-triage-pdf2w/classification"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomyconflicts"
)

// Deps is the explicit injected surface: exactly the two pure values the TS module reads from the
// infrastructure stores (getEntityDictionary / getPromptPersonalization). See the package comment.
type Deps struct {
	EntityDictionary      documentschema.EntityDictionary
	PromptPersonalization promptpersonalization.PromptPersonalization
}

// EntityPriorityOverride mirrors the TS return shape
// `{ categorie; subcategorie; overridden; reason? }`; an empty Reason is the TS `reason` undefined.
type EntityPriorityOverride struct {
	Categorie    string
	Subcategorie string
	Overridden   bool
	Reason       string
}

// ResolveCategoryResult mirrors the TS
// `{ category: CategoryItem; isNew: boolean; conflict?: TaxonomyConflict }`.
type ResolveCategoryResult struct {
	Category *documentschema.CategoryItem
	IsNew    bool
	Conflict *taxonomyconflicts.TaxonomyConflict
}

// ResolveSubcategoryResult mirrors the TS
// `{ subcategoryId; isNew; newSubcategory?; rawSubSlug; conflict? }`.
type ResolveSubcategoryResult struct {
	SubcategoryID  string
	IsNew          bool
	NewSubcategory *documentschema.SubcategoryItem
	RawSubSlug     string
	Conflict       *taxonomyconflicts.TaxonomyConflict
}

// Categories the 13-step classification flow only ever reaches when nothing more specific
// matched (see docs/workflows/classification-flow.md step 12 — "Plain postal letters or emails
// without invoice, tax, or contract context"). A grounded entity-dictionary match is trusted
// enough to override one of these fallback guesses, but never a category Step D chose with
// apparent confidence (e.g. 'bulletin_salaire', 'contracts') — the same real-world entity can
// legitimately issue documents under several different categories (France Travail issues both
// 'administrative' letters and 'bulletin_salaire' unemployment payment statements), and Step D's
// full-text read is better positioned to tell those apart than a bare entity name is.
var weakFallbackCategories = map[string]struct{}{
	"correspondence": {},
	"other":          {},
	"personal":       {},
	"":               {},
}

// forbiddenSubcategories is the TS module's local FORBIDDEN_SUBCATEGORIES set (Golden Rule #4).
// It is narrower than taxonomy.IsForbiddenSubcategory on purpose: this guard blocks only the
// literal sentinel/extension slugs before auto-creation, while the wider predicate is applied at
// other layers. See package comment deviation 8.
var forbiddenSubcategories = map[string]struct{}{
	"general": {}, "other": {}, "divers": {}, "unknown": {}, "none": {},
	"anyscanner": {}, "camscanner": {}, "geniusscan": {}, "adobescan": {}, "tinyscanner": {}, "simplescan": {}, "docscanner": {},
	"jpg": {}, "jpeg": {}, "png": {}, "webp": {}, "bmp": {}, "tiff": {}, "pdf": {}, "txt": {}, "docx": {}, "xlsx": {},
}

var (
	// /impot|tax/i
	impotTaxRe = regexp.MustCompile(`(?i)impot|tax`)
	// /_\d{4,8}$/g
	dateUnderscoreSuffixRe = regexp.MustCompile(`_\d{4,8}$`)
	// /\d{4,8}$/g
	dateSuffixRe = regexp.MustCompile(`\d{4,8}$`)
	// /^\d{4}$/
	bareYearRe = regexp.MustCompile(`^\d{4}$`)
)

// Step A (buildEntityExtractionPrompt) runs a dedicated, narrowly-scoped extraction pass whose
// only job is identifying the document's issuing entity — this is far more reliable than hoping
// Step D's single freeform full-classification call both re-derives the entity AND picks the
// correct top-level category. When that extracted entity is recognized in the curated
// entity_dictionary.json (each entity belongs to exactly one domain -> exactly one category, so
// this is unambiguous by construction, unlike fuzzy-matching against the noisier, auto-created
// categories.json), and Step D's own category choice landed in one of the fallback buckets above,
// prefer the entity dictionary's grounded category+subcategory. This directly fixes the regression
// where a Crédit Mutuel bank statement (Step A correctly extracted the full branch name from the
// header) still got filed under 'correspondence' by Step D.
func ApplyEntityPriorityOverride(validated documentschema.DocumentMetadata, extractedEntity string, dictionary documentschema.EntityDictionary) EntityPriorityOverride {
	noop := EntityPriorityOverride{Categorie: validated.Categorie, Subcategorie: validated.Subcategorie, Overridden: false}
	if extractedEntity == "" || jsTrim(extractedEntity) == "" {
		return noop
	}

	entityLower := strings.ToLower(extractedEntity)
	currentCategorySlug := classification.NormalizeSlug(validated.Categorie)

	// Golden Rule #6 singles out bank statements as the "archetypal trap": a vendor name inside a
	// transaction row (SFR, PayPal, Amazon) must never outrank the issuing bank's own header. Manual
	// probing against a real Crédit Mutuel statement showed Step D landing on an arbitrary wrong
	// category — not just the 'correspondence' fallback below, but also e.g. 'reports' — so unlike
	// the general case, a curated bank-domain match overrides UNCONDITIONALLY rather than being
	// gated to "weak fallback" categories: in this taxonomy a recognized bank entity essentially
	// never legitimately produces a non-'bank' document.
	bankMatch := classification.MatchEntityDictionary(entityLower, []string{"banks"}, dictionary)
	if bankMatch != nil && currentCategorySlug != bankMatch.Categorie {
		return EntityPriorityOverride{
			Categorie:    bankMatch.Categorie,
			Subcategorie: bankMatch.Subcategorie,
			Overridden:   true,
			Reason: fmt.Sprintf(`Step A entity "%s" is grounded in entity_dictionary.json's curated bank list as %s/%s (Golden Rule #6) — overriding Step D's category '%s'`,
				extractedEntity, bankMatch.Categorie, bankMatch.Subcategorie, validated.Categorie),
		}
	}

	if _, weak := weakFallbackCategories[currentCategorySlug]; !weak {
		return noop
	}

	dictMatch := classification.MatchEntityDictionary(entityLower, classification.AllEntityDomains, dictionary)
	if dictMatch == nil {
		return noop
	}

	return EntityPriorityOverride{
		Categorie:    dictMatch.Categorie,
		Subcategorie: dictMatch.Subcategorie,
		Overridden:   true,
		Reason: fmt.Sprintf(`Step A entity "%s" is grounded in entity_dictionary.json as %s/%s — overriding Step D's fallback category '%s'`,
			extractedEntity, dictMatch.Categorie, dictMatch.Subcategorie, validated.Categorie),
	}
}

// Refine Category & Subcategory using ruleBasedClassify if AI returned 'general', 'personal', 'other', or 'correspondence' for a Tax/Bank document
func RefineClassification(validated documentschema.DocumentMetadata, rawText, filename string, dictionary documentschema.EntityDictionary, personalNameDenylist []string, deps Deps) documentschema.DocumentMetadata {
	if !(validated.Categorie == "personal" || validated.Categorie == "other" || validated.Subcategorie == "general" || (validated.Categorie == "correspondence" && impotTaxRe.MatchString(filename))) {
		return validated
	}

	rb := classification.RuleBasedClassify(rawText, filename, dictionary, personalNameDenylist, deps.PromptPersonalization)
	result := validated

	if validated.Categorie == "personal" || validated.Categorie == "other" || validated.Categorie == "" || (validated.Categorie == "correspondence" && rb.Categorie == "administrative") {
		result.Categorie = rb.Categorie
	}
	if validated.Subcategorie == "general" {
		if rb.Subcategorie != "general" {
			result.Subcategorie = rb.Subcategorie
			if rb.Categorie != "" && (result.Categorie == "correspondence" || result.Categorie == "other") {
				result.Categorie = rb.Categorie
			}
		} else if result.Categorie == "bulletin_salaire" {
			result.Subcategorie = "bulletin_salaire"
		}
	}

	return result
}

// Normalize category ID & resolve to an existing entry, or describe a new one to be
// auto-created BEFORE the file is moved (Golden Rule #5).
//
// Duplicate guard: before auto-creating a brand-new top-level category, check the taxonomy for a
// near-duplicate category id (e.g. 'administratif' -> 'administrative') or for the proposed name
// actually being an entity that already exists as a subcategory ('france_travail' as a category —
// it is a subcategory of 'administrative'). A hit blocks the creation and remaps to the existing
// entry; the caller records the conflict as a hint that teaches future runs.
func ResolveCategory(categoriesConfig *documentschema.CategoriesConfig, rawCategorie string, rawSubcategorie ...string) ResolveCategoryResult {
	rawCatSlug := classification.NormalizeSlug(orAdministrative(rawCategorie))
	var matchedCategory *documentschema.CategoryItem
	for _, c := range categoriesConfig.Categories {
		if c == nil {
			continue
		}
		if c.ID == rawCatSlug || (c.Aliases != nil && aliasContainedIn(c.Aliases, rawCatSlug)) {
			matchedCategory = c
			break
		}
	}

	if matchedCategory != nil {
		return ResolveCategoryResult{Category: matchedCategory, IsNew: false}
	}

	var rawSubArg string
	if len(rawSubcategorie) > 0 {
		rawSubArg = rawSubcategorie[0]
	}
	conflict := taxonomyconflicts.FindCategoryConflict(toTaxonomyConfig(categoriesConfig), rawCatSlug, rawSubArg)
	if conflict != nil {
		var existing *documentschema.CategoryItem
		for _, c := range categoriesConfig.Categories {
			if c != nil && c.ID == conflict.MappedCategoryID {
				existing = c
				break
			}
		}
		if existing != nil {
			return ResolveCategoryResult{Category: existing, IsNew: false, Conflict: conflict}
		}
		// mapped category missing is unexpected (conflict detection only maps onto real entries);
		// fall through to creation rather than hang the pipeline.
	}

	newCatSlug := rawCatSlug
	newCatName := prettifySlug(newCatSlug)

	newCatObj := &documentschema.CategoryItem{
		ID:            newCatSlug,
		Name:          newCatName,
		Description:   "Category auto-created for " + newCatName,
		Aliases:       []string{newCatSlug},
		Subcategories: []*documentschema.SubcategoryItem{},
	}

	categoriesConfig.Categories = append(categoriesConfig.Categories, newCatObj)
	return ResolveCategoryResult{Category: newCatObj, IsNew: true}
}

// Normalize subcategory ID & resolve to an existing entry under `matchedCategory`, or
// describe a new one to be auto-created BEFORE the file is moved (Golden Rule #5) — unless
// the slug is forbidden (Golden Rule #4) or ungrounded (see isGroundedSubcategorySlug),
// in which case it resolves to 'general' so the caller's strict fail guard can BLOCK it.
//
// Duplicate guard: when `categoriesConfig` is supplied, a slug that already exists (exactly, by
// alias, or as a near-duplicate spelling) anywhere else in the taxonomy is NOT auto-created a
// second time — it resolves to the existing entry (and, when that entry lives under another
// category, `conflict.mappedCategoryId` tells the caller the document must be re-filed there).
func ResolveSubcategory(matchedCategory *documentschema.CategoryItem, rawSubcategorie, rawText, filename string, personalNameDenylist []string, deps Deps, categoriesConfig *documentschema.CategoriesConfig) ResolveSubcategoryResult {
	rawSubSlug := classification.NormalizeSlug(rawSubcategorie)
	// Clean dates from subcategory slugs
	rawSubSlug = dateUnderscoreSuffixRe.ReplaceAllString(rawSubSlug, "")
	rawSubSlug = dateSuffixRe.ReplaceAllString(rawSubSlug, "")

	if rawSubSlug == "" || bareYearRe.MatchString(rawSubSlug) {
		rawSubSlug = "general"
	}

	if matchedCategory.Subcategories == nil {
		matchedCategory.Subcategories = []*documentschema.SubcategoryItem{}
	}

	var matchedSub *documentschema.SubcategoryItem
	if _, forbidden := forbiddenSubcategories[rawSubSlug]; !forbidden {
		for _, s := range matchedCategory.Subcategories {
			if s == nil {
				continue
			}
			if s.ID == rawSubSlug || (s.Aliases != nil && aliasContainedIn(s.Aliases, rawSubSlug)) {
				matchedSub = s
				break
			}
		}
	}

	if matchedSub != nil {
		return ResolveSubcategoryResult{SubcategoryID: matchedSub.ID, IsNew: false, RawSubSlug: rawSubSlug}
	}

	if _, forbidden := forbiddenSubcategories[rawSubSlug]; forbidden {
		// Forbidden sentinel value — never auto-create it as a real taxonomy entry. Return it
		// as-is so the caller's strict fail guard (Golden Rule #4) BLOCKs the file and keeps
		// it in __raws.
		return ResolveSubcategoryResult{SubcategoryID: rawSubSlug, IsNew: false, RawSubSlug: rawSubSlug}
	}

	// Duplicate guard: never auto-create a second instance of a slug that already exists
	// elsewhere in the taxonomy (see domain/taxonomy-conflicts.ts).
	if categoriesConfig != nil {
		conflict := taxonomyconflicts.FindSubcategoryConflict(toTaxonomyConfig(categoriesConfig), matchedCategory.ID, rawSubSlug)
		if conflict != nil {
			return ResolveSubcategoryResult{
				SubcategoryID: conflictOrRaw(conflict.MappedSubcategoryID, rawSubSlug),
				IsNew:         false,
				RawSubSlug:    rawSubSlug,
				Conflict:      conflict,
			}
		}
	}

	if !classification.IsGroundedSubcategorySlug(rawSubSlug, rawText, filename, personalNameDenylist) {
		// Before giving up and collapsing to 'general' (which triggers Golden Rule #4 block),
		// check if ruleBasedClassify can extract a valid, grounded subcategory fallback
		// (e.g. 'facture', 'bulletin_salaire', 'attestation_confirmation', 'cpf', 'bctc')
		rb := classification.RuleBasedClassify(rawText, filename, deps.EntityDictionary, personalNameDenylist, deps.PromptPersonalization)
		if rb.Subcategorie != "" && rb.Subcategorie != "general" && !isForbiddenSubcategory(rb.Subcategorie) {
			var matchedFallbackSub *documentschema.SubcategoryItem
			for _, s := range matchedCategory.Subcategories {
				if s == nil {
					continue
				}
				if s.ID == rb.Subcategorie || (s.Aliases != nil && aliasIntersects(s.Aliases, rb.Subcategorie)) {
					matchedFallbackSub = s
					break
				}
			}
			if matchedFallbackSub != nil {
				return ResolveSubcategoryResult{SubcategoryID: matchedFallbackSub.ID, IsNew: false, RawSubSlug: rawSubSlug}
			}
			if classification.IsGroundedSubcategorySlug(rb.Subcategorie, rawText, filename, personalNameDenylist) {
				// Same duplicate guard for the rule-based fallback's candidate slug.
				if categoriesConfig != nil {
					rbConflict := taxonomyconflicts.FindSubcategoryConflict(toTaxonomyConfig(categoriesConfig), matchedCategory.ID, rb.Subcategorie)
					if rbConflict != nil {
						return ResolveSubcategoryResult{
							SubcategoryID: conflictOrRaw(rbConflict.MappedSubcategoryID, rb.Subcategorie),
							IsNew:         false,
							RawSubSlug:    rawSubSlug,
							Conflict:      rbConflict,
						}
					}
				}
				newSubName := prettifySlug(rb.Subcategorie)
				newSubObj := &documentschema.SubcategoryItem{ID: rb.Subcategorie, Name: newSubName, Aliases: []string{rb.Subcategorie}}
				matchedCategory.Subcategories = append(matchedCategory.Subcategories, newSubObj)
				return ResolveSubcategoryResult{SubcategoryID: rb.Subcategorie, IsNew: true, NewSubcategory: newSubObj, RawSubSlug: rawSubSlug}
			}
		}

		return ResolveSubcategoryResult{SubcategoryID: "general", IsNew: false, RawSubSlug: rawSubSlug}
	}

	newSubName := prettifySlug(rawSubSlug)

	newSubObj := &documentschema.SubcategoryItem{
		ID:      rawSubSlug,
		Name:    newSubName,
		Aliases: []string{rawSubSlug},
	}

	matchedCategory.Subcategories = append(matchedCategory.Subcategories, newSubObj)
	return ResolveSubcategoryResult{SubcategoryID: rawSubSlug, IsNew: true, NewSubcategory: newSubObj, RawSubSlug: rawSubSlug}
}

// orAdministrative ports `rawCategorie || 'administrative'`: JS `||` treats only the empty string
// as falsy here.
func orAdministrative(raw string) string {
	if raw == "" {
		return "administrative"
	}
	return raw
}

// conflictOrRaw mirrors `conflict.mappedSubcategoryId || rawSubSlug`: the empty mapped id is the
// TS undefined case, falling back to the raw slug.
func conflictOrRaw(mapped, raw string) string {
	if mapped == "" {
		return raw
	}
	return mapped
}

// aliasContainedIn ports `aliases.some(a => haystack.includes(normalizeSlug(a)))` — one
// directional. Note strings.Contains(haystack, "") is true, exactly as JS `includes(”)` is.
func aliasContainedIn(aliases []string, haystack string) bool {
	for _, a := range aliases {
		if strings.Contains(haystack, classification.NormalizeSlug(a)) {
			return true
		}
	}
	return false
}

// aliasIntersects ports the bidirectional alias predicate of the rule-based fallback:
// `rb.subcategorie.includes(normalizeSlug(a)) || normalizeSlug(a).includes(rb.subcategorie)`.
func aliasIntersects(aliases []string, slug string) bool {
	for _, a := range aliases {
		normalizedAlias := classification.NormalizeSlug(a)
		if strings.Contains(slug, normalizedAlias) || strings.Contains(normalizedAlias, slug) {
			return true
		}
	}
	return false
}

// isForbiddenSubcategory is the local narrow-set lookup used for the rule-based fallback's
// candidate slug (TS `FORBIDDEN_SUBCATEGORIES.has(rb.subcategorie)`).
func isForbiddenSubcategory(slug string) bool {
	_, forbidden := forbiddenSubcategories[slug]
	return forbidden
}

// prettifySlug ports `.split('_').map(w => w.charAt(0).toUpperCase() + w.slice(1)).join(' ')`.
// An empty segment stays empty (JS `”.charAt(0).toUpperCase() + ”.slice(1)` === "").
func prettifySlug(slug string) string {
	parts := strings.Split(slug, "_")
	for i, w := range parts {
		if w == "" {
			continue
		}
		r, size := utf8.DecodeRuneInString(w)
		parts[i] = strings.ToUpper(string(r)) + w[size:]
	}
	return strings.Join(parts, " ")
}

// toTaxonomyConfig builds a read-only taxonomy.TaxonomyConfig view of the documentschema config so
// the taxonomyconflicts duplicate guard (typed against taxonomy.TaxonomyConfig) can read it.
// taxonomyconflicts only reads category/subcategory ids and aliases, so dropping name/description
// is lossless for the guard; the caller's documentschema config remains the object that is mutated
// and persisted. See package comment deviation 6.
func toTaxonomyConfig(config *documentschema.CategoriesConfig) *taxonomy.TaxonomyConfig {
	if config == nil {
		return nil
	}
	cats := make([]*taxonomy.Category, 0, len(config.Categories))
	for _, c := range config.Categories {
		if c == nil {
			continue
		}
		tc := &taxonomy.Category{ID: c.ID}
		if c.Subcategories != nil {
			tc.Subcategories = make([]*taxonomy.Subcategory, 0, len(c.Subcategories))
			for _, s := range c.Subcategories {
				if s == nil {
					continue
				}
				tc.Subcategories = append(tc.Subcategories, &taxonomy.Subcategory{ID: s.ID, Name: s.Name, Aliases: s.Aliases})
			}
		}
		cats = append(cats, tc)
	}
	return &taxonomy.TaxonomyConfig{Categories: cats}
}

// jsTrim is String.prototype.trim(): it strips exactly the JavaScript whitespace set. Go's
// strings.TrimSpace would additionally strip U+0085 NEL and would leave U+FEFF. See package
// comment deviation 1.
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
