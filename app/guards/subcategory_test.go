package guards

import (
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// Ported guard cases from web-server.test.ts (PUT /api/documents/:id and
// PUT /api/manual-decisions/:id), mcp-server.test.ts (update_document_metadata),
// relocalize-document.test.ts (reclassifyAndRelocalizeDocument) and triage-scan.ts's strict
// no-subcategory block. The upstream files are GREEN except for the two known-red web-server cases
// (neither is a subcategory guard).

func TestForbiddenSubcategoryViolation_ExplicitWrites(t *testing.T) {
	const full = "'%s' is not a valid subcategory (general/other/divers/year strings are not allowed — Golden Rule #4). Please choose a specific entity or document-type name."
	const short = "'%s' is not a valid subcategory (general/other/divers/year strings are not allowed — Golden Rule #4)."

	t.Run("web-server PUT /api/documents/:id rejects a forbidden subcategory — Golden Rule #4", func(t *testing.T) {
		// web-server.test.ts:464, :476
		for _, in := range []string{"general", "2026"} {
			v := ForbiddenSubcategoryViolation(in)
			if v == nil {
				t.Fatalf("ForbiddenSubcategoryViolation(%q) = nil, want violation", in)
			}
			if v.Code != CodeForbiddenSubcategory || v.HTTPStatus != 400 {
				t.Fatalf("code/status = %q/%d, want %q/400", v.Code, v.HTTPStatus, CodeForbiddenSubcategory)
			}
			if want := strings.Replace(full, "%s", in, 1); v.Message != want {
				t.Fatalf("message = %q, want %q", v.Message, want)
			}
			if !strings.Contains(v.Message, "not a valid subcategory") {
				t.Fatalf("message %q must contain the TS phrase", v.Message)
			}
			if v.Subcategory != in {
				t.Fatalf("Subcategory = %q, want %q", v.Subcategory, in)
			}
		}
	})

	t.Run("web-server PUT /api/manual-decisions/:id rejects divers with the short message", func(t *testing.T) {
		// web-server.test.ts:706; the exact message is web-server.ts:587 (no trailing guidance).
		v := ForbiddenSubcategoryViolation("divers")
		if v == nil {
			t.Fatal("want a violation for divers")
		}
		if want := strings.Replace(short, "%s", "divers", 1); v.ShortMessage != want {
			t.Fatalf("ShortMessage = %q, want %q", v.ShortMessage, want)
		}
	})

	t.Run("mcp-server update_document_metadata rejects general/year/divers — Golden Rule #4", func(t *testing.T) {
		// mcp-server.test.ts:209-222 (subcategory, subcategorie aliases).
		for _, in := range []string{"general", "2024", "divers"} {
			v := ForbiddenSubcategoryViolation(in)
			if v == nil {
				t.Fatalf("ForbiddenSubcategoryViolation(%q) = nil, want violation", in)
			}
			if !strings.Contains(v.Message, "Golden Rule #4") {
				t.Fatalf("MCP message %q must contain 'Golden Rule #4'", v.Message)
			}
		}
	})

	t.Run("relocalize-document reclassify rejects an explicit forbidden subcategory", func(t *testing.T) {
		// relocalize-document.test.ts:315-323.
		v := ForbiddenSubcategoryViolation("general")
		if v == nil || !strings.Contains(v.Message, "Golden Rule #4") {
			t.Fatalf("violation = %v, want one containing 'Golden Rule #4'", v)
		}
	})

	t.Run("allows a real, specific subcategory slug", func(t *testing.T) {
		// taxonomy.test.ts / taxonomy_test.go's allow list.
		for _, in := range []string{"sfr", "credit_mutuel", "orange", "societe_generale", "permis_conduire"} {
			if v := ForbiddenSubcategoryViolation(in); v != nil {
				t.Fatalf("ForbiddenSubcategoryViolation(%q) = %q, want nil", in, v.Message)
			}
		}
	})
}

func TestIsForbiddenSubcategory_MatchesTaxonomy(t *testing.T) {
	// The canonical predicate must BE taxonomy.IsForbiddenSubcategory — never a hand-rolled list.
	for _, in := range []string{"", "   ", "general", "GENERAL", "other", "divers", "unknown", "none",
		"camscanner", "anyscanner", "2024", "102818", "verso_png", "img_123_png", "pdf", "docx"} {
		if got, want := IsForbiddenSubcategory(in), taxonomy.IsForbiddenSubcategory(in); got != want {
			t.Fatalf("IsForbiddenSubcategory(%q) = %v, taxonomy = %v", in, got, want)
		}
		if !IsForbiddenSubcategory(in) {
			t.Fatalf("IsForbiddenSubcategory(%q) = false, want true", in)
		}
	}
}

func TestStrictNoSubcategoryViolation(t *testing.T) {
	t.Run("blocks a resolved sentinel with the exact scan message and reason", func(t *testing.T) {
		// triage-scan.ts:301-325: resolveSubcategory returns these sentinels verbatim.
		for _, resolved := range []string{"general", "unknown", "camscanner", "2024", "verso_png"} {
			v := StrictNoSubcategoryViolation("incoming.pdf", resolved)
			if v == nil {
				t.Fatalf("StrictNoSubcategoryViolation(%q) = nil, want violation", resolved)
			}
			if v.Code != CodeNoSubcategory {
				t.Fatalf("code = %q, want %q", v.Code, CodeNoSubcategory)
			}
			want := "❌ Blocked: Failed to assign specific subcategory to 'incoming.pdf'. Moved to __raws/.blocked_files."
			if v.Message != want {
				t.Fatalf("message = %q, want %q", v.Message, want)
			}
			if v.BlockedFileReason != "NO_SUBCATEGORY" {
				t.Fatalf("reason = %q, want NO_SUBCATEGORY", v.BlockedFileReason)
			}
		}
	})

	t.Run("passes a grounded resolved subcategory", func(t *testing.T) {
		if v := StrictNoSubcategoryViolation("incoming.pdf", "sfr"); v != nil {
			t.Fatalf("want nil, got %q", v.Message)
		}
	})
}

func TestGenericCategoryViolation(t *testing.T) {
	// web-server.test.ts:715-721 plus web-server.ts:589's four-way test.
	t.Run("rejects general/other/divers/empty", func(t *testing.T) {
		for _, in := range []string{"", "general", "GENERAL", "other", "divers", "  divers  "} {
			v := GenericCategoryViolation(in)
			if v == nil {
				t.Fatalf("GenericCategoryViolation(%q) = nil, want violation", in)
			}
			if v.Code != CodeGenericCategory || v.HTTPStatus != 400 {
				t.Fatalf("code/status = %q/%d", v.Code, v.HTTPStatus)
			}
			want := "A decision needs a concrete target category — general/other/divers are not allowed."
			if v.Message != want {
				t.Fatalf("message = %q, want %q", v.Message, want)
			}
		}
	})

	t.Run("accepts a concrete category", func(t *testing.T) {
		for _, in := range []string{"telecom", "invoices", "administrative"} {
			if v := GenericCategoryViolation(in); v != nil {
				t.Fatalf("GenericCategoryViolation(%q) = %q, want nil", in, v.Message)
			}
		}
	})
}
