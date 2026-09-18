package pdfscanner

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// These cases are ported verbatim from pdf-triage's src/infrastructure/pdf-scanner.test.ts (7
// cases: 4 for getPDFsRecursively, 3 for getAllFilesRecursively). The upstream TypeScript suite is
// GREEN at port time (`npx vitest run src/infrastructure/pdf-scanner.test.ts` -> 7 passed), so no
// upstream case is pinned red.

func writeFile(t *testing.T, dir, relPath string) string {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(full, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	return full
}

func basenames(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, filepath.Base(p))
	}
	return out
}

func sorted(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGetPDFsRecursively(t *testing.T) {
	t.Run("finds supported document and image files recursively, case-insensitively, ignoring unsupported files", func(t *testing.T) {
		tempDir := t.TempDir()
		writeFile(t, tempDir, "a.pdf")
		writeFile(t, tempDir, "sub/b.PDF")
		writeFile(t, tempDir, "sub/deeper/c.Pdf")
		writeFile(t, tempDir, "notes.txt")
		writeFile(t, tempDir, "image.png")
		writeFile(t, tempDir, "invoice.docx")
		writeFile(t, tempDir, "sheet.xlsx")
		writeFile(t, tempDir, "program.exe")

		found := sorted(basenames(GetPDFsRecursively(tempDir)))
		want := sorted([]string{"a.pdf", "b.PDF", "c.Pdf", "image.png", "invoice.docx", "notes.txt", "sheet.xlsx"})
		if !equalStrings(found, want) {
			t.Fatalf("found = %v, want %v", found, want)
		}
	})

	t.Run("skips duplicates_files/duplicates/blocked_files/blocked directories entirely", func(t *testing.T) {
		tempDir := t.TempDir()
		writeFile(t, tempDir, "normal.pdf")
		writeFile(t, tempDir, "duplicates_files/dup1.pdf")
		writeFile(t, tempDir, "duplicates/dup2.pdf")
		writeFile(t, tempDir, "blocked_files/blk1.pdf")
		writeFile(t, tempDir, "blocked/blk2.pdf")

		found := basenames(GetPDFsRecursively(tempDir))
		if !equalStrings(found, []string{"normal.pdf"}) {
			t.Fatalf("found = %v, want [normal.pdf]", found)
		}
	})

	t.Run("excludes files inside ignoreDir when provided", func(t *testing.T) {
		tempDir := t.TempDir()
		writeFile(t, tempDir, "keep/one.pdf")
		writeFile(t, tempDir, "skip/two.pdf")

		found := basenames(GetPDFsRecursively(tempDir, filepath.Join(tempDir, "skip")))
		if !equalStrings(found, []string{"one.pdf"}) {
			t.Fatalf("found = %v, want [one.pdf]", found)
		}
	})

	t.Run("returns an empty array when the directory does not exist", func(t *testing.T) {
		if got := GetPDFsRecursively(filepath.Join(t.TempDir(), "nonexistent")); len(got) != 0 {
			t.Fatalf("found = %v, want []", got)
		}
	})
}

func TestGetAllFilesRecursively(t *testing.T) {
	t.Run("finds supported files recursively without skipping duplicates/blocked directories", func(t *testing.T) {
		tempDir := t.TempDir()
		writeFile(t, tempDir, "a.pdf")
		writeFile(t, tempDir, "duplicates_files/dup1.pdf")
		writeFile(t, tempDir, "blocked_files/blk1.pdf")

		found := sorted(basenames(GetAllFilesRecursively(tempDir)))
		want := sorted([]string{"a.pdf", "blk1.pdf", "dup1.pdf"})
		if !equalStrings(found, want) {
			t.Fatalf("found = %v, want %v", found, want)
		}
	})

	t.Run("ignores unsupported binary files", func(t *testing.T) {
		tempDir := t.TempDir()
		writeFile(t, tempDir, "doc.pdf")
		writeFile(t, tempDir, "readme.md")
		writeFile(t, tempDir, "binary.exe")

		found := sorted(basenames(GetAllFilesRecursively(tempDir)))
		want := sorted([]string{"doc.pdf", "readme.md"})
		if !equalStrings(found, want) {
			t.Fatalf("found = %v, want %v", found, want)
		}
	})

	t.Run("returns an empty array when the directory does not exist", func(t *testing.T) {
		if got := GetAllFilesRecursively(filepath.Join(t.TempDir(), "nonexistent")); len(got) != 0 {
			t.Fatalf("found = %v, want []", got)
		}
	})
}

func TestIsSupportedFile(t *testing.T) {
	// Added coverage for the extension rule and the Node-compatible posixExtname port (see the
	// package comment deviation 3): a leading-dot name has no extension in Node.
	for _, name := range []string{
		"a.pdf", "b.PDF", "c.Png", "d.jpeg", "e.webp", "f.bmp", "g.tiff",
		"h.txt", "i.md", "j.csv", "k.log", "l.json", "m.docx", "n.xlsx", "o.xls",
	} {
		if !IsSupportedFile(name) {
			t.Fatalf("IsSupportedFile(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"a.exe", "b.doc", "c", "d.pdfx", ".pdf", "a."} {
		if IsSupportedFile(name) {
			t.Fatalf("IsSupportedFile(%q) = true, want false", name)
		}
	}
}
