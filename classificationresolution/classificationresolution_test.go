package classificationresolution

import (
	"reflect"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomyconflicts"
)

// All cases below are ported verbatim from pdf-triage's
// src/domain/classification-resolution.test.ts (27 cases upstream). The TypeScript source is the
// behavioral source of truth. Where the TS test relied on the real infrastructure stores
// (getEntityDictionary / getPromptPersonalization), the Go tests inject the same EMPTY fakes the
// TS fixtures use, per the package's dependency-injection contract.

var defaultPersonalNameDenylist = []string{"dupond", "martin", "lefebvre", "bernard"}

func emptyDictionary() documentschema.EntityDictionary {
	return documentschema.EntityDictionary{
		Banks:     []*documentschema.EntityItem{},
		Energy:    []*documentschema.EntityItem{},
		Telecom:   []*documentschema.EntityItem{},
		Insurance: []*documentschema.EntityItem{},
		Gov:       []*documentschema.EntityItem{},
		Health:    []*documentschema.EntityItem{},
	}
}

// testDeps is the fake injected surface: the TS test's EMPTY_DICTIONARY plus the store's empty
// personalization default.
func testDeps() Deps {
	return Deps{
		EntityDictionary:      emptyDictionary(),
		PromptPersonalization: promptpersonalization.EMPTY_PROMPT_PERSONALIZATION,
	}
}

func subcat(id, name string, aliases ...string) *documentschema.SubcategoryItem {
	if aliases == nil {
		aliases = []string{}
	}
	return &documentschema.SubcategoryItem{ID: id, Name: name, Aliases: aliases}
}

func category(id, name string, aliases []string, subs ...*documentschema.SubcategoryItem) *documentschema.CategoryItem {
	if aliases == nil {
		aliases = []string{}
	}
	if subs == nil {
		subs = []*documentschema.SubcategoryItem{}
	}
	return &documentschema.CategoryItem{
		ID:            id,
		Name:          name,
		Description:   "",
		Aliases:       aliases,
		Subcategories: subs,
	}
}

func baseMetadata(apply func(*documentschema.DocumentMetadata)) documentschema.DocumentMetadata {
	m := documentschema.DocumentMetadata{
		Titre:           "Test",
		Registre:        "",
		Date:            "",
		Categorie:       "administrative",
		Subcategorie:    "general",
		Summary:         "",
		Tags:            []string{},
		MarkdownContent: "",
		Other:           map[string]any{},
		Thinking:        "",
		// Transform-produced fields: DocumentMetadataSchema normalizes null/undefined to '', so the
		// parsed OUTPUT type has them as plain required strings. Spell them out here rather than
		// widening the production type just to satisfy a fixture.
		TotalAmount:    "",
		VatAmount:      "",
		Siren:          "",
		Iban:           "",
		ExpiryDate:     "",
		ContactName:    "",
		ContactEmail:   "",
		ContactPhone:   "",
		ContactAddress: "",
		ContactWebsite: "",
	}
	if apply != nil {
		apply(&m)
	}
	return m
}

// baseMetadataCorrespondence mirrors the nested baseMetadata in the applyEntityPriorityOverride
// describe block, whose default categorie is 'correspondence'.
func baseMetadataCorrespondence(apply func(*documentschema.DocumentMetadata)) documentschema.DocumentMetadata {
	return baseMetadata(func(m *documentschema.DocumentMetadata) {
		m.Categorie = "correspondence"
		if apply != nil {
			apply(m)
		}
	})
}

func TestRefineClassification(t *testing.T) {
	t.Run("leaves a specific classification untouched", func(t *testing.T) {
		input := baseMetadata(func(m *documentschema.DocumentMetadata) {
			m.Categorie = "invoices"
			m.Subcategorie = "sfr"
		})
		result := RefineClassification(input, "SFR Facture Total TTC", "facture.pdf", emptyDictionary(), defaultPersonalNameDenylist, testDeps())
		if !reflect.DeepEqual(result, input) {
			t.Fatalf("RefineClassification = %#v, want %#v", result, input)
		}
	})

	t.Run("replaces categorie \"personal\" with the rule-based result", func(t *testing.T) {
		input := baseMetadata(func(m *documentschema.DocumentMetadata) {
			m.Categorie = "personal"
			m.Subcategorie = "sfr"
		})
		result := RefineClassification(input, "SFR Facture Total TTC", "facture.pdf", emptyDictionary(), defaultPersonalNameDenylist, testDeps())
		if result.Categorie != "invoices" {
			t.Fatalf("result.Categorie = %q, want %q", result.Categorie, "invoices")
		}
	})

	t.Run("replaces a \"general\" subcategorie with the rule-based result when the rule-based classifier finds something specific", func(t *testing.T) {
		input := baseMetadata(func(m *documentschema.DocumentMetadata) {
			m.Categorie = "invoices"
			m.Subcategorie = "general"
		})
		result := RefineClassification(input, "Facture SFR Total TTC 45.99", "facture.pdf", emptyDictionary(), defaultPersonalNameDenylist, testDeps())
		if result.Subcategorie != "sfr" {
			t.Fatalf("result.Subcategorie = %q, want %q", result.Subcategorie, "sfr")
		}
	})

	t.Run("does not mutate the input object", func(t *testing.T) {
		input := baseMetadata(func(m *documentschema.DocumentMetadata) {
			m.Categorie = "personal"
			m.Subcategorie = "sfr"
		})
		RefineClassification(input, "SFR Facture Total TTC", "facture.pdf", emptyDictionary(), defaultPersonalNameDenylist, testDeps())
		if input.Categorie != "personal" {
			t.Fatalf("input.Categorie = %q, want %q", input.Categorie, "personal")
		}
	})
}

func TestResolveCategory(t *testing.T) {
	t.Run("matches an existing category by id", func(t *testing.T) {
		config := &documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
			category("invoices", "Invoices", nil),
		}}
		got := ResolveCategory(config, "invoices")
		if got.Category.ID != "invoices" {
			t.Fatalf("category.id = %q, want %q", got.Category.ID, "invoices")
		}
		if got.IsNew {
			t.Fatalf("isNew = true, want false")
		}
	})

	t.Run("creates and appends a new category when none matches", func(t *testing.T) {
		config := &documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{}}
		got := ResolveCategory(config, "new_category")
		if !got.IsNew {
			t.Fatalf("isNew = false, want true")
		}
		if got.Category.ID != "new_category" {
			t.Fatalf("category.id = %q, want %q", got.Category.ID, "new_category")
		}
		found := false
		for _, c := range config.Categories {
			if c == got.Category {
				found = true
			}
		}
		if !found {
			t.Fatalf("config.Categories does not contain the returned category")
		}
	})

	t.Run("defaults an empty/falsy categorie to \"administrative\"", func(t *testing.T) {
		config := &documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{}}
		got := ResolveCategory(config, "")
		if got.Category.ID != "administrative" {
			t.Fatalf("category.id = %q, want %q", got.Category.ID, "administrative")
		}
	})
}

func TestResolveSubcategory(t *testing.T) {
	t.Run("matches an existing subcategory by id", func(t *testing.T) {
		cat := category("invoices", "Invoices", nil, subcat("sfr", "SFR"))
		got := ResolveSubcategory(cat, "sfr", "text", "file.pdf", defaultPersonalNameDenylist, testDeps(), nil)
		if got.SubcategoryID != "sfr" {
			t.Fatalf("subcategoryId = %q, want %q", got.SubcategoryID, "sfr")
		}
		if got.IsNew {
			t.Fatalf("isNew = true, want false")
		}
	})

	t.Run("resolves a forbidden slug (general/other/divers) as-is without creating it", func(t *testing.T) {
		cat := category("invoices", "Invoices", nil)
		got := ResolveSubcategory(cat, "other", "text", "file.pdf", defaultPersonalNameDenylist, testDeps(), nil)
		if got.SubcategoryID != "other" {
			t.Fatalf("subcategoryId = %q, want %q", got.SubcategoryID, "other")
		}
		if got.IsNew {
			t.Fatalf("isNew = true, want false")
		}
		if len(cat.Subcategories) != 0 {
			t.Fatalf("category.Subcategories length = %d, want 0", len(cat.Subcategories))
		}
	})

	t.Run("resolves an ungrounded slug to \"general\" instead of creating it", func(t *testing.T) {
		cat := category("invoices", "Invoices", nil)
		got := ResolveSubcategory(cat, "veolia", "nothing here about that entity", "file.pdf", defaultPersonalNameDenylist, testDeps(), nil)
		if got.SubcategoryID != "general" {
			t.Fatalf("subcategoryId = %q, want %q", got.SubcategoryID, "general")
		}
		if got.IsNew {
			t.Fatalf("isNew = true, want false")
		}
	})

	t.Run("creates and appends a new subcategory when the slug is genuinely grounded", func(t *testing.T) {
		cat := category("invoices", "Invoices", nil)
		got := ResolveSubcategory(cat, "veolia", "Veolia here and Veolia there", "veolia_invoice.pdf", defaultPersonalNameDenylist, testDeps(), nil)
		if !got.IsNew {
			t.Fatalf("isNew = false, want true")
		}
		if got.SubcategoryID != "veolia" {
			t.Fatalf("subcategoryId = %q, want %q", got.SubcategoryID, "veolia")
		}
		if got.NewSubcategory == nil || got.NewSubcategory.ID != "veolia" {
			t.Fatalf("newSubcategory = %#v, want id veolia", got.NewSubcategory)
		}
		found := false
		for _, s := range cat.Subcategories {
			if s == got.NewSubcategory {
				found = true
			}
		}
		if !found {
			t.Fatalf("category.Subcategories does not contain newSubcategory")
		}
	})

	t.Run("coerces a bare-year subcategorie to \"general\"", func(t *testing.T) {
		cat := category("administrative", "Administrative", nil)
		got := ResolveSubcategory(cat, "2023", "text", "file.pdf", defaultPersonalNameDenylist, testDeps(), nil)
		if got.SubcategoryID != "general" {
			t.Fatalf("subcategoryId = %q, want %q", got.SubcategoryID, "general")
		}
	})

	t.Run("collapses a verbose underscore-normalized slug onto an existing subcategory whose alias is stored with spaces (regression: a private-overlay credit_mutuel alias set like \"credit mutuel\" / \"ccm springfield\")", func(t *testing.T) {
		cat := category("bank", "Banque & Relevés", nil,
			subcat("credit_mutuel", "Crédit Mutuel", "creditmutuel", "credit mutuel", "ccm springfield", "ccm"),
		)
		got := ResolveSubcategory(cat, "Caisse Credit Mutuel Springfield Centre", "Crédit Mutuel bank statement text", "releve.pdf", defaultPersonalNameDenylist, testDeps(), nil)
		if got.SubcategoryID != "credit_mutuel" {
			t.Fatalf("subcategoryId = %q, want %q", got.SubcategoryID, "credit_mutuel")
		}
		if got.IsNew {
			t.Fatalf("isNew = true, want false")
		}
		if len(cat.Subcategories) != 1 {
			t.Fatalf("category.Subcategories length = %d, want 1", len(cat.Subcategories))
		}
	})

	t.Run("matches a category alias stored with spaces against an underscore-normalized rawCategorie", func(t *testing.T) {
		config := &documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
			category("bank", "Bank", []string{"bank", "compte bancaire"}),
		}}
		got := ResolveCategory(config, "Compte Bancaire")
		if got.IsNew {
			t.Fatalf("isNew = true, want false")
		}
		if got.Category.ID != "bank" {
			t.Fatalf("category.id = %q, want %q", got.Category.ID, "bank")
		}
	})
}

func TestResolveCategoryDuplicateGuard(t *testing.T) {
	config := &documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
		category("administrative", "Administrative", nil, subcat("france_travail", "France Travail")),
		category("housing", "Housing", nil),
	}}

	t.Run("blocks a near-duplicate category name and remaps to the existing one", func(t *testing.T) {
		got := ResolveCategory(config, "administratif")
		if got.IsNew {
			t.Fatalf("isNew = true, want false")
		}
		if got.Category.ID != "administrative" {
			t.Fatalf("category.id = %q, want %q", got.Category.ID, "administrative")
		}
		if got.Conflict == nil || got.Conflict.Kind != taxonomyconflicts.KindCategoryNear {
			t.Fatalf("conflict.Kind = %#v, want %q", got.Conflict, taxonomyconflicts.KindCategoryNear)
		}
		if len(config.Categories) != 2 { // nothing auto-created
			t.Fatalf("config.Categories length = %d, want 2", len(config.Categories))
		}
	})

	t.Run("blocks an entity name proposed as a top-level category", func(t *testing.T) {
		got := ResolveCategory(config, "france_travail", "france_travail")
		if got.IsNew {
			t.Fatalf("isNew = true, want false")
		}
		if got.Category.ID != "administrative" {
			t.Fatalf("category.id = %q, want %q", got.Category.ID, "administrative")
		}
		if got.Conflict == nil || got.Conflict.Kind != taxonomyconflicts.KindEntityAsCategory {
			t.Fatalf("conflict.Kind = %#v, want %q", got.Conflict, taxonomyconflicts.KindEntityAsCategory)
		}
		if got.Conflict.MappedSubcategoryID != "france_travail" {
			t.Fatalf("conflict.MappedSubcategoryID = %q, want %q", got.Conflict.MappedSubcategoryID, "france_travail")
		}
		if len(config.Categories) != 2 {
			t.Fatalf("config.Categories length = %d, want 2", len(config.Categories))
		}
	})

	t.Run("still auto-creates a genuinely new category", func(t *testing.T) {
		got := ResolveCategory(config, "justice")
		if !got.IsNew {
			t.Fatalf("isNew = false, want true")
		}
		if got.Category.ID != "justice" {
			t.Fatalf("category.id = %q, want %q", got.Category.ID, "justice")
		}
	})
}

func TestResolveSubcategoryDuplicateGuard(t *testing.T) {
	config := &documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
		category("invoices", "Invoices", nil, subcat("bouygues_telecom", "Bouygues Telecom", "bouyguestelecom")),
		category("housing", "Housing", nil, subcat("foncia", "Foncia", "loyer")),
	}}
	invoices := config.Categories[0]

	t.Run("blocks a slug that exists under another category and maps to the canonical owner", func(t *testing.T) {
		got := ResolveSubcategory(invoices, "foncia", "Quittance de loyer Foncia", "quittance.pdf", defaultPersonalNameDenylist, testDeps(), config)
		if got.IsNew {
			t.Fatalf("isNew = true, want false")
		}
		if got.SubcategoryID != "foncia" {
			t.Fatalf("subcategoryId = %q, want %q", got.SubcategoryID, "foncia")
		}
		if got.Conflict == nil || got.Conflict.Kind != taxonomyconflicts.KindCrossCategory {
			t.Fatalf("conflict.Kind = %#v, want %q", got.Conflict, taxonomyconflicts.KindCrossCategory)
		}
		if got.Conflict.MappedCategoryID != "housing" {
			t.Fatalf("conflict.MappedCategoryID = %q, want %q", got.Conflict.MappedCategoryID, "housing")
		}
		if len(invoices.Subcategories) != 1 { // nothing auto-created under invoices
			t.Fatalf("invoices.Subcategories length = %d, want 1", len(invoices.Subcategories))
		}
	})

	t.Run("merges a same-category near-duplicate spelling onto the existing entry", func(t *testing.T) {
		// 'bouygue_telecom' (dropped 's') is not caught by the caller's exact/alias lookup — only the
		// guard's fuzzy pass recognizes it as the same entity as 'bouygues_telecom'.
		got := ResolveSubcategory(invoices, "bouygue_telecom", "Bouygues Telecom facture", "bouygues.pdf", defaultPersonalNameDenylist, testDeps(), config)
		if got.IsNew {
			t.Fatalf("isNew = true, want false")
		}
		if got.SubcategoryID != "bouygues_telecom" {
			t.Fatalf("subcategoryId = %q, want %q", got.SubcategoryID, "bouygues_telecom")
		}
		if got.Conflict == nil || got.Conflict.Kind != taxonomyconflicts.KindSpellingMerge {
			t.Fatalf("conflict.Kind = %#v, want %q", got.Conflict, taxonomyconflicts.KindSpellingMerge)
		}
		if len(invoices.Subcategories) != 1 {
			t.Fatalf("invoices.Subcategories length = %d, want 1", len(invoices.Subcategories))
		}
	})

	t.Run("does not block without a full taxonomy (backward compatible)", func(t *testing.T) {
		got := ResolveSubcategory(invoices, "foncia", "Quittance de loyer Foncia", "quittance.pdf", defaultPersonalNameDenylist, testDeps(), nil)
		if !got.IsNew { // legacy behavior: no cross-category knowledge
			t.Fatalf("isNew = false, want true")
		}
		if got.SubcategoryID != "foncia" {
			t.Fatalf("subcategoryId = %q, want %q", got.SubcategoryID, "foncia")
		}
	})
}

func TestApplyEntityPriorityOverride(t *testing.T) {
	bankDictionary := documentschema.EntityDictionary{
		Banks:     []*documentschema.EntityItem{{Slug: "credit_mutuel", Name: "Crédit Mutuel", Aliases: []string{"credit mutuel", "ccm"}}},
		Energy:    []*documentschema.EntityItem{},
		Telecom:   []*documentschema.EntityItem{},
		Insurance: []*documentschema.EntityItem{},
		Gov:       []*documentschema.EntityItem{},
		Health:    []*documentschema.EntityItem{},
	}
	govDictionary := documentschema.EntityDictionary{
		Banks:     []*documentschema.EntityItem{},
		Energy:    []*documentschema.EntityItem{},
		Telecom:   []*documentschema.EntityItem{},
		Insurance: []*documentschema.EntityItem{},
		Gov:       []*documentschema.EntityItem{{Slug: "france_travail", Name: "France Travail", Aliases: []string{"france travail", "pole emploi"}}},
		Health:    []*documentschema.EntityItem{},
	}

	t.Run("overrides a weak/fallback category (correspondence) with the entity-dictionary-grounded category+subcategory", func(t *testing.T) {
		input := baseMetadataCorrespondence(func(m *documentschema.DocumentMetadata) {
			m.Subcategorie = "credit_mutuel_springfield_centre"
		})
		result := ApplyEntityPriorityOverride(input, "CAISSE DE CREDIT MUTUEL SPRINGFIELD CENTRE", bankDictionary)
		if !result.Overridden {
			t.Fatalf("overridden = false, want true")
		}
		if result.Categorie != "bank" {
			t.Fatalf("categorie = %q, want %q", result.Categorie, "bank")
		}
		if result.Subcategorie != "credit_mutuel" {
			t.Fatalf("subcategorie = %q, want %q", result.Subcategorie, "credit_mutuel")
		}
	})

	t.Run("does not override a specific (non-fallback) category even when the entity is dictionary-grounded under a different category", func(t *testing.T) {
		input := baseMetadata(func(m *documentschema.DocumentMetadata) {
			m.Categorie = "bulletin_salaire"
			m.Subcategorie = "france_travail"
		})
		result := ApplyEntityPriorityOverride(input, "France Travail", govDictionary)
		if result.Overridden {
			t.Fatalf("overridden = true, want false")
		}
		if result.Categorie != "bulletin_salaire" {
			t.Fatalf("categorie = %q, want %q", result.Categorie, "bulletin_salaire")
		}
		if result.Subcategorie != "france_travail" {
			t.Fatalf("subcategorie = %q, want %q", result.Subcategorie, "france_travail")
		}
	})

	t.Run("does not override when no entity was extracted", func(t *testing.T) {
		input := baseMetadataCorrespondence(nil)
		result := ApplyEntityPriorityOverride(input, "", bankDictionary)
		if result.Overridden {
			t.Fatalf("overridden = true, want false")
		}
	})

	t.Run("does not override when the entity is not recognized in the entity dictionary", func(t *testing.T) {
		input := baseMetadataCorrespondence(nil)
		result := ApplyEntityPriorityOverride(input, "Some Unknown Local Shop", bankDictionary)
		if result.Overridden {
			t.Fatalf("overridden = true, want false")
		}
	})

	t.Run("treats \"other\" and \"personal\" as weak/fallback categories too", func(t *testing.T) {
		input := baseMetadata(func(m *documentschema.DocumentMetadata) {
			m.Categorie = "other"
			m.Subcategorie = "general"
		})
		result := ApplyEntityPriorityOverride(input, "Crédit Mutuel", bankDictionary)
		if !result.Overridden {
			t.Fatalf("overridden = false, want true")
		}
		if result.Categorie != "bank" {
			t.Fatalf("categorie = %q, want %q", result.Categorie, "bank")
		}
	})

	t.Run("overrides a bank-domain entity unconditionally, even against a non-weak/arbitrary wrong category (regression: real Step D output landed on \"reports\", not just \"correspondence\")", func(t *testing.T) {
		input := baseMetadata(func(m *documentschema.DocumentMetadata) {
			m.Categorie = "reports"
			m.Subcategorie = "credit_mutuel_springfield_centre"
		})
		result := ApplyEntityPriorityOverride(input, "CAISSE DE CREDIT MUTUEL SPRINGFIELD CENTRE", bankDictionary)
		if !result.Overridden {
			t.Fatalf("overridden = false, want true")
		}
		if result.Categorie != "bank" {
			t.Fatalf("categorie = %q, want %q", result.Categorie, "bank")
		}
		if result.Subcategorie != "credit_mutuel" {
			t.Fatalf("subcategorie = %q, want %q", result.Subcategorie, "credit_mutuel")
		}
	})

	t.Run("does not override when Step D already agrees the category is \"bank\"", func(t *testing.T) {
		input := baseMetadata(func(m *documentschema.DocumentMetadata) {
			m.Categorie = "bank"
			m.Subcategorie = "credit_mutuel"
		})
		result := ApplyEntityPriorityOverride(input, "Crédit Mutuel", bankDictionary)
		if result.Overridden {
			t.Fatalf("overridden = true, want false")
		}
	})
}
