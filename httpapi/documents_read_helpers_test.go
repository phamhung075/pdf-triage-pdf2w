package httpapi

import "testing"

// TestDocumentReadHelperParity pins the JS algorithms the document READ/EXPORT routes reproduce.
func TestDocumentReadHelperParity(t *testing.T) {
	t.Run("encodeURIComponent matches the JS algorithm", func(t *testing.T) {
		cases := map[string]string{
			"Avis de Taxes Foncières.md": "Avis%20de%20Taxes%20Fonci%C3%A8res.md",
			"a+b#c.md":                   "a%2Bb%23c.md",
			"safe-_.!~*'().md":           "safe-_.!~*'().md",
			"é":                          "%C3%A9",
		}
		for in, want := range cases {
			if got := encodeURIComponent(in); got != want {
				t.Errorf("encodeURIComponent(%q) = %q, want %q", in, got, want)
			}
		}
	})

	t.Run("contentDispositionAttachment emits the ASCII fallback and filename*", func(t *testing.T) {
		got := contentDispositionAttachment("Avis de Taxes Foncières.md")
		want := `attachment; filename="Avis de Taxes Fonci_res.md"; filename*=UTF-8''Avis%20de%20Taxes%20Fonci%C3%A8res.md`
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("replaceNewlines only collapses CRLF and bare LF", func(t *testing.T) {
		if got := replaceNewlines("a\r\nb\nc\rd"); got != "a b c\rd" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("sanitizeTitle and sanitizeZipName match the JS character classes", func(t *testing.T) {
		if got := sanitizeTitle(`a/b\c?d%e*f:g|h"i<j>k`); got != "a_b_c_d_e_f_g_h_i_j_k" {
			t.Fatalf("sanitizeTitle = %q", got)
		}
		if got := sanitizeZipName("my/dossier:2026.pdf"); got != "my_dossier_2026.pdf" {
			t.Fatalf("sanitizeZipName = %q", got)
		}
	})

	t.Run("truncateRawText counts UTF-16 code units", func(t *testing.T) {
		if got := truncateRawText("abc"); got != "abc" {
			t.Fatalf("short = %q", got)
		}
		long := make([]rune, 801)
		for i := range long {
			long[i] = 'x'
		}
		if got := truncateRawText(string(long)); len(got) != 800 {
			t.Fatalf("len = %d, want 800", len(got))
		}
		// One astral rune (2 UTF-16 units) plus 798 ASCII units is exactly 800 UTF-16 units.
		astral := "\U0001F600" + string(long[:798])
		if got := truncateRawText(astral); got != astral {
			t.Fatalf("astral exactly-800 got len %d", utf16Len(got))
		}
		if got := truncateRawText(astral + "x"); utf16Len(got) != 800 {
			t.Fatalf("astral+1 utf16 len = %d, want 800", utf16Len(got))
		}
	})

	t.Run("jsParseInt mirrors parseInt(value, 10)", func(t *testing.T) {
		cases := []struct {
			in   any
			want int64
			ok   bool
		}{
			{float64(3), 3, true},
			{float64(3.9), 3, true},
			{float64(-4), -4, true},
			{"  42abc", 42, true},
			{"-7", -7, true},
			{"+8", 8, true},
			{"abc", 0, false},
			{"", 0, false},
			{nil, 0, false},
			{true, 0, false},
		}
		for _, tc := range cases {
			got, ok := jsParseInt(tc.in)
			if got != tc.want || ok != tc.ok {
				t.Errorf("jsParseInt(%#v) = (%d,%v), want (%d,%v)", tc.in, got, ok, tc.want, tc.ok)
			}
		}
	})
}
