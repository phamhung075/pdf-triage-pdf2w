package aiprovider

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
)

// cloudHealthCacheTTL is how long a cloud health check is cached. CheckModelCanGenerate runs a real,
// billable provider call and the dashboard polls GET /api/ollama/status every 10 s per open tab, so
// without this cache every poll billed a provider call.
const cloudHealthCacheTTL = 60 * time.Second

// cloudHealthEntry is one cached cloud health result: the ok flag plus the exact error text that was
// reported (so a cached failure still carries its message). servedModel is the model id the upstream
// response echoed on a successful probe, or "" when the provider omitted it; it is never copied from
// configuration.
type cloudHealthEntry struct {
	ok          bool
	errText     string
	servedModel string
}

// ProviderType represents whether AI runs locally or in the cloud.
type ProviderType string

const (
	ProviderLocal ProviderType = "local"
	ProviderCloud ProviderType = "cloud"
)

// CloudService represents a specific cloud AI provider.
type CloudService string

const (
	CloudGoogle    CloudService = "google"
	CloudClaude    CloudService = "claude"
	CloudAnthropic CloudService = "anthropic" // alias for claude
	CloudDeepSeek  CloudService = "deepseek"
	CloudOpenAI    CloudService = "openai"
)

// NormalizeCloud maps a user-supplied cloud provider name to its canonical identifier. Matching is
// case-insensitive and ignores surrounding whitespace. It is the single normaliser every layer
// (Manager, HTTP status, classify labels, app wiring) uses, so a non-empty unknown name is always
// rejected instead of silently routing to a different provider. An empty name is not a known
// alias: callers decide whether it keeps the historical Google default.
func NormalizeCloud(name string) (canonical string, ok bool) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "google", "gemini":
		return "google", true
	case "claude", "anthropic":
		return "claude", true
	case "deepseek":
		return "deepseek", true
	case "openai":
		return "openai", true
	default:
		return "", false
	}
}

// Provider is the common interface implemented by each AI backend.
type Provider interface {
	Name() string
	RequestClassification(ctx context.Context, system, user string) (ollama.Completion, error)
	RequestTextChat(ctx context.Context, system, user string) (ollama.TextCompletion, error)
	Test(ctx context.Context) (string, error)
}

// modelProber is the OPTIONAL extension of Provider that returns the model id the upstream response
// echoed alongside the connection-test reply. Every cloud provider implements it; the Provider
// interface itself is intentionally not grown, because test fakes and other callers only need Test.
// The returned servedModel is "" when the provider omitted the echo — it is never read from config.
type modelProber interface {
	TestWithModel(ctx context.Context) (reply, servedModel string, err error)
}

// modelBuildSuffix matches the only provider suffixes ModelMatches tolerates after "-": a dated build
// (e.g. "2024-08-06"), a numeric build (e.g. "001" or "20250219") or "latest". Any other suffix
// ("-mini", "-lite", "-pro", "-preview"...) names a DIFFERENT model. Compiled once at package level.
var modelBuildSuffix = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}|\d{3,8}|latest)$`)

// ModelMatches reports whether a model id confirmed by the upstream API is the requested model,
// tolerating only provider build suffixes. Comparison trims surrounding whitespace and is
// case-insensitive. An exact match is true, and so is a confirmed id that starts with the requested
// id followed by ":" (a tag, e.g. "qwen3.5" -> "qwen3.5:9b") or by a "-" whose remainder is a build
// suffix modelBuildSuffix accepts (e.g. "gpt-4o" -> "gpt-4o-2024-08-06", "claude-3-7-sonnet" ->
// "claude-3-7-sonnet-20250219", "gemini-2.5-flash" -> "gemini-2.5-flash-001"). A "-" suffix such as
// "-mini" or "-lite" names a different model and returns false. A confirmed id of "" is never a
// match, so a missing echo is never treated as verified.
func ModelMatches(requested, confirmed string) bool {
	req := strings.ToLower(strings.TrimSpace(requested))
	conf := strings.ToLower(strings.TrimSpace(confirmed))
	if conf == "" {
		return false
	}
	if req == "" {
		return false
	}
	if req == conf {
		return true
	}
	if strings.HasPrefix(conf, req+":") {
		return true
	}
	if strings.HasPrefix(conf, req+"-") {
		return modelBuildSuffix.MatchString(strings.TrimPrefix(conf, req+"-"))
	}
	return false
}

// Config holds the full AI provider configuration.
type Config struct {
	AIProvider    string // "local" | "cloud"
	CloudProvider string // "google" | "claude" | "deepseek" | "openai"

	// Local (Ollama)
	OllamaHost  string
	OllamaModel string

	// Google Gemini
	GoogleAPIKey  string
	GoogleModel   string
	GoogleBaseURL string

	// Anthropic Claude
	AnthropicAPIKey  string
	AnthropicModel   string
	AnthropicBaseURL string

	// DeepSeek
	DeepSeekAPIKey  string
	DeepSeekModel   string
	DeepSeekBaseURL string

	// OpenAI
	OpenAIAPIKey  string
	OpenAIModel   string
	OpenAIBaseURL string
}

// Manager coordinates the active AI provider (local vs cloud services) and implements the
// Ollama and TextChat interfaces required by the triage pipeline.
type Manager struct {
	mu     sync.RWMutex
	cfg    Config
	ollama *ollama.Client

	google   Provider
	claude   Provider
	deepseek Provider
	openai   Provider

	// healthMu guards the cloud health cache below. It is held across the upstream provider call so
	// concurrent callers on a cache miss trigger exactly one (billable) call, like the stats cache
	// in httpapi/system.go.
	healthMu   sync.Mutex
	healthInit bool
	healthKey  string
	healthAt   time.Time
	health     cloudHealthEntry
	// now is the clock seam: time.Now in production, replaced by tests with a fake clock.
	now func() time.Time

	// lastMu guards the "last successful classification" record below. It is deliberately separate
	// from mu so a classification in flight never blocks a config read or vice versa.
	lastMu    sync.Mutex
	lastModel string
	lastAt    time.Time
}

// NewManager creates an AI Provider Manager.
func NewManager(cfg Config, ollamaClient *ollama.Client) *Manager {
	m := &Manager{
		cfg:    cfg,
		ollama: ollamaClient,
		now:    time.Now,
	}
	m.rebuildProviders()
	return m
}

func (m *Manager) rebuildProviders() {
	m.google = NewGoogleProvider(GoogleConfig{
		APIKey:  m.cfg.GoogleAPIKey,
		Model:   m.cfg.GoogleModel,
		BaseURL: m.cfg.GoogleBaseURL,
	})
	m.claude = NewClaudeProvider(ClaudeConfig{
		APIKey:  m.cfg.AnthropicAPIKey,
		Model:   m.cfg.AnthropicModel,
		BaseURL: m.cfg.AnthropicBaseURL,
	})
	m.deepseek = NewDeepSeekProvider(DeepSeekConfig{
		APIKey:  m.cfg.DeepSeekAPIKey,
		Model:   m.cfg.DeepSeekModel,
		BaseURL: m.cfg.DeepSeekBaseURL,
	})
	m.openai = NewOpenAIProvider(OpenAIConfig{
		APIKey:  m.cfg.OpenAIAPIKey,
		Model:   m.cfg.OpenAIModel,
		BaseURL: m.cfg.OpenAIBaseURL,
	})
}

// UpdateConfig updates the provider configuration on the fly. It rebuilds the cloud providers and
// retargets the Ollama client at the new host/model, preserving the client's other constructor
// options (embed model, spawn hooks, logger), so a changed AI Engine takes effect immediately.
func (m *Manager) UpdateConfig(cfg Config) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = cfg
	m.rebuildProviders()
	if m.ollama != nil {
		m.ollama = m.ollama.WithHostModel(cfg.OllamaHost, cfg.OllamaModel)
	}
	// Every config update resets the record: UpdateConfig cannot tell which field changed, so the
	// previous classification's model id can no longer be assumed to describe what the running
	// configuration will serve.
	m.lastMu.Lock()
	m.lastModel = ""
	m.lastAt = time.Time{}
	m.lastMu.Unlock()
}

// LastClassification returns the model id echoed by the most recent successful
// RequestClassificationCompletion, plus when it happened. Both are zero until a classification has
// succeeded since start or since the last UpdateConfig. Chat calls are never recorded here.
func (m *Manager) LastClassification() (string, time.Time) {
	m.lastMu.Lock()
	defer m.lastMu.Unlock()
	return m.lastModel, m.lastAt
}

// recordClassification stores a non-empty echoed model id. The caller has already filtered empty ids.
func (m *Manager) recordClassification(model string) {
	m.lastMu.Lock()
	m.lastModel = strings.TrimSpace(model)
	m.lastAt = m.currentTime()
	m.lastMu.Unlock()
}

// currentTime reads the clock seam, falling back to time.Now when it is unset.
func (m *Manager) currentTime() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// ActiveProvider returns the current active provider instance. The second value is a display name
// for diagnostics; for an unknown cloud provider the provider is nil and the name carries the error
// text. Callers that must not silently fall back use selectProvider instead.
func (m *Manager) ActiveProvider() (Provider, string) {
	provider, name, err := m.selectProvider()
	if err != nil {
		return nil, err.Error()
	}
	return provider, name
}

// selectProvider resolves the active provider under a read lock. A local configuration returns
// (nil, "Local (Ollama)", nil) so callers use the Ollama client. An unknown non-empty cloud
// provider returns an error, never another provider.
func (m *Manager) selectProvider() (Provider, string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if strings.EqualFold(m.cfg.AIProvider, "cloud") {
		provider, name, ok := m.resolveCloudLocked()
		if !ok {
			return nil, "", fmt.Errorf("unknown cloud provider %q", strings.TrimSpace(m.cfg.CloudProvider))
		}
		return provider, name, nil
	}
	return nil, "Local (Ollama)", nil
}

// resolveCloudLocked maps the configured cloud provider to its instance. An empty name keeps the
// historical Google default (settings.Config leaves it empty); a non-empty unknown name is rejected
// so it never routes to Google. The caller holds m.mu.
func (m *Manager) resolveCloudLocked() (Provider, string, bool) {
	canonical, ok := NormalizeCloud(m.cfg.CloudProvider)
	if !ok {
		if strings.TrimSpace(m.cfg.CloudProvider) != "" {
			return nil, "", false
		}
		canonical = "google"
	}
	switch canonical {
	case "google":
		return m.google, "Google Gemini", true
	case "claude":
		return m.claude, "Anthropic Claude", true
	case "deepseek":
		return m.deepseek, "DeepSeek", true
	case "openai":
		return m.openai, "OpenAI", true
	}
	return nil, "", false
}

// ollamaClient reads the current Ollama client under a read lock. UpdateConfig swaps the pointer
// (never mutating a client in place), so a local caller can safely use the returned value after the
// lock is released.
func (m *Manager) ollamaClient() *ollama.Client {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.ollama
}

// EnsureOllamaModel implements the pipeline's Ollama interface.
// When using a cloud provider, it returns true if an API key is configured.
func (m *Manager) EnsureOllamaModel(modelName string) bool {
	provider, _, err := m.selectProvider()
	if err != nil {
		return false
	}
	if provider != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, testErr := provider.Test(ctx)
		return testErr == nil
	}
	if client := m.ollamaClient(); client != nil {
		return client.EnsureOllamaModel(modelName)
	}
	return false
}

// RequestClassificationCompletion satisfies app/classify.Ollama.
func (m *Manager) RequestClassificationCompletion(system, user string) (ollama.Completion, error) {
	provider, _, err := m.selectProvider()
	if err != nil {
		return ollama.Completion{}, err
	}
	if provider != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		comp, err := provider.RequestClassification(ctx, system, user)
		if err == nil && strings.TrimSpace(comp.Model) != "" {
			m.recordClassification(comp.Model)
		}
		return comp, err
	}
	if client := m.ollamaClient(); client != nil {
		comp, err := client.RequestClassificationCompletion(system, user)
		if err == nil && strings.TrimSpace(comp.Model) != "" {
			m.recordClassification(comp.Model)
		}
		return comp, err
	}
	return ollama.Completion{}, errors.New("no AI provider configured")
}

// RequestTextChatCompletion satisfies app/aichat.TextChat.
func (m *Manager) RequestTextChatCompletion(system, user string) (ollama.TextCompletion, error) {
	provider, _, err := m.selectProvider()
	if err != nil {
		return ollama.TextCompletion{}, err
	}
	if provider != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		return provider.RequestTextChat(ctx, system, user)
	}
	if client := m.ollamaClient(); client != nil {
		return client.RequestTextChatCompletion(system, user)
	}
	return ollama.TextCompletion{}, errors.New("no AI provider configured")
}

// GenerateEmbedding satisfies embedding calls (delegates to Ollama).
func (m *Manager) GenerateEmbedding(text string) []float64 {
	if client := m.ollamaClient(); client != nil {
		return client.GenerateEmbedding(text)
	}
	return []float64{}
}

// CheckModelCanGenerate implements the health check interface.
func (m *Manager) CheckModelCanGenerate(modelName string, forceRefresh bool) ollama.ModelHealth {
	provider, name, err := m.selectProvider()
	if err != nil {
		return ollama.ModelHealth{OK: false, Error: err.Error()}
	}
	if provider != nil {
		return m.checkCloudHealth(provider, name, modelName, forceRefresh)
	}
	if client := m.ollamaClient(); client != nil {
		return client.CheckModelCanGenerate(modelName, forceRefresh)
	}
	return ollama.ModelHealth{OK: false, Error: "no AI provider"}
}

// checkCloudHealth runs provider.Test behind the 60 s cloud health cache. The cache key is the
// active provider + model + base URL + SHA-256 of the API key, so a settings change misses
// automatically and the key itself is never stored or logged. forceRefresh bypasses the cache and
// refreshes it; failures are cached for the same TTL. healthMu is held across the upstream call so
// concurrent callers on a miss trigger a single billable call.
func (m *Manager) checkCloudHealth(provider Provider, name, modelName string, forceRefresh bool) ollama.ModelHealth {
	key := m.cloudHealthKey(modelName)

	m.healthMu.Lock()
	defer m.healthMu.Unlock()

	now := m.now
	if now == nil {
		now = time.Now
	}
	if !forceRefresh && m.healthInit && m.healthKey == key && now().Sub(m.healthAt) < cloudHealthCacheTTL {
		return m.cloudHealthResult()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var servedModel string
	var testErr error
	if prober, ok := provider.(modelProber); ok {
		_, servedModel, testErr = prober.TestWithModel(ctx)
	} else {
		_, testErr = provider.Test(ctx)
	}
	if testErr != nil {
		m.health = cloudHealthEntry{ok: false, errText: fmt.Sprintf("%s error: %v", name, testErr)}
	} else {
		m.health = cloudHealthEntry{ok: true, servedModel: servedModel}
	}
	m.healthInit = true
	m.healthKey = key
	m.healthAt = now()
	return m.cloudHealthResult()
}

// cloudHealthResult is the ModelHealth for the cached entry. The caller holds healthMu. ServedModel
// is the id the upstream echoed on success and stays "" when the provider omitted it.
func (m *Manager) cloudHealthResult() ollama.ModelHealth {
	if m.health.ok {
		return ollama.ModelHealth{OK: true, ServedModel: m.health.servedModel}
	}
	return ollama.ModelHealth{OK: false, Error: m.health.errText}
}

// cloudHealthKey fingerprints the active cloud provider's model, base URL and API key. Only the
// SHA-256 hex digest of the key is kept; the key itself never enters the cache. The caller must not
// hold m.mu.
func (m *Manager) cloudHealthKey(modelName string) string {
	m.mu.RLock()
	defer m.mu.RUnlock()

	canonical, ok := NormalizeCloud(m.cfg.CloudProvider)
	if !ok {
		canonical = "google"
	}
	var model, baseURL, apiKey string
	switch canonical {
	case "claude":
		model, baseURL, apiKey = m.cfg.AnthropicModel, m.cfg.AnthropicBaseURL, m.cfg.AnthropicAPIKey
	case "deepseek":
		model, baseURL, apiKey = m.cfg.DeepSeekModel, m.cfg.DeepSeekBaseURL, m.cfg.DeepSeekAPIKey
	case "openai":
		model, baseURL, apiKey = m.cfg.OpenAIModel, m.cfg.OpenAIBaseURL, m.cfg.OpenAIAPIKey
	default: // "google"
		model, baseURL, apiKey = m.cfg.GoogleModel, m.cfg.GoogleBaseURL, m.cfg.GoogleAPIKey
	}
	sum := sha256.Sum256([]byte(apiKey))
	return fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%x", canonical, model, modelName, baseURL, sum)
}

// ListModels returns available models for the active provider. For the local provider it delegates
// to the Ollama client, so an unreachable Ollama returns an error instead of a hardcoded list.
func (m *Manager) ListModels(host string) ([]string, error) {
	m.mu.RLock()
	isCloud := strings.EqualFold(m.cfg.AIProvider, "cloud")
	cloudProv := m.cfg.CloudProvider
	client := m.ollama
	m.mu.RUnlock()

	if isCloud {
		canonical, ok := NormalizeCloud(cloudProv)
		if !ok {
			if strings.TrimSpace(cloudProv) != "" {
				return nil, fmt.Errorf("unknown cloud provider %q", strings.TrimSpace(cloudProv))
			}
			canonical = "google"
		}
		switch canonical {
		case "google":
			return []string{"gemini-3.8-flash", "gemini-2.5-flash", "gemini-2.5-pro", "gemini-1.5-flash", "gemini-1.5-pro"}, nil
		case "claude":
			return []string{"claude-3-7-sonnet-20250219", "claude-3-5-sonnet-20241022", "claude-3-5-haiku-20241022"}, nil
		case "deepseek":
			return []string{"deepseek-flash", "deepseek-chat", "deepseek-reasoner"}, nil
		case "openai":
			return []string{"gpt-4o-mini", "gpt-4o", "o3-mini", "gpt-4.5-preview"}, nil
		}
	}
	if client != nil {
		return client.ListModels(host)
	}
	return []string{}, nil
}
