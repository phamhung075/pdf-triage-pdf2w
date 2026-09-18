package guards

import "testing"

// Golden Rule 3's no-text guard, ported from triage-scan.ts:216-240. The predicate is pure and the
// threshold is exactly 10 (JS-trimmed, UTF-16) characters.

func TestIsNoTextBlocked(t *testing.T) {
	cases := []struct {
		name string
		text string
		want bool
	}{
		{"empty string is blocked", "", true},
		{"whitespace-only is blocked", "   \n\t  ", true},
		{"9 characters is blocked", "123456789", true},
		{"exactly 10 characters passes", "1234567890", false},
		{"11 characters passes", "12345678901", false},
		{"surrounding whitespace is trimmed before counting", "   1234567890   ", false},
		{"trimmed length below 10 with padding is blocked", "   123456789    ", true},
		// JS String.prototype.trim removes U+FEFF (ZWNBSP), Go strings.TrimSpace does not.
		{"U+FEFF is JavaScript whitespace", "\uFEFF123456789", true},
		// JS trim does NOT remove U+0085 NEL; Go strings.TrimSpace does. The guard matches JS.
		{"U+0085 NEL is not JavaScript whitespace", "\u0085123456789", false},
		{"U+00A0 NBSP is JavaScript whitespace", "\u00A0123456789", true},
		// String.prototype.length counts UTF-16 code units: an astral emoji is 2.
		{"5 astral emoji = 10 UTF-16 units passes", "😀😀😀😀😀", false},
		{"4 astral emoji = 8 UTF-16 units is blocked", "😀😀😀😀", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := IsNoTextBlocked(tc.text); got != tc.want {
				t.Fatalf("IsNoTextBlocked(%q) = %v, want %v (len bytes=%d, utf16=%d)",
					tc.text, got, tc.want, len(tc.text), utf16Len(jsTrim(tc.text)))
			}
		})
	}
}

func TestNoTextViolation(t *testing.T) {
	v := NoTextViolation()
	if v.Code != CodeNoTextExtracted {
		t.Fatalf("code = %q", v.Code)
	}
	if v.Message != "❌ Blocked: No text extracted from PDF. Moved to __raws/blocked_files." {
		t.Fatalf("message = %q", v.Message)
	}
	if v.BlockedFileReason != "NO_TEXT_EXTRACTED" {
		t.Fatalf("reason = %q", v.BlockedFileReason)
	}
}

func TestNoTextThresholdConstant(t *testing.T) {
	if NoTextMinCleanCharacters != 10 {
		t.Fatalf("threshold = %d, want 10", NoTextMinCleanCharacters)
	}
}
