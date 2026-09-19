package aiprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGoogleProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.String(), "key=") || r.URL.RawQuery != "" {
			http.Error(w, `{"error":{"code":400,"message":"api key must not be in the URL","status":"INVALID_ARGUMENT"}}`, http.StatusBadRequest)
			return
		}
		if r.Header.Get("x-goog-api-key") != "test-google-key" {
			http.Error(w, `{"error":{"code":401,"message":"bad key","status":"UNAUTHENTICATED"}}`, http.StatusUnauthorized)
			return
		}
		var req geminiRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{
				{
					"content": map[string]any{
						"parts": []map[string]any{
							{"text": `{"category":"finance","document_type":"invoice"}`},
						},
					},
					"finishReason": "STOP",
				},
			},
		})
	}))
	defer srv.Close()

	prov := NewGoogleProvider(GoogleConfig{
		APIKey:  "test-google-key",
		Model:   "gemini-2.5-flash",
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
	})

	ctx := context.Background()
	comp, err := prov.RequestClassification(ctx, "system prompt", "user prompt")
	if err != nil {
		t.Fatalf("RequestClassification failed: %v", err)
	}
	if comp.Response != `{"category":"finance","document_type":"invoice"}` {
		t.Errorf("unexpected response: %q", comp.Response)
	}

	testMsg, err := prov.Test(ctx)
	if err != nil {
		t.Fatalf("Test failed: %v", err)
	}
	if testMsg == "" {
		t.Errorf("empty test message")
	}
}

func TestClaudeProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-claude-key" {
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`, 401)
			return
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			http.Error(w, "missing anthropic version", 400)
			return
		}
		var req claudeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"content": []map[string]any{
				{"type": "text", "text": `{"document_type":"receipt"}`},
			},
			"stop_reason": "end_turn",
		})
	}))
	defer srv.Close()

	prov := NewClaudeProvider(ClaudeConfig{
		APIKey:  "test-claude-key",
		Model:   "claude-3-7-sonnet-20250219",
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
	})

	ctx := context.Background()
	comp, err := prov.RequestClassification(ctx, "system prompt", "user prompt")
	if err != nil {
		t.Fatalf("RequestClassification failed: %v", err)
	}
	if comp.Response != `{"document_type":"receipt"}` {
		t.Errorf("unexpected response: %q", comp.Response)
	}

	chat, err := prov.RequestTextChat(ctx, "system prompt", "hello")
	if err != nil {
		t.Fatalf("RequestTextChat failed: %v", err)
	}
	if chat.DoneReason != "end_turn" {
		t.Errorf("doneReason = %q, want end_turn", chat.DoneReason)
	}
}

func TestOpenAIProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-openai-key" {
			http.Error(w, `{"error":{"message":"bad auth","type":"auth_error"}}`, 401)
			return
		}
		var req openAIRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": `{"category":"taxes"}`,
					},
					"finish_reason": "stop",
				},
			},
		})
	}))
	defer srv.Close()

	prov := NewOpenAIProvider(OpenAIConfig{
		APIKey:  "test-openai-key",
		Model:   "gpt-4o-mini",
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
	})

	ctx := context.Background()
	comp, err := prov.RequestClassification(ctx, "system", "user")
	if err != nil {
		t.Fatalf("RequestClassification failed: %v", err)
	}
	if comp.Response != `{"category":"taxes"}` {
		t.Errorf("unexpected response: %q", comp.Response)
	}
}

func TestDeepSeekProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-deepseek-key" {
			http.Error(w, `{"error":{"message":"bad auth"}}`, 401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":              "assistant",
						"content":           `{"category":"health"}`,
						"reasoning_content": "DeepSeek R1 thinking trace",
					},
					"finish_reason": "stop",
				},
			},
		})
	}))
	defer srv.Close()

	prov := NewDeepSeekProvider(DeepSeekConfig{
		APIKey:  "test-deepseek-key",
		Model:   "deepseek-reasoner",
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
	})

	ctx := context.Background()
	comp, err := prov.RequestClassification(ctx, "system", "user")
	if err != nil {
		t.Fatalf("RequestClassification failed: %v", err)
	}
	if comp.Response != `{"category":"health"}` {
		t.Errorf("unexpected response: %q", comp.Response)
	}
	if comp.Thinking != "DeepSeek R1 thinking trace" {
		t.Errorf("unexpected thinking: %q", comp.Thinking)
	}
}

func TestManagerSwitching(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{
					"message": map[string]any{
						"role":    "assistant",
						"content": `{"status":"ok"}`,
					},
					"finish_reason": "stop",
				},
			},
		})
	}))
	defer srv.Close()

	mgr := NewManager(Config{
		AIProvider:    "cloud",
		CloudProvider: "openai",
		OpenAIAPIKey:  "key",
		OpenAIBaseURL: srv.URL,
	}, nil)

	// Override HTTP client for test
	mgr.openai = NewOpenAIProvider(OpenAIConfig{
		APIKey:  "key",
		Model:   "gpt-4o-mini",
		BaseURL: srv.URL,
		HTTP:    srv.Client(),
	})

	prov, name := mgr.ActiveProvider()
	if prov == nil || name != "OpenAI" {
		t.Fatalf("active provider: %v, %s", prov, name)
	}

	comp, err := mgr.RequestClassificationCompletion("sys", "usr")
	if err != nil {
		t.Fatalf("RequestClassificationCompletion failed: %v", err)
	}
	if comp.Response != `{"status":"ok"}` {
		t.Errorf("got %s", comp.Response)
	}

	// Switch to google
	mgr.UpdateConfig(Config{
		AIProvider:    "cloud",
		CloudProvider: "google",
		GoogleAPIKey:  "gkey",
	})
	_, newName := mgr.ActiveProvider()
	if newName != "Google Gemini" {
		t.Errorf("expected Google Gemini, got %s", newName)
	}
}
