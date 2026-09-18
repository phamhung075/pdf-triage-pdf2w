package taxonomy

import "testing"

// The isYearString, isForbiddenSubcategory, findCanonicalCategoryForSubcategory and
// mergeSubcategoryInTaxonomy cases below are ported verbatim from
// pdf-triage's src/domain/taxonomy.test.ts. isPathInsideDir and detectFileType have no upstream
// test cases (the TS suite never covered them); the extra block at the end is added so the two
// ported functions are not shipped unverified.

func TestIsYearString(t *testing.T) {
	t.Run("accepts a plain 4-digit year", func(t *testing.T) {
		if !IsYearString("2023") {
			t.Fatalf("IsYearString(%q) = false, want true", "2023")
		}
	})

	t.Run("accepts a 4-digit year with surrounding whitespace", func(t *testing.T) {
		if !IsYearString("  2023  ") {
			t.Fatalf("IsYearString(%q) = false, want true", "  2023  ")
		}
	})

	t.Run("rejects a 5-digit number", func(t *testing.T) {
		if IsYearString("20233") {
			t.Fatalf("IsYearString(%q) = true, want false", "20233")
		}
	})

	t.Run("rejects non-numeric text", func(t *testing.T) {
		if IsYearString("abcd") {
			t.Fatalf("IsYearString(%q) = true, want false", "abcd")
		}
	})

	t.Run("rejects undefined", func(t *testing.T) {
		// TS passes `undefined`; the Go zero value "" is the equivalent input.
		if IsYearString("") {
			t.Fatalf("IsYearString(%q) = true, want false", "")
		}
	})
}

func TestIsForbiddenSubcategory(t *testing.T) {
	t.Run(`forbids "general", "other", "divers" case-insensitively`, func(t *testing.T) {
		for _, s := range []string{"general", "GENERAL", "other", "divers"} {
			if !IsForbiddenSubcategory(s) {
				t.Fatalf("IsForbiddenSubcategory(%q) = false, want true", s)
			}
		}
	})

	t.Run("forbids a bare year string", func(t *testing.T) {
		if !IsForbiddenSubcategory("2023") {
			t.Fatalf("IsForbiddenSubcategory(%q) = false, want true", "2023")
		}
	})

	t.Run("forbids undefined and empty string", func(t *testing.T) {
		for _, s := range []string{"", "   "} {
			if !IsForbiddenSubcategory(s) {
				t.Fatalf("IsForbiddenSubcategory(%q) = false, want true", s)
			}
		}
	})

	t.Run("allows a real, specific subcategory slug", func(t *testing.T) {
		for _, s := range []string{"sfr", "credit_mutuel"} {
			if IsForbiddenSubcategory(s) {
				t.Fatalf("IsForbiddenSubcategory(%q) = true, want false", s)
			}
		}
	})
}

// repair-registry.ts:121 uses this to decide whether a document's category is "wrong", then
// overwrites the DB row and hands the new category to relocalizeFileIfNeeded, which PHYSICALLY
// MOVES the file. The lookup returned the first category containing the slug *by array position*,
// and the live taxonomy has 42 subcategory slugs sitting under more than one category — so a
// Repair run would have relocated 87 of 276 archived documents into a category nobody chose:
// payslips filed under bulletin_salaire/clinic_x moved to contracts/clinic_x,
// identity/permis_conduire moved to administrative, and so on. Ambiguity must never be resolved
// by array order.
func TestFindCanonicalCategoryForSubcategory(t *testing.T) {
	config := &TaxonomyConfig{
		Categories: []*Category{
			{ID: "contracts", Subcategories: []*Subcategory{{ID: "clinic_x"}, {ID: "acme"}}},
			{ID: "bulletin_salaire", Subcategories: []*Subcategory{{ID: "clinic_x"}}},
			{ID: "health", Subcategories: []*Subcategory{{ID: "clinic_x"}, {ID: "ameli"}}},
			{ID: "invoices", Subcategories: []*Subcategory{{ID: "engie"}}},
		},
	}

	t.Run("resolves a slug that belongs to exactly one category", func(t *testing.T) {
		if got := FindCanonicalCategoryForSubcategory("engie", config, ""); got == nil || *got != "invoices" {
			t.Fatalf("engie => %v, want invoices", got)
		}
		if got := FindCanonicalCategoryForSubcategory("ameli", config, ""); got == nil || *got != "health" {
			t.Fatalf("ameli => %v, want health", got)
		}
	})

	t.Run("returns null for an unknown slug", func(t *testing.T) {
		if got := FindCanonicalCategoryForSubcategory("not_a_real_slug", config, ""); got != nil {
			t.Fatalf("got %q, want nil", *got)
		}
	})

	t.Run("returns null for a forbidden slug rather than resolving it", func(t *testing.T) {
		if got := FindCanonicalCategoryForSubcategory("general", config, ""); got != nil {
			t.Fatalf("got %q, want nil", *got)
		}
		if got := FindCanonicalCategoryForSubcategory("2024", config, ""); got != nil {
			t.Fatalf("got %q, want nil", *got)
		}
	})

	t.Run("refuses to pick a winner when the slug exists under several categories", func(t *testing.T) {
		// 'clinic_x' is under contracts, bulletin_salaire and health. Array order previously made
		// 'contracts' win and dragged every payslip with it.
		if got := FindCanonicalCategoryForSubcategory("clinic_x", config, ""); got != nil {
			t.Fatalf("got %q, want nil", *got)
		}
	})

	t.Run("keeps the document where it is when its current category is one of the candidates", func(t *testing.T) {
		if got := FindCanonicalCategoryForSubcategory("clinic_x", config, "bulletin_salaire"); got == nil || *got != "bulletin_salaire" {
			t.Fatalf("bulletin_salaire => %v, want bulletin_salaire", got)
		}
		if got := FindCanonicalCategoryForSubcategory("clinic_x", config, "health"); got == nil || *got != "health" {
			t.Fatalf("health => %v, want health", got)
		}
	})

	t.Run("still returns null for an ambiguous slug when the current category is not a candidate", func(t *testing.T) {
		if got := FindCanonicalCategoryForSubcategory("clinic_x", config, "invoices"); got != nil {
			t.Fatalf("got %q, want nil", *got)
		}
	})

	t.Run("still corrects a genuinely misfiled document when the slug is unambiguous", func(t *testing.T) {
		if got := FindCanonicalCategoryForSubcategory("engie", config, "correspondence"); got == nil || *got != "invoices" {
			t.Fatalf("got %v, want invoices", got)
		}
	})
}

func TestMergeSubcategoryInTaxonomy(t *testing.T) {
	build := func() []*Category {
		return []*Category{
			{
				ID: "invoices",
				Subcategories: []*Subcategory{
					{ID: "bouyguestelecom", Name: "Bouyguestelecom", Aliases: []string{"bouyguestelecom"}},
					{ID: "bouygues_telecom", Name: "Bouygues Telecom", Aliases: []string{"bouygues_telecom"}},
					{ID: "engie", Name: "Engie", Aliases: []string{"engie"}},
				},
			},
		}
	}

	t.Run("merges into an existing target without leaving a duplicate id", func(t *testing.T) {
		cats := MergeSubcategoryInTaxonomy(build(), "invoices", "bouyguestelecom", "bouygues_telecom")
		subs := cats[0].Subcategories
		count := 0
		for _, s := range subs {
			if s.ID == "bouygues_telecom" {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("found %d entries with id bouygues_telecom, want 1", count)
		}
		for _, s := range subs {
			if s.ID == "bouyguestelecom" {
				t.Fatalf("losing spelling bouyguestelecom still present")
			}
		}
		if len(subs) != 2 { // survivor + engie
			t.Fatalf("got %d subcategories, want 2", len(subs))
		}
	})

	t.Run("keeps the losing spelling as an alias so old references still resolve", func(t *testing.T) {
		cats := MergeSubcategoryInTaxonomy(build(), "invoices", "bouyguestelecom", "bouygues_telecom")
		var winner *Subcategory
		for _, s := range cats[0].Subcategories {
			if s.ID == "bouygues_telecom" {
				winner = s
			}
		}
		if winner == nil {
			t.Fatalf("winning subcategory not found")
		}
		if !contains(winner.Aliases, "bouyguestelecom") {
			t.Fatalf("aliases %v missing bouyguestelecom", winner.Aliases)
		}
		if !contains(winner.Aliases, "bouygues_telecom") {
			t.Fatalf("aliases %v missing bouygues_telecom", winner.Aliases)
		}
	})

	t.Run("still renames in place when the target does not exist", func(t *testing.T) {
		cats := MergeSubcategoryInTaxonomy(build(), "invoices", "engie", "engie_sa")
		subs := cats[0].Subcategories
		for _, s := range subs {
			if s.ID == "engie" {
				t.Fatalf("old id engie still present")
			}
		}
		var renamed *Subcategory
		for _, s := range subs {
			if s.ID == "engie_sa" {
				renamed = s
			}
		}
		if renamed == nil {
			t.Fatalf("renamed subcategory engie_sa not found")
		}
		if renamed.Name != "Engie Sa" {
			t.Fatalf("name = %q, want %q", renamed.Name, "Engie Sa")
		}
		if !contains(renamed.Aliases, "engie") {
			t.Fatalf("aliases %v missing engie", renamed.Aliases)
		}
	})

	t.Run("leaves the taxonomy untouched for a no-op or unknown category", func(t *testing.T) {
		if got := MergeSubcategoryInTaxonomy(build(), "invoices", "engie", "engie"); len(got[0].Subcategories) != 3 {
			t.Fatalf("no-op merge changed subcategory count to %d, want 3", len(got[0].Subcategories))
		}
		if got := MergeSubcategoryInTaxonomy(build(), "nope", "engie", "x"); len(got[0].Subcategories) != 3 {
			t.Fatalf("unknown-category merge changed subcategory count to %d, want 3", len(got[0].Subcategories))
		}
	})

	t.Run("creates the target when neither side exists", func(t *testing.T) {
		cats := MergeSubcategoryInTaxonomy(build(), "invoices", "ghost", "newthing")
		var found bool
		for _, s := range cats[0].Subcategories {
			if s.ID == "newthing" {
				found = true
			}
		}
		if !found {
			t.Fatalf("created target newthing not found")
		}
	})
}

// Extra coverage for the two in-scope functions the TS suite did not test. The expectations are
// taken from the TS implementations' documented behavior, not invented.
func TestIsPathInsideDir(t *testing.T) {
	cases := []struct {
		name     string
		fullPath string
		dirPath  string
		want     bool
	}{
		{"exact path is inside", "/a/b", "/a/b", true},
		{"nested path is inside", "/a/b/c", "/a/b", true},
		{"parent is not inside child", "/a", "/a/b", false},
		{"prefix-sharing sibling is not inside", "/archive_old/x", "/archive", false},
		{"path is normalized before comparing", "/a/./b/../c", "/a", true},
		{"comparison is case-insensitive", "/A/B", "/a", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsPathInsideDir(tc.fullPath, tc.dirPath); got != tc.want {
				t.Fatalf("IsPathInsideDir(%q, %q) = %v, want %v", tc.fullPath, tc.dirPath, got, tc.want)
			}
		})
	}
}

func TestDetectFileType(t *testing.T) {
	cases := []struct {
		filename string
		want     DocumentFileType
	}{
		{"facture.pdf", DocumentFileTypePDF},
		{"photo.PNG", DocumentFileTypeImage},
		{"scan.jpeg", DocumentFileTypeImage},
		{"notes.txt", DocumentFileTypeText},
		{"data.MD", DocumentFileTypeText},
		{"contract.docx", DocumentFileTypeWord},
		{"legacy.doc", DocumentFileTypeWord},
		{"ledger.xlsx", DocumentFileTypeExcel},
		{"legacy.xls", DocumentFileTypeExcel},
		{"no-extension", DocumentFileTypePDF},
		{"", DocumentFileTypePDF},
		{"animation.gif", DocumentFileTypePDF},
		{"report.final.pdf", DocumentFileTypePDF},
	}
	for _, tc := range cases {
		if got := DetectFileType(tc.filename); got != tc.want {
			t.Fatalf("DetectFileType(%q) = %q, want %q", tc.filename, got, tc.want)
		}
	}
}

// TestPosixExtname locks the port to Node's path.posix.extname. Every expected value below was
// produced by running `require('path').posix.extname(x)` on Node, because Go's path.Ext disagrees
// on the leading-dot and trailing-dot cases this exists to match.
func TestPosixExtname(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"index.html", ".html"},
		{".bashrc", ""},
		{"foo.", "."},
		{".foo.bar", ".bar"},
		{"..foo", ".foo"},
		{"a.b.c", ".c"},
		{"a/b/c.PDF", ".PDF"},
		{`C:\x\y.jpg`, ".jpg"},
		{"file", ""},
		{"a/b", ""},
		{"", ""},
		{"..", ""},
		{".", ""},
		{"report.final.pdf", ".pdf"},
	}
	for _, tc := range cases {
		if got := posixExtname(tc.in); got != tc.want {
			t.Fatalf("posixExtname(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
