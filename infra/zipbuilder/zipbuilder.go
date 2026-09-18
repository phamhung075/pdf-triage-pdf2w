// Package zipbuilder is a Go port of pdf-triage's src/infrastructure/zip-builder.ts (105 lines):
// ZipFileEntry and createZipArchive. It builds the standard ZIP archive buffer used by the PDF
// package export and the bulk Markdown export.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/zip-builder.test.ts` -> 4 passed), so no upstream case is
// pinned red. All four cases are ported, plus a round-trip case that reads the buffer back with
// archive/zip to verify the entry contents and CRC the original tests never opened.
//
// CHOICE: this port uses the stdlib archive/zip writer instead of reimplementing the hand-written
// local/central-directory/EOCD writer. The objective explicitly permits that when the observable
// behaviour the tests assert is preserved. Preserved: entry names (empty -> "document.pdf"), content
// taken directly from ZipFileEntry.Content when set and otherwise read from Path, `archive/zip`'s
// CRC-32, one central-directory entry per surviving file, the EOCD at the tail, and the general
// purpose UTF-8 flag (bit 11) for non-ASCII names. The deviations below are mechanical and all
// resolved in favor of matching the TS acceptance bar:
//
//  1. STORE method, not deflate. The header is created with Method: zip.Store so entries are stored
//     uncompressed exactly as the TS writer stored them. Because archive/zip streams, it advertises
//     general purpose bit 3 (data descriptor) and writes a trailing data descriptor after each
//     entry; the hand-written TS writer did not. No ported test asserts bit 3 (only bit 11), and
//     every stdlib/Windows ZIP reader accepts a data descriptor.
//  2. Empty content is kept. TS's `if (!content) continue` treats a zero-length Buffer as truthy, so
//     an explicitly supplied empty entry is included with size 0; only a missing/None content is
//     skipped. Go's nil []byte models "unset" and a non-nil empty slice models the empty Buffer.
//  3. Reading from Path. TS does existsSync + readFileSync and can throw between the two; Go calls
//     os.ReadFile and skips the entry on any read error, which is the same observable result for the
//     documented case (a path that does not exist). A path that exists but is unreadable is skipped
//     here and would have thrown in TS; no upstream case covers it.
package zipbuilder

import (
	"archive/zip"
	"bytes"
	"os"
)

// ZipFileEntry mirrors the TS `ZipFileEntry` interface.
//
// Content, when non-nil, is used directly with no disk I/O (generated content such as exported
// markdown). A non-nil empty slice is an empty file and is included. When Content is nil the entry
// is read from Path; a missing Path is skipped. A nil Content with an empty Path is skipped.
type ZipFileEntry struct {
	Name    string
	Path    string
	Content []byte
}

// CreateZipArchive is `createZipArchive(files: ZipFileEntry[]): Buffer`. It returns a standard ZIP
// archive buffer; the error replaces the exceptions the TS write could throw.
func CreateZipArchive(files []ZipFileEntry) ([]byte, error) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	for _, file := range files {
		var content []byte
		switch {
		case file.Content != nil:
			content = file.Content
		case file.Path != "":
			read, err := os.ReadFile(file.Path)
			if err != nil {
				continue // TS: no valid path nor content -> skip without throwing
			}
			content = read
		default:
			continue
		}

		name := file.Name
		if name == "" {
			name = "document.pdf"
		}
		// Method: zip.Store mirrors the TS writer's compression method 0. Modified is left zero so
		// the DOS date/time fields stay 0/0 like the TS writer's.
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(content); err != nil {
			return nil, err
		}
	}

	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
