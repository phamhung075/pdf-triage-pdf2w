package guards

import (
	"strings"
	"unicode/utf8"
)

// This file holds the small JS-faithful string helpers the guards need. They mirror the unexported
// helpers in extractionqualitygate / classificationresolution / taxonomy, which cannot be imported;
// they are reproduced here rather than approximated with Go's divergent stdlib (see the package
// comment deviation 1).

// jsTrim is String.prototype.trim(): it strips exactly the JavaScript WhiteSpace+LineTerminator
// set. Go's strings.TrimSpace would additionally strip U+0085 NEL and would leave U+FEFF.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSWhitespace)
}

// isJSWhitespace reports whether r is in JavaScript's WhiteSpace+LineTerminator set.
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// utf16Len is JavaScript's String.prototype.length: the number of UTF-16 code units, so an astral
// character counts as two. Go's len counts bytes and utf8.RuneCountInString counts runes; neither
// matches the TS `< 10` threshold for astral input.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// trimLower is `x.toLowerCase().trim()` for the ASCII slugs these guards compare.
func trimLower(s string) string {
	return strings.ToLower(jsTrim(s))
}

// capitalizeFirst ports `str.charAt(0).toUpperCase() + str.slice(1)` (relocalize-document.ts:173).
// An empty string stays empty, exactly as JS charAt(0) === "" does.
func capitalizeFirst(s string) string {
	if s == "" {
		return ""
	}
	r, size := utf8.DecodeRuneInString(s)
	return strings.ToUpper(string(r)) + s[size:]
}

// prettifySubcategory ports `slug.split('_').map(w => w.charAt(0).toUpperCase() + w.slice(1)).join(' ')`
// (relocalize-document.ts:184), the human name given to an auto-created subcategory.
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
