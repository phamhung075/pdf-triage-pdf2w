package aiprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestGoogleProviderSendsKeyInHeaderNotURL asserts every Google request carries
// the API key in the x-goog-api-key header and never in the URL.
func TestGoogleProviderSendsKeyInHeaderNotURL(t *testing.T) {
	const key = "secret-google-key"
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if r.Header.Get("x-goog-api-key") != key {
			http.Error(w, `{"error":{"code":401,"message":"missing header","status":"UNAUTHENTICATED"}}`, http.StatusUnauthorized)
			return
		}
		if r.URL.RawQuery != "" || strings.Contains(r.URL.String(), key) {
			http.Error(w, `{"error":{"code":400,"message":"api key leaked into URL","status":"INVALID_ARGUMENT"}}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}]}`))
	}))
	defer srv.Close()

	prov := NewGoogleProvider(GoogleConfig{
		APIKey:  key,
		Model:   "gemini-2.5-flash",
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
	})
	ctx := context.Background()
	if _, err := prov.RequestClassification(ctx, "system", "user"); err != nil {
		t.Fatalf("RequestClassification: %v", err)
	}
	if _, err := prov.RequestTextChat(ctx, "system", "user"); err != nil {
		t.Fatalf("RequestTextChat: %v", err)
	}
	if _, err := prov.Test(ctx); err != nil {
		t.Fatalf("Test: %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Errorf("request count = %d, want 3", got)
	}
}

type failingRoundTripper struct{ err error }

func (f failingRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, f.err
}

// TestGoogleProviderTransportErrorHidesKey asserts a transport error is
// redacted so the API key cannot leak through the wrapped error text.
func TestGoogleProviderTransportErrorHidesKey(t *testing.T) {
	const key = "secret-google-key"
	client := &http.Client{Transport: failingRoundTripper{
		err: errors.New("dial tcp: connection refused while sending key " + key),
	}}
	prov := NewGoogleProvider(GoogleConfig{
		APIKey:  key,
		Model:   "gemini-2.5-flash",
		BaseURL: "https://example.invalid",
		HTTP:    client,
	})

	_, err := prov.RequestClassification(context.Background(), "system", "user")
	if err == nil {
		t.Fatal("expected transport error")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("transport error leaks API key: %v", err)
	}
	if !strings.Contains(err.Error(), "[REDACTED]") {
		t.Errorf("transport error not redacted: %v", err)
	}
}

// TestGoogleProviderErrorPathsRedactKey covers the non-2xx, JSON-decode and
// API-error response paths and asserts none can surface the API key.
func TestGoogleProviderErrorPathsRedactKey(t *testing.T) {
	const key = "secret-google-key"
	tests := []struct {
		name        string
		status      int
		body        string
		wantMessage string
	}{
		{
			name:        "non-2xx body",
			status:      http.StatusInternalServerError,
			body:        `{"candidates":[{"content":{"parts":[{"text":"leak ` + key + `"}]}}]}`,
			wantMessage: "Google API HTTP 500",
		},
		{
			name:        "invalid json",
			status:      http.StatusOK,
			body:        `not-json ` + key,
			wantMessage: "parse Google API response",
		},
		{
			name:        "api error object",
			status:      http.StatusOK,
			body:        `{"error":{"code":403,"message":"key ` + key + ` is invalid","status":"PERMISSION_DENIED"}}`,
			wantMessage: "Google API error",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			prov := NewGoogleProvider(GoogleConfig{
				APIKey:  key,
				Model:   "gemini-2.5-flash",
				BaseURL: srv.URL,
				HTTP:    srv.Client(),
			})
			_, err := prov.RequestClassification(context.Background(), "system", "user")
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), key) {
				t.Errorf("error leaks API key: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantMessage) {
				t.Errorf("error = %v, want substring %q", err, tc.wantMessage)
			}
		})
	}
}
