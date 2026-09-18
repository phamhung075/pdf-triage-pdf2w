// Package cleantext is a Go port of cleanExtractedText from pdf-triage's src/domain/pdf-text.ts.
package cleantext

import (
	"regexp"
	"strings"
)

var (
	crlfRe        = regexp.MustCompile("\r\n")
	tripleBlankRe = regexp.MustCompile(`\n{3,}`)
)

// CleanExtractedText ports src/domain/pdf-text.ts's cleanExtractedText function-for-function:
// text under 10 trimmed characters is discarded outright (the same floor as the Golden Rule 3
// "< 10 chars" no-text guard), null bytes are stripped, CRLF is normalized to LF, and 3+
// consecutive newlines collapse to exactly 2.
func CleanExtractedText(text string) string {
	if len(strings.TrimSpace(text)) < 10 {
		return ""
	}
	cleaned := strings.ReplaceAll(text, "\x00", "")
	cleaned = crlfRe.ReplaceAllString(cleaned, "\n")
	cleaned = tripleBlankRe.ReplaceAllString(cleaned, "\n\n")
	return strings.TrimSpace(cleaned)
}
