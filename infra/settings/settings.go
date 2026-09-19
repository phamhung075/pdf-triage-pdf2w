// Package settings is a Go port of pdf-triage's src/infrastructure/settings.ts (297 lines):
// BASE_DIR/DATA_DIR resolution, .env loading, the CONFIG object with its defaults and env
// overrides, the pure sanitizers, reloadConfigFromDisk / updateConfig (writing settings.json),
// ensureDirectoriesExist, and the WSL path normalization applied on config load.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/settings.test.ts` -> 15 passed), so no upstream case is pinned
// red. All 15 cases are ported, plus cases for the remaining CONFIG keys, the BASE_DIR/DATA_DIR
// split, .env semantics, the JS parseInt/finite-timeout parsing, directory creation and the legacy
// folder migration, isFirstRun, and the persisted settings.json shape — behaviors the TS suite never
// asserted.
//
// # BASE_DIR choice (the one intentional architectural difference)
//
// TS derives BASE_DIR from this module's own file location: src/infrastructure/settings.ts and
// dist/infrastructure/settings.js are each exactly two directories below the project root, which is
// stable for both tsx (dev) and the packaged Electron app, where process.cwd() is not. A Go package
// has no equivalent module URL at runtime. This port therefore resolves BASE_DIR as:
//
//  1. PDF_TRIAGE_BASE_DIR, when set (same env name and precedence as TS);
//  2. an explicit Options.BaseDir, when non-empty (the caller/composition root knows where it
//     installed itself — the intended production path for cmd/pdf-triage);
//  3. otherwise the running executable's directory (os.Executable), which is the closest Go
//     analogue of "the directory the app lives in"; it falls back to the working directory if the
//     executable path cannot be determined.
//
// DATA_DIR resolves the same way: PDF_TRIAGE_DATA_DIR, then Options.DataDir, then BASE_DIR. As in
// TS, PDF_TRIAGE_BASE_DIR / PDF_TRIAGE_DATA_DIR are resolved BEFORE .env is loaded, so neither can
// be moved by a .env entry.
//
// # Deliberate omissions at cutover
//
//   - PDF_TRIAGE_PDF2W_SERVICE_URL and PDF_TRIAGE_PDF2W_SERVICE_TIMEOUT_MS are NOT ported. They
//     point at the pdf-triage-pdf2w Go service's HTTP endpoints (POST /canonical-path, POST
//     /clean-text); at cutover those callers become in-process Go calls
//     (github.com/phamhung075/pdf-triage-pdf2w/canonicalpath and .../cleantext), so a base URL and
//     timeout for an HTTP hop that no longer exists are dead config. Every other env var NAME is
//     preserved exactly.
//   - PDF2W_SERVICE_URL and PDF2W_SERVICE_TIMEOUT_MS are KEPT: pdf2w is the external, self-hosted
//     markdown-extract-service, which stays an HTTP dependency after cutover.
//
// # Deviations, all resolved in favor of matching the TS acceptance bar
//
//  1. updateConfig writes settings.json ATOMICALLY (temp file + rename) where TS used a plain
//     fs.writeFileSync. The task requires atomicity; the observable bytes are the same
//     (JSON.stringify(data, null, 2)) and no .tmp file survives a successful write. The EPERM/EBUSY
//     rename fallback mirrors infra/jsonregistry (duplicated because that package's helper is
//     unexported and it may not be modified).
//  2. dotenv mutates process.env; this port keeps the parsed .env values in a local overlay so no
//     global state changes. The observable rules are preserved: DATA_DIR/.env wins over
//     BASE_DIR/.env, and an already-set process variable is never overridden by .env.
//  3. JS `parseInt(x, 10)` can return NaN; a Go int cannot. An unparseable PORT / VISION_LAB_PORT /
//     MCP_HTTP_PORT falls back to its default instead of storing NaN. Timeouts already map NaN and
//     negatives to 0 in TS, and keep doing so here.
//  4. settings.json is decoded into map[string]any (JSON.parse semantics) rather than a strict
//     struct: a valid non-object document, or a field of the wrong type, behaves as TS did
//     (field access yields undefined -> default) instead of failing the whole read.
//  5. sanitizePersonalNameDenylist's jsString(nil) would render "undefined" only for a value that
//     JSON cannot represent; a JSON null element renders "null" exactly as JS String(null) did.
//     The normalization order is ToLower then Trim, matching the TS chain.
//  6. The historical re-exports normalizePathInput / toWindowsPath are host-based
//     (pathconv.WindowsToWSLPathHost), because Go has no default parameter equivalent to TS's
//     `platform = process.platform`. On a Windows host WindowsToWSLPath is a deliberate no-op.
//  7. Go's WSL path normalization applies only where TS applied it: INPUT_DIR and OUTPUT_ROOT_DIR.
//     JSON_REGISTRY_PATH and DB_PATH keep the raw env string, as in TS.
package settings

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/phamhung075/pdf-triage-pdf2w/pathconv"
)

// Allowed models are pinned: Golden Rule #14 (only qwen3.5:9b classifies) and the separate Vision
// Lab pipeline (minicpm-v4.6:latest). These comments are preserved from the TS source.
const (
	allowedOllamaModel       = "qwen3.5:9b"
	allowedOllamaVisionModel = "minicpm-v4.6:latest"
)

// Config mirrors the TS CONFIG object. Every TS key is present except the two deliberate cutover
// omissions documented in the package comment. PersonalNameDenylist is never nil (TS's
// DEFAULT_PERSONAL_NAME_DENYLIST is []).
type Config struct {
	Language string

	InputDir      string
	OutputRootDir string

	JSONRegistryPath string
	DBPath           string

	CategoriesFile        string
	CategoriesPrivateFile string
	EntityDictionaryFile  string
	ManualDecisionsFile   string
	TaxonomyHintsFile     string
	PromptsDir            string
	PromptsPrivateFile    string

	OllamaHost        string
	OllamaModel       string
	OllamaEmbedModel  string
	OllamaVisionModel string
	VisionLabPort     int

	Port int
	Host string

	PDF2WServiceURL       string
	PDF2WServiceTimeoutMS int

	MCPHTTPPort int
	MCPHTTPHost string

	PersonalNameDenylist []string

	// AI Provider configuration: Local (Ollama) vs Cloud (Google, Claude, DeepSeek, OpenAI)
	AIProvider    string
	CloudProvider string

	GoogleAPIKey  string
	GoogleModel   string
	GoogleBaseURL string

	AnthropicAPIKey  string
	AnthropicModel   string
	AnthropicBaseURL string

	DeepSeekAPIKey  string
	DeepSeekModel   string
	DeepSeekBaseURL string

	OpenAIAPIKey  string
	OpenAIModel   string
	OpenAIBaseURL string
}

// Options configures New. BaseDir/DataDir are explicit overrides used only when the matching env
// var is unset; Stderr receives the sanitizer warnings and the settings-read error (TS console.warn
// / console.error), defaulting to os.Stderr.
type Options struct {
	BaseDir string
	DataDir string
	Stderr  io.Writer
}

// Env is a process-environment lookup matching process.env. It is used only by tests indirectly;
// production always reads os.LookupEnv.
type envLookup func(string) (string, bool)

// envSource overlays .env values on the process environment. A .env entry is visible only when the
// process environment does not already define the name, matching dotenv's default override=false.
type envSource struct {
	lookup    envLookup
	overrides map[string]string
}

func newEnvSource(lookup envLookup) *envSource {
	if lookup == nil {
		lookup = os.LookupEnv
	}
	return &envSource{lookup: lookup, overrides: map[string]string{}}
}

func (e *envSource) get(name string) (string, bool) {
	if v, ok := e.overrides[name]; ok {
		return v, true
	}
	return e.lookup(name)
}

// value is `process.env.NAME`: the empty string for a missing or empty variable.
func (e *envSource) value(name string) string {
	v, _ := e.get(name)
	return v
}

// or is `process.env.NAME || fallback`: a present-but-empty value falls back.
func (e *envSource) or(name, fallback string) string {
	if v := e.value(name); v != "" {
		return v
	}
	return fallback
}

func (e *envSource) applyDotEnv(values map[string]string) {
	for k, v := range values {
		if _, present := e.lookup(k); present {
			continue
		}
		e.overrides[k] = v
	}
}

// Store owns a mutable Config, mirroring TS's module-level CONFIG object. Methods are safe for
// concurrent callers; Config returns a copy.
type Store struct {
	mu           sync.Mutex
	cfg          Config
	baseDir      string
	dataDir      string
	settingsFile string
	stderr       io.Writer
}

// New resolves BASE_DIR/DATA_DIR, creates DATA_DIR, loads .env, reads settings.json and derives the
// initial Config. This is the TS module-load sequence.
func New(opts Options) (*Store, error) {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	env := newEnvSource(nil)

	baseDir := resolveBaseDir(opts.BaseDir, env)
	dataDir := resolveDataDir(opts.DataDir, baseDir, env)

	// TS: if (!fs.existsSync(DATA_DIR)) fs.mkdirSync(DATA_DIR, { recursive: true });
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("settings: create DATA_DIR %q: %w", dataDir, err)
	}

	// .env is read from DATA_DIR first (an installed app's own config) and falls back to BASE_DIR so
	// a checkout keeps working unchanged. dotenv.config() with no explicit path would use cwd, which
	// is exactly the unreliable-in-packaged-Electron problem BASE_DIR exists to avoid.
	envPath := filepath.Join(baseDir, ".env")
	dataEnv := filepath.Join(dataDir, ".env")
	if _, err := os.Stat(dataEnv); err == nil {
		envPath = dataEnv
	}
	env.applyDotEnv(loadDotEnv(envPath))

	settingsFile := filepath.Join(dataDir, "settings.json")
	custom := LoadCustomSettings(settingsFile, stderr)
	cfg := buildConfig(custom, env, baseDir, dataDir, stderr)

	return &Store{
		cfg:          cfg,
		baseDir:      baseDir,
		dataDir:      dataDir,
		settingsFile: settingsFile,
		stderr:       stderr,
	}, nil
}

// BaseDir is the read-only asset root (prompts/, categories.json, entity_dictionary.json).
func (s *Store) BaseDir() string { return s.baseDir }

// DataDir is the writable state root (settings.json, the DB, registry.json, private overlays).
func (s *Store) DataDir() string { return s.dataDir }

// SettingsFile is DATA_DIR/settings.json.
func (s *Store) SettingsFile() string { return s.settingsFile }

// Config returns a copy of the current configuration.
func (s *Store) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	if s.cfg.PersonalNameDenylist != nil {
		cfg.PersonalNameDenylist = make([]string, len(s.cfg.PersonalNameDenylist))
		copy(cfg.PersonalNameDenylist, s.cfg.PersonalNameDenylist)
	}
	return cfg
}

// IsFirstRun is `isFirstRun()`: no settings.json, or one without the two folder paths the pipeline
// cannot run without. Deliberately re-read from disk each call because the wizard writes
// settings.json and then asks again.
func (s *Store) IsFirstRun() bool {
	current := LoadCustomSettings(s.settingsFile, s.stderr)
	return !jsTruthy(current["input_dir"]) || !jsTruthy(current["output_root_dir"])
}

// ReloadFromDisk is `reloadConfigFromDisk()`: re-read settings.json and mutate the existing Config.
// LANGUAGE, OLLAMA_MODEL and PERSONAL_NAME_DENYLIST are always reset (absent -> default); the
// folder paths and OLLAMA_HOST are only replaced when the file supplies a truthy value.
func (s *Store) ReloadFromDisk() {
	current := LoadCustomSettings(s.settingsFile, s.stderr)

	s.mu.Lock()
	defer s.mu.Unlock()

	s.cfg.Language = sanitizeLanguage(current["language"])
	if v := current["input_dir"]; jsTruthy(v) {
		s.cfg.InputDir = pathconv.WindowsToWSLPathHost(jsString(v))
	}
	if v := current["output_root_dir"]; jsTruthy(v) {
		s.cfg.OutputRootDir = pathconv.WindowsToWSLPathHost(jsString(v))
	}
	s.cfg.OllamaModel = sanitizeOllamaModel(current["ollama_model"], s.stderr)
	if v := current["ollama_host"]; jsTruthy(v) {
		s.cfg.OllamaHost = jsString(v)
	}
	s.cfg.PersonalNameDenylist = sanitizePersonalNameDenylist(current["personal_name_denylist"])
	if v := current["ai_provider"]; jsTruthy(v) {
		s.cfg.AIProvider = jsString(v)
	}
	if v := current["cloud_provider"]; jsTruthy(v) {
		s.cfg.CloudProvider = jsString(v)
	}
	if v := current["google_api_key"]; jsTruthy(v) {
		s.cfg.GoogleAPIKey = jsString(v)
	}
	if v := current["google_model"]; jsTruthy(v) {
		s.cfg.GoogleModel = jsString(v)
	}
	if v := current["google_base_url"]; jsTruthy(v) {
		s.cfg.GoogleBaseURL = jsString(v)
	}
	if v := current["anthropic_api_key"]; jsTruthy(v) {
		s.cfg.AnthropicAPIKey = jsString(v)
	}
	if v := current["anthropic_model"]; jsTruthy(v) {
		s.cfg.AnthropicModel = jsString(v)
	}
	if v := current["anthropic_base_url"]; jsTruthy(v) {
		s.cfg.AnthropicBaseURL = jsString(v)
	}
	if v := current["deepseek_api_key"]; jsTruthy(v) {
		s.cfg.DeepSeekAPIKey = jsString(v)
	}
	if v := current["deepseek_model"]; jsTruthy(v) {
		s.cfg.DeepSeekModel = jsString(v)
	}
	if v := current["deepseek_base_url"]; jsTruthy(v) {
		s.cfg.DeepSeekBaseURL = jsString(v)
	}
	if v := current["openai_api_key"]; jsTruthy(v) {
		s.cfg.OpenAIAPIKey = jsString(v)
	}
	if v := current["openai_model"]; jsTruthy(v) {
		s.cfg.OpenAIModel = jsString(v)
	}
	if v := current["openai_base_url"]; jsTruthy(v) {
		s.cfg.OpenAIBaseURL = jsString(v)
	}
}

// UpdateConfig is `updateConfig(newSettings)`: mutate Config in place, persist the six-field
// settings.json atomically, then ensure the managed directories exist. A nil pointer / nil slice
// means "field omitted"; an explicitly empty []string is a provided empty denylist (TS `if (arr)`
// is true for []).
func (s *Store) UpdateConfig(patch UpdateSettings) error {
	s.mu.Lock()
	if patch.Language != nil && *patch.Language != "" {
		s.cfg.Language = sanitizeLanguage(*patch.Language)
	}
	if patch.InputDir != nil && *patch.InputDir != "" {
		s.cfg.InputDir = pathconv.WindowsToWSLPathHost(*patch.InputDir)
	}
	if patch.OutputRootDir != nil && *patch.OutputRootDir != "" {
		s.cfg.OutputRootDir = pathconv.WindowsToWSLPathHost(*patch.OutputRootDir)
	}
	if patch.OllamaModel != nil && *patch.OllamaModel != "" {
		s.cfg.OllamaModel = sanitizeOllamaModel(*patch.OllamaModel, s.stderr)
	}
	if patch.OllamaHost != nil && *patch.OllamaHost != "" {
		s.cfg.OllamaHost = *patch.OllamaHost
	}
	if patch.PersonalNameDenylist != nil {
		s.cfg.PersonalNameDenylist = sanitizePersonalNameDenylist(patch.PersonalNameDenylist)
	}
	if patch.AIProvider != nil && *patch.AIProvider != "" {
		s.cfg.AIProvider = *patch.AIProvider
	}
	if patch.CloudProvider != nil && *patch.CloudProvider != "" {
		s.cfg.CloudProvider = *patch.CloudProvider
	}
	if patch.GoogleAPIKey != nil {
		s.cfg.GoogleAPIKey = *patch.GoogleAPIKey
	}
	if patch.GoogleModel != nil && *patch.GoogleModel != "" {
		s.cfg.GoogleModel = *patch.GoogleModel
	}
	if patch.GoogleBaseURL != nil {
		s.cfg.GoogleBaseURL = *patch.GoogleBaseURL
	}
	if patch.AnthropicAPIKey != nil {
		s.cfg.AnthropicAPIKey = *patch.AnthropicAPIKey
	}
	if patch.AnthropicModel != nil && *patch.AnthropicModel != "" {
		s.cfg.AnthropicModel = *patch.AnthropicModel
	}
	if patch.AnthropicBaseURL != nil {
		s.cfg.AnthropicBaseURL = *patch.AnthropicBaseURL
	}
	if patch.DeepSeekAPIKey != nil {
		s.cfg.DeepSeekAPIKey = *patch.DeepSeekAPIKey
	}
	if patch.DeepSeekModel != nil && *patch.DeepSeekModel != "" {
		s.cfg.DeepSeekModel = *patch.DeepSeekModel
	}
	if patch.DeepSeekBaseURL != nil {
		s.cfg.DeepSeekBaseURL = *patch.DeepSeekBaseURL
	}
	if patch.OpenAIAPIKey != nil {
		s.cfg.OpenAIAPIKey = *patch.OpenAIAPIKey
	}
	if patch.OpenAIModel != nil && *patch.OpenAIModel != "" {
		s.cfg.OpenAIModel = *patch.OpenAIModel
	}
	if patch.OpenAIBaseURL != nil {
		s.cfg.OpenAIBaseURL = *patch.OpenAIBaseURL
	}
	cfg := s.cfg
	s.mu.Unlock()

	aiProv := cfg.AIProvider
	if aiProv == "local" {
		aiProv = ""
	}
	cloudProv := cfg.CloudProvider
	if aiProv == "" {
		cloudProv = ""
	}

	data := persistedSettings{
		Language:             cfg.Language,
		InputDir:             cfg.InputDir,
		OutputRootDir:        cfg.OutputRootDir,
		OllamaModel:          cfg.OllamaModel,
		OllamaHost:           cfg.OllamaHost,
		PersonalNameDenylist: cfg.PersonalNameDenylist,
		AIProvider:           aiProv,
		CloudProvider:        cloudProv,
		GoogleAPIKey:         cfg.GoogleAPIKey,
		GoogleModel:          cfg.GoogleModel,
		GoogleBaseURL:        cfg.GoogleBaseURL,
		AnthropicAPIKey:      cfg.AnthropicAPIKey,
		AnthropicModel:       cfg.AnthropicModel,
		AnthropicBaseURL:     cfg.AnthropicBaseURL,
		DeepSeekAPIKey:       cfg.DeepSeekAPIKey,
		DeepSeekModel:        cfg.DeepSeekModel,
		DeepSeekBaseURL:      cfg.DeepSeekBaseURL,
		OpenAIAPIKey:         cfg.OpenAIAPIKey,
		OpenAIModel:          cfg.OpenAIModel,
		OpenAIBaseURL:        cfg.OpenAIBaseURL,
	}
	payload, err := marshalSettings(data)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(s.settingsFile, payload, 0o600); err != nil {
		return err
	}
	return EnsureDirectoriesExist(cfg)
}

// EnsureDirectoriesExist is the Store form of ensureDirectoriesExist().
func (s *Store) EnsureDirectoriesExist() error {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	return EnsureDirectoriesExist(cfg)
}

// UpdateSettings is `updateConfig`'s argument object. Pointer fields distinguish "omitted" from
// "set to the zero value"; PersonalNameDenylist is nil when omitted.
type UpdateSettings struct {
	Language             *string
	InputDir             *string
	OutputRootDir        *string
	OllamaModel          *string
	OllamaHost           *string
	PersonalNameDenylist []string

	AIProvider    *string
	CloudProvider *string

	GoogleAPIKey  *string
	GoogleModel   *string
	GoogleBaseURL *string

	AnthropicAPIKey  *string
	AnthropicModel   *string
	AnthropicBaseURL *string

	DeepSeekAPIKey  *string
	DeepSeekModel   *string
	DeepSeekBaseURL *string

	OpenAIAPIKey  *string
	OpenAIModel   *string
	OpenAIBaseURL *string
}

// persistedSettings mirrors the dataToSave object updateConfig writes. Field order matches the TS
// object literal so the emitted JSON keys stay in the same order.
type persistedSettings struct {
	Language             string   `json:"language"`
	InputDir             string   `json:"input_dir"`
	OutputRootDir        string   `json:"output_root_dir"`
	OllamaModel          string   `json:"ollama_model"`
	OllamaHost           string   `json:"ollama_host"`
	PersonalNameDenylist []string `json:"personal_name_denylist"`

	AIProvider    string `json:"ai_provider,omitempty"`
	CloudProvider string `json:"cloud_provider,omitempty"`

	GoogleAPIKey  string `json:"google_api_key,omitempty"`
	GoogleModel   string `json:"google_model,omitempty"`
	GoogleBaseURL string `json:"google_base_url,omitempty"`

	AnthropicAPIKey  string `json:"anthropic_api_key,omitempty"`
	AnthropicModel   string `json:"anthropic_model,omitempty"`
	AnthropicBaseURL string `json:"anthropic_base_url,omitempty"`

	DeepSeekAPIKey  string `json:"deepseek_api_key,omitempty"`
	DeepSeekModel   string `json:"deepseek_model,omitempty"`
	DeepSeekBaseURL string `json:"deepseek_base_url,omitempty"`

	OpenAIAPIKey  string `json:"openai_api_key,omitempty"`
	OpenAIModel   string `json:"openai_model,omitempty"`
	OpenAIBaseURL string `json:"openai_base_url,omitempty"`
}

// LoadCustomSettings is `loadCustomSettings()`: the parsed settings.json, or an empty map when the
// file is missing or malformed. A malformed file reports the TS console.error text to stderr and
// still returns an empty map. settingsFile replaces the module-level SETTINGS_FILE so this is
// testable without global state.
func LoadCustomSettings(settingsFile string, stderr io.Writer) map[string]any {
	raw, err := os.ReadFile(settingsFile)
	if err != nil {
		if !os.IsNotExist(err) {
			logSettingsError(stderr, err)
		}
		return map[string]any{}
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		logSettingsError(stderr, err)
		return map[string]any{}
	}
	if m, ok := parsed.(map[string]any); ok {
		return m
	}
	// Valid but non-object JSON: TS returned it and every field access read undefined, so all
	// defaults applied. An empty map is the equivalent.
	return map[string]any{}
}

func logSettingsError(stderr io.Writer, err error) {
	if stderr == nil {
		stderr = os.Stderr
	}
	fmt.Fprintf(stderr, "Error reading settings.json %v\n", err)
}

// ParseDotEnv implements the subset of dotenv v16 parsing the module relied on: optional `export`
// prefix, `KEY=value` or `KEY: value`, quoted values (single/double/backtick), `\n`/`\r` unescaping
// in double quotes only, inline `#` comments, blank lines and `#` comment lines. It returns every
// successfully matched key.
func ParseDotEnv(src string) map[string]string {
	out := map[string]string{}
	normalized := strings.ReplaceAll(src, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")

	for _, m := range dotEnvLineRe.FindAllStringSubmatch(normalized, -1) {
		key := m[1]
		value := ""
		if len(m) > 2 {
			value = m[2]
		}
		value = strings.TrimSpace(value)
		if value != "" {
			quote := value[0]
			if (quote == '\'' || quote == '"' || quote == '`') &&
				len(value) >= 2 && value[len(value)-1] == quote {
				value = value[1 : len(value)-1]
			}
			if quote == '"' {
				value = strings.ReplaceAll(value, `\n`, "\n")
				value = strings.ReplaceAll(value, `\r`, "\r")
			}
		}
		out[key] = value
	}
	return out
}

// dotEnvLineRe is dotenv v16's LINE regex with the platform-independent parts kept. Backticks are
// spliced in because a raw Go string cannot contain one; RE2 has no backreferences, so the outer
// quote pair is stripped in ParseDotEnv instead of by the regex.
var dotEnvLineRe = regexp.MustCompile(
	`(?m)^\s*(?:export\s+)?([\w.-]+)(?:\s*=\s*?|:\s+?)(\s*'(?:\\'|[^'])*'|\s*"(?:\\"|[^"])*"|\s*` + "`" +
		`(?:\\` + "`" + `|[^` + "`" + `])*` + "`" + `|[^#\r\n]+)?\s*(?:#.*)?$`)

func loadDotEnv(path string) map[string]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	return ParseDotEnv(string(raw))
}

func resolveBaseDir(explicit string, env *envSource) string {
	candidate := ""
	if v := env.value("PDF_TRIAGE_BASE_DIR"); v != "" {
		candidate = resolveDir(v)
	} else if explicit != "" {
		candidate = resolveDir(explicit)
	} else {
		candidate = executableDir()
	}
	dir := candidate
	for {
		if hasAssetRoot(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return candidate
}

func hasAssetRoot(dir string) bool {
	if info, err := os.Stat(filepath.Join(dir, "public")); err == nil && info.IsDir() {
		return true
	}
	if info, err := os.Stat(filepath.Join(dir, "categories.json")); err == nil && !info.IsDir() {
		return true
	}
	return false
}

func resolveDataDir(explicit, baseDir string, env *envSource) string {
	if v := env.value("PDF_TRIAGE_DATA_DIR"); v != "" {
		return resolveDir(v)
	}
	if explicit != "" {
		return resolveDir(explicit)
	}
	return baseDir
}

// resolveDir is `path.resolve(...)`: absolute and cleaned.
func resolveDir(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	return abs
}

// executableDir is the Go analogue of "the directory this module lives in": the directory holding
// the running binary. Falls back to the working directory.
func executableDir() string {
	if exe, err := os.Executable(); err == nil && exe != "" {
		return filepath.Dir(exe)
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

func buildConfig(custom map[string]any, env *envSource, baseDir, dataDir string, stderr io.Writer) Config {
	language := sanitizeLanguage(firstTruthy(custom["language"], env.value("SYSTEM_LANGUAGE")))

	inputDir := firstNonEmpty(
		pathconv.WindowsToWSLPathHost(jsString(custom["input_dir"])),
		pathconv.WindowsToWSLPathHost(env.value("PDF_INPUT_DIR")),
		filepath.Join(dataDir, "input"),
	)
	outputRootDir := firstNonEmpty(
		pathconv.WindowsToWSLPathHost(jsString(custom["output_root_dir"])),
		pathconv.WindowsToWSLPathHost(env.value("PDF_OUTPUT_DIR")),
		filepath.Join(dataDir, "organized"),
	)

	ollamaHost := jsString(firstTruthy(
		custom["ollama_host"],
		env.value("OLLAMA_HOST"),
		"http://127.0.0.1:11434",
	))

	return Config{
		Language:      language,
		InputDir:      inputDir,
		OutputRootDir: outputRootDir,

		JSONRegistryPath: env.or("PDF_REGISTRY_PATH", filepath.Join(dataDir, "registry.json")),
		DBPath:           env.or("PDF_DB_PATH", filepath.Join(dataDir, "pdf_triage.db")),

		// Public, generic, committed starter taxonomy (top-level categories only, no personal
		// subcategories). CATEGORIES_PRIVATE_FILE holds everything auto-created from the user's own
		// documents (real bank branches, employers, etc.) — gitignored, never committed.
		CategoriesFile:        filepath.Join(baseDir, "categories.json"),
		CategoriesPrivateFile: filepath.Join(dataDir, ".categories.private.json"),
		EntityDictionaryFile:  filepath.Join(baseDir, "entity_dictionary.json"),
		ManualDecisionsFile:   filepath.Join(dataDir, "manual_decisions.json"),
		TaxonomyHintsFile:     filepath.Join(dataDir, "taxonomy_hints.json"),
		PromptsDir:            filepath.Join(baseDir, "prompts"),
		PromptsPrivateFile:    filepath.Join(dataDir, ".prompts.private.json"),

		OllamaHost:        ollamaHost,
		OllamaModel:       sanitizeOllamaModel(firstTruthy(custom["ollama_model"], env.value("OLLAMA_MODEL")), stderr),
		OllamaEmbedModel:  env.or("OLLAMA_EMBED_MODEL", "nomic-embed-text"),
		OllamaVisionModel: sanitizeOllamaVisionModel(env.value("OLLAMA_VISION_MODEL"), stderr),
		VisionLabPort:     parsePort(env.or("VISION_LAB_PORT", "3179"), 3179),

		Port: parsePort(env.or("PORT", "3971"), 3971),
		// Security default: bind to localhost only. This server has no authentication — binding to
		// 0.0.0.0 would expose the full API, including document contents and destructive actions,
		// to anyone on the same network. Only override this if you specifically want LAN access.
		Host: env.or("PDF_TRIAGE_HOST", "127.0.0.1"),

		// pdf2w extraction service (self-hosted markdown-extract-service): required, no fallback.
		PDF2WServiceURL: strings.TrimSpace(jsString(firstTruthy(
			env.value("PDF2W_SERVICE_URL"),
			custom["pdf2w_service_url"],
		))),
		PDF2WServiceTimeoutMS: parseTimeout(env.value("PDF2W_SERVICE_TIMEOUT_MS")),

		// MCP Streamable HTTP transport (npm run mcp). Unlike HOST above, this one defaults to
		// LAN-reachable (0.0.0.0) by design — mitigated by the required bearer token, not by
		// binding.
		MCPHTTPPort: parsePort(env.or("MCP_HTTP_PORT", "3972"), 3972),
		MCPHTTPHost: env.or("MCP_HTTP_HOST", "0.0.0.0"),

		PersonalNameDenylist: sanitizePersonalNameDenylist(custom["personal_name_denylist"]),

		AIProvider: jsString(firstTruthy(
			custom["ai_provider"],
			env.value("AI_PROVIDER"),
		)),
		CloudProvider: jsString(firstTruthy(
			custom["cloud_provider"],
			env.value("CLOUD_PROVIDER"),
		)),

		GoogleAPIKey: jsString(firstTruthy(
			custom["google_api_key"],
			env.value("GEMINI_API_KEY"),
			env.value("GOOGLE_API_KEY"),
		)),
		GoogleModel: jsString(firstTruthy(
			custom["google_model"],
			env.value("GEMINI_MODEL"),
			env.value("GOOGLE_MODEL"),
		)),
		GoogleBaseURL: jsString(firstTruthy(custom["google_base_url"], env.value("GOOGLE_BASE_URL"))),

		AnthropicAPIKey: jsString(firstTruthy(
			custom["anthropic_api_key"],
			env.value("ANTHROPIC_API_KEY"),
			env.value("CLAUDE_API_KEY"),
		)),
		AnthropicModel: jsString(firstTruthy(
			custom["anthropic_model"],
			env.value("ANTHROPIC_MODEL"),
			env.value("CLAUDE_MODEL"),
		)),
		AnthropicBaseURL: jsString(firstTruthy(custom["anthropic_base_url"], env.value("ANTHROPIC_BASE_URL"))),

		DeepSeekAPIKey: jsString(firstTruthy(
			custom["deepseek_api_key"],
			env.value("DEEPSEEK_API_KEY"),
		)),
		DeepSeekModel: jsString(firstTruthy(
			custom["deepseek_model"],
			env.value("DEEPSEEK_MODEL"),
		)),
		DeepSeekBaseURL: jsString(firstTruthy(custom["deepseek_base_url"], env.value("DEEPSEEK_BASE_URL"))),

		OpenAIAPIKey: jsString(firstTruthy(
			custom["openai_api_key"],
			env.value("OPENAI_API_KEY"),
		)),
		OpenAIModel: jsString(firstTruthy(
			custom["openai_model"],
			env.value("OPENAI_MODEL"),
		)),
		OpenAIBaseURL: jsString(firstTruthy(custom["openai_base_url"], env.value("OPENAI_BASE_URL"))),
	}
}

// sanitizeLanguage is `sanitizeLanguage(lang)`: anything that is not the string EN (case
// insensitive) is FR.
func sanitizeLanguage(lang any) string {
	if s, ok := lang.(string); ok && strings.ToUpper(s) == "EN" {
		return "EN"
	}
	return "FR"
}

// sanitizeOllamaModel is `sanitizeOllamaModel(model)`: Golden Rule #14 — only qwen3.5:9b is
// supported; legacy/cloud/subscription-gated models are rejected even if they end up in
// settings.json, rather than silently trusted.
func sanitizeOllamaModel(model any, stderr io.Writer) string {
	if s, ok := model.(string); ok && s == allowedOllamaModel {
		return allowedOllamaModel
	}
	if jsTruthy(model) {
		writeWarning(stderr, "Ignoring unsupported ollama_model '%s' (only '%s' is allowed per Golden Rule #14) — falling back to '%s'.\n",
			jsString(model), allowedOllamaModel, allowedOllamaModel)
	}
	return allowedOllamaModel
}

// sanitizeOllamaVisionModel is `sanitizeOllamaVisionModel(model)`: the separate Vision Lab model,
// pinned the same way but for the image-to-PDF orientation/crop pipeline.
func sanitizeOllamaVisionModel(model any, stderr io.Writer) string {
	if s, ok := model.(string); ok && s == allowedOllamaVisionModel {
		return allowedOllamaVisionModel
	}
	if jsTruthy(model) {
		writeWarning(stderr, "Ignoring unsupported OLLAMA_VISION_MODEL env value '%s' (only '%s' is supported by the Vision Lab pipeline) — falling back to '%s'.\n",
			jsString(model), allowedOllamaVisionModel, allowedOllamaVisionModel)
	}
	return allowedOllamaVisionModel
}

// sanitizePersonalNameDenylist is `sanitizePersonalNameDenylist(list)`: lowercase, trim, drop empty
// entries. A non-array value yields the default (empty) list. Accepts []any (decoded JSON) and
// []string (the UpdateSettings patch).
func sanitizePersonalNameDenylist(list any) []string {
	var raw []string
	switch v := list.(type) {
	case []string:
		raw = v
	case []any:
		raw = make([]string, 0, len(v))
		for _, item := range v {
			if item == nil {
				// JSON null: JS String(null) === "null", which survives filter(Boolean).
				raw = append(raw, "null")
				continue
			}
			raw = append(raw, jsString(item))
		}
	default:
		return []string{}
	}

	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s := strings.TrimSpace(strings.ToLower(item)); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func writeWarning(stderr io.Writer, format string, args ...any) {
	if stderr == nil {
		stderr = os.Stderr
	}
	fmt.Fprintf(stderr, format, args...)
}

// EnsureDirectoriesExist is `ensureDirectoriesExist()`: create every managed folder, then migrate
// the legacy non-dot blocked_files / duplicates_files folders into their dot-prefixed successors.
// The migration is best-effort and silently tolerates failure, exactly as the TS try/catch did.
func EnsureDirectoriesExist(cfg Config) error {
	dirs := []string{
		cfg.InputDir,
		filepath.Join(cfg.InputDir, ".duplicates_files"),
		filepath.Join(cfg.InputDir, ".blocked_files"),
		filepath.Join(cfg.InputDir, ".delete_files"),
		cfg.OutputRootDir,
		filepath.Dir(cfg.JSONRegistryPath),
		filepath.Dir(cfg.DBPath),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}

	// Automatic migration of legacy non-dot folders (blocked_files -> .blocked_files,
	// duplicates_files -> .duplicates_files).
	migrateLegacyDir(filepath.Join(cfg.InputDir, "blocked_files"), filepath.Join(cfg.InputDir, ".blocked_files"))
	migrateLegacyDir(filepath.Join(cfg.InputDir, "duplicates_files"), filepath.Join(cfg.InputDir, ".duplicates_files"))
	return nil
}

// migrateLegacyDir moves each entry of legacy into target, falling back to copy+unlink when rename
// fails, then removes legacy if it ended up empty. Every failure is swallowed, as in TS.
func migrateLegacyDir(legacy, target string) {
	entries, err := os.ReadDir(legacy)
	if err != nil {
		return
	}
	for _, entry := range entries {
		oldPath := filepath.Join(legacy, entry.Name())
		newPath := filepath.Join(target, entry.Name())
		if _, err := os.Stat(oldPath); err != nil {
			continue
		}
		if err := os.Rename(oldPath, newPath); err != nil {
			if err := copyFile(oldPath, newPath); err == nil {
				_ = os.Remove(oldPath)
			}
		}
	}
	// os.Remove only deletes an empty directory, matching fs.rmdirSync.
	_ = os.Remove(legacy)
}

// NormalizePathInput is the historical re-export of pathconv.WindowsToWSLPathHost (TS
// `windowsToWslPath`). On WSL it turns Windows spellings into the /mnt/<drive>/... form the app's
// filesystem needs; on a Windows host it is a no-op.
func NormalizePathInput(raw string) string { return pathconv.WindowsToWSLPathHost(raw) }

// ToWindowsPath is the historical re-export of pathconv.WSLToWindowsPath (TS `wslToWindowsPath`):
// the form handed to Windows programs (Explorer, Chrome).
func ToWindowsPath(input string) string { return pathconv.WSLToWindowsPath(input) }

// IsWSLMountPath reports whether the path is a WSL mount path /mnt/<letter>[/...].
func IsWSLMountPath(input string) bool { return pathconv.IsWSLMountPath(input) }

// ---- JS-semantics helpers ----------------------------------------------------------------------

// jsString is String(value) for the JSON-decoded scalar types settings.ts can encounter.
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'f', -1, 32)
	case int:
		return strconv.Itoa(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case json.Number:
		return t.String()
	default:
		return ""
	}
}

// jsTruthy is JS truthiness for JSON-decoded values. NaN has no Go representation; all JSON
// numbers are finite.
func jsTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0
	default:
		return true
	}
}

// firstTruthy is JS `a || b || c`.
func firstTruthy(values ...any) any {
	for _, v := range values {
		if jsTruthy(v) {
			return v
		}
	}
	return nil
}

// firstNonEmpty is JS `a || b || c` for already-string values.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// parseIntJS is `parseInt(s, 10)`: leading whitespace and sign, then decimal digits until a
// non-digit. ok is false when there is no digit (JS NaN).
func parseIntJS(s string) (int, bool) {
	s = strings.TrimLeft(s, " \t\n\r\f\v\u00a0\ufeff")
	if s == "" {
		return 0, false
	}
	sign := 1
	i := 0
	if s[0] == '+' {
		i = 1
	} else if s[0] == '-' {
		sign = -1
		i = 1
	}
	start := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == start {
		return 0, false
	}
	n, err := strconv.Atoi(s[start:i])
	if err != nil {
		return 0, false
	}
	return sign * n, true
}

// parsePort converts a port env value, falling back to def when it is not a number. TS stored NaN
// in this case; a Go int cannot, and the port is unusable either way.
func parsePort(s string, def int) int {
	if v, ok := parseIntJS(s); ok {
		return v
	}
	return def
}

// parseTimeout is the TS `Number.isFinite(raw) && raw >= 0 ? raw : 0` check.
func parseTimeout(s string) int {
	v, ok := parseIntJS(s)
	if !ok || v < 0 {
		return 0
	}
	return v
}

// marshalSettings is `JSON.stringify(data, null, 2)`: two-space indent, no HTML escaping (Go
// escapes <, > and & by default), and no trailing newline.
func marshalSettings(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// writeFileAtomic writes data to a temp file next to path and renames it over path, so a crash
// mid-write cannot truncate settings.json. On a Windows EPERM/EBUSY rename failure it falls back to
// a copy, mirroring infra/jsonregistry.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) error {
	tmpPath := path + ".tmp"
	if err := os.WriteFile(tmpPath, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		if isPermOrBusy(err) {
			if copyErr := copyFile(tmpPath, path); copyErr != nil {
				return copyErr
			}
			_ = os.Remove(tmpPath)
			return nil
		}
		return err
	}
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
