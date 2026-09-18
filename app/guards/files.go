package guards

import (
	"os"
	"path"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfscanner"
)

// DocumentLocation is the subset of a stored document record FindActualFileOnDisk reads. It is a
// small local struct rather than an import of store/database.DocumentRecord so the guard stays
// usable from any caller that can project those three fields.
type DocumentLocation struct {
	NewPath          string
	OriginalPath     string
	OriginalFilename string
}

// FindActualFileOnDisk is the ONE implementation of relocalize-document.ts:142's
// findActualFileOnDisk. It locates the physical file a DB row points at, newest location first:
// new_path, then original_path, then a direct filename match inside inputDir (__raws), then a
// recursive case-insensitive basename search under outputDir (__archive). Returns "" when nothing
// matches, which the caller treats as a stale ghost row (relocalize-document.ts:212-226).
//
// It is a guard because a move must never be planned from a row whose file is gone: the caller
// purges the ghost record instead of moving.
func FindActualFileOnDisk(doc DocumentLocation, inputDir, outputDir string) string {
	if doc.NewPath != "" && fileExists(doc.NewPath) {
		return doc.NewPath
	}
	if doc.OriginalPath != "" && fileExists(doc.OriginalPath) {
		return doc.OriginalPath
	}

	filename := doc.OriginalFilename
	if filename == "" && doc.OriginalPath != "" {
		filename = path.Base(doc.OriginalPath)
	}
	if filename == "" {
		return ""
	}

	rawMatch := path.Join(inputDir, filename)
	if fileExists(rawMatch) {
		return rawMatch
	}

	lower := strings.ToLower(filename)
	for _, f := range pdfscanner.GetPDFsRecursively(outputDir) {
		if strings.ToLower(path.Base(f)) == lower {
			return f
		}
	}
	return ""
}

// fileExists is `fs.existsSync`: true only when os.Stat succeeds (so a broken symlink is "missing",
// exactly as Node reports it).
func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
