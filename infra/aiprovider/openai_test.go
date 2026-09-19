package aiprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestIsReasoningModel(t *testing.T) {
	tests := []struct {
		model string
		want  bool
	}{
		{"gpt-4o-mini", false},
		{"gpt-4.5-preview", false},
		{"o3-mini", true},
		{"o1", true},
		{"o4-mini", true},
		{"gpt-5", true},
		{"gpt-5-mini", true},
		{"deepseek-chat", false},
		{"", false},
		{"O3-MINI", true},
		{"  o1  ", true},
		{"omni", false},
		{"o", false},
	}
	for _, tc := range tests {
		if got := isReasoningModel(tc.model); got != tc.want {
			t.Errorf("isReasoningModel(%q) = %v, want %v", tc.model, got, tc.want)
		}
	}
}

// TestOpenAIRequestBodyForReasoningAndStandardModels captures the outgoing JSON
// and asserts reasoning models drop temperature and use max_completion_tokens,
// while every other model keeps the legacy body shape.
func TestOpenAIRequestBodyForReasoningAndStandardModels(t *testing.T) {
	cases := []struct {
		model             string
		wantTemperature   bool
		wantMaxTokens     bool
		wantMaxCompletion bool
	}{
		{"o3-mini", false, false, true},
		{"gpt-4o-mini", true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			bodies := make(chan map[string]any, 3)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				bodies <- body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
			}))
			defer srv.Close()

			prov := NewOpenAIProvider(OpenAIConfig{
				APIKey:  "test-key",
				Model:   tc.model,
				BaseURL: srv.URL,
				HTTP:    srv.Client(),
			})
			ctx := context.Background()

			if _, err := prov.RequestClassification(ctx, "system", "user"); err != nil {
				t.Fatalf("RequestClassification: %v", err)
			}
			classificationBody := <-bodies
			if _, ok := classificationBody["temperature"]; ok != tc.wantTemperature {
				t.Errorf("RequestClassification temperature present = %v, want %v", ok, tc.wantTemperature)
			}

			if _, err := prov.RequestTextChat(ctx, "system", "user"); err != nil {
				t.Fatalf("RequestTextChat: %v", err)
			}
			chatBody := <-bodies
			if _, ok := chatBody["temperature"]; ok != tc.wantTemperature {
				t.Errorf("RequestTextChat temperature present = %v, want %v", ok, tc.wantTemperature)
			}

			if _, err := prov.Test(ctx); err != nil {
				t.Fatalf("Test: %v", err)
			}
			testBody := <-bodies
			if _, ok := testBody["max_tokens"]; ok != tc.wantMaxTokens {
				t.Errorf("Test max_tokens present = %v, want %v", ok, tc.wantMaxTokens)
			}
			if _, ok := testBody["max_completion_tokens"]; ok != tc.wantMaxCompletion {
				t.Errorf("Test max_completion_tokens present = %v, want %v", ok, tc.wantMaxCompletion)
			}
		})
	}
}
