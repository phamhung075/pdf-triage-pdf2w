package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestE2EDashboardAndAPI verifies end-to-end that the web dashboard and its API endpoints
// work correctly when started from the submodule directory, asserting that:
// 1. Root and static assets (/, /style.css, /js/TriageApp.js) return 200 OK with correct content types.
// 2. Core API routes (/api/config, /api/categories, /api/documents) return 200 OK with valid JSON.
// 3. The SSE event stream (/api/triage/events) connects with text/event-stream.
func TestE2EDashboardAndAPI(t *testing.T) {
	app, ok := newStartupApplication()
	if !ok {
		t.Fatalf("failed to initialize application")
	}
	defer app.Close()

	if !strings.HasSuffix(app.publicDir, "public") {
		t.Fatalf("expected publicDir to end with 'public', got %q", app.publicDir)
	}

	ts := httptest.NewServer(app.handler)
	defer ts.Close()

	client := &http.Client{Timeout: 5 * time.Second}

	// 1. GET / -> index.html
	t.Run("GET /", func(t *testing.T) {
		res, err := client.Get(ts.URL + "/")
		if err != nil {
			t.Fatalf("GET / failed: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for /, got %d", res.StatusCode)
		}
		ct := res.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/html") {
			t.Errorf("expected Content-Type text/html, got %q", ct)
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatalf("failed to read body: %v", err)
		}
		if !strings.Contains(string(body), "Smart PDF Triage Dashboard") {
			t.Errorf("expected body to contain 'Smart PDF Triage Dashboard', got: %s", string(body[:min(len(body), 200)]))
		}
	})

	// 2. GET /style.css
	t.Run("GET /style.css", func(t *testing.T) {
		res, err := client.Get(ts.URL + "/style.css")
		if err != nil {
			t.Fatalf("GET /style.css failed: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for /style.css, got %d", res.StatusCode)
		}
		ct := res.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/css") {
			t.Errorf("expected Content-Type text/css, got %q", ct)
		}
	})

	// 3. GET /js/TriageApp.js
	t.Run("GET /js/TriageApp.js", func(t *testing.T) {
		res, err := client.Get(ts.URL + "/js/TriageApp.js")
		if err != nil {
			t.Fatalf("GET /js/TriageApp.js failed: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for /js/TriageApp.js, got %d", res.StatusCode)
		}
	})

	// 4. GET /api/config
	t.Run("GET /api/config", func(t *testing.T) {
		res, err := client.Get(ts.URL + "/api/config")
		if err != nil {
			t.Fatalf("GET /api/config failed: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for /api/config, got %d", res.StatusCode)
		}
		var cfg map[string]any
		if err := json.NewDecoder(res.Body).Decode(&cfg); err != nil {
			t.Fatalf("failed to decode JSON from /api/config: %v", err)
		}
		if _, ok := cfg["language"]; !ok {
			t.Errorf("expected 'language' in /api/config response, got %v", cfg)
		}
	})

	// 5. GET /api/categories
	t.Run("GET /api/categories", func(t *testing.T) {
		res, err := client.Get(ts.URL + "/api/categories")
		if err != nil {
			t.Fatalf("GET /api/categories failed: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for /api/categories, got %d", res.StatusCode)
		}
		var categories any
		if err := json.NewDecoder(res.Body).Decode(&categories); err != nil {
			t.Fatalf("failed to decode JSON from /api/categories: %v", err)
		}
	})

	// 6. GET /api/documents
	t.Run("GET /api/documents", func(t *testing.T) {
		res, err := client.Get(ts.URL + "/api/documents")
		if err != nil {
			t.Fatalf("GET /api/documents failed: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for /api/documents, got %d", res.StatusCode)
		}
	})

	// 7. GET /api/triage/events (SSE)
	t.Run("GET /api/triage/events SSE", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/triage/events", nil)
		if err != nil {
			t.Fatalf("failed to create SSE request: %v", err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/triage/events failed: %v", err)
		}
		defer res.Body.Close()

		if res.StatusCode != http.StatusOK {
			t.Fatalf("expected 200 OK for SSE, got %d", res.StatusCode)
		}
		ct := res.Header.Get("Content-Type")
		if !strings.Contains(ct, "text/event-stream") {
			t.Errorf("expected Content-Type text/event-stream, got %q", ct)
		}
	})
}
