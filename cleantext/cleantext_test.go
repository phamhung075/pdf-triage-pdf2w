package cleantext

import (
	"strings"
	"testing"
)

func TestCleanExtractedText(t *testing.T) {
	t.Run("returns empty string for text under 10 clean chars", func(t *testing.T) {
		if got := CleanExtractedText("short"); got != "" {
			t.Fatalf("got %q, want empty string", got)
		}
		if got := CleanExtractedText(""); got != "" {
			t.Fatalf("got %q, want empty string", got)
		}
	})

	t.Run("strips null bytes, normalizes newlines, and collapses excess blank lines", func(t *testing.T) {
		got := CleanExtractedText("Hello\x00World\r\n\r\n\r\n\r\nMore text here")
		if strings.Contains(got, "\x00") {
			t.Fatalf("result still contains a null byte: %q", got)
		}
		if strings.Contains(got, "\r\n") {
			t.Fatalf("result still contains \\r\\n: %q", got)
		}
		if strings.Contains(got, "\n\n\n") {
			t.Fatalf("result still contains 3+ consecutive newlines: %q", got)
		}
	})
}
