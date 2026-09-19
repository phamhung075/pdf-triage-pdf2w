package aichat

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/chatquery"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// TestSanitizeProviderError pins the pure helper that makes a raw provider/SDK error safe to show
// in the chat answer: whitespace collapsed, credential-looking substrings redacted, benign text
// left intact.
func TestSanitizeProviderError(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty stays empty", "", ""},
		{"trims and collapses whitespace", "  Google API key\n is not\tconfigured  ", "Google API key is not configured"},
		{"collapses mixed whitespace", "line1\nline2\t  line3", "line1 line2 line3"},
		{"redacts key query parameter", "GET https://api.example.com/v1?key=abc123&x=1", "GET https://api.example.com/v1?key=[REDACTED]&x=1"},
		{"redacts api_key at start", "api_key=secretvalue&foo=bar", "api_key=[REDACTED]&foo=bar"},
		{"redacts bearer token", "Authorization: Bearer sk-live-abc123", "Authorization: [REDACTED]"},
		{"redacts basic authorization", "Authorization: Basic dXNlcjpwYXNz", "Authorization: [REDACTED]"},
		{"redacts sk token", "invalid key sk-abcdef123456", "invalid key [REDACTED]"},
		{"redacts AIza token", "API key AIzaSyD-1234567890 rejected", "API key [REDACTED] rejected"},
		{"keeps benign message", "Google API key is not configured", "Google API key is not configured"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sanitizeProviderError(tt.in); got != tt.want {
				t.Fatalf("sanitizeProviderError(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestSanitizeProviderErrorCapsLength pins the ~300-rune cap so a provider error that embeds a whole
// response body cannot flood the chat answer.
func TestSanitizeProviderErrorCapsLength(t *testing.T) {
	got := sanitizeProviderError(strings.Repeat("x", providerErrorMaxRunes+200))
	if n := len([]rune(got)); n > providerErrorMaxRunes {
		t.Fatalf("rune length = %d, want <= %d", n, providerErrorMaxRunes)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncated message = %q, want a trailing ellipsis", got)
	}
}

// providerErrorDeps retrieves one pay slip and fails the answer step with the given error.
func providerErrorDeps(chat *fakeChat) Deps {
	docs := mockDocs()
	store := &fakeStore{searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
		return []database.DocumentRecord{docs[2]}, nil
	}}
	return Deps{
		Store: store, Ollama: chat,
		PlanQuery: planFunc(chatquery.StructuredQuery{DocTypes: []string{"bulletin"}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{}}),
	}
}

// A generic provider failure (missing/invalid cloud API key, HTTP status error, timeout) must be
// visible to the user while the matched documents are still returned.
func TestProcessChatQuerySurfacesGenericProviderError(t *testing.T) {
	res, err := ProcessChatQuery(
		providerErrorDeps(&fakeChat{err: errors.New("Google API key is not configured")}),
		"bulletin", nil, time.Date(2026, 8, 12, 0, 0, 0, 0, time.Local),
	)
	if err != nil {
		t.Fatalf("ProcessChatQuery must not surface the provider failure as an error: %v", err)
	}
	if !strings.Contains(res.Answer, "⚠️ Erreur du fournisseur IA : Google API key is not configured") {
		t.Fatalf("answer = %q, want the marked provider-error line", res.Answer)
	}
	if !strings.Contains(res.Answer, "trouvé(s) dans vos archives") {
		t.Fatalf("answer = %q, want the document-list fallback kept", res.Answer)
	}
	if len(res.MatchedDocuments) != 1 || res.MatchedDocuments[0].ID != 3 {
		t.Fatalf("matchedDocuments = %#v, want the single retrieved doc", res.MatchedDocuments)
	}
}

// The Ollama-unavailable case keeps the pinned fallback answer byte-for-byte: no warning line is
// added, because that case has its own reminder elsewhere.
func TestProcessChatQueryKeepsOllamaUnavailableFallbackUnchanged(t *testing.T) {
	down := &ollama.OllamaUnavailableError{Message: "Ollama is down — cannot reach qwen3.5:9b at http://127.0.0.1:11434: connection refused. Start Ollama, then retry."}
	res, err := ProcessChatQuery(
		providerErrorDeps(&fakeChat{err: down}),
		"bulletin", nil, time.Date(2026, 8, 12, 0, 0, 0, 0, time.Local),
	)
	if err != nil {
		t.Fatalf("ProcessChatQuery must not surface the Ollama failure: %v", err)
	}
	want := "Voici les 1 document(s) trouvé(s) dans vos archives pour votre demande :"
	if res.Answer != want {
		t.Fatalf("answer = %q, want the pinned fallback %q", res.Answer, want)
	}
	if strings.Contains(res.Answer, "⚠️") {
		t.Fatalf("answer = %q, must not add a provider warning for Ollama-unavailable", res.Answer)
	}
	if len(res.MatchedDocuments) != 1 || res.MatchedDocuments[0].ID != 3 {
		t.Fatalf("matchedDocuments = %#v, want the single retrieved doc", res.MatchedDocuments)
	}
}

// A secret embedded in the provider error must never reach the user-visible answer.
func TestProcessChatQueryRedactsSecretsInProviderError(t *testing.T) {
	secret := "AIzaSyD-1234567890abcdef"
	providerErr := fmt.Errorf("Google API request failed: GET https://generativelanguage.googleapis.com?key=%s", secret)
	res, err := ProcessChatQuery(
		providerErrorDeps(&fakeChat{err: providerErr}),
		"bulletin", nil, time.Date(2026, 8, 12, 0, 0, 0, 0, time.Local),
	)
	if err != nil {
		t.Fatalf("ProcessChatQuery: %v", err)
	}
	if strings.Contains(res.Answer, secret) {
		t.Fatalf("answer leaks the API key: %q", res.Answer)
	}
	if !strings.Contains(res.Answer, "[REDACTED]") {
		t.Fatalf("answer = %q, want the credential redacted", res.Answer)
	}
}
