package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/aiprovider"
)

// echoDeepSeekUpstream is an in-process DeepSeek chat/completions server that answers with the model
// id `echo` selects from the requested model. An empty echo omits the "model" field entirely, which
// mimics a provider that does not report the served model. It counts upstream hits.
func echoDeepSeekUpstream(t *testing.T, echo func(requested string) string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var req struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body := map[string]any{
			"choices": []map[string]any{{
				"message":       map[string]any{"role": "assistant", "content": "OK"},
				"finish_reason": "stop",
			}},
		}
		if served := echo(req.Model); served != "" {
			body["model"] = served
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// cloudManagerEnv builds a testEnv whose Ollama dep is a real *aiprovider.Manager pointed at the
// in-process DeepSeek upstream, so the status handler exercises the full confirmation path and the
// optional LastClassification assertion.
func cloudManagerEnv(t *testing.T, upstreamURL, requested string) (*testEnv, *aiprovider.Manager) {
	t.Helper()
	env := newTestEnv()
	env.settings.cfg.AIProvider = "cloud"
	env.settings.cfg.CloudProvider = "deepseek"
	env.settings.cfg.DeepSeekAPIKey = "test-key"
	env.settings.cfg.DeepSeekModel = requested
	env.settings.cfg.DeepSeekBaseURL = upstreamURL

	mgr := aiprovider.NewManager(aiprovider.Config{
		AIProvider:      "cloud",
		CloudProvider:   "deepseek",
		DeepSeekAPIKey:  "test-key",
		DeepSeekModel:   requested,
		DeepSeekBaseURL: upstreamURL,
	}, nil)

	deps := testDeps(env)
	deps.Ollama = mgr
	env.handler = NewServer(deps)
	return env, mgr
}

// TestCloudStatusReportsConfirmedModel: the upstream echoes the requested model, so the status
// reports model_verified=true with the confirmed id.
func TestCloudStatusReportsConfirmedModel(t *testing.T) {
	const requested = "deepseek-chat"
	srv, _ := echoDeepSeekUpstream(t, func(r string) string { return r })
	env, _ := cloudManagerEnv(t, srv.URL, requested)

	body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/ollama/status", nil, nil))
	if body["model"] != requested {
		t.Fatalf("model = %v, want %q", body["model"], requested)
	}
	if body["model_confirmed"] != requested {
		t.Fatalf("model_confirmed = %v, want %q", body["model_confirmed"], requested)
	}
	if body["model_verified"] != true {
		t.Fatalf("model_verified = %v, want true", body["model_verified"])
	}
	if _, ok := body["last_classification_model"]; ok {
		t.Fatalf("last_classification_model present before any classification: %v", body)
	}
	if _, ok := body["last_classification_at"]; ok {
		t.Fatalf("last_classification_at present before any classification: %v", body)
	}
}

// TestCloudStatusReportsEchoNotConfig: the upstream echoes a DIFFERENT model, so model_confirmed is
// that echoed value and model_verified is false. The echoed value is not the configured one.
func TestCloudStatusReportsEchoNotConfig(t *testing.T) {
	const requested = "deepseek-chat"
	const echoed = "deepseek-flash"
	srv, _ := echoDeepSeekUpstream(t, func(string) string { return echoed })
	env, _ := cloudManagerEnv(t, srv.URL, requested)

	body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/ollama/status", nil, nil))
	if body["model"] != requested {
		t.Fatalf("model = %v, want %q", body["model"], requested)
	}
	if body["model_confirmed"] != echoed {
		t.Fatalf("model_confirmed = %v, want the echoed %q", body["model_confirmed"], echoed)
	}
	if body["model_confirmed"] == requested {
		t.Fatalf("model_confirmed was copied from config (%q), not the upstream echo", requested)
	}
	if body["model_verified"] != false {
		t.Fatalf("model_verified = %v, want false", body["model_verified"])
	}
}

// TestCloudStatusOmitsUnverifiedEcho: the upstream omits the model field, so model_confirmed is ""
// and model_verified is false — a missing echo is never treated as verified.
func TestCloudStatusOmitsUnverifiedEcho(t *testing.T) {
	srv, _ := echoDeepSeekUpstream(t, func(string) string { return "" })
	env, _ := cloudManagerEnv(t, srv.URL, "deepseek-chat")

	body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/ollama/status", nil, nil))
	if body["model_confirmed"] != "" {
		t.Fatalf("model_confirmed = %v, want empty", body["model_confirmed"])
	}
	if body["model_verified"] != false {
		t.Fatalf("model_verified = %v, want false", body["model_verified"])
	}
}

// TestCloudStatusRefreshBypassesCache: two plain status reads hit the upstream once (60 s cache); a
// ?refresh=1 read forces a fresh probe.
func TestCloudStatusRefreshBypassesCache(t *testing.T) {
	srv, calls := echoDeepSeekUpstream(t, func(r string) string { return r })
	env, _ := cloudManagerEnv(t, srv.URL, "deepseek-chat")

	_ = doJSON(t, env.handler, http.MethodGet, "/api/ollama/status", nil, nil)
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("upstream calls after first status = %d, want 1", got)
	}
	_ = doJSON(t, env.handler, http.MethodGet, "/api/ollama/status", nil, nil)
	if got := atomic.LoadInt32(calls); got != 1 {
		t.Fatalf("upstream calls within cache TTL = %d, want 1", got)
	}
	_ = doJSON(t, env.handler, http.MethodGet, "/api/ollama/status?refresh=1", nil, nil)
	if got := atomic.LoadInt32(calls); got != 2 {
		t.Fatalf("upstream calls after ?refresh=1 = %d, want 2 (cache not bypassed)", got)
	}
}

// TestCloudStatusLastClassificationKeysAfterSuccess: the keys are absent until a classification
// succeeds, then carry the echoed model and an RFC3339 timestamp.
func TestCloudStatusLastClassificationKeysAfterSuccess(t *testing.T) {
	srv, _ := echoDeepSeekUpstream(t, func(r string) string { return r })
	env, mgr := cloudManagerEnv(t, srv.URL, "deepseek-chat")

	if _, err := mgr.RequestClassificationCompletion("sys", "usr"); err != nil {
		t.Fatalf("RequestClassificationCompletion: %v", err)
	}

	body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/ollama/status", nil, nil))
	if body["last_classification_model"] != "deepseek-chat" {
		t.Fatalf("last_classification_model = %v, want deepseek-chat", body["last_classification_model"])
	}
	at, ok := body["last_classification_at"].(string)
	if !ok {
		t.Fatalf("last_classification_at = %v (%T), want an RFC3339 string", body["last_classification_at"], body["last_classification_at"])
	}
	if _, err := time.Parse(time.RFC3339, at); err != nil {
		t.Fatalf("last_classification_at = %q, not RFC3339: %v", at, err)
	}
}

// TestAITestCloudReportsModelConfirmation covers POST /api/ai/test for the three upstream echo
// behaviours.
func TestAITestCloudReportsModelConfirmation(t *testing.T) {
	tests := []struct {
		name          string
		echo          func(string) string
		wantConfirmed string
		wantVerified  bool
	}{
		{"echoes requested", func(r string) string { return r }, "deepseek-chat", true},
		{"echoes different", func(string) string { return "deepseek-flash" }, "deepseek-flash", false},
		{"omits echo", func(string) string { return "" }, "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := echoDeepSeekUpstream(t, tc.echo)
			env := newTestEnv()
			rec := doJSON(t, env.handler, http.MethodPost, "/api/ai/test", map[string]string{
				"provider": "deepseek", "api_key": "test-key",
				"model": "deepseek-chat", "base_url": srv.URL,
			}, nil)
			body := decodeJSON(t, rec)
			if rec.Code != 200 || body["ok"] != true {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if body["model_requested"] != "deepseek-chat" {
				t.Fatalf("model_requested = %v, want deepseek-chat", body["model_requested"])
			}
			if body["model_confirmed"] != tc.wantConfirmed {
				t.Fatalf("model_confirmed = %v, want %q", body["model_confirmed"], tc.wantConfirmed)
			}
			if body["model_verified"] != tc.wantVerified {
				t.Fatalf("model_verified = %v, want %v", body["model_verified"], tc.wantVerified)
			}
		})
	}
}

// TestAITestGoogleUsesModelVersionEcho pins the second echo field (modelVersion) through the HTTP
// test endpoint, not just the provider unit test.
func TestAITestGoogleUsesModelVersionEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"modelVersion":"gemini-2.5-flash","candidates":[{"content":{"parts":[{"text":"OK"}]},"finishReason":"STOP"}]}`))
	}))
	t.Cleanup(srv.Close)

	env := newTestEnv()
	rec := doJSON(t, env.handler, http.MethodPost, "/api/ai/test", map[string]string{
		"provider": "google", "api_key": "test-key",
		"model": "gemini-2.5-flash", "base_url": srv.URL,
	}, nil)
	body := decodeJSON(t, rec)
	if rec.Code != 200 || body["ok"] != true {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if body["model_confirmed"] != "gemini-2.5-flash" {
		t.Fatalf("model_confirmed = %v, want the modelVersion echo", body["model_confirmed"])
	}
	if body["model_verified"] != true {
		t.Fatalf("model_verified = %v, want true", body["model_verified"])
	}
}
