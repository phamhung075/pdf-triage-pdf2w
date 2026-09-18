// Package taxonomy is a Go port of the pure functions still living in pdf-triage's
// src/domain/taxonomy.ts: isPathInsideDir, isYearString, detectFileType, isForbiddenSubcategory,
// findCanonicalCategoryForSubcategory and mergeSubcategoryInTaxonomy. computeCanonicalPath and its
// private helpers already live in the sibling canonicalpath package and are deliberately not
// duplicated here.
//
// The TypeScript source is the behavioral source of truth. Two deviations are intentional:
//
//  1. isPathInsideDir uses Go's path.Clean / '/' separator where TS uses Node's host-dependent
//     path.normalize / path.sep. Go's stdlib equivalent, path/filepath, would resolve to '\' on
//     Windows and to the host's separators; the archive being checked lives on WSL POSIX paths
//     (`/mnt/c/...`), so the POSIX semantics of path.Clean are the faithful port of Node's path
//     module on the Linux target. This mirrors the canonicalpath package comment's first deviation.
//  2. detectFileType needs Node's path.extname. Go's path.Ext is NOT equivalent for dotfiles and
//     trailing dots (path.Ext(".bashrc") == ".bashrc" while Node returns ""; path.Ext("foo.") ==
//     "." while Node also returns "."); to keep the TS acceptance bar exact, posixExtname below is
//     a faithful port of Node's path.posix.extname scanning algorithm rather than a call to path.Ext.
package taxonomy

import (
	"path"
	"regexp"
	"strings"
	"unicode/utf8"
)

// DocumentFileType mirrors the TS `DocumentFileType` string union.
type DocumentFileType string

const (
	DocumentFileTypePDF   DocumentFileType = "PDF"
	DocumentFileTypeImage DocumentFileType = "IMAGE"
	DocumentFileTypeText  DocumentFileType = "TEXT"
	DocumentFileTypeWord  DocumentFileType = "WORD"
	DocumentFileTypeExcel DocumentFileType = "EXCEL"
)

// Subcategory and Category model the subset of the taxonomy JSON these functions read and mutate.
// Pointers are used for the slice elements because mergeSubcategoryInTaxonomy removes an element by
// identity (TS compares with `s !== source`), which requires reference semantics.
type Subcategory struct {
	ID      string   `json:"id"`
	Name    string   `json:"name,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
}

type Category struct {
	ID            string         `json:"id"`
	Subcategories []*Subcategory `json:"subcategories"`
}

// TaxonomyConfig models the `{ categories: [...] }` object findCanonicalCategoryForSubcategory
// receives. A nil *TaxonomyConfig is the Go equivalent of TS's `categoriesConfig?.categories || []`.
type TaxonomyConfig struct {
	Categories []*Category `json:"categories"`
}

var (
	yearRe        = regexp.MustCompile(`^\d{4}$`)
	nonAlnumRe    = regexp.MustCompile(`[^a-z0-9]+`)
	allDigitsRe   = regexp.MustCompile(`^\d+$`)
	extEndRe      = regexp.MustCompile(`(?i).*[._-](pdf|jpg|jpeg|png|webp|tiff|bmp|txt|docx|xlsx)$`)
	dateExtRe     = regexp.MustCompile(`(?i)^(\d+|img\d*|scan\d*|photo\d*|doc\d*|file\d*)[_.]?(pdf|jpg|jpeg|png|webp|tiff|bmp|txt|docx|xlsx)$`)
	scannerNameRe = regexp.MustCompile(`(?i)^(anyscanner|camscanner|geniusscan|adobescan|tinyscanner|simplescan|docscanner)`)
)

// forbiddenSubcategories mirrors taxonomy.ts's FORBIDDEN_SUBCATEGORIES set (Golden Rule #4).
var forbiddenSubcategories = map[string]struct{}{
	"general": {}, "other": {}, "divers": {}, "unknown": {}, "none": {},
	"anyscanner": {}, "camscanner": {}, "geniusscan": {}, "adobescan": {}, "tinyscanner": {}, "simplescan": {}, "docscanner": {},
	"jpg": {}, "jpeg": {}, "png": {}, "webp": {}, "bmp": {}, "tiff": {}, "pdf": {}, "txt": {}, "docx": {}, "xlsx": {},
}

// IsPathInsideDir reports whether fullPath IS dirPath or is actually nested inside it — a plain
// string-prefix check would also match an unrelated sibling that merely shares a prefix
// (e.g. "__archive" vs "__archive_old"). The comment and the lower-casing come straight from
// taxonomy.ts.
func IsPathInsideDir(fullPath, dirPath string) bool {
	normFull := strings.ToLower(path.Clean(fullPath))
	normDir := strings.ToLower(path.Clean(dirPath))
	return normFull == normDir || strings.HasPrefix(normFull, normDir+"/")
}

// IsYearString mirrors taxonomy.ts's isYearString. TS's `undefined` maps to the Go zero value "".
func IsYearString(str string) bool {
	return yearRe.MatchString(strings.TrimSpace(str))
}

// DetectFileType mirrors taxonomy.ts's detectFileType. An empty (or unknown) filename is 'PDF',
// exactly as the TS `if (!filename) return 'PDF'` fallback and final `return 'PDF'` do.
func DetectFileType(filename string) DocumentFileType {
	if filename == "" {
		return DocumentFileTypePDF
	}
	ext := strings.ToLower(strings.TrimSpace(posixExtname(filename)))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".bmp", ".tiff":
		return DocumentFileTypeImage
	case ".txt", ".md", ".csv", ".log", ".json":
		return DocumentFileTypeText
	case ".docx", ".doc":
		return DocumentFileTypeWord
	case ".xlsx", ".xls":
		return DocumentFileTypeExcel
	}
	return DocumentFileTypePDF
}

// IsForbiddenSubcategory ports taxonomy.ts's function of the same name (Golden Rule #4):
// general/other/divers/empty/year-only, pure numeric or date-like slugs, slugs ending in a file
// extension, and scanner-software names are never valid final subcategories. TS's `undefined`
// maps to Go's "".
func IsForbiddenSubcategory(subcategory string) bool {
	if subcategory == "" {
		return true
	}
	rawLower := strings.ToLower(strings.TrimSpace(subcategory))
	normalized := nonAlnumRe.ReplaceAllString(rawLower, "")
	if normalized == "" {
		return true
	}
	if _, ok := forbiddenSubcategories[rawLower]; ok {
		return true
	}
	if _, ok := forbiddenSubcategories[normalized]; ok {
		return true
	}
	if IsYearString(rawLower) {
		return true
	}
	// Reject pure numeric or date-like slugs (e.g. "102818", "20260811")
	if allDigitsRe.MatchString(normalized) {
		return true
	}
	// Reject slugs ending in file extensions (e.g. "verso_png", "sinistre_docx", "102818_jpg", "img_123_png")
	if extEndRe.MatchString(rawLower) {
		return true
	}
	// Reject raw date / random number + file extension patterns (e.g. "102818jpg", "img123png")
	if dateExtRe.MatchString(rawLower) {
		return true
	}
	// Reject scanner software names (e.g. "anyscanner", "camscanner", "geniusscan")
	if scannerNameRe.MatchString(normalized) {
		return true
	}
	return false
}

// FindCanonicalCategoryForSubcategory ports taxonomy.ts's function of the same name. It returns a
// pointer because the TS return type is `string | null`; nil means "leave this document's category
// alone".
//
// The doc comment below is preserved from the TS source because it records a real incident:
//
// The category a subcategory slug canonically belongs to, or nil when that cannot be decided.
//
// nil means "leave this document's category alone" — it is not an error. The caller
// (repair-registry.ts) overwrites the DB row and physically relocates the file on a non-nil
// answer, so guessing here silently rewrites the archive.
//
// This used to return the first category containing the slug, by array position. Subcategories are
// namespaced per-category, so the same slug legitimately appears under several of them — the live
// taxonomy has 42 such slugs — and array order was deciding which one won. A Repair run would have
// relocated 87 of 276 archived documents on that basis: every `clinic_x` payslip out of
// `bulletin_salaire` into `contracts`, `permis_conduire` out of `identity` into `administrative`,
// and so on. Order in a JSON file is not evidence about a document.
//
// currentCategory is the document's existing category. When the slug is ambiguous and the document
// already sits under one of the candidates, that placement is the answer — it was chosen by
// classification (or by the user), which is strictly better information than array order.
func FindCanonicalCategoryForSubcategory(subcategorySlug string, categoriesConfig *TaxonomyConfig, currentCategory string) *string {
	if IsForbiddenSubcategory(subcategorySlug) {
		return nil
	}
	subLower := strings.ToLower(strings.TrimSpace(subcategorySlug))

	var matches []string
	if categoriesConfig != nil {
		for _, cat := range categoriesConfig.Categories {
			if cat == nil {
				continue
			}
			for _, s := range cat.Subcategories {
				if s == nil {
					continue
				}
				if strings.ToLower(s.ID) == subLower {
					matches = append(matches, cat.ID)
					break
				}
			}
		}
	}

	if len(matches) == 0 {
		return nil
	}

	// Already sitting under a category that legitimately owns this slug — nothing to correct.
	current := strings.ToLower(strings.TrimSpace(currentCategory))
	if current != "" {
		for _, m := range matches {
			if strings.ToLower(m) == current {
				winner := m
				return &winner
			}
		}
	}

	// Exactly one owner: an unambiguous correction, which is what this function is for.
	if len(matches) == 1 {
		winner := matches[0]
		return &winner
	}

	// Several owners and the document is under none of them. There is no evidence here for choosing
	// between them, so decline rather than move the file somewhere arbitrary.
	return nil
}

// MergeSubcategoryInTaxonomy ports taxonomy.ts's function of the same name. The doc comment below
// is preserved from the TS source because it records a real incident:
//
// Point every document's subcategory `oldSub` at `newSub` within one category, in the taxonomy.
//
// Renaming and merging are the same user gesture ("these two are the same thing") and differ only
// in whether the destination already exists. Treating a merge as a rename — mutating the old entry's
// id in place — leaves TWO entries sharing that id, and every later lookup has to guess between
// them. Reconciling two spellings of one entity is the normal case here: an archive accrues
// `bouyguestelecom` alongside `bouygues_telecom` because slug normalisation has no near-duplicate
// check, so this path gets used precisely when the destination is already there.
//
// Mutates and returns `categories`. The old spelling survives as an alias on the winner, so a
// document or classifier lookup using it still resolves.
func MergeSubcategoryInTaxonomy(categories []*Category, categoryID, oldSub, newSub string) []*Category {
	if categories == nil {
		return categories
	}

	catID := strings.ToLower(strings.TrimSpace(categoryID))
	var cat *Category
	for _, c := range categories {
		if c != nil && c.ID == catID {
			cat = c
			break
		}
	}
	// `!Array.isArray(cat.subcategories)` — a nil slice is the Go equivalent of "not an array";
	// an empty non-nil slice still proceeds, exactly as JS does.
	if cat == nil || cat.Subcategories == nil {
		return categories
	}

	from := strings.ToLower(strings.TrimSpace(oldSub))
	to := strings.ToLower(strings.TrimSpace(newSub))
	if from == "" || to == "" || from == to {
		return categories
	}

	prettyName := prettifySubcategory(to)

	var source, target *Subcategory
	for _, s := range cat.Subcategories {
		if s == nil {
			continue
		}
		if source == nil && s.ID == from {
			source = s
		}
		if target == nil && s.ID == to {
			target = s
		}
	}

	if target != nil {
		var aliases []string
		aliases = append(aliases, target.Aliases...)
		if source != nil {
			aliases = append(aliases, source.Aliases...)
		}
		aliases = append(aliases, from, to)
		target.Aliases = uniqueStrings(aliases)

		if source != nil {
			kept := make([]*Subcategory, 0, len(cat.Subcategories))
			for _, s := range cat.Subcategories {
				if s != source {
					kept = append(kept, s)
				}
			}
			cat.Subcategories = kept
		}
	} else if source != nil {
		source.ID = to
		source.Name = prettyName
		source.Aliases = uniqueStrings(append(append([]string{}, source.Aliases...), from, to))
	} else {
		cat.Subcategories = append(cat.Subcategories, &Subcategory{ID: to, Name: prettyName, Aliases: []string{to}})
	}

	return categories
}

// prettifySubcategory ports `to.split('_').map(w => w.charAt(0).toUpperCase() + w.slice(1)).join(' ')`.
func prettifySubcategory(slug string) string {
	parts := strings.Split(slug, "_")
	for i, w := range parts {
		if w == "" {
			continue
		}
		r, size := utf8.DecodeRuneInString(w)
		parts[i] = strings.ToUpper(string(r)) + w[size:]
	}
	return strings.Join(parts, " ")
}

// uniqueStrings reproduces `Array.from(new Set(values))`: same-value de-duplication that preserves
// first-seen order.
func uniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// posixExtname is a faithful port of Node's path.posix.extname scanning algorithm. It exists
// because Go's path.Ext disagrees with Node on leading-dot and trailing-dot names; see the package
// comment. Only ASCII '/' and '.' drive the scan, so byte indexing is equivalent to the JS
// charCodeAt loop for UTF-8 input.
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
