package aiprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// --- Echo parsing per provider ---------------------------------------------------------------

// decodeModel reads the model id a request selected from its JSON body.
func decodeModel(t *testing.T, r *http.Request) string {
	t.Helper()
	var req struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		t.Errorf("decode upstream request: %v", err)
	}
	return req.Model
}

// TestDeepSeekModelEcho covers the chat/completions "model" echo: present-and-equal, present-but-
// different (proving the value is not config), and omitted.
func TestDeepSeekModelEcho(t *testing.T) {
	const requested = "deepseek-chat"
	tests := []struct {
		name      string
		echo      string // "" omits the field
		want      string
		wantMatch bool
	}{
		{"echoes requested", requested, requested, true},
		{"echoes different model", "deepseek-flash", "deepseek-flash", false},
		{"omits echo", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = decodeModel(t, r)
				body := map[string]any{
					"choices": []map[string]any{{
						"message":       map[string]any{"role": "assistant", "content": "OK"},
						"finish_reason": "stop",
					}},
				}
				if tc.echo != "" {
					body["model"] = tc.echo
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer srv.Close()

			prov := NewDeepSeekProvider(DeepSeekConfig{
				APIKey: "test-key", Model: requested, BaseURL: srv.URL, HTTP: srv.Client(),
			})
			ctx := context.Background()

			comp, err := prov.RequestClassification(ctx, "sys", "usr")
			if err != nil {
				t.Fatalf("RequestClassification: %v", err)
			}
			if comp.Model != tc.want {
				t.Fatalf("Completion.Model = %q, want %q", comp.Model, tc.want)
			}

			_, served, err := prov.TestWithModel(ctx)
			if err != nil {
				t.Fatalf("TestWithModel: %v", err)
			}
			if served != tc.want {
				t.Fatalf("served = %q, want the echoed %q", served, tc.want)
			}
			if got := ModelMatches(requested, served); got != tc.wantMatch {
				t.Fatalf("ModelMatches(%q, %q) = %v, want %v", requested, served, got, tc.wantMatch)
			}
		})
	}
}

// TestGoogleModelEcho covers Gemini's generateContent "modelVersion" echo, which is a different
// top-level field from the OpenAI-compatible providers.
func TestGoogleModelEcho(t *testing.T) {
	const requested = "gemini-2.5-flash"
	tests := []struct {
		name      string
		echo      string
		want      string
		wantMatch bool
	}{
		{"echoes requested", requested, requested, true},
		{"echoes suffixed build", "gemini-2.5-flash-001", "gemini-2.5-flash-001", true},
		{"echoes different model", "gemini-3.8-flash", "gemini-3.8-flash", false},
		{"omits echo", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				body := map[string]any{
					"candidates": []map[string]any{{
						"content":      map[string]any{"parts": []map[string]any{{"text": "OK"}}},
						"finishReason": "STOP",
					}},
				}
				if tc.echo != "" {
					body["modelVersion"] = tc.echo
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer srv.Close()

			prov := NewGoogleProvider(GoogleConfig{
				APIKey: "test-key", Model: requested, BaseURL: srv.URL, HTTP: srv.Client(),
			})
			_, served, err := prov.TestWithModel(context.Background())
			if err != nil {
				t.Fatalf("TestWithModel: %v", err)
			}
			if served != tc.want {
				t.Fatalf("served = %q, want the echoed %q", served, tc.want)
			}
			if got := ModelMatches(requested, served); got != tc.wantMatch {
				t.Fatalf("ModelMatches(%q, %q) = %v, want %v", requested, served, got, tc.wantMatch)
			}
		})
	}
}

// TestClaudeModelEcho covers Anthropic's messages "model" echo.
func TestClaudeModelEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"claude-3-7-sonnet-20250219","content":[{"type":"text","text":"OK"}],"stop_reason":"end_turn"}`))
	}))
	defer srv.Close()

	prov := NewClaudeProvider(ClaudeConfig{
		APIKey: "test-key", Model: "claude-3-7-sonnet", BaseURL: srv.URL, HTTP: srv.Client(),
	})
	comp, err := prov.RequestClassification(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("RequestClassification: %v", err)
	}
	if comp.Model != "claude-3-7-sonnet-20250219" {
		t.Fatalf("Completion.Model = %q", comp.Model)
	}
	_, served, err := prov.TestWithModel(context.Background())
	if err != nil {
		t.Fatalf("TestWithModel: %v", err)
	}
	if !ModelMatches("claude-3-7-sonnet", served) {
		t.Fatalf("ModelMatches = false for requested claude-3-7-sonnet and served %q", served)
	}
}

// TestOpenAIModelEcho covers OpenAI's chat/completions "model" echo.
func TestOpenAIModelEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"gpt-4o-mini-2024-07-18","choices":[{"message":{"role":"assistant","content":"OK"},"finish_reason":"stop"}]}`))
	}))
	defer srv.Close()

	prov := NewOpenAIProvider(OpenAIConfig{
		APIKey: "test-key", Model: "gpt-4o-mini", BaseURL: srv.URL, HTTP: srv.Client(),
	})
	comp, err := prov.RequestClassification(context.Background(), "sys", "usr")
	if err != nil {
		t.Fatalf("RequestClassification: %v", err)
	}
	if comp.Model != "gpt-4o-mini-2024-07-18" {
		t.Fatalf("Completion.Model = %q", comp.Model)
	}
	_, served, err := prov.TestWithModel(context.Background())
	if err != nil {
		t.Fatalf("TestWithModel: %v", err)
	}
	if !ModelMatches("gpt-4o-mini", served) {
		t.Fatalf("ModelMatches = false for requested gpt-4o-mini and served %q", served)
	}
}

// --- Manager lifecycle -----------------------------------------------------------------------

// TestManagerLastClassificationLifecycle proves the Manager records the echoed model of the most
// recent successful classification, ignores chat calls, and resets the record on UpdateConfig so a
// model change starts fresh. It also confirms the next probe reports the new confirmed model.
func TestManagerLastClassificationLifecycle(t *testing.T) {
	const (
		modelA = "deepseek-chat"
		modelB = "deepseek-flash"
	)
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&calls, 1)
		model := decodeModel(t, r)
		echo := model
		if n == 2 {
			// The second upstream call is the text-chat probe: echo a model the chat must NOT be
			// able to overwrite the classification record with.
			echo = "deepseek-reasoner"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": echo,
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "pong"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	cfgFor := func(model string) Config {
		return Config{
			AIProvider: "cloud", CloudProvider: "deepseek",
			DeepSeekAPIKey: "test-key", DeepSeekModel: model, DeepSeekBaseURL: srv.URL,
		}
	}
	mgr := NewManager(cfgFor(modelA), nil)

	if model, at := mgr.LastClassification(); model != "" || !at.IsZero() {
		t.Fatalf("record before any classification = (%q, %v), want empty/zero", model, at)
	}

	// 1st call: classification A.
	if _, err := mgr.RequestClassificationCompletion("sys", "usr"); err != nil {
		t.Fatalf("classification A: %v", err)
	}
	gotModel, gotAt := mgr.LastClassification()
	if gotModel != modelA || gotAt.IsZero() {
		t.Fatalf("after classification A = (%q, %v), want (%q, non-zero)", gotModel, gotAt, modelA)
	}

	// 2nd call: chat echoes a different model; the record must not move.
	if _, err := mgr.RequestTextChatCompletion("sys", "usr"); err != nil {
		t.Fatalf("chat A: %v", err)
	}
	if model, _ := mgr.LastClassification(); model != modelA {
		t.Fatalf("chat overwrote the classification record: %q", model)
	}

	// UpdateConfig resets the record; a new model starts fresh.
	mgr.UpdateConfig(cfgFor(modelB))
	if model, at := mgr.LastClassification(); model != "" || !at.IsZero() {
		t.Fatalf("record after UpdateConfig = (%q, %v), want empty/zero", model, at)
	}

	// The next probe reports the new model the upstream echoes.
	health := mgr.CheckModelCanGenerate(modelB, true)
	if !health.OK || health.ServedModel != modelB {
		t.Fatalf("health after UpdateConfig = %+v, want OK with served %q", health, modelB)
	}
	if !ModelMatches(modelB, health.ServedModel) {
		t.Fatalf("health served model %q does not match requested %q", health.ServedModel, modelB)
	}

	if _, err := mgr.RequestClassificationCompletion("sys", "usr"); err != nil {
		t.Fatalf("classification B: %v", err)
	}
	if model, at := mgr.LastClassification(); model != modelB || at.IsZero() {
		t.Fatalf("after classification B = (%q, %v), want (%q, non-zero)", model, at, modelB)
	}
}
