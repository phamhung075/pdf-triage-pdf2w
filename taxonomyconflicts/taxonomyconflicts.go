// Package taxonomyconflicts is a Go port of the pure functions in pdf-triage's
// src/domain/taxonomy-conflicts.ts: normalizeSlugForComparison, levenshteinDistance,
// slugSimilarity, findSubcategoryConflict, findCategoryConflict and
// renderTaxonomyConflictHintsBlock, plus the NEAR_DUPLICATE_THRESHOLD / MIN_COMPARE_LENGTH
// constants and the TaxonomyConflict / TaxonomyHintEntry shapes.
//
// Taxonomy duplicate guard — the BLOCK half of the "block then return a hint" loop that keeps the
// one-instance-per-subcategory invariant (docs/knowledge/taxonomy.md#one-instance-per-subcategory).
// The only external behavior it depends on is taxonomy.IsForbiddenSubcategory, imported from the
// sibling taxonomy package rather than reimplemented.
//
// The TypeScript source is the behavioral source of truth. One deviation is intentional:
//
//  1. normalizeSlugForComparison ports JS `String.prototype.normalize('NFD').replace(/[\u0300-
//     \u036f]/g, ”)` with a hand-written precomposed-Latin fold table (accentFold) plus explicit
//     combining-diacritic stripping, because Go's standard library has no Unicode normalization.
//     The table covers Latin-1 Supplement and Latin Extended-A — the scripts this archive's French
//     slugs use — and, like the TS `[^a-z0-9]+` removal, drops any letter it cannot fold (e.g. 'œ',
//     'æ', 'ø'), matching TS exactly (those characters have no canonical NFD decomposition either).
//     Slugs outside those ranges whose NFD form would reduce to an ASCII base letter could fold to
//     "" here where TS keeps the base letter; the slug comparison result is unchanged for them only
//     when the whole normalized form is already decided by the characters that do fold.
package taxonomyconflicts

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// TaxonomyConflictKind mirrors the TS `TaxonomyConflictKind` string union.
type TaxonomyConflictKind string

const (
	// KindCrossCategory is an exact slug id that exists under another category.
	KindCrossCategory TaxonomyConflictKind = "cross-category"
	// KindCrossCategoryAlias is a slug that matches an alias of an entry under another category.
	KindCrossCategoryAlias TaxonomyConflictKind = "cross-category-alias"
	// KindCrossCategoryNear is a slug that is a near-duplicate spelling of an entry under another category.
	KindCrossCategoryNear TaxonomyConflictKind = "cross-category-near"
	// KindSpellingMerge is a slug that is a near-duplicate spelling of an entry in the SAME category.
	KindSpellingMerge TaxonomyConflictKind = "spelling-merge"
	// KindCategoryNear is a proposed new top-level category that near-duplicates an existing one.
	KindCategoryNear TaxonomyConflictKind = "category-near"
	// KindEntityAsCategory is a proposed category that is actually an existing subcategory (entity) name.
	KindEntityAsCategory TaxonomyConflictKind = "entity-as-category"
)

// TaxonomyConflict mirrors the TS `TaxonomyConflict` interface. MappedSubcategoryID is the
// optional `mappedSubcategoryId`; the empty string is the Go equivalent of `undefined` (it is
// only ever absent for KindCategoryNear, where no subcategory is mapped).
type TaxonomyConflict struct {
	Kind                TaxonomyConflictKind `json:"kind"`
	MappedCategoryID    string               `json:"mappedCategoryId"`
	MappedSubcategoryID string               `json:"mappedSubcategoryId,omitempty"`
	Hint                string               `json:"hint"`
}

// TaxonomyHintEntry mirrors the TS `TaxonomyHintEntry` interface — a recorded conflict, persisted
// newest-first in taxonomy_hints.json and re-injected into STEP 0. Optional string fields use the
// empty string for `undefined`.
type TaxonomyHintEntry struct {
	ProposedCategory    string `json:"proposed_category,omitempty"`
	ProposedSubcategory string `json:"proposed_subcategory,omitempty"`
	MappedCategory      string `json:"mapped_category"`
	MappedSubcategory   string `json:"mapped_subcategory,omitempty"`
	Hint                string `json:"hint"`
	CreatedAt           string `json:"created_at,omitempty"`
}

// NearDuplicateThreshold is the ratio below which a near-match is a coincidence, not a duplicate.
const NearDuplicateThreshold = 0.82

// MinCompareLength is the minimum normalized length for which near-duplicate detection is
// meaningful — edit distance on very short slugs is noise.
const MinCompareLength = 4

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

// NormalizeSlugForComparison ports normalizeSlugForComparison: lowercase, accent-stripped,
// separators removed, so 'la_poste'/'laposte', 'bouygues_telecom'/'bouyguestelecom' and
// 'crédit mutuel'/'creditmutuel' all compare equal where they denote the same entity.
func NormalizeSlugForComparison(slug string) string {
	var b strings.Builder
	for _, r := range slug {
		// Strip combining diacritics so already-decomposed input folds the same way NFD would.
		if r >= 0x0300 && r <= 0x036F {
			continue
		}
		r = unicode.ToLower(r)
		if base, ok := accentFold[r]; ok {
			r = base
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// LevenshteinDistance ports levenshteinDistance: classic Levenshtein distance (small strings
// only — this is a hot path over short slugs). Input is measured in runes, matching JS
// `String.prototype.length` for the BMP text this code compares.
func LevenshteinDistance(a, b string) int {
	if a == b {
		return 0
	}
	ar := []rune(a)
	br := []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := 0; j <= len(br); j++ {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = minInt(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		copy(prev, curr)
	}
	return prev[len(br)]
}

// SlugSimilarity ports slugSimilarity: 1 - distance/maxLen over normalized forms; 1 means
// identical spelling.
func SlugSimilarity(a, b string) float64 {
	na := NormalizeSlugForComparison(a)
	nb := NormalizeSlugForComparison(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 1
	}
	// Normalized forms are ASCII, so byte length equals rune length.
	maxLen := len(na)
	if len(nb) > maxLen {
		maxLen = len(nb)
	}
	if maxLen == 0 {
		return 1
	}
	return 1 - float64(LevenshteinDistance(na, nb))/float64(maxLen)
}

// isComparable ports isComparable.
func isComparable(slug string) bool {
	n := NormalizeSlugForComparison(slug)
	return utf8.RuneCountInString(n) >= MinCompareLength
}

// candidateSub mirrors the TS `CandidateSub` interface.
type candidateSub struct {
	categoryID    string
	subcategoryID string
	normalized    string
	aliases       []string
}

// collectSubcategoryCandidates ports collectSubcategoryCandidates. Forbidden subcategories are
// skipped via the imported taxonomy.IsForbiddenSubcategory.
func collectSubcategoryCandidates(categories *taxonomy.TaxonomyConfig) []candidateSub {
	out := []candidateSub{}
	if categories == nil {
		return out
	}
	for _, cat := range categories.Categories {
		if cat == nil {
			continue
		}
		for _, sub := range cat.Subcategories {
			if sub == nil {
				continue
			}
			if taxonomy.IsForbiddenSubcategory(sub.ID) {
				continue
			}
			aliases := []string{}
			for _, a := range sub.Aliases {
				n := NormalizeSlugForComparison(a)
				if n != "" {
					aliases = append(aliases, n)
				}
			}
			out = append(out, candidateSub{
				categoryID:    cat.ID,
				subcategoryID: sub.ID,
				normalized:    NormalizeSlugForComparison(sub.ID),
				aliases:       aliases,
			})
		}
	}
	return out
}

// FindSubcategoryConflict ports findSubcategoryConflict: detects a duplicate BEFORE a new
// subcategory would be auto-created under currentCategoryID.
//
// The current category is NOT excluded from the scan: the caller (resolveSubcategory) already
// resolved exact id/alias matches within the current category before this hook runs, so any exact
// hit here is by definition under another category — but a near-duplicate spelling in the SAME
// category (bouyguestelecom vs bouygues_telecom) is exactly what the caller's exact-only lookup
// misses, and is the case this guard exists to merge. Kind reflects whether the winning entry
// lives under the current category (spelling-merge, no category change) or elsewhere (cross-*).
func FindSubcategoryConflict(categories *taxonomy.TaxonomyConfig, currentCategoryID, rawSubSlug string) *TaxonomyConflict {
	slug := strings.ToLower(strings.TrimSpace(rawSubSlug))
	if slug == "" || taxonomy.IsForbiddenSubcategory(slug) {
		return nil
	}
	slugNorm := NormalizeSlugForComparison(slug)
	if slugNorm == "" {
		return nil
	}

	candidates := collectSubcategoryCandidates(categories)
	sameCategory := func(c candidateSub) bool { return c.categoryID == currentCategoryID }

	// 1) exact id
	for _, c := range candidates {
		if c.normalized != slugNorm {
			continue
		}
		if sameCategory(c) {
			return nil // caller's own lookup already handled this
		}
		return &TaxonomyConflict{
			Kind:                KindCrossCategory,
			MappedCategoryID:    c.categoryID,
			MappedSubcategoryID: c.subcategoryID,
			Hint: fmt.Sprintf("Duplicate subcategory BLOCKED: '%s' already exists as '%s' under category '%s' — reusing it instead of creating a second instance (one-instance-per-subcategory rule).",
				slug, c.subcategoryID, c.categoryID),
		}
	}

	// 2) alias match — EXACT normalized equality only. The caller's within-category rule already
	//    allows substring alias matches (verbose slugs like 'Caisse Credit Mutuel Springfield
	//    Centre' containing 'creditmutuel'), but cross-category that would be a collision trap:
	//    'cdiscount_energie' contains 'cdiscount' and must NOT be dragged onto the e-commerce
	//    vendor. Here an alias must denote the entity wholesale ('creditmutuel' == alias, 'amazon'
	//    == cdiscount alias).
	for _, c := range candidates {
		hit := false
		for _, a := range c.aliases {
			if a == slugNorm {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		kind := KindCrossCategoryAlias
		if sameCategory(c) {
			kind = KindSpellingMerge
		}
		return &TaxonomyConflict{
			Kind:                kind,
			MappedCategoryID:    c.categoryID,
			MappedSubcategoryID: c.subcategoryID,
			Hint: fmt.Sprintf("Duplicate subcategory BLOCKED: '%s' matches an alias of existing '%s' under category '%s' — reuse the existing slug instead of creating a new one.",
				slug, c.subcategoryID, c.categoryID),
		}
	}

	// 3) near-duplicate spelling
	if isComparable(slug) {
		var best *candidateSub
		bestRatio := 0.0
		for i := range candidates {
			c := candidates[i]
			ratio := SlugSimilarity(slug, c.subcategoryID)
			if ratio >= NearDuplicateThreshold && (best == nil || ratio > bestRatio) {
				best = &candidates[i]
				bestRatio = ratio
			}
		}
		// also compare against aliases (e.g. 'lai dental' vs existing alias 'lai_dental')
		if best == nil {
			for i := range candidates {
				c := candidates[i]
				for _, a := range c.aliases {
					if !isComparable(a) {
						continue
					}
					ratio := SlugSimilarity(slug, a)
					if ratio >= NearDuplicateThreshold && (best == nil || ratio > bestRatio) {
						best = &candidates[i]
						bestRatio = ratio
					}
				}
			}
		}
		if best != nil {
			kind := KindCrossCategoryNear
			if sameCategory(*best) {
				kind = KindSpellingMerge
			}
			return &TaxonomyConflict{
				Kind:                kind,
				MappedCategoryID:    best.categoryID,
				MappedSubcategoryID: best.subcategoryID,
				Hint: fmt.Sprintf("Duplicate subcategory BLOCKED: '%s' is a near-duplicate spelling of existing '%s' under category '%s' (similarity %d%%) — reusing the existing slug instead of creating '%s'.",
					slug, best.subcategoryID, best.categoryID, int(math.Round(bestRatio*100)), slug),
			}
		}
	}

	return nil
}

// FindCategoryConflict ports findCategoryConflict: detects a duplicate BEFORE a new top-level
// category would be auto-created. Two guards:
//  1. the proposed name near-duplicates an existing category id (e.g. 'administratif' -> 'administrative')
//  2. the proposed "category" is actually an entity that already exists as a subcategory
//     (e.g. 'france_travail' -> subcategory of 'administrative' — the AI-created top-level
//     category that this mechanism exists to prevent coming back).
//
// rawSubcategorie is accepted for signature parity with the TS caller; the TS implementation
// assigns it to `proposedSub` but never reads it, so this port carries no behavior for it.
func FindCategoryConflict(categories *taxonomy.TaxonomyConfig, rawCategorie, rawSubcategorie string) *TaxonomyConflict {
	_ = rawSubcategorie
	rawCat := strings.ToLower(strings.TrimSpace(rawCategorie))
	catNorm := NormalizeSlugForComparison(rawCat)
	if catNorm == "" {
		return nil
	}

	var cats []*taxonomy.Category
	if categories != nil {
		cats = categories.Categories
	}

	// 1) near-duplicate of an existing top-level category
	if isComparable(rawCat) {
		var bestCat *taxonomy.Category
		bestRatio := 0.0
		for _, c := range cats {
			if c == nil {
				continue
			}
			if taxonomy.IsForbiddenSubcategory(c.ID) {
				continue
			}
			if NormalizeSlugForComparison(c.ID) == catNorm {
				continue // exact id — caller handles it
			}
			ratio := SlugSimilarity(rawCat, c.ID)
			if ratio >= NearDuplicateThreshold && (bestCat == nil || ratio > bestRatio) {
				bestCat = c
				bestRatio = ratio
			}
		}
		if bestCat != nil {
			return &TaxonomyConflict{
				Kind:             KindCategoryNear,
				MappedCategoryID: bestCat.ID,
				Hint: fmt.Sprintf("Duplicate category BLOCKED: '%s' is a near-duplicate of existing category '%s' (similarity %d%%) — using '%s' instead of auto-creating a second top-level category.",
					rawCat, bestCat.ID, int(math.Round(bestRatio*100)), bestCat.ID),
			}
		}
	}

	// 2) entity-as-category: the proposed category name is really an existing subcategory
	type subMatch struct {
		categoryID    string
		subcategoryID string
		ratio         float64
	}
	subMatches := []subMatch{}
	for _, c := range cats {
		if c == nil {
			continue
		}
		for _, s := range c.Subcategories {
			if s == nil {
				continue
			}
			if taxonomy.IsForbiddenSubcategory(s.ID) {
				continue
			}
			ratio := SlugSimilarity(rawCat, s.ID)
			if ratio == 1 || ratio >= NearDuplicateThreshold {
				subMatches = append(subMatches, subMatch{categoryID: c.ID, subcategoryID: s.ID, ratio: ratio})
			}
		}
	}
	if len(subMatches) > 0 {
		// prefer an exact slug match; otherwise the closest near-match
		sort.SliceStable(subMatches, func(i, j int) bool {
			ai, bi := 0, 0
			if subMatches[i].ratio == 1 {
				ai = -1
			}
			if subMatches[j].ratio == 1 {
				bi = -1
			}
			key := ai - bi
			if key != 0 {
				return key < 0
			}
			return subMatches[i].ratio > subMatches[j].ratio
		})
		best := subMatches[0]
		return &TaxonomyConflict{
			Kind:                KindEntityAsCategory,
			MappedCategoryID:    best.categoryID,
			MappedSubcategoryID: best.subcategoryID,
			Hint: fmt.Sprintf("Duplicate category BLOCKED: '%s' is an entity that already exists as subcategory '%s' under category '%s' — entities are never top-level categories. Filing under '%s/%s' instead of creating category '%s'.",
				rawCat, best.subcategoryID, best.categoryID, best.categoryID, best.subcategoryID, rawCat),
		}
	}

	return nil
}

// RenderTaxonomyConflictHintsBlock ports renderTaxonomyConflictHintsBlock: renders persisted
// taxonomy conflicts into a compact STEP 0 block the model reads on every classification run —
// the concrete "do not create these" list. Returns ” when there is nothing to inject (placeholder
// must vanish cleanly).
func RenderTaxonomyConflictHintsBlock(hints []*TaxonomyHintEntry) string {
	clean := make([]*TaxonomyHintEntry, 0, len(hints))
	for _, h := range hints {
		if h != nil && h.MappedCategory != "" && h.Hint != "" {
			clean = append(clean, h)
		}
	}
	if len(clean) == 0 {
		return ""
	}

	lines := []string{
		"",
		"STEP 0: TAXONOMY DUPLICATE GUARD (read BEFORE classifying):",
		"- The following category/subcategory slugs were proposed by a previous run but BLOCKED because an identical or near-identical instance already exists. NEVER return them as new categories/subcategories — always reuse the exact existing slug under its existing category:",
	}
	for _, h := range clean {
		proposed := joinNonEmpty("/", h.ProposedCategory, h.ProposedSubcategory)
		if proposed == "" {
			proposed = "?"
		}
		mapped := joinNonEmpty("/", h.MappedCategory, h.MappedSubcategory)
		lines = append(lines, fmt.Sprintf(`- "%s" → ALWAYS use "%s". %s`, proposed, mapped, h.Hint))
	}
	return strings.Join(lines, "\n") + "\n"
}

// joinNonEmpty reproduces JS `[a, b].filter(Boolean).join(sep)` for two string fields.
func joinNonEmpty(sep, a, b string) string {
	if a == "" {
		return b
	}
	if b == "" {
		return a
	}
	return a + sep + b
}

func minInt(values ...int) int {
	m := values[0]
	for _, v := range values[1:] {
		if v < m {
			m = v
		}
	}
	return m
}
