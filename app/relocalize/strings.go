package relocalize

import "strings"

// This file holds the small JS-faithful string helpers this package needs. They mirror the
// unexported helpers in app/guards (jsTrim, isJSWhitespace, utf16Len) and app/classify; those
// packages cannot export them, so the exact behavior is reproduced here rather than approximated
// with Go's divergent stdlib (see the package comment gap 2).

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
// matches the TS `> 10` liveness threshold for astral input.
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

// firstNonEmpty is the TS `a || b` fallback for strings: the first non-empty value, or "".
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// deref is the TS optional-argument read: a nil pointer means `undefined`, i.e. "".
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func ptrString(s string) *string { return &s }
