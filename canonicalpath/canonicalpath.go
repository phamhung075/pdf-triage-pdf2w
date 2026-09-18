// Package canonicalpath is a Go port of computeCanonicalPath and its private helpers from
// pdf-triage's src/domain/taxonomy.ts (lines 104-259).
//
// The TypeScript source is the behavioral source of truth. Two deviations from the plan's Go
// sketch in docs/superpowers/plans/2026-09-18-pdf2w-extraction-swap.md are intentional and
// required:
//
//  1. Basename/extension helpers treat BOTH '/' and '\' as path separators. The plan used
//     filepath.Base/filepath.Ext directly; on the Linux build/runtime target those do not split
//     Windows-style paths like `C:\raws\facture.pdf`, so the ported five tests would fail and
//     the service would emit garbage filenames in its Linux Docker image. taxonomy.ts runs on
//     the host path module and its tests expect `facture.pdf` from that input.
//  2. Accent stripping is implemented with an in-package table instead of `stripAccents`
//     (which the plan referenced but never defined). Go's stdlib has no NFD normalization and
//     the module must stay dependency-free for the multi-stage Docker build (which copies only
//     go.mod).
package canonicalpath

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	yearRe       = regexp.MustCompile(`^\d{4}$`)
	nonSlugRe    = regexp.MustCompile(`[^a-z0-9_-]+`)
	trimSlugRe   = regexp.MustCompile(`^[_.-]+|[_.-]+$`)
	subSplitRe   = regexp.MustCompile(`[/\\]+`)
	entitySplit  = regexp.MustCompile(`[/\\_\s-]+`)
	nonTitleRe   = regexp.MustCompile(`[^a-zA-Z0-9\s_-]`)
	isoDateRe    = regexp.MustCompile(`\b(20\d{2})[-/._]?(\d{2})[-/._]?(\d{2})\b`)
	yearAnyRe    = regexp.MustCompile(`\b(20\d{2})\b`)
	digitsOnlyRe = regexp.MustCompile(`^\d+$`)
	allDigitsRe  = regexp.MustCompile(`^\d+$`)

	hashRe          = regexp.MustCompile(`(?i)^[a-f0-9]{16,64}`)
	uuidRe          = regexp.MustCompile(`(?i)[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}`)
	suffixRe        = regexp.MustCompile(`(?i)(_blocked\d*|_dup\d*|\(\d+\)|_copy\d*)$`)
	scannerRe       = regexp.MustCompile(`(?i)^(qptmp|scan[-_]?\d*|doc[-_]?\d*|img[-_]?\d*|photo[-_]?\d*|file[-_]?\d*|tmp[-_]?\d*|temp[-_]?\d*|fileopen[-_]?\d*|untitled|non_nomme|recap)`)
	genericPrefixRe = regexp.MustCompile(`(?i)^(invoice|facture|receipt|download|document|recap)[-_\s\(\d]+$`)
	digitsHyphenRe  = regexp.MustCompile(`^\d{8,}[-_]`)

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

// baseName mirrors path.basename on a Windows host: both '/' and '\' are separators. This is
// required because computeCanonicalPath receives Windows-style paths while the service runs on
// Linux (see the package comment).
func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if p == "" {
		return ""
	}
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// extName mirrors path.extname for the basenames computeCanonicalPath sees.
func extName(p string) string {
	base := baseName(p)
	if base == "" || base == "." || base == ".." {
		return ""
	}
	i := strings.LastIndexByte(base, '.')
	if i <= 0 {
		return ""
	}
	return base[i:]
}

// baseNameWithoutExt mirrors path.basename(filename, ext).
func baseNameWithoutExt(p, ext string) string {
	base := baseName(p)
	if ext != "" && strings.HasSuffix(base, ext) {
		return base[:len(base)-len(ext)]
	}
	return base
}

func isYearString(s string) bool {
	return yearRe.MatchString(strings.TrimSpace(s))
}

// isForbiddenSubcategory mirrors taxonomy.ts's function of the same name. It is a dependency of
// generateIntelligentFilename, which the plan did not call out but computeCanonicalPath needs.
func isForbiddenSubcategory(subcategory string) bool {
	if subcategory == "" {
		return true
	}
	rawLower := strings.ToLower(strings.TrimSpace(subcategory))
	normalized := regexp.MustCompile(`[^a-z0-9]+`).ReplaceAllString(rawLower, "")
	if normalized == "" {
		return true
	}
	if _, ok := forbiddenSubcategories[rawLower]; ok {
		return true
	}
	if _, ok := forbiddenSubcategories[normalized]; ok {
		return true
	}
	if isYearString(rawLower) {
		return true
	}
	if allDigitsRe.MatchString(normalized) {
		return true
	}
	if extEndRe.MatchString(rawLower) {
		return true
	}
	if dateExtRe.MatchString(rawLower) {
		return true
	}
	if scannerNameRe.MatchString(normalized) {
		return true
	}
	return false
}

// isGenericFilename is a direct port of taxonomy.ts's function of the same name.
func isGenericFilename(filename string) bool {
	ext := extName(filename)
	stem := strings.ToLower(strings.TrimSpace(baseNameWithoutExt(filename, ext)))

	if utf8.RuneCountInString(stem) <= 3 {
		return true
	}
	if digitsOnlyRe.MatchString(stem) {
		return true
	}
	if hashRe.MatchString(stem) {
		return true
	}
	if uuidRe.MatchString(stem) {
		return true
	}
	if suffixRe.MatchString(stem) {
		return true
	}
	if scannerRe.MatchString(stem) {
		return true
	}
	if genericPrefixRe.MatchString(stem) {
		return true
	}
	if digitsHyphenRe.MatchString(stem) {
		return true
	}
	return false
}

// formatEntitySlug is a direct port of taxonomy.ts's function of the same name.
func formatEntitySlug(str string) string {
	if str == "" {
		return ""
	}
	words := entitySplit.Split(str, -1)
	var b strings.Builder
	for _, w := range words {
		if w == "" {
			continue
		}
		b.WriteString(capitalize(w))
	}
	return b.String()
}

func capitalize(w string) string {
	if w == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(w)
	return strings.ToUpper(string(r)) + strings.ToLower(w[size:])
}

// generateIntelligentFilename is a direct port of taxonomy.ts's function of the same name.
func generateIntelligentFilename(originalFilename string, title, category, subcategory, dateStr string) string {
	ext := extName(originalFilename)
	if ext == "" {
		ext = ".pdf"
	}

	// Only rename if the original filename is generic AND a non-empty title is provided.
	if strings.TrimSpace(title) == "" || !isGenericFilename(originalFilename) {
		return originalFilename
	}

	cleanCat := "other"
	if category != "" {
		cleanCat = strings.ToLower(strings.TrimSpace(category))
	}
	cleanSub := ""
	if subcategory != "" && !isForbiddenSubcategory(subcategory) {
		cleanSub = strings.ToLower(strings.TrimSpace(subcategory))
	}

	entityName := formatEntitySlug(cleanSub)
	if entityName == "" {
		entityName = formatEntitySlug(cleanCat)
	}
	if entityName == "" {
		entityName = "Document"
	}

	formattedDate := ""
	if dateStr != "" {
		if m := isoDateRe.FindStringSubmatch(dateStr); m != nil {
			formattedDate = m[1] + "-" + m[2] + "-" + m[3]
		} else if m := yearAnyRe.FindStringSubmatch(dateStr); m != nil {
			formattedDate = m[1]
		}
	}
	if formattedDate == "" {
		// Matches new Date().toISOString().split('T')[0] — UTC, not local.
		formattedDate = time.Now().UTC().Format("2006-01-02")
	}

	cleanTitle := strings.TrimSpace(nonTitleRe.ReplaceAllString(stripAccents(title), ""))

	titleWords := entitySplit.Split(cleanTitle, -1)
	var filtered []string
	for _, w := range titleWords {
		if w == "" {
			continue
		}
		word := capitalize(w)
		lower := strings.ToLower(word)
		if strings.Contains(formattedDate, lower) {
			continue
		}
		if strings.Contains(strings.ToLower(entityName), lower) {
			continue
		}
		filtered = append(filtered, word)
	}

	finalTitle := "Document"
	if len(filtered) > 0 {
		finalTitle = strings.Join(filtered, "_")
	}
	finalTitle = truncateRunes(finalTitle, 45)

	return formattedDate + "_" + entityName + "_" + finalTitle + strings.ToLower(ext)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// sanitizePathSegment mirrors taxonomy.ts's private function of the same name: anything that is
// not a slug character is collapsed to '_', neutralizing '..'/'.'/drive prefixes while leaving
// real slugs byte-identical.
func sanitizePathSegment(segment, fallback string) string {
	s := strings.ToLower(strings.TrimSpace(stripAccents(segment)))
	s = nonSlugRe.ReplaceAllString(s, "_")
	s = trimSlugRe.ReplaceAllString(s, "")
	if s == "" {
		return fallback
	}
	return s
}

// ComputeCanonicalPath ports src/domain/taxonomy.ts's computeCanonicalPath function-for-function.
// dateStr and title are optional (nil = not provided), matching the TS function's optional
// parameters.
func ComputeCanonicalPath(originalPath, category, outputRootDir string, subcategory, dateStr, title *string) string {
	originalFile := baseName(originalPath)
	file := generateIntelligentFilename(originalFile, deref(title), category, deref(subcategory), deref(dateStr))

	cleanCat := "other"
	if category != "" {
		cleanCat = strings.ToLower(strings.TrimSpace(category))
	}

	// Faithful to `subcategory ? subcategory.toLowerCase().trim() : 'general'`: a nil or
	// empty-string subcategory falls back to "general", but a whitespace-only (truthy in JS)
	// subcategory trims to "" and therefore contributes NO path segment. The plan's sketch
	// instead used TrimSpace(subcategory) != "" and would have inserted "general" there.
	cleanSub := "general"
	if subcategory != nil && *subcategory != "" {
		cleanSub = strings.ToLower(strings.TrimSpace(*subcategory))
	}

	if isYearString(cleanSub) {
		cleanSub = "general"
	}

	yearStr := strconv.Itoa(time.Now().Year())
	if dateStr != nil && len(*dateStr) >= 4 {
		if m := yearAnyRe.FindStringSubmatch(*dateStr); m != nil {
			yearStr = m[1]
		}
	}

	safeCat := sanitizePathSegment(cleanCat, "other")
	var subParts []string
	for _, part := range subSplitRe.Split(cleanSub, -1) {
		if part == "" {
			continue
		}
		if sp := sanitizePathSegment(part, "general"); sp != "" {
			subParts = append(subParts, sp)
		}
	}

	segments := append([]string{outputRootDir, safeCat}, subParts...)
	segments = append(segments, yearStr, file)
	return filepath.Join(segments...)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func stripAccents(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if isCombiningMark(r) {
			continue
		}
		if base, ok := accentBase[r]; ok {
			b.WriteRune(base)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// isCombiningMark reports the Unicode combining-diacritical blocks that JS's
// normalize('NFD') would leave as standalone marks for the characters we decompose.
func isCombiningMark(r rune) bool {
	return (r >= 0x0300 && r <= 0x036F) ||
		(r >= 0x1AB0 && r <= 0x1AFF) ||
		(r >= 0x1DC0 && r <= 0x1DFF) ||
		(r >= 0x20D0 && r <= 0x20FF) ||
		(r >= 0xFE20 && r <= 0xFE2F)
}

// accentBase maps the precomposed Latin letters that have a canonical (NFD) decomposition onto
// their base ASCII letter. Characters without a canonical decomposition (ø, œ, ß, đ, ł, ...) are
// deliberately absent, matching normalize('NFD') which leaves them alone — the slug/title regexes
// then drop or neutralize them exactly as in taxonomy.ts.
var accentBase = map[rune]rune{
	'À': 'A', 'Á': 'A', 'Â': 'A', 'Ã': 'A', 'Ä': 'A', 'Å': 'A',
	'Ç': 'C',
	'È': 'E', 'É': 'E', 'Ê': 'E', 'Ë': 'E',
	'Ì': 'I', 'Í': 'I', 'Î': 'I', 'Ï': 'I',
	'Ñ': 'N',
	'Ò': 'O', 'Ó': 'O', 'Ô': 'O', 'Õ': 'O', 'Ö': 'O',
	'Ù': 'U', 'Ú': 'U', 'Û': 'U', 'Ü': 'U',
	'Ý': 'Y',
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a',
	'ç': 'c',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i',
	'ñ': 'n',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u',
	'ý': 'y', 'ÿ': 'y',

	'Ā': 'A', 'Ă': 'A', 'Ą': 'A', 'ā': 'a', 'ă': 'a', 'ą': 'a',
	'Ć': 'C', 'Ĉ': 'C', 'Ċ': 'C', 'Č': 'C', 'ć': 'c', 'ĉ': 'c', 'ċ': 'c', 'č': 'c',
	'Ď': 'D', 'ď': 'd',
	'Ē': 'E', 'Ĕ': 'E', 'Ė': 'E', 'Ę': 'E', 'Ě': 'E', 'ē': 'e', 'ĕ': 'e', 'ė': 'e', 'ę': 'e', 'ě': 'e',
	'Ĝ': 'G', 'Ğ': 'G', 'Ġ': 'G', 'Ģ': 'G', 'ĝ': 'g', 'ğ': 'g', 'ġ': 'g', 'ģ': 'g',
	'Ĥ': 'H', 'ĥ': 'h',
	'Ĩ': 'I', 'Ī': 'I', 'Ĭ': 'I', 'Į': 'I', 'İ': 'I', 'ĩ': 'i', 'ī': 'i', 'ĭ': 'i', 'į': 'i',
	'Ĵ': 'J', 'ĵ': 'j',
	'Ķ': 'K', 'ķ': 'k',
	'Ĺ': 'L', 'Ļ': 'L', 'Ľ': 'L', 'ĺ': 'l', 'ļ': 'l', 'ľ': 'l',
	'Ń': 'N', 'Ņ': 'N', 'Ň': 'N', 'ń': 'n', 'ņ': 'n', 'ň': 'n',
	'Ō': 'O', 'Ŏ': 'O', 'Ő': 'O', 'ō': 'o', 'ŏ': 'o', 'ő': 'o',
	'Ŕ': 'R', 'Ŗ': 'R', 'Ř': 'R', 'ŕ': 'r', 'ŗ': 'r', 'ř': 'r',
	'Ś': 'S', 'Ŝ': 'S', 'Ş': 'S', 'Š': 'S', 'ś': 's', 'ŝ': 's', 'ş': 's', 'š': 's',
	'Ţ': 'T', 'Ť': 'T', 'ţ': 't', 'ť': 't',
	'Ũ': 'U', 'Ū': 'U', 'Ŭ': 'U', 'Ů': 'U', 'Ű': 'U', 'Ų': 'U', 'ũ': 'u', 'ū': 'u', 'ŭ': 'u', 'ů': 'u', 'ű': 'u', 'ų': 'u',
	'Ŵ': 'W', 'ŵ': 'w',
	'Ŷ': 'Y', 'Ÿ': 'Y', 'ŷ': 'y',
	'Ź': 'Z', 'Ż': 'Z', 'Ž': 'Z', 'ź': 'z', 'ż': 'z', 'ž': 'z',

	'Ș': 'S', 'ș': 's', 'Ț': 'T', 'ț': 't',
}
