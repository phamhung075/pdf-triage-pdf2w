package guards

import (
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/classification"
	"github.com/phamhung075/pdf-triage-pdf2w/classificationresolution"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
)

// TestClassificationResolutionSentinelsAreCaught proves the guard's "reuse classificationresolution's
// sentinel handling" claim behaviorally: resolveSubcategory returns forbidden sentinels verbatim
// ('general', 'unknown', 'camscanner', a bare year, a file-extension slug) so the caller's strict
// guard BLOCKS them. classificationresolution's own narrow FORBIDDEN_SUBCATEGORIES set is
// unexported, so the guard reaches them through taxonomy.IsForbiddenSubcategory, which is a superset
// (see subcategory.go).
func TestClassificationResolutionSentinelsAreCaught(t *testing.T) {
	deps := classificationresolution.Deps{}

	for _, raw := range []string{"general", "other", "divers", "unknown", "camscanner", "2024", "verso_png", "102818_jpg"} {
		raw := raw
		t.Run(raw, func(t *testing.T) {
			cat := &documentschema.CategoryItem{ID: "invoices", Subcategories: []*documentschema.SubcategoryItem{}}
			res := classificationresolution.ResolveSubcategory(cat, raw, "", "incoming.pdf", nil, deps, nil)
			if !IsForbiddenSubcategory(res.SubcategoryID) {
				t.Fatalf("ResolveSubcategory(%q) returned subcategory %q, which the guard does NOT block", raw, res.SubcategoryID)
			}
		})
	}

	// Positive control: a grounded slug must survive the resolution and pass the guard.
	t.Run("grounded slug passes", func(t *testing.T) {
		rawText := "Facture sfr janvier 2026. Prelevement sfr pour l'abonnement mobile."
		cat := &documentschema.CategoryItem{ID: "invoices", Subcategories: []*documentschema.SubcategoryItem{}}
		res := classificationresolution.ResolveSubcategory(cat, "sfr", rawText, "facture_sfr.pdf", nil, deps, nil)
		t.Logf("resolved=%q grounded=%v", res.SubcategoryID, classification.IsGroundedSubcategorySlug("sfr", rawText, "facture_sfr.pdf", nil))
		if IsForbiddenSubcategory(res.SubcategoryID) {
			t.Fatalf("grounded result %q was blocked", res.SubcategoryID)
		}
	})
}
