package guards

import (
	"os"
	"path/filepath"
	"testing"
)

// Ported from relocalize-document.test.ts:230-266 (findActualFileOnDisk). All paths are inside
// t.TempDir(); no operator file is touched.

func TestFindActualFileOnDisk(t *testing.T) {
	t.Run("returns doc.new_path when it exists on disk", func(t *testing.T) {
		inputDir := filepath.Join(t.TempDir(), "__raws")
		outputDir := filepath.Join(t.TempDir(), "__archive")
		mustMkdir(t, inputDir)
		mustMkdir(t, outputDir)
		p := filepath.Join(outputDir, "exists.pdf")
		mustWrite(t, p)

		if got := FindActualFileOnDisk(DocumentLocation{NewPath: p}, inputDir, outputDir); got != p {
			t.Fatalf("got %q, want %q", got, p)
		}
	})

	t.Run("falls back to original_path when new_path is missing/nonexistent", func(t *testing.T) {
		inputDir := filepath.Join(t.TempDir(), "__raws")
		outputDir := filepath.Join(t.TempDir(), "__archive")
		mustMkdir(t, inputDir)
		mustMkdir(t, outputDir)
		p := filepath.Join(inputDir, "orig.pdf")
		mustWrite(t, p)

		got := FindActualFileOnDisk(DocumentLocation{
			NewPath:      filepath.Join(outputDir, "ghost.pdf"),
			OriginalPath: p,
		}, inputDir, outputDir)
		if got != p {
			t.Fatalf("got %q, want %q", got, p)
		}
	})

	t.Run("falls back to a direct filename match inside INPUT_DIR", func(t *testing.T) {
		inputDir := filepath.Join(t.TempDir(), "__raws")
		outputDir := filepath.Join(t.TempDir(), "__archive")
		mustMkdir(t, inputDir)
		mustMkdir(t, outputDir)
		p := filepath.Join(inputDir, "renamed_folder_lost.pdf")
		mustWrite(t, p)

		got := FindActualFileOnDisk(DocumentLocation{
			OriginalFilename: "renamed_folder_lost.pdf",
			OriginalPath:     "C:/gone/renamed_folder_lost.pdf",
		}, inputDir, outputDir)
		if got != p {
			t.Fatalf("got %q, want %q", got, p)
		}
	})

	t.Run("falls back to a recursive case-insensitive basename search under OUTPUT_ROOT_DIR", func(t *testing.T) {
		inputDir := filepath.Join(t.TempDir(), "__raws")
		outputDir := filepath.Join(t.TempDir(), "__archive")
		mustMkdir(t, inputDir)
		nested := filepath.Join(outputDir, "invoices", "sfr", "2026")
		mustMkdir(t, nested)
		p := filepath.Join(nested, "MovedFile.pdf")
		mustWrite(t, p)

		got := FindActualFileOnDisk(DocumentLocation{
			OriginalFilename: "movedfile.PDF",
			OriginalPath:     "C:/gone/movedfile.PDF",
		}, inputDir, outputDir)
		if got != p {
			t.Fatalf("got %q, want %q", got, p)
		}
	})

	t.Run("returns empty when nothing matches anywhere", func(t *testing.T) {
		inputDir := filepath.Join(t.TempDir(), "__raws")
		outputDir := filepath.Join(t.TempDir(), "__archive")
		mustMkdir(t, inputDir)
		mustMkdir(t, outputDir)

		got := FindActualFileOnDisk(DocumentLocation{
			OriginalFilename: "does_not_exist.pdf",
			OriginalPath:     "C:/gone/does_not_exist.pdf",
		}, inputDir, outputDir)
		if got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})

	t.Run("returns empty when there is no filename at all", func(t *testing.T) {
		if got := FindActualFileOnDisk(DocumentLocation{}, "/nope", "/nope2"); got != "" {
			t.Fatalf("got %q, want empty", got)
		}
	})
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustWrite(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
}
