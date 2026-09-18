// Package pdfscanner is a Go port of pdf-triage's src/infrastructure/pdf-scanner.ts (57 lines):
// SUPPORTED_EXTENSIONS, isSupportedFile, getPDFsRecursively and getAllFilesRecursively.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/pdf-scanner.test.ts` -> 7 passed), so no upstream case is
// pinned red. All seven cases are ported.
//
// It reuses the already-ported taxonomy package for IsPathInsideDir rather than reimplementing it,
// as the objective requires. Deviations, each resolved in favor of matching the TS acceptance bar:
//
//  1. Directory listing. TS uses fs.readdirSync(dir, { withFileTypes: true }); Go uses os.ReadDir,
//     which additionally sorts entries by filename. The TS tests sort the multi-file results
//     themselves, and the single-file results are unaffected, so ordering is not observable here.
//  2. Missing/unreadable directory. TS does existsSync -> [] and otherwise lets readdirSync throw;
//     Go returns an empty result for any os.ReadDir error. The documented case (directory does not
//     exist) matches; a path that is a file or is unreadable yields [] here where TS threw. No
//     upstream case covers that.
//  3. Extension extraction. Node's path.extname disagrees with Go's path.Ext for leading-dot and
//     trailing-dot names (`path.extname(".bashrc") == ""` but `path.Ext(".bashrc") == ".bashrc"`),
//     so posixExtname below is a faithful port of Node's path.posix.extname scan, matching the
//     precedent set by the taxonomy package's private helper.
//  4. Optional ignoreDir. TS's `ignoreDir?: string` maps to a Go variadic parameter; pass zero or
//     one argument. An empty string behaves like the unset case, as `if (ignoreDir && ...)` did.
//  5. `item.isDirectory()` / `item.isFile()` map to DirEntry.IsDir() / DirEntry.Type().IsRegular(),
//     so symlinks are neither followed nor reported, exactly as Node's dirent checks behave.
package pdfscanner

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// SupportedExtensions mirrors the TS `SUPPORTED_EXTENSIONS` Set.
var SupportedExtensions = map[string]struct{}{
	".pdf": {},
	".png": {}, ".jpg": {}, ".jpeg": {}, ".webp": {}, ".bmp": {}, ".tiff": {},
	".txt": {}, ".md": {}, ".csv": {}, ".log": {}, ".json": {},
	".docx": {}, ".xlsx": {}, ".xls": {},
}

// IsSupportedFile is `isSupportedFile(filePath: string): boolean`.
func IsSupportedFile(filePath string) bool {
	ext := strings.ToLower(posixExtname(filePath))
	_, ok := SupportedExtensions[ext]
	return ok
}

// GetPDFsRecursively is `getPDFsRecursively(dir, ignoreDir?)`. When ignoreDir is provided, files
// inside that directory (and the directory itself) are skipped, and the hidden/duplicates/blocked
// directories are never descended into.
func GetPDFsRecursively(dir string, ignoreDir ...string) []string {
	results := []string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return results
	}

	ignore := ""
	if len(ignoreDir) > 0 {
		ignore = ignoreDir[0]
	}

	for _, entry := range entries {
		fullPath := filepath.Join(dir, entry.Name())

		if ignore != "" && taxonomy.IsPathInsideDir(fullPath, ignore) {
			continue
		}

		if entry.IsDir() {
			name := entry.Name()
			if strings.HasPrefix(name, ".") ||
				name == "duplicates_files" || name == "duplicates" ||
				name == "blocked_files" || name == "blocked" {
				continue
			}
			results = append(results, GetPDFsRecursively(fullPath, ignoreDir...)...)
		} else if entry.Type().IsRegular() && IsSupportedFile(entry.Name()) {
			results = append(results, fullPath)
		}
	}
	return results
}

// GetAllFilesRecursively is `getAllFilesRecursively(dir)`: no directory filtering at all.
func GetAllFilesRecursively(dir string) []string {
	results := []string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return results
	}

	for _, entry := range entries {
		fullPath := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			results = append(results, GetAllFilesRecursively(fullPath)...)
		} else if entry.Type().IsRegular() && IsSupportedFile(entry.Name()) {
			results = append(results, fullPath)
		}
	}
	return results
}

// posixExtname is a faithful port of Node's path.posix.extname scanning algorithm (see the package
// comment). It exists because Go's path.Ext disagrees with Node on leading-dot and trailing-dot
// names. Only ASCII '/' and '.' drive the scan, so byte indexing is equivalent to the JS charCodeAt
// loop for UTF-8 input.
func posixExtname(p string) string {
	startDot := -1
	startPart := 0
	end := -1
	matchedSlash := true
	preDotState := 0
	for i := len(p) - 1; i >= 0; i-- {
		code := p[i]
		if code == '/' {
			if !matchedSlash {
				startPart = i + 1
				break
			}
			continue
		}
		if end == -1 {
			matchedSlash = false
			end = i + 1
		}
		if code == '.' {
			if startDot == -1 {
				startDot = i
			} else if preDotState != 1 {
				preDotState = 1
			}
		} else if startDot != -1 {
			// We saw a non-dot and non-slash character before the first dot.
			preDotState = -1
		}
	}

	if startDot == -1 || end == -1 ||
		preDotState == 0 ||
		(preDotState == 1 && startDot == end-1 && startDot == startPart+1) {
		return ""
	}
	return p[startDot:end]
}
