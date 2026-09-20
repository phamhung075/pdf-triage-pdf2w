package aiprovider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// modelRecorder captures, in order, the model id each upstream request selected.
type modelRecorder struct {
	mu    sync.Mutex
	seen  []string
	calls int
}

func (r *modelRecorder) record(model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, model)
	r.calls++
}

func (r *modelRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

// TestManagerLiveModelChangeDeepSeek proves a DeepSeek model changed in Settings reaches the running
// Manager without a restart: both classification and chat send model A, then UpdateConfig retargets
// the rebuilt provider so the next classification and chat send model B.
func TestManagerLiveModelChangeDeepSeek(t *testing.T) {
	var rec modelRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req deepSeekRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.record(req.Model)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "pong"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer srv.Close()

	cfgFor := func(model string) Config {
		return Config{
			AIProvider:      "cloud",
			CloudProvider:   "deepseek",
			DeepSeekAPIKey:  "test-key",
			DeepSeekModel:   model,
			DeepSeekBaseURL: srv.URL,
		}
	}

	const modelA, modelB = "deepseek-chat", "deepseek-flash"
	mgr := NewManager(cfgFor(modelA), nil)

	if _, err := mgr.RequestClassificationCompletion("sys", "usr"); err != nil {
		t.Fatalf("classification (A): %v", err)
	}
	if _, err := mgr.RequestTextChatCompletion("sys", "usr"); err != nil {
		t.Fatalf("chat (A): %v", err)
	}
	if got := rec.snapshot(); len(got) != 2 || got[0] != modelA || got[1] != modelA {
		t.Fatalf("models before UpdateConfig = %v, want [%s %s]", got, modelA, modelA)
	}

	mgr.UpdateConfig(cfgFor(modelB))

	if _, err := mgr.RequestClassificationCompletion("sys", "usr"); err != nil {
		t.Fatalf("classification (B): %v", err)
	}
	if _, err := mgr.RequestTextChatCompletion("sys", "usr"); err != nil {
		t.Fatalf("chat (B): %v", err)
	}
	got := rec.snapshot()
	if len(got) != 4 || got[2] != modelB || got[3] != modelB {
		t.Fatalf("models after UpdateConfig = %v, want [%s %s %s %s]", got, modelA, modelA, modelB, modelB)
	}
}

// TestManagerLiveModelChangeGoogle proves the same for Google, whose selected model is carried in the
// generateContent request URL path rather than in a JSON body.
func TestManagerLiveModelChangeGoogle(t *testing.T) {
	var rec modelRecorder
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "/v1beta/models/"
		model := ""
		if strings.HasPrefix(r.URL.Path, prefix) {
			model = strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, prefix), ":generateContent")
		}
		rec.record(model)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"candidates": []map[string]any{{
				"content":      map[string]any{"parts": []map[string]any{{"text": "pong"}}},
				"finishReason": "STOP",
			}},
		})
	}))
	defer srv.Close()

	cfgFor := func(model string) Config {
		return Config{
			AIProvider:    "cloud",
			CloudProvider: "google",
			GoogleAPIKey:  "test-key",
			GoogleModel:   model,
			GoogleBaseURL: srv.URL,
		}
	}

	const modelA, modelB = "gemini-2.5-flash", "gemini-3.8-flash"
	mgr := NewManager(cfgFor(modelA), nil)

	if _, err := mgr.RequestClassificationCompletion("sys", "usr"); err != nil {
		t.Fatalf("classification (A): %v", err)
	}
	if _, err := mgr.RequestTextChatCompletion("sys", "usr"); err != nil {
		t.Fatalf("chat (A): %v", err)
	}
	if got := rec.snapshot(); len(got) != 2 || got[0] != modelA || got[1] != modelA {
		t.Fatalf("models before UpdateConfig = %v, want [%s %s]", got, modelA, modelA)
	}

	mgr.UpdateConfig(cfgFor(modelB))

	if _, err := mgr.RequestClassificationCompletion("sys", "usr"); err != nil {
		t.Fatalf("classification (B): %v", err)
	}
	if _, err := mgr.RequestTextChatCompletion("sys", "usr"); err != nil {
		t.Fatalf("chat (B): %v", err)
	}
	got := rec.snapshot()
	if len(got) != 4 || got[2] != modelB || got[3] != modelB {
		t.Fatalf("models after UpdateConfig = %v, want [%s %s %s %s]", got, modelA, modelA, modelB, modelB)
	}
}

// TestManagerListModelsDefaultsFirst pins the new defaults as the first entry of each provider list,
// with the previous ids still selectable behind them.
func TestManagerListModelsDefaultsFirst(t *testing.T) {
	tests := []struct {
		provider string
		want     []string
	}{
		{"google", []string{"gemini-3.8-flash", "gemini-2.5-flash", "gemini-2.5-pro", "gemini-1.5-flash", "gemini-1.5-pro"}},
		{"deepseek", []string{"deepseek-flash", "deepseek-chat", "deepseek-reasoner"}},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			mgr := NewManager(Config{AIProvider: "cloud", CloudProvider: tc.provider}, nil)
			got, err := mgr.ListModels("")
			if err != nil {
				t.Fatalf("ListModels: %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("ListModels = %v, want %v", got, tc.want)
			}
		})
	}
}
