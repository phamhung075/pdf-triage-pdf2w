package aichat

import (
	"regexp"
	"strings"
)

// providerErrorMaxRunes caps how much of a provider error message reaches the chat answer. A raw
// HTTP/SDK error can embed a whole response body; the user needs the cause, not the payload.
const providerErrorMaxRunes = 300

var (
	// providerErrorBearerRe is `Bearer <token>` in an Authorization header or URL.
	providerErrorBearerRe = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/=-]+`)

	// providerErrorSkRe is an OpenAI-style secret token (`sk-...`).
	providerErrorSkRe = regexp.MustCompile(`\bsk-[A-Za-z0-9_-]+`)

	// providerErrorGoogleKeyRe is a Google API key (`AIza...`).
	providerErrorGoogleKeyRe = regexp.MustCompile(`\bAIza[A-Za-z0-9_-]+`)

	// providerErrorKeyParamRe is a query-string/parameter credential: `key=...`, `api_key=...`,
	// `apikey=...`, `token=...`, `secret=...`, `password=...`. The leading separator (or start of
	// string) and the parameter name are preserved; only the value is replaced.
	providerErrorKeyParamRe = regexp.MustCompile(`(?i)([?&\s]|^)([A-Za-z0-9_.-]*(?:key|token|secret|password))=([^&\s]+)`)

	// providerErrorAuthRe is a bare Authorization header (`Authorization: <scheme> <token>`). The
	// optional second token covers Basic auth (`Basic <base64>`), which Bearer's own rule misses.
	providerErrorAuthRe = regexp.MustCompile(`(?i)\bauthorization\b\s*[:=]\s*\S+(?:\s+\S+)?`)
)

// sanitizeProviderError makes a provider/SDK error message safe to show in the chat answer: it
// collapses whitespace, redacts credential-looking substrings, and caps the length (~300 runes).
//
// The redaction is deliberately broad — a false positive costs a bit of diagnostic detail, while a
// missed `key=...` or `Bearer ...` leaks a live credential into a user-visible answer.
func sanitizeProviderError(message string) string {
	s := strings.Join(strings.Fields(message), " ")
	s = providerErrorBearerRe.ReplaceAllString(s, "[REDACTED]")
	s = providerErrorSkRe.ReplaceAllString(s, "[REDACTED]")
	s = providerErrorGoogleKeyRe.ReplaceAllString(s, "[REDACTED]")
	s = providerErrorKeyParamRe.ReplaceAllString(s, "$1$2=[REDACTED]")
	s = providerErrorAuthRe.ReplaceAllString(s, "Authorization: [REDACTED]")

	runes := []rune(s)
	if len(runes) > providerErrorMaxRunes {
		s = string(runes[:providerErrorMaxRunes-1]) + "…"
	}
	return s
}
