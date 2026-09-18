package canonicalpath

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func strp(s string) *string { return &s }

func TestComputeCanonicalPath(t *testing.T) {
	root := filepath.Join("C:", "test-archive")

	t.Run("builds category/subcategory/year/filename under outputRootDir", func(t *testing.T) {
		got := ComputeCanonicalPath(`C:\raws\facture.pdf`, "invoices", root, strp("sfr"), strp("2024-05-12"), nil)
		want := filepath.Join(root, "invoices", "sfr", "2024", "facture.pdf")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("falls back to the current year when dateStr has no 20xx year", func(t *testing.T) {
		got := ComputeCanonicalPath(`C:\raws\facture.pdf`, "invoices", root, strp("sfr"), nil, nil)
		year := strconv.Itoa(time.Now().Year())
		want := filepath.Join(root, "invoices", "sfr", year, "facture.pdf")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run(`coerces a bare-year subcategory to "general" instead of nesting under a year folder`, func(t *testing.T) {
		got := ComputeCanonicalPath(`C:\raws\doc.pdf`, "administrative", root, strp("2023"), strp("2024-01-01"), nil)
		want := filepath.Join(root, "administrative", "general", "2024", "doc.pdf")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run(`defaults an empty category to "other" and empty subcategory to "general"`, func(t *testing.T) {
		got := ComputeCanonicalPath(`C:\raws\doc.pdf`, "", root, strp(""), strp("2024-01-01"), nil)
		want := filepath.Join(root, "other", "general", "2024", "doc.pdf")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("splits a subcategory containing a slash into nested path segments", func(t *testing.T) {
		got := ComputeCanonicalPath(`C:\raws\doc.pdf`, "invoices", root, strp("foo/bar"), strp("2024-01-01"), nil)
		want := filepath.Join(root, "invoices", "foo", "bar", "2024", "doc.pdf")
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("renames a generic filename using the title when provided", func(t *testing.T) {
		got := ComputeCanonicalPath(`C:\raws\img001.jpg`, "invoices", root, strp("sfr"), strp("2024-05-12"), strp("Facture Mai"))
		if !strings.Contains(got, "2024-05-12_Sfr_Facture") {
			t.Fatalf("expected an intelligent filename containing '2024-05-12_Sfr_Facture', got %q", got)
		}
	})
}
