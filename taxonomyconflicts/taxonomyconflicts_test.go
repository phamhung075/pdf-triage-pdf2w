package taxonomyconflicts

import (
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// All cases below are ported verbatim from pdf-triage's
// src/domain/taxonomy-conflicts.test.ts. The TypeScript source is the behavioral source of truth.

// buildTaxonomy mirrors the fixture `buildTaxonomy()` in the TS test: a taxonomy shaped like the
// merged one after the 2026-08-27 one-instance merge.
func buildTaxonomy() *taxonomy.TaxonomyConfig {
	return &taxonomy.TaxonomyConfig{
		Categories: []*taxonomy.Category{
			{
				ID: "invoices",
				Subcategories: []*taxonomy.Subcategory{
					{ID: "sfr", Name: "SFR", Aliases: []string{"red"}},
					{ID: "bouygues_telecom", Name: "Bouygues Telecom", Aliases: []string{"bouyguestelecom"}},
					{ID: "cdiscount", Name: "Cdiscount", Aliases: []string{"cdiscount", "amazon", "fnac"}},
				},
			},
			{
				ID: "housing",
				Subcategories: []*taxonomy.Subcategory{
					{ID: "foncia", Name: "Foncia", Aliases: []string{"loyer"}},
				},
			},
			{
				ID: "bank",
				Subcategories: []*taxonomy.Subcategory{
					{ID: "credit_mutuel", Name: "Crédit Mutuel", Aliases: []string{"creditmutuel", "credit mutuel", "ccm"}},
				},
			},
			{
				ID: "administrative",
				Subcategories: []*taxonomy.Subcategory{
					{ID: "france_travail", Name: "France Travail", Aliases: []string{"francetravail"}},
					{ID: "impot", Name: "Impôts", Aliases: []string{"taxe", "avis"}},
					{ID: "inpi", Name: "Inpi", Aliases: []string{}},
				},
			},
			{
				ID: "correspondence",
				Subcategories: []*taxonomy.Subcategory{
					{ID: "la_poste", Name: "La Poste", Aliases: []string{"laposte"}},
				},
			},
			{
				ID: "bulletin_salaire",
				Subcategories: []*taxonomy.Subcategory{
					{ID: "lai_dentail", Name: "Lai Dentail", Aliases: []string{"lai dental"}},
				},
			},
			{
				ID: "health",
				Subcategories: []*taxonomy.Subcategory{
					{ID: "gps", Name: "Gps", Aliases: []string{}},
				},
			},
			{
				ID: "education",
				Subcategories: []*taxonomy.Subcategory{
					{ID: "cdiscount_energie", Name: "Cdiscount Energie", Aliases: []string{}},
				},
			},
		},
	}
}

func TestSlugComparison(t *testing.T) {
	t.Run("normalizes separators, accents and case for comparison", func(t *testing.T) {
		cases := map[string]string{
			"la_poste":         "laposte",
			"La Poste":         "laposte",
			"bouygues_telecom": "bouyguestelecom",
			"crédit mutuel":    "creditmutuel",
		}
		for in, want := range cases {
			if got := NormalizeSlugForComparison(in); got != want {
				t.Fatalf("NormalizeSlugForComparison(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("scores near-identical spellings highly and unrelated slugs lowly", func(t *testing.T) {
		if got := SlugSimilarity("bouyguestelecom", "bouygues_telecom"); got != 1 {
			t.Fatalf("SlugSimilarity(bouyguestelecom, bouygues_telecom) = %v, want 1", got)
		}
		if got := SlugSimilarity("lai_dental", "lai_dentail"); !(got > 0.85) {
			t.Fatalf("SlugSimilarity(lai_dental, lai_dentail) = %v, want > 0.85", got)
		}
		if got := SlugSimilarity("impot", "inpi"); !(got < 0.7) {
			t.Fatalf("SlugSimilarity(impot, inpi) = %v, want < 0.7", got)
		}
		if got := SlugSimilarity("cdiscount", "cdiscount_energie"); !(got < 0.7) {
			t.Fatalf("SlugSimilarity(cdiscount, cdiscount_energie) = %v, want < 0.7", got)
		}
	})
}

func TestFindSubcategoryConflict(t *testing.T) {
	t.Run("blocks an exact slug that already exists under another category", func(t *testing.T) {
		conflict := FindSubcategoryConflict(buildTaxonomy(), "invoices", "foncia")
		if conflict == nil {
			t.Fatal("conflict = nil, want non-nil")
		}
		if conflict.Kind != KindCrossCategory {
			t.Fatalf("kind = %q, want %q", conflict.Kind, KindCrossCategory)
		}
		if conflict.MappedCategoryID != "housing" {
			t.Fatalf("mappedCategoryId = %q, want housing", conflict.MappedCategoryID)
		}
		if conflict.MappedSubcategoryID != "foncia" {
			t.Fatalf("mappedSubcategoryId = %q, want foncia", conflict.MappedSubcategoryID)
		}
	})

	t.Run("blocks a slug matching an alias of an entry under another category", func(t *testing.T) {
		// 'creditmutuel' normalizes identically to the id itself (underscore stripped) — exact id
		// path. A genuinely distinct alias (cdiscount's 'amazon') exercises the alias branch.
		conflict := FindSubcategoryConflict(buildTaxonomy(), "housing", "amazon")
		if conflict == nil {
			t.Fatal("conflict = nil, want non-nil")
		}
		if conflict.Kind != KindCrossCategoryAlias {
			t.Fatalf("kind = %q, want %q", conflict.Kind, KindCrossCategoryAlias)
		}
		if conflict.MappedCategoryID != "invoices" {
			t.Fatalf("mappedCategoryId = %q, want invoices", conflict.MappedCategoryID)
		}
		if conflict.MappedSubcategoryID != "cdiscount" {
			t.Fatalf("mappedSubcategoryId = %q, want cdiscount", conflict.MappedSubcategoryID)
		}
	})

	t.Run("blocks a same-entity spelling variant across categories (la_poste/laposte)", func(t *testing.T) {
		// 'laposte' normalizes identically to the existing id 'la_poste' (separator stripped) — the
		// exact-id cross-category path, mapping the proposal onto correspondence/la_poste.
		conflict := FindSubcategoryConflict(buildTaxonomy(), "invoices", "laposte")
		if conflict == nil {
			t.Fatal("conflict = nil, want non-nil")
		}
		if conflict.Kind != KindCrossCategory {
			t.Fatalf("kind = %q, want %q", conflict.Kind, KindCrossCategory)
		}
		if conflict.MappedCategoryID != "correspondence" {
			t.Fatalf("mappedCategoryId = %q, want correspondence", conflict.MappedCategoryID)
		}
		if conflict.MappedSubcategoryID != "la_poste" {
			t.Fatalf("mappedSubcategoryId = %q, want la_poste", conflict.MappedSubcategoryID)
		}
	})

	t.Run("merges a near-duplicate spelling within the SAME category (spelling-merge)", func(t *testing.T) {
		// typo variant not present in the aliases — the caller's exact lookup misses it and only the
		// guard's fuzzy pass catches it
		conflict := FindSubcategoryConflict(buildTaxonomy(), "invoices", "bouyguestelecomme")
		if conflict == nil {
			t.Fatal("conflict = nil, want non-nil")
		}
		if conflict.Kind != KindSpellingMerge {
			t.Fatalf("kind = %q, want %q", conflict.Kind, KindSpellingMerge)
		}
		if conflict.MappedCategoryID != "invoices" {
			t.Fatalf("mappedCategoryId = %q, want invoices", conflict.MappedCategoryID)
		}
		if conflict.MappedSubcategoryID != "bouygues_telecom" {
			t.Fatalf("mappedSubcategoryId = %q, want bouygues_telecom", conflict.MappedSubcategoryID)
		}
	})

	t.Run("blocks a near-duplicate spelling across categories", func(t *testing.T) {
		conflict := FindSubcategoryConflict(buildTaxonomy(), "health", "lai_dentaile")
		if conflict == nil {
			t.Fatal("conflict = nil, want non-nil")
		}
		if conflict.Kind != KindCrossCategoryNear {
			t.Fatalf("kind = %q, want %q", conflict.Kind, KindCrossCategoryNear)
		}
		if conflict.MappedCategoryID != "bulletin_salaire" {
			t.Fatalf("mappedCategoryId = %q, want bulletin_salaire", conflict.MappedCategoryID)
		}
		if conflict.MappedSubcategoryID != "lai_dentail" {
			t.Fatalf("mappedSubcategoryId = %q, want lai_dentail", conflict.MappedSubcategoryID)
		}
	})

	t.Run("does not fire on a genuinely new slug", func(t *testing.T) {
		if got := FindSubcategoryConflict(buildTaxonomy(), "invoices", "veolia"); got != nil {
			t.Fatalf("conflict = %+v, want nil", got)
		}
	})

	t.Run("does not fire on forbidden slugs", func(t *testing.T) {
		if got := FindSubcategoryConflict(buildTaxonomy(), "invoices", "general"); got != nil {
			t.Fatalf("general: conflict = %+v, want nil", got)
		}
		if got := FindSubcategoryConflict(buildTaxonomy(), "invoices", "divers"); got != nil {
			t.Fatalf("divers: conflict = %+v, want nil", got)
		}
	})

	t.Run("does not conflate entities that share a word (cdiscount vs cdiscount_energie)", func(t *testing.T) {
		// proposed cdiscount_energie under invoices: exact owner is education — one-instance remap,
		// NOT a collapse onto the cdiscount vendor entry
		conflict := FindSubcategoryConflict(buildTaxonomy(), "invoices", "cdiscount_energie")
		if conflict == nil {
			t.Fatal("conflict = nil, want non-nil")
		}
		if conflict.Kind != KindCrossCategory {
			t.Fatalf("kind = %q, want %q", conflict.Kind, KindCrossCategory)
		}
		if conflict.MappedCategoryID != "education" {
			t.Fatalf("mappedCategoryId = %q, want education", conflict.MappedCategoryID)
		}
		if conflict.MappedSubcategoryID != "cdiscount_energie" {
			t.Fatalf("mappedSubcategoryId = %q, want cdiscount_energie", conflict.MappedSubcategoryID)
		}
		// the vendor slug proposed under education maps to invoices/cdiscount, never to the energy arm
		conflict2 := FindSubcategoryConflict(buildTaxonomy(), "education", "cdiscount")
		if conflict2 == nil {
			t.Fatal("conflict2 = nil, want non-nil")
		}
		if conflict2.Kind != KindCrossCategory {
			t.Fatalf("conflict2.kind = %q, want %q", conflict2.Kind, KindCrossCategory)
		}
		if conflict2.MappedCategoryID != "invoices" {
			t.Fatalf("conflict2.mappedCategoryId = %q, want invoices", conflict2.MappedCategoryID)
		}
		if conflict2.MappedSubcategoryID != "cdiscount" {
			t.Fatalf("conflict2.mappedSubcategoryId = %q, want cdiscount", conflict2.MappedSubcategoryID)
		}
		// fuzzy pass must NOT merge cdiscount with cdiscount_energie (0.53 similarity)
		near := FindSubcategoryConflict(buildTaxonomy(), "education", "cdiscountv2")
		if near != nil {
			t.Fatalf("near = %+v, want nil", near)
		}
	})

	t.Run("does not fire on short slugs where edit distance is noise", func(t *testing.T) {
		if got := FindSubcategoryConflict(buildTaxonomy(), "housing", "sfx"); got != nil {
			t.Fatalf("sfx: conflict = %+v, want nil", got)
		}
		if got := FindSubcategoryConflict(buildTaxonomy(), "housing", "gpsx"); got != nil {
			t.Fatalf("gpsx: conflict = %+v, want nil", got)
		}
	})
}

func TestFindCategoryConflict(t *testing.T) {
	t.Run("blocks a near-duplicate top-level category (administratif -> administrative)", func(t *testing.T) {
		conflict := FindCategoryConflict(buildTaxonomy(), "administratif", "")
		if conflict == nil {
			t.Fatal("conflict = nil, want non-nil")
		}
		if conflict.Kind != KindCategoryNear {
			t.Fatalf("kind = %q, want %q", conflict.Kind, KindCategoryNear)
		}
		if conflict.MappedCategoryID != "administrative" {
			t.Fatalf("mappedCategoryId = %q, want administrative", conflict.MappedCategoryID)
		}
	})

	t.Run("blocks an entity name proposed as a top-level category (france_travail)", func(t *testing.T) {
		conflict := FindCategoryConflict(buildTaxonomy(), "france_travail", "france_travail")
		if conflict == nil {
			t.Fatal("conflict = nil, want non-nil")
		}
		if conflict.Kind != KindEntityAsCategory {
			t.Fatalf("kind = %q, want %q", conflict.Kind, KindEntityAsCategory)
		}
		if conflict.MappedCategoryID != "administrative" {
			t.Fatalf("mappedCategoryId = %q, want administrative", conflict.MappedCategoryID)
		}
		if conflict.MappedSubcategoryID != "france_travail" {
			t.Fatalf("mappedSubcategoryId = %q, want france_travail", conflict.MappedSubcategoryID)
		}
	})

	t.Run("does not fire when the proposed category is genuinely new", func(t *testing.T) {
		if got := FindCategoryConflict(buildTaxonomy(), "justice", ""); got != nil {
			t.Fatalf("conflict = %+v, want nil", got)
		}
	})

	t.Run("does not fire for an existing category id", func(t *testing.T) {
		// callers only run the guard after their own exact/alias lookup missed
		if got := FindCategoryConflict(buildTaxonomy(), "bank", ""); got != nil {
			t.Fatalf("conflict = %+v, want nil", got)
		}
	})
}

func TestRenderTaxonomyConflictHintsBlock(t *testing.T) {
	t.Run("renders nothing for an empty list", func(t *testing.T) {
		if got := RenderTaxonomyConflictHintsBlock([]*TaxonomyHintEntry{}); got != "" {
			t.Fatalf("got %q, want empty string", got)
		}
	})

	t.Run("renders the proposed -> mapped guard lines", func(t *testing.T) {
		hints := []*TaxonomyHintEntry{{
			ProposedCategory:    "invoices",
			ProposedSubcategory: "foncia",
			MappedCategory:      "housing",
			MappedSubcategory:   "foncia",
			Hint:                "Duplicate subcategory BLOCKED.",
			CreatedAt:           "2026-08-27T00:00:00.000Z",
		}}
		block := RenderTaxonomyConflictHintsBlock(hints)
		for _, want := range []string{"TAXONOMY DUPLICATE GUARD", "invoices/foncia", "housing/foncia"} {
			if !strings.Contains(block, want) {
				t.Fatalf("block = %q, want it to contain %q", block, want)
			}
		}
	})

	t.Run("skips malformed entries", func(t *testing.T) {
		block := RenderTaxonomyConflictHintsBlock([]*TaxonomyHintEntry{
			{ProposedCategory: "x", MappedCategory: "", MappedSubcategory: "", Hint: "no target", CreatedAt: ""},
			nil,
		})
		if block != "" {
			t.Fatalf("got %q, want empty string", block)
		}
	})
}
