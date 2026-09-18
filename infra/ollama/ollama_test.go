package ollama

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// capturedOllama records the wire-level requests the client sent, so tests can assert the exact
// bytes/fields the TypeScript ollama SDK's fetch would have sent. The expected bodies were captured
// by running the real `ollama@0.5.18` SDK against a local capture server.
type capturedOllama struct {
	mu   sync.Mutex
	reqs []ollamaRequest
}

type ollamaRequest struct {
	Method      string
	Path        string
	ContentType string
	Body        string
}

func (c *capturedOllama) record(r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	c.mu.Lock()
	c.reqs = append(c.reqs, ollamaRequest{
		Method:      r.Method,
		Path:        r.URL.Path,
		ContentType: r.Header.Get("Content-Type"),
		Body:        string(raw),
	})
	c.mu.Unlock()
}

func (c *capturedOllama) countPath(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.reqs {
		if r.Path == path {
			n++
		}
	}
	return n
}

func (c *capturedOllama) bodyFor(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.reqs {
		if r.Path == path {
			return r.Body
		}
	}
	return ""
}

func (c *capturedOllama) contentTypeFor(path string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.reqs {
		if r.Path == path {
			return r.ContentType
		}
	}
	return ""
}

func newTestClient(t *testing.T, baseURL string) *Client {
	t.Helper()
	resetModelHealthCache()
	return New(Config{
		BaseURL:    baseURL,
		Timeout:    5 * time.Second,
		Model:      "qwen3.5:9b",
		EmbedModel: "nomic-embed-text",
	})
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// TS: ollama-client.test.ts "always sends think:false (qwen3.5:9b routes JSON into .thinking
// otherwise)" + "returns the response and thinking fields from the Ollama result". The TS suite
// mocks the SDK; the Go port asserts the exact JSON body the SDK's fetch would send (captured from
// ollama@0.5.18).
func TestRequestClassificationCompletionSendsContractAndReturnsFields(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		writeJSON(w, http.StatusOK, `{"response":"{\"a\":1}","thinking":"some internal reasoning"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	got, err := c.RequestClassificationCompletion("system prompt", "user prompt")
	if err != nil {
		t.Fatalf("RequestClassificationCompletion returned error: %v", err)
	}
	if got.Response != `{"a":1}` {
		t.Errorf("Response = %q, want %q", got.Response, `{"a":1}`)
	}
	if got.Thinking != "some internal reasoning" {
		t.Errorf("Thinking = %q, want %q", got.Thinking, "some internal reasoning")
	}

	want := `{"model":"qwen3.5:9b","system":"system prompt","prompt":"user prompt","format":"json","think":false,"options":{"temperature":0.1,"num_ctx":16384,"num_predict":4096},"stream":false}`
	if body := cap.bodyFor("/api/generate"); body != want {
		t.Errorf("request body =\n  %s\nwant\n  %s", body, want)
	}
	if ct := cap.contentTypeFor("/api/generate"); !strings.Contains(ct, "application/json") {
		t.Errorf("content-type = %q, want application/json", ct)
	}
}

// TS: ollama-client.test.ts "returns ok:true when the model successfully generates". Also pins the
// minimal 1-token health-check body.
func TestCheckModelCanGenerateSendsMinimalGenerateBody(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		writeJSON(w, http.StatusOK, `{"response":"ok"}`)
	}))
	defer srv.Close()

	health := newTestClient(t, srv.URL).CheckModelCanGenerate("qwen3.5:9b", false)
	if !health.OK {
		t.Fatalf("health.OK = false, want true (error: %q)", health.Error)
	}
	want := `{"model":"qwen3.5:9b","prompt":"test","options":{"num_predict":1},"stream":false}`
	if body := cap.bodyFor("/api/generate"); body != want {
		t.Errorf("request body =\n  %s\nwant\n  %s", body, want)
	}
}

// TS: ollama-client.test.ts "returns ok:false with the error message when generate rejects".
func TestCheckModelCanGenerateReturnsReachableModelError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"error":"model is subscription-gated"}`)
	}))
	defer srv.Close()

	health := newTestClient(t, srv.URL).CheckModelCanGenerate("some-cloud-model", false)
	if health.OK {
		t.Fatal("health.OK = true, want false")
	}
	if health.Error != "model is subscription-gated" {
		t.Errorf("health.Error = %q, want %q", health.Error, "model is subscription-gated")
	}
}

// TS: ollama-client.test.ts "caches a passing result and does not call generate() again within the TTL".
func TestCheckModelCanGenerateCachesWithinTTL(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		writeJSON(w, http.StatusOK, `{"response":"ok"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	if !c.CheckModelCanGenerate("qwen3.5:9b", false).OK {
		t.Fatal("first check failed")
	}
	if !c.CheckModelCanGenerate("qwen3.5:9b", false).OK {
		t.Fatal("second check failed")
	}
	if n := cap.countPath("/api/generate"); n != 1 {
		t.Errorf("generate called %d times, want 1 (5-minute cache)", n)
	}
}

// TS: ollama-client.test.ts "bypasses the cache when forceRefresh is true".
func TestCheckModelCanGenerateForceRefreshBypassesCache(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		writeJSON(w, http.StatusOK, `{"response":"ok"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_ = c.CheckModelCanGenerate("qwen3.5:9b", false)
	_ = c.CheckModelCanGenerate("qwen3.5:9b", true)
	if n := cap.countPath("/api/generate"); n != 2 {
		t.Errorf("generate called %d times, want 2", n)
	}
}

// TS: ollama-client.test.ts "re-checks when the model name differs from the cached entry".
func TestCheckModelCanGenerateRechecksDifferentModel(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		writeJSON(w, http.StatusOK, `{"response":"ok"}`)
	}))
	defer srv.Close()

	c := newTestClient(t, srv.URL)
	_ = c.CheckModelCanGenerate("qwen3.5:9b", false)
	_ = c.CheckModelCanGenerate("a-different-model", false)
	if n := cap.countPath("/api/generate"); n != 2 {
		t.Errorf("generate called %d times, want 2", n)
	}
}

// TS: ollama-client.test.ts "returns true when the model already exists locally and can generate".
func TestEnsureOllamaModelReturnsTrueWhenModelExists(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		switch r.URL.Path {
		case "/api/tags":
			writeJSON(w, http.StatusOK, `{"models":[{"name":"qwen3.5:9b"}]}`)
		case "/api/generate":
			writeJSON(w, http.StatusOK, `{"response":"ok"}`)
		default:
			writeJSON(w, http.StatusNotFound, `{"error":"not found"}`)
		}
	}))
	defer srv.Close()

	if !newTestClient(t, srv.URL).EnsureOllamaModel("qwen3.5:9b") {
		t.Fatal("EnsureOllamaModel = false, want true")
	}
	if n := cap.countPath("/api/pull"); n != 0 {
		t.Errorf("pull called %d times, want 0", n)
	}
}

// TS: ollama-client.test.ts "pulls the model when it is not found locally". Also pins the pull body
// (`{"name":...,"stream":false}` — the SDK maps model->name and defaults stream to false).
func TestEnsureOllamaModelPullsWhenMissing(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		switch r.URL.Path {
		case "/api/tags":
			writeJSON(w, http.StatusOK, `{"models":[]}`)
		case "/api/pull":
			writeJSON(w, http.StatusOK, `{"status":"success"}`)
		case "/api/generate":
			writeJSON(w, http.StatusOK, `{"response":"ok"}`)
		default:
			writeJSON(w, http.StatusNotFound, `{"error":"not found"}`)
		}
	}))
	defer srv.Close()

	if !newTestClient(t, srv.URL).EnsureOllamaModel("qwen3.5:9b") {
		t.Fatal("EnsureOllamaModel = false, want true")
	}
	if n := cap.countPath("/api/pull"); n != 1 {
		t.Fatalf("pull called %d times, want 1", n)
	}
	want := `{"name":"qwen3.5:9b","stream":false}`
	if body := cap.bodyFor("/api/pull"); body != want {
		t.Errorf("pull body = %s, want %s", body, want)
	}
}

// TS: ollama-client.test.ts "returns false when the model exists but cannot generate (subscription-gated)".
func TestEnsureOllamaModelReturnsFalseWhenCannotGenerate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			writeJSON(w, http.StatusOK, `{"models":[{"name":"qwen3.5:9b"}]}`)
		case "/api/generate":
			writeJSON(w, http.StatusInternalServerError, `{"error":"gated"}`)
		}
	}))
	defer srv.Close()

	if newTestClient(t, srv.URL).EnsureOllamaModel("qwen3.5:9b") {
		t.Fatal("EnsureOllamaModel = true, want false")
	}
}

// TS: ollama-client.test.ts "attempts to auto-spawn 'ollama serve' when list() fails, then retries".
// The spawn is injected so the test never launches a process; the 2s post-spawn wait is injected too.
func TestEnsureOllamaModelAutoSpawnsWhenListFails(t *testing.T) {
	var tagsCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			if atomic.AddInt32(&tagsCalls, 1) == 1 {
				writeJSON(w, http.StatusInternalServerError, `{"error":"ECONNREFUSED"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"models":[{"name":"qwen3.5:9b"}]}`)
		case "/api/generate":
			writeJSON(w, http.StatusOK, `{"response":"ok"}`)
		}
	}))
	defer srv.Close()

	var spawned bool
	c := New(Config{
		BaseURL:    srv.URL,
		Timeout:    5 * time.Second,
		Model:      "qwen3.5:9b",
		EmbedModel: "nomic-embed-text",
		SpawnServe: func() error { spawned = true; return nil },
		Sleep:      func(time.Duration) {},
	})
	if !c.EnsureOllamaModel("qwen3.5:9b") {
		t.Fatal("EnsureOllamaModel = false, want true")
	}
	if !spawned {
		t.Error("spawn function was not called")
	}
	if n := atomic.LoadInt32(&tagsCalls); n != 2 {
		t.Errorf("tags called %d times, want 2 (initial + retry)", n)
	}
}

// TS: ollama-client.test.ts "returns false when auto-spawn retry still cannot find the model".
func TestEnsureOllamaModelReturnsFalseWhenSpawnRetryMissing(t *testing.T) {
	var tagsCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			if atomic.AddInt32(&tagsCalls, 1) == 1 {
				writeJSON(w, http.StatusInternalServerError, `{"error":"ECONNREFUSED"}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"models":[]}`)
		case "/api/generate":
			writeJSON(w, http.StatusOK, `{"response":"ok"}`)
		}
	}))
	defer srv.Close()

	var spawned bool
	c := New(Config{
		BaseURL:    srv.URL,
		Timeout:    5 * time.Second,
		Model:      "qwen3.5:9b",
		SpawnServe: func() error { spawned = true; return nil },
		Sleep:      func(time.Duration) {},
	})
	if c.EnsureOllamaModel("qwen3.5:9b") {
		t.Fatal("EnsureOllamaModel = true, want false")
	}
	if !spawned {
		t.Error("spawn function was not called")
	}
}

// Added case (no TS test): requestTextChatCompletion returns done_reason so the Step C caller can
// detect `done_reason == "length"` truncation (classify-document.ts:175), and sends temperature 0.2
// with no format constraint.
func TestRequestTextChatCompletionSendsContractAndReturnsDoneReason(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		writeJSON(w, http.StatusOK, `{"response":"markdown","thinking":"t","done_reason":"length"}`)
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv.URL).RequestTextChatCompletion("sys", "usr")
	if err != nil {
		t.Fatalf("RequestTextChatCompletion returned error: %v", err)
	}
	if got.Response != "markdown" || got.Thinking != "t" {
		t.Errorf("got %+v, want response=markdown thinking=t", got)
	}
	if got.DoneReason != "length" {
		t.Errorf("DoneReason = %q, want %q", got.DoneReason, "length")
	}
	want := `{"model":"qwen3.5:9b","system":"sys","prompt":"usr","think":false,"options":{"temperature":0.2,"num_ctx":16384,"num_predict":4096},"stream":false}`
	if body := cap.bodyFor("/api/generate"); body != want {
		t.Errorf("request body =\n  %s\nwant\n  %s", body, want)
	}
}

// Added case (no TS test): a missing response field becomes "" (`result.response || ”`).
func TestRequestTextChatCompletionDefaultsEmptyResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{}`)
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv.URL).RequestTextChatCompletion("sys", "usr")
	if err != nil {
		t.Fatalf("RequestTextChatCompletion returned error: %v", err)
	}
	if got.Response != "" || got.Thinking != "" || got.DoneReason != "" {
		t.Errorf("got %+v, want the zero value", got)
	}
}

// TS: ollama-client.test.ts "returns the embedding vector on success". Also pins the /api/embeddings
// body (the SDK `embeddings()` method, not `embed()`).
func TestGenerateEmbeddingSendsContractAndReturnsVector(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		writeJSON(w, http.StatusOK, `{"embedding":[0.1,0.2,0.3]}`)
	}))
	defer srv.Close()

	got := newTestClient(t, srv.URL).GenerateEmbedding("some text to embed")
	if !reflect.DeepEqual(got, []float64{0.1, 0.2, 0.3}) {
		t.Errorf("embedding = %#v, want [0.1 0.2 0.3]", got)
	}
	want := `{"model":"nomic-embed-text","prompt":"some text to embed"}`
	if body := cap.bodyFor("/api/embeddings"); body != want {
		t.Errorf("request body = %s, want %s", body, want)
	}
}

// TS: ollama-client.test.ts "truncates the prompt to 1000 characters".
func TestGenerateEmbeddingTruncatesPromptTo1000(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		writeJSON(w, http.StatusOK, `{"embedding":[]}`)
	}))
	defer srv.Close()

	_ = newTestClient(t, srv.URL).GenerateEmbedding(strings.Repeat("x", 2000))
	want := `{"model":"nomic-embed-text","prompt":"` + strings.Repeat("x", 1000) + `"}`
	if body := cap.bodyFor("/api/embeddings"); body != want {
		t.Errorf("prompt was not truncated to 1000 chars; body length %d", len(body))
	}
}

// TS: ollama-client.test.ts "returns an empty array instead of throwing when the embeddings call fails".
func TestGenerateEmbeddingReturnsEmptyOnFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"error":"embed model not found"}`)
	}))
	defer srv.Close()

	got := newTestClient(t, srv.URL).GenerateEmbedding("text")
	if len(got) != 0 {
		t.Errorf("embedding = %#v, want empty", got)
	}
}

// Added case (load-bearing per the migration design §8): a connection-level failure becomes an
// *OllamaUnavailableError so callers never fall back to the rule-based classifier.
func TestRequestClassificationCompletionWrapsConnectionFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := New(Config{BaseURL: url, Timeout: 2 * time.Second, Model: "qwen3.5:9b"})
	_, err := c.RequestClassificationCompletion("system", "user")
	if err == nil {
		t.Fatal("expected an error")
	}
	var down *OllamaUnavailableError
	if !errors.As(err, &down) {
		t.Fatalf("error type = %T, want *OllamaUnavailableError: %v", err, err)
	}
	if !strings.Contains(err.Error(), "Ollama is down") ||
		!strings.Contains(err.Error(), "qwen3.5:9b") ||
		!strings.Contains(err.Error(), url) {
		t.Errorf("OllamaUnavailableError message = %q, want model and host context", err.Error())
	}
}

// Added case: a reachable model that returns an application error is NOT an Ollama-down error, so
// the original error is rethrown unchanged (the "bad JSON vs down" distinction).
func TestRequestClassificationCompletionDoesNotWrapReachableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"error":"model is subscription-gated"}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).RequestClassificationCompletion("system", "user")
	if err == nil {
		t.Fatal("expected an error")
	}
	var down *OllamaUnavailableError
	if errors.As(err, &down) {
		t.Fatalf("reachable-model error was classified as Ollama-down: %v", err)
	}
	if err.Error() != "model is subscription-gated" {
		t.Errorf("err = %q, want %q", err.Error(), "model is subscription-gated")
	}
}

// Added case: an HTTP client timeout is a transport failure and must be classified as down.
func TestRequestClassificationCompletionTimeoutIsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		writeJSON(w, http.StatusOK, `{"response":"late"}`)
	}))
	defer srv.Close()

	c := New(Config{BaseURL: srv.URL, Timeout: 50 * time.Millisecond, Model: "qwen3.5:9b"})
	_, err := c.RequestClassificationCompletion("system", "user")
	var down *OllamaUnavailableError
	if !errors.As(err, &down) {
		t.Fatalf("error = %v (%T), want *OllamaUnavailableError", err, err)
	}
}

// Added case: the down-pattern classifier is a literal port of ollama-client.ts:20, extended with
// Go-native transport checks because Go error strings differ from Node's.
func TestIsOllamaDownError(t *testing.T) {
	down := []string{
		"fetch failed",
		"connect ECONNREFUSED 127.0.0.1:11434",
		"read ECONNRESET",
		"connect ETIMEDOUT",
		"getaddrinfo EAI_AGAIN",
		"getaddrinfo ENOTFOUND localhost",
		"socket hang up",
		"network is unreachable",
		"connect: connection refused",
		"server is offline",
	}
	for _, msg := range down {
		if !isOllamaDownError(errors.New(msg)) {
			t.Errorf("isOllamaDownError(%q) = false, want true", msg)
		}
	}
	notDown := []string{
		"model is subscription-gated",
		"unexpected end of JSON input",
		"invalid character 'x' looking for beginning of value",
	}
	for _, msg := range notDown {
		if isOllamaDownError(errors.New(msg)) {
			t.Errorf("isOllamaDownError(%q) = true, want false", msg)
		}
	}
}

// Added case (no TS test): ListModels returns the model names from GET /api/tags in order, using an
// explicit host that differs from the client's configured base URL.
func TestListModelsReturnsNamesInOrder(t *testing.T) {
	cap := &capturedOllama{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.record(r)
		writeJSON(w, http.StatusOK, `{"models":[{"name":"qwen3.5:9b"},{"name":"nomic-embed-text"},{"name":"llama3:latest"}]}`)
	}))
	defer srv.Close()

	got, err := newTestClient(t, "http://127.0.0.1:1").ListModels(srv.URL)
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	want := []string{"qwen3.5:9b", "nomic-embed-text", "llama3:latest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ListModels = %#v, want %#v", got, want)
	}
	if n := cap.countPath("/api/tags"); n != 1 {
		t.Errorf("tags called %d times, want 1", n)
	}
}

// Added case (no TS test): an empty host targets the client's configured base URL (the TS
// `new Ollama({ host: host || CONFIG.OLLAMA_HOST })` fallback).
func TestListModelsEmptyHostUsesConfiguredBaseURL(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tags" {
			writeJSON(w, http.StatusNotFound, `{"error":"not found"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"models":[{"name":"configured-model"}]}`)
	}))
	defer srv.Close()

	got, err := newTestClient(t, srv.URL).ListModels("")
	if err != nil {
		t.Fatalf("ListModels returned error: %v", err)
	}
	if want := []string{"configured-model"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ListModels = %#v, want %#v", got, want)
	}
}

// Added case (no TS test): a reachable non-2xx response is returned unchanged, exactly like the
// generate wrappers' "reachable model rejected the request" path.
func TestListModelsReturnsErrorOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, `{"error":"boom"}`)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv.URL).ListModels("")
	if err == nil {
		t.Fatal("expected an error")
	}
	var down *OllamaUnavailableError
	if errors.As(err, &down) {
		t.Fatalf("reachable non-2xx error was classified as Ollama-down: %v", err)
	}
	if err.Error() != "boom" {
		t.Errorf("err = %q, want %q", err.Error(), "boom")
	}
}

// Added case (no TS test): a connection-level failure becomes an *OllamaUnavailableError, the same
// classification the generate wrappers use.
func TestListModelsWrapsConnectionFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := New(Config{BaseURL: url, Timeout: 2 * time.Second, Model: "qwen3.5:9b"})
	_, err := c.ListModels("")
	if err == nil {
		t.Fatal("expected an error")
	}
	var down *OllamaUnavailableError
	if !errors.As(err, &down) {
		t.Fatalf("error type = %T, want *OllamaUnavailableError: %v", err, err)
	}
}
