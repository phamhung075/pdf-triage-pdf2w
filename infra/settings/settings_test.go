package settings

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These cases are ported from pdf-triage's src/infrastructure/settings.test.ts (15 cases). The
// upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/settings.test.ts` -> 15 passed), so no upstream case is pinned
// red. The TS suite mocks `fs` entirely; this port uses real files under t.TempDir() instead, which
// is both closer to production and incapable of touching the real settings.json.
//
// Environment isolation: every test unsets the full set of variables settings.ts reads before it
// builds a store, and sets only the ones it needs through t.Setenv (see isolateEnv). Nothing here
// reads or writes the developer's real environment or settings.json.

// knownEnv is every environment variable the port reads (PDF_TRIAGE_PDF2W_SERVICE_* are deliberately
// not read at all — see the package comment). Clearing all of them makes a test independent of the
// shell it runs in.
var knownEnv = []string{
	"PDF_TRIAGE_BASE_DIR",
	"PDF_TRIAGE_DATA_DIR",
	"SYSTEM_LANGUAGE",
	"PDF_INPUT_DIR",
	"PDF_OUTPUT_DIR",
	"PDF_REGISTRY_PATH",
	"PDF_DB_PATH",
	"OLLAMA_HOST",
	"OLLAMA_MODEL",
	"OLLAMA_EMBED_MODEL",
	"OLLAMA_VISION_MODEL",
	"VISION_LAB_PORT",
	"PORT",
	"PDF_TRIAGE_HOST",
	"PDF2W_SERVICE_URL",
	"PDF2W_SERVICE_TIMEOUT_MS",
	"MCP_HTTP_PORT",
	"MCP_HTTP_HOST",
}

// isolateEnv truly unsets each known variable (t.Setenv cannot unset, and dotenv's "already present"
// rule depends on set-vs-unset), then restores the original process environment on cleanup. Tests
// then set values with t.Setenv. Tests are deliberately not parallel.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, name := range knownEnv {
		orig, ok := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("Unsetenv(%s): %v", name, err)
		}
		if ok {
			t.Cleanup(func() { _ = os.Setenv(name, orig) })
		} else {
			t.Cleanup(func() { _ = os.Unsetenv(name) })
		}
	}
}

type setup struct {
	t      *testing.T
	base   string
	data   string
	stderr *bytes.Buffer
}

func newSetup(t *testing.T) *setup {
	t.Helper()
	isolateEnv(t)
	return &setup{t: t, base: t.TempDir(), data: t.TempDir(), stderr: &bytes.Buffer{}}
}

func (s *setup) setEnv(name, value string) { s.t.Setenv(name, value) }

func (s *setup) writeFile(dir, name, body string) string {
	s.t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		s.t.Fatalf("WriteFile(%s): %v", path, err)
	}
	return path
}

func (s *setup) writeSettings(body string) { s.writeFile(s.data, "settings.json", body) }

func (s *setup) open() *Store {
	s.t.Helper()
	st, err := New(Options{BaseDir: s.base, DataDir: s.data, Stderr: s.stderr})
	if err != nil {
		s.t.Fatalf("New: %v", err)
	}
	return st
}

func strp(s string) *string { return &s }

func readSettingsFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("Unmarshal(%s): %v\nraw=%s", path, err, raw)
	}
	return parsed
}

func wantStrings(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s = %v, want %v", name, got, want)
		}
	}
}

// ---- loadCustomSettings (ported: 3 cases) ------------------------------------------------------

func TestLoadCustomSettings(t *testing.T) {
	t.Run("returns an empty map when settings.json does not exist", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		got := LoadCustomSettings(path, &bytes.Buffer{})
		if len(got) != 0 {
			t.Fatalf("LoadCustomSettings = %v, want {}", got)
		}
	})

	t.Run("returns the parsed object when settings.json is valid JSON", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(`{"input_dir":"X"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		got := LoadCustomSettings(path, &bytes.Buffer{})
		if got["input_dir"] != "X" {
			t.Fatalf("input_dir = %v, want X", got["input_dir"])
		}
	})

	t.Run("returns an empty map (not a panic) when settings.json is malformed", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "settings.json")
		if err := os.WriteFile(path, []byte(`{not valid json`), 0o644); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		got := LoadCustomSettings(path, &stderr)
		if len(got) != 0 {
			t.Fatalf("LoadCustomSettings = %v, want {}", got)
		}
		if !strings.Contains(stderr.String(), "Error reading settings.json") {
			t.Fatalf("stderr = %q, want the TS console.error message", stderr.String())
		}
	})
}

// ---- CONFIG derivation at module load (ported: 7 cases) ----------------------------------------

func TestConfigDerivation(t *testing.T) {
	t.Run("picks up input_dir/output_root_dir/ollama_host/personal_name_denylist from settings.json", func(t *testing.T) {
		s := newSetup(t)
		s.writeSettings(`{"input_dir":"/custom/in","output_root_dir":"/custom/out",` +
			`"ollama_host":"http://custom-host:1234","personal_name_denylist":["Alice"," Bob "]}`)
		cfg := s.open().Config()

		if cfg.InputDir != "/custom/in" {
			t.Fatalf("INPUT_DIR = %q, want /custom/in", cfg.InputDir)
		}
		if cfg.OutputRootDir != "/custom/out" {
			t.Fatalf("OUTPUT_ROOT_DIR = %q, want /custom/out", cfg.OutputRootDir)
		}
		if cfg.OllamaHost != "http://custom-host:1234" {
			t.Fatalf("OLLAMA_HOST = %q, want http://custom-host:1234", cfg.OllamaHost)
		}
		wantStrings(t, "PERSONAL_NAME_DENYLIST", cfg.PersonalNameDenylist, []string{"alice", "bob"})
	})

	t.Run("rejects an unsupported ollama_model and falls back to qwen3.5:9b (Golden Rule #14)", func(t *testing.T) {
		s := newSetup(t)
		s.writeSettings(`{"ollama_model":"kimi-k3:cloud"}`)
		cfg := s.open().Config()
		if cfg.OllamaModel != "qwen3.5:9b" {
			t.Fatalf("OLLAMA_MODEL = %q, want qwen3.5:9b", cfg.OllamaModel)
		}
		if !strings.Contains(s.stderr.String(), "kimi-k3:cloud") {
			t.Fatalf("stderr = %q, want a warning naming the rejected model", s.stderr.String())
		}
	})

	t.Run("defaults OLLAMA_VISION_MODEL to minicpm-v4.6:latest with no env override", func(t *testing.T) {
		cfg := newSetup(t).open().Config()
		if cfg.OllamaVisionModel != "minicpm-v4.6:latest" {
			t.Fatalf("OLLAMA_VISION_MODEL = %q, want minicpm-v4.6:latest", cfg.OllamaVisionModel)
		}
	})

	t.Run("rejects an unsupported OLLAMA_VISION_MODEL env override and falls back", func(t *testing.T) {
		s := newSetup(t)
		s.setEnv("OLLAMA_VISION_MODEL", "llava:7b")
		cfg := s.open().Config()
		if cfg.OllamaVisionModel != "minicpm-v4.6:latest" {
			t.Fatalf("OLLAMA_VISION_MODEL = %q, want minicpm-v4.6:latest", cfg.OllamaVisionModel)
		}
		if !strings.Contains(s.stderr.String(), "llava:7b") {
			t.Fatalf("stderr = %q, want a warning naming llava:7b", s.stderr.String())
		}
	})

	t.Run("defaults VISION_LAB_PORT to 3179 with no env override", func(t *testing.T) {
		cfg := newSetup(t).open().Config()
		if cfg.VisionLabPort != 3179 {
			t.Fatalf("VISION_LAB_PORT = %d, want 3179", cfg.VisionLabPort)
		}
	})

	t.Run("reads VISION_LAB_PORT from an env override", func(t *testing.T) {
		s := newSetup(t)
		s.setEnv("VISION_LAB_PORT", "4000")
		if got := s.open().Config().VisionLabPort; got != 4000 {
			t.Fatalf("VISION_LAB_PORT = %d, want 4000", got)
		}
	})

	t.Run("defaults PERSONAL_NAME_DENYLIST to an empty array when settings.json has none", func(t *testing.T) {
		cfg := newSetup(t).open().Config()
		if cfg.PersonalNameDenylist == nil {
			t.Fatal("PERSONAL_NAME_DENYLIST is nil, want a non-nil empty slice")
		}
		if len(cfg.PersonalNameDenylist) != 0 {
			t.Fatalf("PERSONAL_NAME_DENYLIST = %v, want []", cfg.PersonalNameDenylist)
		}
	})
}

// ---- updateConfig (ported: 1 case) -------------------------------------------------------------

func TestUpdateConfig(t *testing.T) {
	t.Run("mutates CONFIG in place and persists sanitized settings to disk", func(t *testing.T) {
		s := newSetup(t)
		st := s.open()

		// The TS case used '/new/in'; because this port drives the real filesystem (the TS suite
		// mocks fs), the path must live under the temp dir so ensureDirectoriesExist can create it.
		newIn := filepath.Join(s.base, "new-in")
		if err := st.UpdateConfig(UpdateSettings{
			InputDir:    strp(newIn),
			OllamaModel: strp("not-allowed-model"),
		}); err != nil {
			t.Fatalf("UpdateConfig: %v", err)
		}

		cfg := st.Config()
		if cfg.InputDir != newIn {
			t.Fatalf("INPUT_DIR = %q, want %q", cfg.InputDir, newIn)
		}
		if cfg.OllamaModel != "qwen3.5:9b" {
			t.Fatalf("OLLAMA_MODEL = %q, want qwen3.5:9b", cfg.OllamaModel)
		}

		if filepath.Base(st.SettingsFile()) != "settings.json" {
			t.Fatalf("SETTINGS_FILE = %q, want a path ending in settings.json", st.SettingsFile())
		}
		raw, err := os.ReadFile(st.SettingsFile())
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if !strings.Contains(string(raw), `"qwen3.5:9b"`) {
			t.Fatalf("settings.json = %s, want it to contain \"qwen3.5:9b\"", raw)
		}
		if _, err := os.Stat(st.SettingsFile() + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf(".tmp left behind after atomic write, stat err = %v", err)
		}
	})
}

// ---- reloadConfigFromDisk (ported: 2 cases) ----------------------------------------------------

func TestReloadConfigFromDisk(t *testing.T) {
	t.Run("re-reads settings.json and mutates the existing CONFIG object", func(t *testing.T) {
		s := newSetup(t)
		s.writeSettings(`{"input_dir":"/first"}`)
		st := s.open()
		if got := st.Config().InputDir; got != "/first" {
			t.Fatalf("INPUT_DIR = %q, want /first", got)
		}

		s.writeSettings(`{"input_dir":"/second"}`)
		st.ReloadFromDisk()
		if got := st.Config().InputDir; got != "/second" {
			t.Fatalf("INPUT_DIR after reload = %q, want /second", got)
		}
	})

	t.Run("normalizes a mangled Windows path from settings.json into a real WSL /mnt path", func(t *testing.T) {
		// Mirrors the TS `it.skipIf(process.platform === 'win32')`: on a Windows host
		// WindowsToWSLPath is a deliberate no-op, so the TS suite skips it too.
		if runtime.GOOS == "windows" {
			t.Skip("windows host: WindowsToWSLPath is a no-op, matching the TS skipIf(win32)")
		}
		s := newSetup(t)
		s.writeSettings(`{"input_dir":"\\mnt\\C:\\Users\\you\\Documents\\__raws",` +
			`"output_root_dir":"C:\\Users\\you\\Documents\\__archive"}`)
		st := s.open()
		st.ReloadFromDisk()
		cfg := st.Config()
		if cfg.InputDir != "/mnt/c/Users/you/Documents/__raws" {
			t.Fatalf("INPUT_DIR = %q, want /mnt/c/Users/you/Documents/__raws", cfg.InputDir)
		}
		if cfg.OutputRootDir != "/mnt/c/Users/you/Documents/__archive" {
			t.Fatalf("OUTPUT_ROOT_DIR = %q, want /mnt/c/Users/you/Documents/__archive", cfg.OutputRootDir)
		}
	})
}

// ---- path-conversion re-exports (ported: 2 cases) ----------------------------------------------

func TestPathConversionReExports(t *testing.T) {
	// The conversion logic itself lives and is fully tested in the pathconv package; settings only
	// re-exports it under the historical names so existing importers keep working. These two
	// assertions pin that the re-export stays wired. Deviation: the TS re-export call passed an
	// explicit platform ('linux'), which Go's host-based wrapper cannot; the expectation is
	// therefore host-conditional (on Windows the conversion is a no-op by design).
	t.Run("re-exports windowsToWslPath as normalizePathInput", func(t *testing.T) {
		want := "/mnt/c/Users/you/__raws"
		if runtime.GOOS == "windows" {
			want = `C:\Users\you\__raws`
		}
		if got := NormalizePathInput(`C:\Users\you\__raws`); got != want {
			t.Fatalf("normalizePathInput = %q, want %q", got, want)
		}
	})

	t.Run("re-exports wslToWindowsPath as toWindowsPath", func(t *testing.T) {
		if got := ToWindowsPath("/mnt/c/Users/you/__raws"); got != `C:\Users\you\__raws` {
			t.Fatalf("toWindowsPath = %q, want %q", got, `C:\Users\you\__raws`)
		}
	})
}

// ---- added cases: defaults and every remaining CONFIG key --------------------------------------

func TestConfigDefaults(t *testing.T) {
	s := newSetup(t)
	st := s.open()
	cfg := st.Config()

	checks := []struct {
		name string
		got  string
		want string
	}{
		{"LANGUAGE", cfg.Language, "FR"},
		{"OLLAMA_HOST", cfg.OllamaHost, "http://127.0.0.1:11434"},
		{"OLLAMA_EMBED_MODEL", cfg.OllamaEmbedModel, "nomic-embed-text"},
		{"HOST", cfg.Host, "127.0.0.1"},
		{"PDF2W_SERVICE_URL", cfg.PDF2WServiceURL, ""},
		{"MCP_HTTP_HOST", cfg.MCPHTTPHost, "0.0.0.0"},
		{"INPUT_DIR", cfg.InputDir, filepath.Join(s.data, "input")},
		{"OUTPUT_ROOT_DIR", cfg.OutputRootDir, filepath.Join(s.data, "organized")},
		{"JSON_REGISTRY_PATH", cfg.JSONRegistryPath, filepath.Join(s.data, "registry.json")},
		{"DB_PATH", cfg.DBPath, filepath.Join(s.data, "pdf_triage.db")},
		{"CATEGORIES_FILE", cfg.CategoriesFile, filepath.Join(s.base, "categories.json")},
		{"CATEGORIES_PRIVATE_FILE", cfg.CategoriesPrivateFile, filepath.Join(s.data, ".categories.private.json")},
		{"ENTITY_DICTIONARY_FILE", cfg.EntityDictionaryFile, filepath.Join(s.base, "entity_dictionary.json")},
		{"MANUAL_DECISIONS_FILE", cfg.ManualDecisionsFile, filepath.Join(s.data, "manual_decisions.json")},
		{"TAXONOMY_HINTS_FILE", cfg.TaxonomyHintsFile, filepath.Join(s.data, "taxonomy_hints.json")},
		{"PROMPTS_DIR", cfg.PromptsDir, filepath.Join(s.base, "prompts")},
		{"PROMPTS_PRIVATE_FILE", cfg.PromptsPrivateFile, filepath.Join(s.data, ".prompts.private.json")},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Fatalf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
	if cfg.Port != 3971 {
		t.Fatalf("PORT = %d, want 3971", cfg.Port)
	}
	if cfg.MCPHTTPPort != 3972 {
		t.Fatalf("MCP_HTTP_PORT = %d, want 3972", cfg.MCPHTTPPort)
	}
	if cfg.PDF2WServiceTimeoutMS != 0 {
		t.Fatalf("PDF2W_SERVICE_TIMEOUT_MS = %d, want 0", cfg.PDF2WServiceTimeoutMS)
	}
}

func TestConfigEnvOverrides(t *testing.T) {
	s := newSetup(t)
	for k, v := range map[string]string{
		"SYSTEM_LANGUAGE":          "EN",
		"PDF_INPUT_DIR":            "/env/in",
		"PDF_OUTPUT_DIR":           "/env/out",
		"PDF_REGISTRY_PATH":        "/env/registry.json",
		"PDF_DB_PATH":              "/env/db.sqlite",
		"OLLAMA_HOST":              "http://env-host:1",
		"OLLAMA_EMBED_MODEL":       "env-embed",
		"PORT":                     "4123",
		"MCP_HTTP_PORT":            "5123",
		"MCP_HTTP_HOST":            "127.0.0.2",
		"PDF_TRIAGE_HOST":          "0.0.0.0",
		"PDF2W_SERVICE_URL":        " http://pdf2w:9 ",
		"PDF2W_SERVICE_TIMEOUT_MS": "1500",
	} {
		s.setEnv(k, v)
	}
	cfg := s.open().Config()

	if cfg.Language != "EN" {
		t.Fatalf("LANGUAGE = %q, want EN", cfg.Language)
	}
	if cfg.InputDir != "/env/in" {
		t.Fatalf("INPUT_DIR = %q, want /env/in", cfg.InputDir)
	}
	if cfg.OutputRootDir != "/env/out" {
		t.Fatalf("OUTPUT_ROOT_DIR = %q, want /env/out", cfg.OutputRootDir)
	}
	if cfg.JSONRegistryPath != "/env/registry.json" {
		t.Fatalf("JSON_REGISTRY_PATH = %q", cfg.JSONRegistryPath)
	}
	if cfg.DBPath != "/env/db.sqlite" {
		t.Fatalf("DB_PATH = %q", cfg.DBPath)
	}
	if cfg.OllamaHost != "http://env-host:1" {
		t.Fatalf("OLLAMA_HOST = %q", cfg.OllamaHost)
	}
	if cfg.OllamaEmbedModel != "env-embed" {
		t.Fatalf("OLLAMA_EMBED_MODEL = %q", cfg.OllamaEmbedModel)
	}
	if cfg.Port != 4123 {
		t.Fatalf("PORT = %d, want 4123", cfg.Port)
	}
	if cfg.MCPHTTPPort != 5123 {
		t.Fatalf("MCP_HTTP_PORT = %d, want 5123", cfg.MCPHTTPPort)
	}
	if cfg.MCPHTTPHost != "127.0.0.2" {
		t.Fatalf("MCP_HTTP_HOST = %q", cfg.MCPHTTPHost)
	}
	if cfg.Host != "0.0.0.0" {
		t.Fatalf("HOST = %q", cfg.Host)
	}
	if cfg.PDF2WServiceURL != "http://pdf2w:9" {
		t.Fatalf("PDF2W_SERVICE_URL = %q, want the trimmed value", cfg.PDF2WServiceURL)
	}
	if cfg.PDF2WServiceTimeoutMS != 1500 {
		t.Fatalf("PDF2W_SERVICE_TIMEOUT_MS = %d, want 1500", cfg.PDF2WServiceTimeoutMS)
	}
}

// A settings.json value beats the matching environment variable, matching the TS `||` chain order.
func TestSettingsJSONBeatsEnv(t *testing.T) {
	s := newSetup(t)
	s.writeSettings(`{"input_dir":"/settings/in","ollama_host":"http://settings-host:1"}`)
	s.setEnv("PDF_INPUT_DIR", "/env/in")
	s.setEnv("OLLAMA_HOST", "http://env-host:1")
	cfg := s.open().Config()

	if cfg.InputDir != "/settings/in" {
		t.Fatalf("INPUT_DIR = %q, want /settings/in", cfg.InputDir)
	}
	if cfg.OllamaHost != "http://settings-host:1" {
		t.Fatalf("OLLAMA_HOST = %q, want http://settings-host:1", cfg.OllamaHost)
	}
}

// An invalid settings.json ollama_model sanitizes to the default and does NOT fall through to the
// env override, matching `sanitizeOllamaModel(customSettings.ollama_model || process.env...)`.
func TestUnsupportedSettingsModelDoesNotFallThroughToEnv(t *testing.T) {
	s := newSetup(t)
	s.writeSettings(`{"ollama_model":"kimi-k3:cloud"}`)
	s.setEnv("OLLAMA_MODEL", "qwen3.5:9b")
	if got := s.open().Config().OllamaModel; got != "qwen3.5:9b" {
		t.Fatalf("OLLAMA_MODEL = %q, want the pinned default", got)
	}
}

// ---- added cases: BASE_DIR / DATA_DIR resolution ----------------------------------------------

func TestBaseDirResolution(t *testing.T) {
	t.Run("uses the explicit parameter when PDF_TRIAGE_BASE_DIR is unset", func(t *testing.T) {
		s := newSetup(t)
		if got := s.open().BaseDir(); got != s.base {
			t.Fatalf("BaseDir = %q, want %q", got, s.base)
		}
	})

	t.Run("PDF_TRIAGE_BASE_DIR overrides the explicit parameter", func(t *testing.T) {
		s := newSetup(t)
		alt := t.TempDir()
		s.setEnv("PDF_TRIAGE_BASE_DIR", alt)
		st := s.open()
		if got := st.BaseDir(); got != alt {
			t.Fatalf("BaseDir = %q, want %q", got, alt)
		}
		if got := st.Config().CategoriesFile; got != filepath.Join(alt, "categories.json") {
			t.Fatalf("CATEGORIES_FILE = %q, want it under the env base dir", got)
		}
	})
}

func TestDataDirResolution(t *testing.T) {
	t.Run("falls back to BASE_DIR when no DATA_DIR is given", func(t *testing.T) {
		s := newSetup(t)
		st, err := New(Options{BaseDir: s.base, Stderr: s.stderr})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if got := st.DataDir(); got != s.base {
			t.Fatalf("DataDir = %q, want %q", got, s.base)
		}
		if got := st.SettingsFile(); got != filepath.Join(s.base, "settings.json") {
			t.Fatalf("SettingsFile = %q", got)
		}
	})

	t.Run("PDF_TRIAGE_DATA_DIR overrides the explicit parameter", func(t *testing.T) {
		s := newSetup(t)
		alt := t.TempDir()
		s.setEnv("PDF_TRIAGE_DATA_DIR", alt)
		st := s.open()
		if got := st.DataDir(); got != alt {
			t.Fatalf("DataDir = %q, want %q", got, alt)
		}
		if got := st.SettingsFile(); got != filepath.Join(alt, "settings.json") {
			t.Fatalf("SettingsFile = %q", got)
		}
	})

	t.Run("creates DATA_DIR when it does not exist", func(t *testing.T) {
		s := newSetup(t)
		missing := filepath.Join(s.base, "nested", "data")
		if _, err := New(Options{BaseDir: s.base, DataDir: missing, Stderr: s.stderr}); err != nil {
			t.Fatalf("New: %v", err)
		}
		info, err := os.Stat(missing)
		if err != nil || !info.IsDir() {
			t.Fatalf("DATA_DIR was not created: stat err = %v", err)
		}
	})
}

// The DATA_DIR split: read-only assets under BASE_DIR, writable state under DATA_DIR.
func TestDataDirSplit(t *testing.T) {
	s := newSetup(t)
	cfg := s.open().Config()
	if cfg.CategoriesFile != filepath.Join(s.base, "categories.json") {
		t.Fatalf("CATEGORIES_FILE = %q, want under BASE_DIR", cfg.CategoriesFile)
	}
	if cfg.CategoriesPrivateFile != filepath.Join(s.data, ".categories.private.json") {
		t.Fatalf("CATEGORIES_PRIVATE_FILE = %q, want under DATA_DIR", cfg.CategoriesPrivateFile)
	}
}

// ---- added cases: .env loading (dotenv semantics) ----------------------------------------------

func TestParseDotEnv(t *testing.T) {
	src := strings.Join([]string{
		"# a comment",
		"FOO=bar",
		"",
		"export EXPORTED=value",
		`QUOTED="hello world"`,
		`SINGLE='literal \n'`,
		"INLINE=value # trailing comment",
		"HASH=va#lue",
		"EMPTY=",
		`DOUBLE_ESC="line1\nline2"`,
		"COLON: colonvalue",
		"NOT_A_PAIR='unterminated",
	}, "\n")

	got := ParseDotEnv(src)
	want := map[string]string{
		"FOO":        "bar",
		"EXPORTED":   "value",
		"QUOTED":     "hello world",
		"SINGLE":     `literal \n`,
		"INLINE":     "value",
		"HASH":       "va",
		"EMPTY":      "",
		"DOUBLE_ESC": "line1\nline2",
		"COLON":      "colonvalue",
		"NOT_A_PAIR": "'unterminated",
	}
	if len(got) != len(want) {
		t.Fatalf("ParseDotEnv size = %d (%v), want %d", len(got), got, len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("ParseDotEnv[%q] = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["# a comment"]; ok {
		t.Fatal("comment line was parsed as a key")
	}
}

func TestDotEnvLoading(t *testing.T) {
	t.Run("reads DATA_DIR/.env and it feeds CONFIG", func(t *testing.T) {
		s := newSetup(t)
		s.writeFile(s.data, ".env", "PORT=4123\nPDF2W_SERVICE_URL=http://dotenv:9\n")
		cfg := s.open().Config()
		if cfg.Port != 4123 {
			t.Fatalf("PORT = %d, want 4123", cfg.Port)
		}
		if cfg.PDF2WServiceURL != "http://dotenv:9" {
			t.Fatalf("PDF2W_SERVICE_URL = %q, want http://dotenv:9", cfg.PDF2WServiceURL)
		}
	})

	t.Run("prefers DATA_DIR/.env over BASE_DIR/.env", func(t *testing.T) {
		s := newSetup(t)
		s.writeFile(s.base, ".env", "PORT=1111\n")
		s.writeFile(s.data, ".env", "PORT=2222\n")
		if got := s.open().Config().Port; got != 2222 {
			t.Fatalf("PORT = %d, want 2222", got)
		}
	})

	t.Run("falls back to BASE_DIR/.env when DATA_DIR has none", func(t *testing.T) {
		s := newSetup(t)
		s.writeFile(s.base, ".env", "PORT=1111\n")
		if got := s.open().Config().Port; got != 1111 {
			t.Fatalf("PORT = %d, want 1111", got)
		}
	})

	t.Run("does not override an already-set process environment variable", func(t *testing.T) {
		s := newSetup(t)
		s.setEnv("PDF2W_SERVICE_URL", "http://from-process-env")
		s.writeFile(s.data, ".env", "PDF2W_SERVICE_URL=http://from-dotenv\n")
		if got := s.open().Config().PDF2WServiceURL; got != "http://from-process-env" {
			t.Fatalf("PDF2W_SERVICE_URL = %q, want the process env value", got)
		}
	})

	t.Run("ignores PDF_TRIAGE_BASE_DIR set only in .env (resolved before dotenv loads)", func(t *testing.T) {
		s := newSetup(t)
		s.writeFile(s.data, ".env", "PDF_TRIAGE_BASE_DIR=/from/dotenv\n")
		if got := s.open().BaseDir(); got != s.base {
			t.Fatalf("BaseDir = %q, want %q (dotenv must not move BASE_DIR)", got, s.base)
		}
	})
}

// ---- added cases: pure sanitizers --------------------------------------------------------------

func TestSanitizers(t *testing.T) {
	t.Run("sanitizeLanguage", func(t *testing.T) {
		cases := []struct {
			in   any
			want string
		}{
			{"EN", "EN"},
			{"en", "EN"},
			{"FR", "FR"},
			{"de", "FR"},
			{"", "FR"},
			{nil, "FR"},
			{123, "FR"},
		}
		for _, c := range cases {
			if got := sanitizeLanguage(c.in); got != c.want {
				t.Fatalf("sanitizeLanguage(%v) = %q, want %q", c.in, got, c.want)
			}
		}
	})

	t.Run("sanitizeOllamaModel", func(t *testing.T) {
		var stderr bytes.Buffer
		if got := sanitizeOllamaModel("qwen3.5:9b", &stderr); got != "qwen3.5:9b" {
			t.Fatalf("allowed model = %q", got)
		}
		if got := sanitizeOllamaModel("kimi-k3:cloud", &stderr); got != "qwen3.5:9b" {
			t.Fatalf("rejected model = %q, want the pinned default", got)
		}
		if !strings.Contains(stderr.String(), "kimi-k3:cloud") {
			t.Fatalf("warning = %q, want it to name the rejected model", stderr.String())
		}
		var quiet bytes.Buffer
		if got := sanitizeOllamaModel(nil, &quiet); got != "qwen3.5:9b" {
			t.Fatalf("nil model = %q", got)
		}
		if quiet.Len() != 0 {
			t.Fatalf("nil model warned: %q", quiet.String())
		}
	})

	t.Run("sanitizeOllamaVisionModel", func(t *testing.T) {
		var stderr bytes.Buffer
		if got := sanitizeOllamaVisionModel("minicpm-v4.6:latest", &stderr); got != "minicpm-v4.6:latest" {
			t.Fatalf("allowed vision model = %q", got)
		}
		if got := sanitizeOllamaVisionModel("llava:7b", &stderr); got != "minicpm-v4.6:latest" {
			t.Fatalf("rejected vision model = %q", got)
		}
	})

	t.Run("sanitizePersonalNameDenylist", func(t *testing.T) {
		if got := sanitizePersonalNameDenylist(nil); len(got) != 0 || got == nil {
			t.Fatalf("nil denylist = %v, want non-nil empty", got)
		}
		if got := sanitizePersonalNameDenylist("not-a-list"); len(got) != 0 {
			t.Fatalf("non-array denylist = %v, want empty", got)
		}
		got := sanitizePersonalNameDenylist([]any{"Alice", " Bob ", "", "  ", nil, 42})
		wantStrings(t, "denylist", got, []string{"alice", "bob", "null", "42"})
		if got := sanitizePersonalNameDenylist([]string{"Carol", " dave "}); len(got) != 2 || got[0] != "carol" || got[1] != "dave" {
			t.Fatalf("[]string denylist = %v", got)
		}
	})
}

// ---- added cases: parse helpers (JS parseInt / finite timeout) ---------------------------------

func TestParseIntJS(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"4000", 4000, true},
		{" 8080 ", 8080, true},
		{"12abc", 12, true},
		{"+5", 5, true},
		{"-5", -5, true},
		{"", 0, false},
		{"abc", 0, false},
		{"0x10", 0, true}, // radix 10: "0x10" parses to 0 then stops at 'x'
	}
	for _, c := range cases {
		got, ok := parseIntJS(c.in)
		if got != c.want || ok != c.ok {
			t.Fatalf("parseIntJS(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestNumericEnvParsing(t *testing.T) {
	t.Run("invalid PORT falls back to the default", func(t *testing.T) {
		s := newSetup(t)
		s.setEnv("PORT", "not-a-number")
		if got := s.open().Config().Port; got != 3971 {
			t.Fatalf("PORT = %d, want 3971", got)
		}
	})

	t.Run("PDF2W_SERVICE_TIMEOUT_MS table", func(t *testing.T) {
		cases := []struct {
			value string
			want  int
		}{
			{"", 0},
			{"abc", 0},
			{"-5", 0},
			{"0", 0},
			{"12abc", 12},
			{"2500", 2500},
		}
		for _, c := range cases {
			s := newSetup(t)
			s.setEnv("PDF2W_SERVICE_TIMEOUT_MS", c.value)
			if got := s.open().Config().PDF2WServiceTimeoutMS; got != c.want {
				t.Fatalf("PDF2W_SERVICE_TIMEOUT_MS=%q -> %d, want %d", c.value, got, c.want)
			}
		}
	})
}

// ---- added cases: EnsureDirectoriesExist -------------------------------------------------------

func TestEnsureDirectoriesExist(t *testing.T) {
	t.Run("creates the managed directories", func(t *testing.T) {
		s := newSetup(t)
		in := filepath.Join(s.base, "in")
		out := filepath.Join(s.base, "out")
		s.writeSettings(`{"input_dir":` + quote(in) + `,"output_root_dir":` + quote(out) + `}`)
		st := s.open()
		if err := st.EnsureDirectoriesExist(); err != nil {
			t.Fatalf("EnsureDirectoriesExist: %v", err)
		}
		cfg := st.Config()
		for _, dir := range []string{
			cfg.InputDir,
			filepath.Join(cfg.InputDir, ".duplicates_files"),
			filepath.Join(cfg.InputDir, ".blocked_files"),
			filepath.Join(cfg.InputDir, ".delete_files"),
			cfg.OutputRootDir,
			filepath.Dir(cfg.JSONRegistryPath),
			filepath.Dir(cfg.DBPath),
		} {
			info, err := os.Stat(dir)
			if err != nil || !info.IsDir() {
				t.Fatalf("directory %s missing: %v", dir, err)
			}
		}
	})

	t.Run("migrates legacy blocked_files/duplicates_files folders", func(t *testing.T) {
		s := newSetup(t)
		in := filepath.Join(s.base, "in")
		out := filepath.Join(s.base, "out")
		s.writeSettings(`{"input_dir":` + quote(in) + `,"output_root_dir":` + quote(out) + `}`)

		legacyBlocked := filepath.Join(in, "blocked_files")
		legacyDups := filepath.Join(in, "duplicates_files")
		for _, d := range []string{legacyBlocked, legacyDups} {
			if err := os.MkdirAll(d, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.WriteFile(filepath.Join(legacyBlocked, "a.pdf"), []byte("blocked"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(legacyDups, "d.pdf"), []byte("dup"), 0o644); err != nil {
			t.Fatal(err)
		}

		st := s.open()
		if err := st.EnsureDirectoriesExist(); err != nil {
			t.Fatalf("EnsureDirectoriesExist: %v", err)
		}

		for _, pair := range [][2]string{
			{filepath.Join(in, ".blocked_files", "a.pdf"), "blocked"},
			{filepath.Join(in, ".duplicates_files", "d.pdf"), "dup"},
		} {
			raw, err := os.ReadFile(pair[0])
			if err != nil {
				t.Fatalf("migrated file %s missing: %v", pair[0], err)
			}
			if string(raw) != pair[1] {
				t.Fatalf("%s content = %q, want %q", pair[0], raw, pair[1])
			}
		}
		if _, err := os.Stat(legacyBlocked); !os.IsNotExist(err) {
			t.Fatalf("legacy blocked_files still exists: %v", err)
		}
		if _, err := os.Stat(legacyDups); !os.IsNotExist(err) {
			t.Fatalf("legacy duplicates_files still exists: %v", err)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		s := newSetup(t)
		st := s.open()
		if err := st.EnsureDirectoriesExist(); err != nil {
			t.Fatalf("first EnsureDirectoriesExist: %v", err)
		}
		if err := st.EnsureDirectoriesExist(); err != nil {
			t.Fatalf("second EnsureDirectoriesExist: %v", err)
		}
	})
}

// ---- added cases: isFirstRun -------------------------------------------------------------------

func TestIsFirstRun(t *testing.T) {
	s := newSetup(t)
	st := s.open()
	if !st.IsFirstRun() {
		t.Fatal("IsFirstRun = false with no settings.json, want true")
	}

	s.writeSettings(`{"input_dir":"/only-input"}`)
	st.ReloadFromDisk()
	if !st.IsFirstRun() {
		t.Fatal("IsFirstRun = false with only input_dir set, want true")
	}

	s.writeSettings(`{"input_dir":"/in","output_root_dir":"/out"}`)
	st.ReloadFromDisk()
	if st.IsFirstRun() {
		t.Fatal("IsFirstRun = true with both dirs set, want false")
	}
}

// ---- added cases: persisted settings shape -----------------------------------------------------

func TestUpdatedSettingsShape(t *testing.T) {
	s := newSetup(t)
	st := s.open()
	if err := st.UpdateConfig(UpdateSettings{
		Language:             strp("en"),
		PersonalNameDenylist: []string{"Eve"},
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	parsed := readSettingsFile(t, st.SettingsFile())
	wantKeys := []string{"language", "input_dir", "output_root_dir", "ollama_model", "ollama_host", "personal_name_denylist"}
	if len(parsed) != len(wantKeys) {
		t.Fatalf("settings.json keys = %v, want exactly %v", parsed, wantKeys)
	}
	for _, k := range wantKeys {
		if _, ok := parsed[k]; !ok {
			t.Fatalf("settings.json missing key %q: %v", k, parsed)
		}
	}
	if parsed["language"] != "EN" {
		t.Fatalf("language = %v, want EN", parsed["language"])
	}
	denylist, ok := parsed["personal_name_denylist"].([]any)
	if !ok || len(denylist) != 1 || denylist[0] != "eve" {
		t.Fatalf("personal_name_denylist = %v, want [eve]", parsed["personal_name_denylist"])
	}
}

func TestAIProviderSettings(t *testing.T) {
	s := newSetup(t)
	st := s.open()

	// Initially local
	cfg := st.Config()
	if cfg.AIProvider != "" && cfg.AIProvider != "local" {
		t.Fatalf("expected local AI provider by default, got %q", cfg.AIProvider)
	}

	// Update to cloud with Gemini and Claude keys
	if err := st.UpdateConfig(UpdateSettings{
		AIProvider:      strp("cloud"),
		CloudProvider:   strp("google"),
		GoogleAPIKey:    strp("g-key-123"),
		GoogleModel:     strp("gemini-2.5-flash"),
		AnthropicAPIKey: strp("ant-key-456"),
		AnthropicModel:  strp("claude-3-7-sonnet-20250219"),
	}); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	updated := st.Config()
	if updated.AIProvider != "cloud" {
		t.Errorf("AIProvider = %q, want cloud", updated.AIProvider)
	}
	if updated.CloudProvider != "google" {
		t.Errorf("CloudProvider = %q, want google", updated.CloudProvider)
	}
	if updated.GoogleAPIKey != "g-key-123" {
		t.Errorf("GoogleAPIKey = %q, want g-key-123", updated.GoogleAPIKey)
	}
	if updated.AnthropicAPIKey != "ant-key-456" {
		t.Errorf("AnthropicAPIKey = %q, want ant-key-456", updated.AnthropicAPIKey)
	}

	// Reload from disk to verify persistence
	st.ReloadFromDisk()
	reloaded := st.Config()
	if reloaded.AIProvider != "cloud" || reloaded.GoogleAPIKey != "g-key-123" {
		t.Errorf("reloaded config mismatch: %+v", reloaded)
	}
}

// quote renders a path as a JSON string for embedding in a settings.json fixture.
func quote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
