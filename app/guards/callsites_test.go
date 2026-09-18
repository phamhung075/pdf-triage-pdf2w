package guards

import (
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// TestFormerCallSitesShareOneVerdict is the required table-driven proof that every former
// duplicated call-site variant yields the same Golden Rule 4 verdict through the single canonical
// implementation. Each row names the TypeScript surface and the literal input that surface feeds
// the guard today; the three guard entry points (the predicate, the explicit-write constructor and
// the scan-block constructor) must all agree with taxonomy.IsForbiddenSubcategory.
func TestFormerCallSitesShareOneVerdict(t *testing.T) {
	cases := []struct {
		surface string
		input   string
		want    bool
	}{
		// web-server.ts:1262 PUT /api/documents/:id — web-server.test.ts:464, :476.
		{"web-server PUT /api/documents/:id", "general", true},
		{"web-server PUT /api/documents/:id", "2026", true},
		// web-server.ts:586 PUT /api/manual-decisions/:id — web-server.test.ts:706.
		{"web-server PUT /api/manual-decisions/:id", "divers", true},
		// mcp-server.ts:236 update_document_metadata — mcp-server.test.ts:209-222 (EN + FR alias).
		{"mcp-server update_document_metadata", "general", true},
		{"mcp-server update_document_metadata", "2024", true},
		{"mcp-server update_document_metadata", "divers", true},
		// relocalize-document.ts:208 reclassifyAndRelocalizeDocument — test:315.
		{"relocalize-document reclassify", "general", true},
		// triage-scan.ts:301 the resolved-subcategory strict guard; sentinels from
		// classification-resolution.ts (resolveSubcategory returns them verbatim).
		{"triage-scan strict no-subcategory", "general", true},
		{"triage-scan strict no-subcategory", "unknown", true},
		{"triage-scan strict no-subcategory", "camscanner", true},
		{"triage-scan strict no-subcategory", "2024", true},
		{"triage-scan strict no-subcategory", "verso_png", true},
		{"triage-scan strict no-subcategory", "102818_jpg", true},
		{"triage-scan strict no-subcategory", "", true},
		// Every surface must PASS a real, grounded slug identically.
		{"web-server PUT /api/documents/:id", "sfr", false},
		{"web-server PUT /api/manual-decisions/:id", "societe_generale", false},
		{"mcp-server update_document_metadata", "orange", false},
		{"relocalize-document reclassify", "credit_mutuel", false},
		{"triage-scan strict no-subcategory", "permis_conduire", false},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.surface+"/"+tc.input, func(t *testing.T) {
			if got := taxonomy.IsForbiddenSubcategory(tc.input); got != tc.want {
				t.Fatalf("taxonomy.IsForbiddenSubcategory(%q) = %v, want %v", tc.input, got, tc.want)
			}
			if got := IsForbiddenSubcategory(tc.input); got != tc.want {
				t.Fatalf("IsForbiddenSubcategory(%q) = %v, want %v", tc.input, got, tc.want)
			}
			if got := ForbiddenSubcategoryViolation(tc.input) != nil; got != tc.want {
				t.Fatalf("ForbiddenSubcategoryViolation(%q) verdict = %v, want %v", tc.input, got, tc.want)
			}
			if got := StrictNoSubcategoryViolation("incoming.pdf", tc.input) != nil; got != tc.want {
				t.Fatalf("StrictNoSubcategoryViolation(%q) verdict = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// TestFormerGenericCategoryCallSitesShareOneVerdict is the same proof for the manual-decisions
// generic-category variant (web-server.ts:589), the only surface that had this guard.
func TestFormerGenericCategoryCallSitesShareOneVerdict(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"", true},
		{"general", true},
		{"GENERAL", true},
		{"other", true},
		{"divers", true},
		{"telecom", false},
		{"invoices", false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run("generic/"+tc.input, func(t *testing.T) {
			if got := IsGenericCategory(tc.input); got != tc.want {
				t.Fatalf("IsGenericCategory(%q) = %v, want %v", tc.input, got, tc.want)
			}
			if got := GenericCategoryViolation(tc.input) != nil; got != tc.want {
				t.Fatalf("GenericCategoryViolation(%q) verdict = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}
