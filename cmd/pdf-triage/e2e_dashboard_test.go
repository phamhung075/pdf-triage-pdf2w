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
	app, err := newApplication(appOptions{DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("failed to initialize application: %v", err)
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

	// 8. POST /api/ai/test validation
	t.Run("POST /api/ai/test validation", func(t *testing.T) {
		// Missing fields
		res, err := client.Post(ts.URL+"/api/ai/test", "application/json", strings.NewReader(`{}`))
		if err != nil {
			t.Fatalf("POST /api/ai/test failed: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 Bad Request, got %d", res.StatusCode)
		}

		// Unknown provider
		res2, err := client.Post(ts.URL+"/api/ai/test", "application/json", strings.NewReader(`{"provider":"unknown","api_key":"test"}`))
		if err != nil {
			t.Fatalf("POST /api/ai/test failed: %v", err)
		}
		defer res2.Body.Close()
		if res2.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 Bad Request for unknown provider, got %d", res2.StatusCode)
		}
	})

	// 9. PUT /api/config with Cloud AI settings
	t.Run("PUT and GET /api/config with Cloud AI", func(t *testing.T) {
		tempIn := t.TempDir()
		tempOut := t.TempDir()
		putBody := `{"language":"FR","input_dir":` + `"` + strings.ReplaceAll(tempIn, `\`, `/`) + `",` +
			`"output_root_dir":` + `"` + strings.ReplaceAll(tempOut, `\`, `/`) + `",` +
			`"ollama_host":"http://127.0.0.1:11434",` +
			`"ollama_model":"qwen3.5:9b",` +
			`"ai_provider":"cloud",` +
			`"cloud_provider":"google",` +
			`"google_api_key":"AIzaSyFakeKeyForTest",` +
			`"google_model":"gemini-2.5-flash",` +
			`"anthropic_api_key":"sk-ant-fake-key",` +
			`"anthropic_model":"claude-3-7-sonnet-20250219",` +
			`"deepseek_api_key":"sk-deepseek-fake-key",` +
			`"deepseek_model":"deepseek-chat",` +
			`"openai_api_key":"sk-openai-fake-key",` +
			`"openai_model":"gpt-4o-mini"}`

		req, err := http.NewRequest(http.MethodPut, ts.URL+"/api/config", strings.NewReader(putBody))
		if err != nil {
			t.Fatalf("failed to create PUT request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")

		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("PUT /api/config failed: %v", err)
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(res.Body)
			t.Fatalf("expected 200 OK for PUT /api/config, got %d: %s", res.StatusCode, string(body))
		}

		// Verify GET /api/config reflects the saved cloud configuration
		getRes, err := client.Get(ts.URL + "/api/config")
		if err != nil {
			t.Fatalf("GET /api/config failed: %v", err)
		}
		defer getRes.Body.Close()

		var cfg map[string]any
		if err := json.NewDecoder(getRes.Body).Decode(&cfg); err != nil {
			t.Fatalf("failed to decode config: %v", err)
		}

		if cfg["ai_provider"] != "cloud" {
			t.Errorf("expected ai_provider 'cloud', got %v", cfg["ai_provider"])
		}
		if cfg["cloud_provider"] != "google" {
			t.Errorf("expected cloud_provider 'google', got %v", cfg["cloud_provider"])
		}
		if cfg["google_model"] != "gemini-2.5-flash" {
			t.Errorf("expected google_model 'gemini-2.5-flash', got %v", cfg["google_model"])
		}
		if cfg["google_api_key"] != "AIzaSyFakeKeyForTest" {
			t.Errorf("expected google_api_key saved, got %v", cfg["google_api_key"])
		}

		// Verify GET /api/ollama/status reports Cloud status
		statusRes, err := client.Get(ts.URL + "/api/ollama/status")
		if err != nil {
			t.Fatalf("GET /api/ollama/status failed: %v", err)
		}
		defer statusRes.Body.Close()

		var status map[string]any
		if err := json.NewDecoder(statusRes.Body).Decode(&status); err != nil {
			t.Fatalf("failed to decode status: %v", err)
		}

		if status["provider"] != "cloud" {
			t.Errorf("expected status.provider 'cloud', got %v", status["provider"])
		}
		if status["cloud_provider"] != "google" {
			t.Errorf("expected status.cloud_provider 'google', got %v", status["cloud_provider"])
		}
		if status["model"] != "gemini-2.5-flash" {
			t.Errorf("expected status.model 'gemini-2.5-flash', got %v", status["model"])
		}
		// Since a fake key was provided, online should be false and modelError present
		if status["online"] == true {
			t.Errorf("expected status.online false for fake key, got %v", status["online"])
		}
	})

	// 10. POST /api/ai/test with mock servers for all 4 providers
	t.Run("POST /api/ai/test mock live servers", func(t *testing.T) {
		// Mock Google
		mockGoogle := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"pong"}]}}]}`))
		}))
		defer mockGoogle.Close()

		// Mock Claude
		mockClaude := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"content":[{"type":"text","text":"pong"}]}`))
		}))
		defer mockClaude.Close()

		// Mock DeepSeek
		mockDeepSeek := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
		}))
		defer mockDeepSeek.Close()

		// Mock OpenAI
		mockOpenAI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"choices":[{"message":{"content":"pong"}}]}`))
		}))
		defer mockOpenAI.Close()

		tests := []struct {
			provider string
			baseURL  string
			model    string
		}{
			{"google", mockGoogle.URL, "gemini-2.5-flash"},
			{"claude", mockClaude.URL, "claude-3-7-sonnet-20250219"},
			{"deepseek", mockDeepSeek.URL, "deepseek-chat"},
			{"openai", mockOpenAI.URL, "gpt-4o-mini"},
		}

		for _, tc := range tests {
			payload := map[string]string{
				"provider": tc.provider,
				"api_key":  "test-api-key",
				"model":    tc.model,
				"base_url": tc.baseURL,
			}
			data, _ := json.Marshal(payload)
			res, err := client.Post(ts.URL+"/api/ai/test", "application/json", strings.NewReader(string(data)))
			if err != nil {
				t.Fatalf("%s POST /api/ai/test failed: %v", tc.provider, err)
			}
			if res.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(res.Body)
				res.Body.Close()
				t.Fatalf("expected 200 OK for %s, got %d: %s", tc.provider, res.StatusCode, string(body))
			}
			var result map[string]any
			if err := json.NewDecoder(res.Body).Decode(&result); err != nil {
				res.Body.Close()
				t.Fatalf("failed to decode response for %s: %v", tc.provider, err)
			}
			res.Body.Close()
			if result["ok"] != true {
				t.Errorf("expected ok: true for %s, got %v (error: %v)", tc.provider, result["ok"], result["error"])
			}
		}
	})
}
