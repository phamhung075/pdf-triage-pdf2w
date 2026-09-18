package zipbuilder

import (
	"archive/zip"
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// These cases are ported verbatim from pdf-triage's
// src/infrastructure/zip-builder.test.ts (4 cases). The upstream TypeScript suite is GREEN at port
// time (`npx vitest run src/infrastructure/zip-builder.test.ts` -> 4 passed), so no upstream case is
// pinned red. A fifth round-trip case is added because the TS suite never actually reads an entry
// back; it verifies the two observable behaviours the objective names but the tests do not
// (entry contents and the CRC-32 archive/zip computes) by parsing the produced buffer with the same
// stdlib reader a consumer would use.

func eocdEntries(t *testing.T, archive []byte) uint16 {
	t.Helper()
	if len(archive) < 22 {
		t.Fatalf("archive length = %d, want >= 22", len(archive))
	}
	return binary.LittleEndian.Uint16(archive[len(archive)-22+8:])
}

func TestCreateZipArchive(t *testing.T) {
	t.Run("packages a file read from disk by path (existing behavior)", func(t *testing.T) {
		tempDir := t.TempDir()
		filePath := filepath.Join(tempDir, "source.pdf")
		if err := os.WriteFile(filePath, []byte("pdf bytes here"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		archive, err := CreateZipArchive([]ZipFileEntry{{Name: "renamed.pdf", Path: filePath}})
		if err != nil {
			t.Fatalf("CreateZipArchive: %v", err)
		}

		if len(archive) == 0 {
			t.Fatal("archive length = 0, want > 0")
		}
		// End Of Central Directory signature must be present with exactly 1 entry recorded.
		eocdSignature := binary.LittleEndian.Uint32(archive[len(archive)-22:])
		if eocdSignature != 0x06054b50 {
			t.Fatalf("EOCD signature = %#x, want 0x06054b50", eocdSignature)
		}
		if got := eocdEntries(t, archive); got != 1 {
			t.Fatalf("total entries = %d, want 1", got)
		}
	})

	t.Run("packages in-memory content directly, without touching disk", func(t *testing.T) {
		archive, err := CreateZipArchive([]ZipFileEntry{{Name: "notes.md", Content: []byte("# Hello\n\nWorld")}})
		if err != nil {
			t.Fatalf("CreateZipArchive: %v", err)
		}

		if len(archive) == 0 {
			t.Fatal("archive length = 0, want > 0")
		}
		if got := eocdEntries(t, archive); got != 1 {
			t.Fatalf("total entries = %d, want 1", got)
		}
		// The local file header's filename field should contain the literal name we gave it.
		got := string(archive[30 : 30+len("notes.md")])
		if got != "notes.md" {
			t.Fatalf("local filename = %q, want %q", got, "notes.md")
		}
	})

	t.Run("sets the UTF-8 filename flag (general purpose bit 11 / 0x0800) so accented entry names are not mojibaked by strict ZIP readers", func(t *testing.T) {
		archive, err := CreateZipArchive([]ZipFileEntry{{Name: "Avis de Taxes Foncières.md", Content: []byte("x")}})
		if err != nil {
			t.Fatalf("CreateZipArchive: %v", err)
		}

		// Local File Header: signature(4) + version(2) + generalFlag(2) at offset 6.
		localGeneralFlag := binary.LittleEndian.Uint16(archive[6:])
		if localGeneralFlag&0x0800 != 0x0800 {
			t.Fatalf("local general purpose flag = %#x, want bit 0x0800 set", localGeneralFlag)
		}
	})

	t.Run("skips an entry that has neither a valid path nor content, without throwing", func(t *testing.T) {
		tempDir := t.TempDir()
		archive, err := CreateZipArchive([]ZipFileEntry{
			{Name: "missing.pdf", Path: filepath.Join(tempDir, "does-not-exist.pdf")},
			{Name: "notes.md", Content: []byte("kept")},
		})
		if err != nil {
			t.Fatalf("CreateZipArchive: %v", err)
		}

		if got := eocdEntries(t, archive); got != 1 { // only the content-based entry survives
			t.Fatalf("total entries = %d, want 1", got)
		}
	})
}

func TestCreateZipArchiveRoundTrip(t *testing.T) {
	// The TS test only inspects header offsets; a consumer reads entry names and contents back.
	entries := []ZipFileEntry{
		{Name: "dir/renamed.pdf", Content: []byte("pdf bytes here")},
		{Name: "notes.md", Content: []byte("# Hello\n\nWorld")},
	}
	archive, err := CreateZipArchive(entries)
	if err != nil {
		t.Fatalf("CreateZipArchive: %v", err)
	}

	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	if len(reader.File) != len(entries) {
		t.Fatalf("entries = %d, want %d", len(reader.File), len(entries))
	}
	for i, want := range entries {
		got := reader.File[i]
		if got.Name != want.Name {
			t.Fatalf("entry %d name = %q, want %q", i, got.Name, want.Name)
		}
		rc, err := got.Open()
		if err != nil {
			t.Fatalf("entry %q Open: %v", got.Name, err)
		}
		var buf bytes.Buffer
		if _, err := buf.ReadFrom(rc); err != nil {
			t.Fatalf("entry %q Read: %v", got.Name, err)
		}
		rc.Close()
		if !bytes.Equal(buf.Bytes(), want.Content) {
			t.Fatalf("entry %q content = %q, want %q", got.Name, buf.Bytes(), want.Content)
		}
	}
}
