// Package ollama is a Go port of pdf-triage's src/infrastructure/ollama-client.ts (171 lines):
// OllamaUnavailableError, the connection-failure classifier, checkModelCanGenerate and its 5-minute
// cache, ensureOllamaModel (auto-pull + auto-spawn of `ollama serve`), the two generate wrappers,
// and generateEmbedding.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/ollama-client.test.ts` -> 15 passed), so no upstream case is
// pinned red. All 15 cases are ported. The TS suite mocks the `ollama` npm SDK; the Go port instead
// drives net/http/httptest and asserts the exact JSON request bodies the SDK's fetch emits —
// captured by running `ollama@0.5.18` against a local capture server:
//
//	generate (classification) {"model":...,"system":...,"prompt":...,"format":"json","think":false,
//	                          "options":{"temperature":0.1,"num_ctx":16384,"num_predict":4096},"stream":false}
//	generate (health check)   {"model":...,"prompt":"test","options":{"num_predict":1},"stream":false}
//	generate (text chat)      {"model":...,"system":...,"prompt":...,"think":false,
//	                          "options":{"temperature":0.2,"num_ctx":16384,"num_predict":4096},"stream":false}
//	tags     GET /api/tags    (empty body)
//	pull                      {"name":...,"stream":false}   (the SDK maps model->name)
//	embeddings                {"model":...,"prompt":...}    (`embeddings()`, i.e. /api/embeddings)
//
// Added cases: requestTextChatCompletion returning done_reason (no TS case, but load-bearing for the
// `done_reason == 'length'` truncation check at classify-document.ts:175), connection-failure and
// timeout wrapping as OllamaUnavailableError, the reachable-vs-down distinction, and the classifier
// table.
//
// The migration design §8 pins this contract: format json, think false, num_ctx 16384,
// num_predict 4096, temperature 0.1 for classification (0.2 for text chat), the done_reason check,
// and the "Ollama down" vs "reachable model rejected the request" split.
//
// Preserved verbatim from the TS source (ollama-client.ts:4-10):
//
//	Thrown when Ollama itself is unreachable or the model cannot generate — the "Ollama is down"
//	case. Callers must NEVER silently fall back to the rule-based classifier on this error: that
//	fallback is exactly what misfiled a SEPA mandate and two tax notices on 2026-08-31 when the
//	capability check failed. Triage stops, the file stays in `__raws`, and the user is reminded
//	to start Ollama (see runTriageScan's scan-level gate).
//
// Preserved verbatim from the TS source (ollama-client.ts:18-19):
//
//	Connection-level failure signatures from the ollama SDK / Node fetch — everything that means
//	"Ollama cannot be reached at all" rather than "the model answered something odd".
//
// Preserved verbatim from the TS source (ollama-client.ts:44-47):
//
//	A model can pass the "exists locally" check (ollama.list()) yet still be unable to
//	generate — e.g. a cloud/subscription-gated model that's listed but rejects requests
//	at generate-time. This does a cheap 1-token generation to catch that proactively,
//	cached briefly so it isn't repeated on every single document classification.
//
// Preserved verbatim from the TS source (ollama-client.ts:99-102):
//
//	Thin wrapper around the raw classification generate() call — think:false is required
//	here: qwen3.5:9b is a thinking-capable model that otherwise routes its whole JSON
//	answer into response.thinking and leaves response.response empty (see the regression
//	test in src/application/classify-document.test.ts).
//
// Preserved verbatim from the TS source (ollama-client.ts:114-120):
//
//	// 16384, not 8192. The system prompt alone is ~6.6k tokens (decision flow + the archive's
//	// taxonomy) and the document text adds ~1.2k, so at 8192 there was almost no room left to
//	// answer: the response was cut off mid-sentence, repairTruncatedJSON silently closed the
//	// JSON, and every field after `tags` in the schema — total_amount, vat_amount, siren, iban,
//	// expiry_date and all five contact_* — came back empty. Measured on a real invoice: at 8192
//	// the reply was unparseable; at 16384 the same prompt returned 18 keys with 7 of those 10
//	// fields populated. Costs more VRAM; that is the trade for the data actually arriving.
//
// Preserved verbatim from the TS source (ollama-client.ts:133-137):
//
//	`doneReason` is surfaced deliberately. Ollama reports 'length' when generation stopped because it
//	hit num_predict rather than because the model finished — a truncated answer that is otherwise
//	indistinguishable from a complete one. Step C discarded it, so an over-long chunk came back
//	cut off mid-document, passed the `length > 10` success gate as "converted", and silently replaced
//	the chunk's real content. Callers that care about losslessness must check it.
//
// Deviations, each resolved in favor of matching the TS acceptance bar:
//
//  1. Host/model config. The TS functions read CONFIG.OLLAMA_HOST / CONFIG.OLLAMA_MODEL /
//     CONFIG.OLLAMA_EMBED_MODEL directly; the settings port is a later phase, so this package takes
//     them as an explicit Config. The defaults mirror the TS config values.
//  2. The 5-minute health cache stays a package-level cache keyed on the model name only, exactly as
//     the TS module-level `modelHealthCache` was (host is not part of the key in TS either). A
//     package-level cache needs synchronization in Go, so access is guarded by a mutex.
//  3. The down-pattern classifier ports the regex literally and additionally recognizes Go-native
//     transport failures (syscall ECONNREFUSED/ECONNRESET/ETIMEDOUT/EHOSTUNREACH/ENETUNREACH,
//     context.DeadlineExceeded, *net.DNSError and net.Error timeouts), because Go's error strings
//     differ from Node fetch's ("connection refused" already matches `connect.*refused`, but
//     "no such host" does not match `getaddrinfo`/`EAI_AGAIN`).
//  4. Non-2xx handling mirrors the ollama SDK's checkOk: with an application/json content type the
//     `error` field is used as the message (which is what makes the ported
//     "model is subscription-gated" case assert the same string), otherwise the body text, otherwise
//     `Error <status>: <statusText>`.
//  5. `console.log`/`console.warn` become an optional Logf hook; nil is silent (the port must not
//     write to stdout in tests). The messages are preserved.
//  6. `exec('ollama serve', { windowsHide: true })` becomes the injectable SpawnServe hook, whose
//     default starts `ollama serve` via os/exec and hides the window on Windows (see
//     spawn_windows.go). Tests inject a no-op so they never launch a process. The 2-second
//     post-spawn wait is also injectable for tests.
//  7. `text.substring(0, 1000)` counts UTF-16 code units; Go truncates by runes here. Identical for
//     all BMP text (the only upstream case uses ASCII); a lone surrogate cannot be represented in a
//     Go string anyway.
//  8. The HTTP timeout is `http.Client.Timeout`, the Go equivalent of the TS fetch having no explicit
//     deadline when timeout <= 0.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Default config values mirror the TS CONFIG defaults (settings.ts:159-161).
const (
	DefaultHost       = "http://127.0.0.1:11434"
	DefaultModel      = "qwen3.5:9b"
	DefaultEmbedModel = "nomic-embed-text"
)

const modelHealthCacheTTL = 5 * time.Minute

// OllamaUnavailableError is the Go form of the TS `OllamaUnavailableError` class.
type OllamaUnavailableError struct {
	Message string
}

func (e *OllamaUnavailableError) Error() string { return e.Message }

// Config carries the connection parameters every client takes explicitly, plus the injectable
// process/wait/log seams used by tests.
type Config struct {
	BaseURL    string
	Timeout    time.Duration
	Model      string
	EmbedModel string

	// SpawnServe starts a local `ollama serve`; nil uses the os/exec default. Tests inject a no-op
	// so they never launch a process.
	SpawnServe func() error
	// Sleep waits after a spawn before retrying; nil uses time.Sleep. Tests inject a no-op.
	Sleep func(time.Duration)
	// Logf receives the TS console.log/console.warn lines; nil is silent.
	Logf func(format string, args ...any)
}

// Client is the stateful form of the TS module. The health cache itself is package-level (see the
// package comment), shared by every Client exactly as the TS module cache was.
type Client struct {
	cfg   Config
	http  *http.Client
	spawn func() error
	sleep func(time.Duration)
}

// New builds a Client, applying the TS CONFIG defaults for any empty field.
func New(cfg Config) *Client {
	if cfg.BaseURL == "" {
		cfg.BaseURL = DefaultHost
	}
	if cfg.Model == "" {
		cfg.Model = DefaultModel
	}
	if cfg.EmbedModel == "" {
		cfg.EmbedModel = DefaultEmbedModel
	}

	spawn := cfg.SpawnServe
	if spawn == nil {
		spawn = defaultSpawnOllamaServe
	}
	sleep := cfg.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}

	return &Client{
		cfg:   cfg,
		http:  &http.Client{Timeout: cfg.Timeout},
		spawn: spawn,
		sleep: sleep,
	}
}

func (c *Client) logf(format string, args ...any) {
	if c.cfg.Logf != nil {
		c.cfg.Logf(format, args...)
	}
}

func (c *Client) baseURL() string {
	return strings.TrimRight(c.cfg.BaseURL, "/")
}

// ModelHealth mirrors the TS “{ ok: boolean; error?: string }“ return of checkModelCanGenerate.
type ModelHealth struct {
	OK    bool
	Error string
}

// Completion mirrors the TS “{ response: string; thinking?: string }“.
type Completion struct {
	Response string
	Thinking string
}

// TextCompletion mirrors the TS “{ response: string; thinking?: string; doneReason?: string }“.
type TextCompletion struct {
	Response   string
	Thinking   string
	DoneReason string
}

type modelHealthCacheEntry struct {
	modelName   string
	checkedAt   time.Time
	canGenerate bool
	err         string
}

var (
	modelHealthMu    sync.Mutex
	modelHealthCache *modelHealthCacheEntry
)

// resetModelHealthCache clears the package-level cache. It exists for tests, which otherwise inherit
// an entry from a previous case; the TS suite reset it by re-importing the module.
func resetModelHealthCache() {
	modelHealthMu.Lock()
	modelHealthCache = nil
	modelHealthMu.Unlock()
}

// generateOptions preserves the exact SDK field order: temperature, num_ctx, num_predict. Pointers +
// omitempty reproduce the SDK dropping fields the caller did not set (the health check sends only
// num_predict).
type generateOptions struct {
	Temperature *float64 `json:"temperature,omitempty"`
	NumCtx      *int     `json:"num_ctx,omitempty"`
	NumPredict  *int     `json:"num_predict,omitempty"`
}

// generateRequest preserves the exact SDK field order: model, system, prompt, format, think, options,
// stream. `stream` is always present and false (the SDK defaults it); system/format/think are omitted
// when not set, matching the captured wire bodies.
type generateRequest struct {
	Model   string           `json:"model"`
	System  string           `json:"system,omitempty"`
	Prompt  string           `json:"prompt"`
	Format  string           `json:"format,omitempty"`
	Think   *bool            `json:"think,omitempty"`
	Options *generateOptions `json:"options,omitempty"`
	Stream  bool             `json:"stream"`
}

type generateResponse struct {
	Response   string `json:"response"`
	Thinking   string `json:"thinking"`
	DoneReason string `json:"done_reason"`
}

type pullRequest struct {
	Name   string `json:"name"`
	Stream bool   `json:"stream"`
}

type modelInfo struct {
	Name string `json:"name"`
}

type tagsResponse struct {
	Models []modelInfo `json:"models"`
}

type embedRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
}

type embedResponse struct {
	Embedding []float64 `json:"embedding"`
}

// CheckModelCanGenerate is `checkModelCanGenerate(modelName, host = CONFIG.OLLAMA_HOST,
// forceRefresh = false)`. It never returns an error: a failure is folded into ModelHealth, exactly
// as the TS function caught and cached it.
func (c *Client) CheckModelCanGenerate(modelName string, forceRefresh bool) ModelHealth {
	now := time.Now()
	modelHealthMu.Lock()
	if !forceRefresh && modelHealthCache != nil &&
		modelHealthCache.modelName == modelName &&
		now.Sub(modelHealthCache.checkedAt) < modelHealthCacheTTL {
		entry := *modelHealthCache
		modelHealthMu.Unlock()
		return ModelHealth{OK: entry.canGenerate, Error: entry.err}
	}
	modelHealthMu.Unlock()

	numPredict := 1
	req := generateRequest{
		Model:   modelName,
		Prompt:  "test",
		Options: &generateOptions{NumPredict: &numPredict},
		Stream:  false,
	}
	var resp generateResponse
	err := c.postJSON("/api/generate", req, &resp)

	entry := &modelHealthCacheEntry{modelName: modelName, checkedAt: now}
	if err != nil {
		entry.canGenerate = false
		entry.err = err.Error()
	} else {
		entry.canGenerate = true
	}
	modelHealthMu.Lock()
	modelHealthCache = entry
	modelHealthMu.Unlock()

	return ModelHealth{OK: entry.canGenerate, Error: entry.err}
}

// EnsureOllamaModel is `ensureOllamaModel(modelName = CONFIG.OLLAMA_MODEL)`.
func (c *Client) EnsureOllamaModel(modelName string) bool {
	if modelName == "" {
		modelName = c.cfg.Model
	}

	models, err := c.list()
	if err != nil {
		return c.autoSpawn(modelName, err)
	}

	if !modelNameExists(models, modelName) {
		c.logf("Model '%s' not found locally in Ollama. Pulling '%s'...", modelName, modelName)
		if pullErr := c.pull(modelName); pullErr != nil {
			return c.autoSpawn(modelName, pullErr)
		}
		c.logf("Model '%s' pulled successfully.", modelName)
	}

	health := c.CheckModelCanGenerate(modelName, false)
	if !health.OK {
		c.logf("Model '%s' exists locally but cannot generate (e.g. subscription-gated cloud model): %s", modelName, health.Error)
		return false
	}
	return true
}

// autoSpawn is the TS `catch` block of ensureOllamaModel: warn, start `ollama serve`, wait, retry the
// model list, and force a fresh health check.
func (c *Client) autoSpawn(modelName string, cause error) bool {
	c.logf("Ollama check/pull warning for model %s: %s", modelName, cause)
	c.logf("Attempting auto-spawn of local Ollama serve process...")
	if err := c.spawn(); err != nil {
		c.logf("Failed to auto-spawn Ollama: %s", err)
		return false
	}
	c.sleep(2 * time.Second)

	models, err := c.list()
	if err != nil {
		return false
	}
	if !modelNameExists(models, modelName) {
		return false
	}
	return c.CheckModelCanGenerate(modelName, true).OK
}

// RequestClassificationCompletion is `requestClassificationCompletion(system, user)`.
func (c *Client) RequestClassificationCompletion(system, user string) (Completion, error) {
	temperature := 0.1
	numCtx := 16384
	numPredict := 4096
	think := false
	req := generateRequest{
		Model:  c.cfg.Model,
		System: system,
		Prompt: user,
		Format: "json",
		Think:  &think,
		Options: &generateOptions{
			Temperature: &temperature,
			NumCtx:      &numCtx,
			NumPredict:  &numPredict,
		},
		Stream: false,
	}
	var resp generateResponse
	if err := c.postJSON("/api/generate", req, &resp); err != nil {
		return Completion{}, c.asOllamaUnavailable(err)
	}
	return Completion{Response: resp.Response, Thinking: resp.Thinking}, nil
}

// RequestTextChatCompletion is `requestTextChatCompletion(system, user)`.
//
// Preserved verbatim from the TS source (ollama-client.ts:131-132):
//
//	General text chat completion wrapper (without format: 'json') for Markdown Q&A responses.
//
// Preserved verbatim from the TS source (ollama-client.ts:148-149):
//
//	// Same reasoning as requestClassificationCompletion above — the chat assistant is fed
//	// document context that easily fills an 8k window before it can reply.
func (c *Client) RequestTextChatCompletion(system, user string) (TextCompletion, error) {
	temperature := 0.2
	numCtx := 16384
	numPredict := 4096
	think := false
	req := generateRequest{
		Model:  c.cfg.Model,
		System: system,
		Prompt: user,
		Think:  &think,
		Options: &generateOptions{
			Temperature: &temperature,
			NumCtx:      &numCtx,
			NumPredict:  &numPredict,
		},
		Stream: false,
	}
	var resp generateResponse
	if err := c.postJSON("/api/generate", req, &resp); err != nil {
		return TextCompletion{}, c.asOllamaUnavailable(err)
	}
	return TextCompletion{
		Response:   resp.Response,
		Thinking:   resp.Thinking,
		DoneReason: resp.DoneReason,
	}, nil
}

// GenerateEmbedding is `generateEmbedding(text)`. Failure returns an empty slice, never an error.
func (c *Client) GenerateEmbedding(text string) []float64 {
	runes := []rune(text)
	if len(runes) > 1000 {
		runes = runes[:1000]
	}
	var resp embedResponse
	if err := c.postJSON("/api/embeddings", embedRequest{Model: c.cfg.EmbedModel, Prompt: string(runes)}, &resp); err != nil {
		return []float64{}
	}
	if resp.Embedding == nil {
		return []float64{}
	}
	return resp.Embedding
}

// asOllamaUnavailable is `rethrowAsOllamaUnavailable`: only a down/transport error becomes an
// OllamaUnavailableError; a reachable model's response error is returned unchanged.
func (c *Client) asOllamaUnavailable(err error) error {
	if isOllamaDownError(err) {
		return &OllamaUnavailableError{Message: fmt.Sprintf(
			"Ollama is down — cannot reach %s at %s: %s. Start Ollama, then retry.",
			c.cfg.Model, c.baseURL(), err.Error(),
		)}
	}
	return err
}

// ListModels returns the model names from GET {host}/api/tags, in the order Ollama reports them.
// An empty host uses the client's configured base URL. It mirrors the TS web-server's
// `new Ollama({ host: host || CONFIG.OLLAMA_HOST }).list()` used by /api/ollama/models and
// /api/ollama/status (web-server.ts:184-225): a transport failure is classified as an
// OllamaUnavailableError exactly like the generate wrappers, while a reachable non-2xx error is
// returned unchanged.
func (c *Client) ListModels(host string) ([]string, error) {
	base := c.baseURL()
	if host != "" {
		base = strings.TrimRight(host, "/")
	}
	models, err := c.listFrom(base)
	if err != nil {
		return nil, c.asOllamaUnavailable(err)
	}
	names := make([]string, 0, len(models))
	for _, m := range models {
		names = append(names, m.Name)
	}
	return names, nil
}

func (c *Client) list() ([]modelInfo, error) {
	return c.listFrom(c.baseURL())
}

// listFrom is list() against an explicit base URL, so ListModels can target another host.
func (c *Client) listFrom(base string) ([]modelInfo, error) {
	var resp tagsResponse
	if err := c.getJSON(base, "/api/tags", &resp); err != nil {
		return nil, err
	}
	return resp.Models, nil
}

func (c *Client) pull(modelName string) error {
	return c.postJSON("/api/pull", pullRequest{Name: modelName, Stream: false}, nil)
}

func modelNameExists(models []modelInfo, modelName string) bool {
	for _, m := range models {
		// TS: m.name.startsWith(modelName) || m.name.includes(modelName). startsWith is subsumed by
		// includes, so the substring check alone is equivalent.
		if strings.Contains(m.Name, modelName) {
			return true
		}
	}
	return false
}

func (c *Client) postJSON(path string, payload any, out any) error {
	body, err := marshalNoHTMLEscape(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.baseURL()+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) getJSON(base, path string, out any) error {
	req, err := http.NewRequest(http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	return c.do(req, out)
}

func (c *Client) do(req *http.Request, out any) error {
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := responseError(resp); err != nil {
		return err
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// responseError mirrors the ollama SDK's checkOk: a non-2xx response's message is the JSON `error`
// field when the content type is application/json, else the body text, else the status line.
func responseError(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return nil
	}
	message := fmt.Sprintf("Error %d: %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	if strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
		var data struct {
			Error string `json:"error"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&data); err == nil && data.Error != "" {
			message = data.Error
		}
	} else if body, err := io.ReadAll(resp.Body); err == nil && len(body) > 0 {
		message = string(body)
	}
	return errors.New(message)
}

// marshalNoHTMLEscape matches JSON.stringify: no HTML escaping (Go escapes <, >, & by default) and
// no trailing newline.
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// ollamaDownPatterns is a literal port of the TS OLLAMA_DOWN_PATTERNS regex (ollama-client.ts:20).
var ollamaDownPatterns = regexp.MustCompile(`(?i)fetch failed|ECONNREFUSED|ECONNRESET|ETIMEDOUT|EAI_AGAIN|getaddrinfo|socket hang up|network.*unreachable|connect.*refused|server.*offline`)

// isOllamaDownError ports `isOllamaDownError` and additionally recognizes Go-native transport
// failures, whose error strings do not carry Node's errno names (see the package comment).
func isOllamaDownError(err error) bool {
	if err == nil {
		return false
	}
	if ollamaDownPatterns.MatchString(err.Error()) {
		return true
	}
	if errors.Is(err, syscall.ECONNREFUSED) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ETIMEDOUT) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}
