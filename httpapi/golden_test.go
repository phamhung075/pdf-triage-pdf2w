package httpapi

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

// Golden-file contract check.
//
// Every httpapi/testdata/golden/*.json was captured from the REAL TypeScript createWebServer()
// mounted with supertest by the throwaway scratch/capture-golden.test.ts harness (same mocks as
// web-server.test.ts), before that harness was deleted. Each case below replays the same request
// against the Go handler and asserts the status, Content-Type and a structurally identical JSON
// body.
//
// Normalizations, all documented TS-vs-Go gaps rather than silent skips:
//   - `body.host` / `body.path` are volatile (random fake-Ollama port, t.TempDir()) and are ignored.
//   - `body.error` for Zod validation failures: documentschema returns the same human message inside
//     a plain Go error, not a pretty-printed Zod issue array. The key and the string type are
//     compared; the exact framing is not.
//   - `optionalKeys` are dropped from both sides when empty/null, because TS marks them optional
//     (`?`) while Go's struct tags use `omitempty`; the frozen contract only requires the same keys
//     for present values.
type goldenFile struct {
	Status      int    `json:"status"`
	ContentType string `json:"contentType"`
	Body        any    `json:"body"`
}

var optionalKeys = map[string]bool{
	"filename": true, "meta": true, "user_feedback_reason": true,
	"category": true, "subcategory": true, "decisionReason": true,
	"name_fr": true, "name_en": true, "description_fr": true, "description_en": true,
	"modelError": true,
}

func loadGolden(t *testing.T, name string) goldenFile {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden", name+".json"))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var out goldenFile
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse golden %s: %v", name, err)
	}
	return out
}

func TestGoldenContract(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*testEnv)
		method string
		target string
		body   any
		ignore map[string]bool
	}{
		{
			name: "get-config",
			setup: func(env *testEnv) {
				env.settings.cfg.Language = "fr"
				env.settings.cfg.InputDir = "/tmp/raws-missing"
				env.settings.cfg.OutputRootDir = "/tmp/archive-missing"
				env.settings.cfg.OllamaHost = "http://127.0.0.1:1"
			},
			method: http.MethodGet, target: "/api/config",
		},
		{
			name: "get-config-setup-state",
			setup: func(env *testEnv) {
				env.settings.dataDir = "/tmp"
				env.settings.cfg.InputDir = "/tmp/raws-missing"
				env.settings.cfg.OutputRootDir = "/tmp/archive-missing"
				env.settings.cfg.OllamaHost = "http://127.0.0.1:1"
			},
			method: http.MethodGet, target: "/api/config/setup-state",
		},
		{
			name:   "get-triage-status",
			method: http.MethodGet, target: "/api/triage/status",
		},
		{
			name: "get-ollama-status",
			setup: func(env *testEnv) {
				env.ollama.models = []string{"qwen3.5:9b", "nomic-embed-text"}
				env.ollama.health = ollama.ModelHealth{OK: true}
			},
			method: http.MethodGet, target: "/api/ollama/status",
			ignore: map[string]bool{"body.host": true},
		},
		{
			name: "get-ollama-models",
			setup: func(env *testEnv) {
				env.ollama.models = []string{"qwen3.5:9b", "nomic-embed-text"}
			},
			method: http.MethodGet, target: "/api/ollama/models",
		},
		{
			name:   "post-ollama-start",
			method: http.MethodPost, target: "/api/ollama/start",
		},
		{
			name: "get-logs-recent",
			setup: func(env *testEnv) {
				env.logs.recent = []logger.LogEntry{{
					ID: 1, Timestamp: "2026-01-15T10:00:00.000Z", Level: logger.LevelInfo,
					ModuleName: "TEST", Message: "hello", Meta: map[string]any{"target": "/tmp/x.pdf"},
					Line: "[2026-01-15T10:00:00.000Z] [INFO] [TEST] hello | Meta: {\"target\":\"/tmp/x.pdf\"}\n",
				}}
			},
			method: http.MethodGet, target: "/api/logs/recent",
		},
		{
			name: "get-logs-sessions",
			setup: func(env *testEnv) {
				env.logs.sessions = []logger.DocumentLogSession{{
					Filename: "facture.pdf", StartedAt: "2026-01-15T10:00:00.000Z", UpdatedAt: "2026-01-15T10:00:05.000Z",
					LogsCount: 2, Status: "COMPLETED", Category: "invoices", Subcategory: "sfr",
					Logs: []logger.LogEntry{{
						ID: 1, Timestamp: "2026-01-15T10:00:00.000Z", Level: logger.LevelInfo,
						ModuleName: "TRIAGE", Message: "start", Meta: map[string]any{"target": "/tmp/y.pdf"}, Line: "line-1\n",
					}},
				}}
			},
			method: http.MethodGet, target: "/api/logs/sessions",
		},
		{
			name: "get-categories",
			setup: func(env *testEnv) {
				env.cats.cfg = documentschemaCategories()
				env.db.stats = database.CategoryStats{
					Total:             5,
					CategoryCounts:    map[string]int{"invoices": 5},
					SubcategoryCounts: map[string]map[string]int{"invoices": {"sfr": 3, "edf": 2, "general": 1, "2026": 1}},
				}
			},
			method: http.MethodGet, target: "/api/categories",
		},
		{
			name: "get-blocked-files",
			setup: func(env *testEnv) {
				env.db.blocked = []database.BlockedFileRecord{{
					OriginalPath: "/raws/a.pdf", Filename: "a.pdf", Reason: "no_text",
					Message: "Blocked: No text extracted from PDF.", MtimeMs: 1, Size: 10, BlockedAt: "2026-08-04T12:00:00.000Z",
				}}
			},
			method: http.MethodGet, target: "/api/blocked-files",
		},
		{
			name: "get-manual-decisions",
			setup: func(env *testEnv) {
				env.decisions.records = []manualdecisions.Record{{
					ID: 3, DocumentID: 1, Checksum: "abc", OriginalFilename: "x.pdf", Title: "X",
					OldCategory: "invoices", OldSubcategory: "sfr", NewCategory: "bank", NewSubcategory: "bnp_paribas",
					UserFeedbackReason: "", RawTextSnippet: "snip", RuleKeywords: []string{"x"}, Enabled: intPtr(1),
					CreatedAt: "2026-01-01T00:00:00.000Z",
				}}
			},
			method: http.MethodGet, target: "/api/manual-decisions",
		},
		{
			name: "put-manual-decision",
			setup: func(env *testEnv) {
				record := manualDecisionRecord
				env.decisions.update = &record
			},
			method: http.MethodPut, target: "/api/manual-decisions/3",
			body: map[string]any{"new_subcategory": "societe_generale", "rule_keywords": []any{"SG", "sg", "  ", "BNP"}, "enabled": false},
		},
		{
			name: "delete-manual-decision",
			setup: func(env *testEnv) {
				env.decisions.deleted = true
			},
			method: http.MethodDelete, target: "/api/manual-decisions/3",
		},
		{
			name:   "delete-manual-decisions-all",
			method: http.MethodDelete, target: "/api/manual-decisions",
		},
		{
			name:   "put-categories",
			method: http.MethodPut, target: "/api/categories",
			body: map[string]any{"categories": []any{map[string]any{"id": "invoices", "name": "Factures", "description": "", "aliases": []any{}, "subcategories": []any{}}}},
		},
		{
			name:   "post-triage-unlock",
			method: http.MethodPost, target: "/api/triage/unlock",
		},
		{
			name:   "post-open-location",
			method: http.MethodPost, target: "/api/open-location",
			body:   map[string]any{"targetPath": t.TempDir()},
			ignore: map[string]bool{"body.path": true},
		},
		{
			name:   "post-open-chrome",
			method: http.MethodPost, target: "/api/open-chrome",
			body:   map[string]any{"targetPath": filepath.Join(t.TempDir(), "doc.pdf")},
			ignore: map[string]bool{"body.path": true},
		},
		{
			name:   "post-open-chrome-404",
			method: http.MethodPost, target: "/api/open-chrome",
			body:   map[string]any{"targetPath": "/tmp/golden-missing/nope.pdf"},
			ignore: map[string]bool{"body.error": true},
		},
		{
			name:   "post-open-chrome-400",
			method: http.MethodPost, target: "/api/open-chrome",
			body:   map[string]any{},
			ignore: map[string]bool{"body.error": true},
		},
		{
			name:   "post-server-restart",
			method: http.MethodPost, target: "/api/server/restart",
		},
		{
			name:   "put-config",
			method: http.MethodPut, target: "/api/config",
			body: map[string]any{"language": "fr", "input_dir": "/tmp/raws", "output_root_dir": "/tmp/archive", "ollama_model": "qwen3.5:9b", "ollama_host": "http://127.0.0.1:11434"},
		},
		{
			name:   "put-config-400",
			method: http.MethodPut, target: "/api/config",
			body:   map[string]any{"language": "fr", "input_dir": "/tmp/raws", "output_root_dir": "/tmp/archive", "ollama_model": "bad", "ollama_host": "http://127.0.0.1:11434"},
			ignore: map[string]bool{"body.error": true},
		},
		{
			name:   "put-manual-decision-400",
			method: http.MethodPut, target: "/api/manual-decisions/3",
			body: map[string]any{"new_subcategory": "divers"},
		},
		{
			name:   "put-manual-decision-400-generic",
			method: http.MethodPut, target: "/api/manual-decisions/3",
			body: map[string]any{"new_category": "general"},
		},
		{
			name: "put-manual-decision-404",
			setup: func(env *testEnv) {
				env.decisions.update = nil
			},
			method: http.MethodPut, target: "/api/manual-decisions/9",
			body: map[string]any{"enabled": true},
		},
		{
			name: "delete-manual-decision-404",
			setup: func(env *testEnv) {
				env.decisions.deleted = false
			},
			method: http.MethodDelete, target: "/api/manual-decisions/9",
		},
		{
			name: "get-system-stats",
			setup: func(env *testEnv) {
				rawDir := t.TempDir()
				_ = os.WriteFile(filepath.Join(rawDir, "a.pdf"), []byte("1234567890"), 0o600)
				_ = os.WriteFile(filepath.Join(rawDir, "b.png"), []byte("12345"), 0o600)
				env.settings.cfg.InputDir = rawDir
				env.settings.cfg.OutputRootDir = filepath.Join(t.TempDir(), "missing")
			},
			method: http.MethodGet, target: "/api/system/stats",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			golden := loadGolden(t, tc.name)
			env := newTestEnv()
			if tc.setup != nil {
				tc.setup(env)
			}
			body := tc.body
			// The temp file the open-chrome golden needs must exist.
			if tc.name == "post-open-chrome" {
				target := body.(map[string]any)["targetPath"].(string)
				_ = os.WriteFile(target, []byte("x"), 0o600)
			}
			rec := doJSON(t, env.handler, tc.method, tc.target, body, nil)

			if rec.Code != golden.Status {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, golden.Status, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != golden.ContentType {
				t.Fatalf("Content-Type = %q, want %q", got, golden.ContentType)
			}
			var got any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode Go body %q: %v", rec.Body.String(), err)
			}
			want := normalizeGolden(golden.Body)
			got = normalizeGolden(got)
			ignored := tc.ignore
			if ignored == nil {
				ignored = map[string]bool{}
			}
			assertGoldenEqual(t, "body", want, got, ignored)
		})
	}
}

func TestGoldenCORS(t *testing.T) {
	golden := loadGolden(t, "cors")
	env := newTestEnv()
	rec := doJSON(t, env.handler, http.MethodGet, "/api/triage/status", nil, map[string]string{"Origin": "https://evil.example.com"})
	if rec.Code != golden.Status {
		t.Fatalf("status = %d, want %d", rec.Code, golden.Status)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want absent", got)
	}
}

// TestGoldenSSELogsStream replays the captured logs/stream frames.
func TestGoldenSSELogsStream(t *testing.T) {
	chunks := loadGoldenChunks(t, "sse-logs-stream")
	env := newTestEnv()
	env.logs.recent = []logger.LogEntry{{
		ID: 1, Timestamp: "2026-01-15T10:00:00.000Z", Level: logger.LevelInfo,
		ModuleName: "TEST", Message: "init", Line: "init\n",
	}}
	server := httptest.NewServer(env.handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/logs/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	first := readSSEFrame(t, reader)
	waitFor(t, "log subscriber", func() bool { return env.logs.subscriberCount() > 0 })
	env.logs.emit(logger.LogEntry{ID: 2, Timestamp: "2026-01-15T10:00:01.000Z", Level: logger.LevelWarn, ModuleName: "TEST", Message: "tick", Filename: "a.pdf", Line: "tick\n"})
	second := readSSEFrame(t, reader)

	wantFrames := splitSSEFrames(t, chunks)
	gotFrames := []any{decodeFrameString(t, first), decodeFrameString(t, second)}
	if len(wantFrames) != len(gotFrames) {
		t.Fatalf("frames = %d, want %d", len(gotFrames), len(wantFrames))
	}
	for i := range wantFrames {
		assertGoldenEqual(t, fmt.Sprintf("frame[%d]", i), normalizeGolden(wantFrames[i]), normalizeGolden(gotFrames[i]), map[string]bool{})
	}
}

// TestGoldenSSETriageEvents replays the captured DECISIONS_UPDATED frame.
func TestGoldenSSETriageEvents(t *testing.T) {
	chunks := loadGoldenChunks(t, "sse-triage-events")
	env := newTestEnv()
	record := manualDecisionRecord
	env.decisions.update = &record
	srv := newServer(testDeps(env))
	env.handler = srv.handler
	server := httptest.NewServer(env.handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/triage/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	waitFor(t, "triage subscriber", func() bool { return srv.hub.ClientCount() == 1 })
	rec := doJSON(t, env.handler, http.MethodPut, "/api/manual-decisions/3", map[string]any{"enabled": true}, nil)
	if rec.Code != 200 {
		t.Fatalf("mutation status = %d", rec.Code)
	}
	frame := readSSEFrame(t, reader)

	wantFrames := splitSSEFrames(t, chunks)
	if len(wantFrames) != 1 {
		t.Fatalf("golden frames = %d, want 1", len(wantFrames))
	}
	assertGoldenEqual(t, "frame[0]", normalizeGolden(wantFrames[0]), normalizeGolden(decodeFrameString(t, frame)), map[string]bool{})
}

// decodeFrameString strips a `data: ` SSE line and decodes its JSON.
func decodeFrameString(t *testing.T, frame string) any {
	t.Helper()
	decoded, ok := decodeDataFrame(strings.TrimRight(frame, "\n"))
	if !ok {
		t.Fatalf("bad frame %q", frame)
	}
	return decoded
}

// loadGoldenChunks reads the `{ "chunks": "<raw SSE>" }` shape the SSE goldens use.
func loadGoldenChunks(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden", name+".json"))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var out struct {
		Chunks string `json:"chunks"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse golden %s: %v", name, err)
	}
	if out.Chunks == "" {
		t.Fatalf("golden %s has no chunks", name)
	}
	return out.Chunks
}

// splitSSEFrames parses the raw chunks into decoded frames.
func splitSSEFrames(t *testing.T, chunks string) []any {
	t.Helper()
	var frames []any
	for _, part := range strings.Split(chunks, "\n\n") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		decoded, ok := decodeDataFrame(part)
		if !ok {
			t.Fatalf("bad frame %q", part)
		}
		frames = append(frames, decoded)
	}
	return frames
}

func decodeDataFrame(part string) (any, bool) {
	payload := strings.TrimPrefix(part, "data: ")
	var out any
	if err := json.Unmarshal([]byte(payload), &out); err != nil {
		return nil, false
	}
	return out, true
}

// normalizeGolden drops optional-empty keys on both sides so a TS `undefined`-omitted key and a Go
// `omitempty` key compare equal.
func normalizeGolden(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, child := range v {
			if optionalKeys[key] && (child == nil || child == "") {
				continue
			}
			out[key] = normalizeGolden(child)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, child := range v {
			out = append(out, normalizeGolden(child))
		}
		return out
	default:
		return value
	}
}

func assertGoldenEqual(t *testing.T, path string, want, got any, ignore map[string]bool) {
	t.Helper()
	if ignore[path] {
		return
	}
	switch wantValue := want.(type) {
	case map[string]any:
		gotValue, ok := got.(map[string]any)
		if !ok {
			t.Fatalf("%s: want object, got %T (%v)", path, got, got)
		}
		keys := unionKeys(wantValue, gotValue)
		for _, key := range keys {
			assertGoldenEqual(t, path+"."+key, wantValue[key], gotValue[key], ignore)
		}
	case []any:
		gotValue, ok := got.([]any)
		if !ok {
			t.Fatalf("%s: want array, got %T (%v)", path, got, got)
		}
		if len(wantValue) != len(gotValue) {
			t.Fatalf("%s: want %d items, got %d (%v vs %v)", path, len(wantValue), len(gotValue), wantValue, gotValue)
		}
		for i := range wantValue {
			assertGoldenEqual(t, fmt.Sprintf("%s[%d]", path, i), wantValue[i], gotValue[i], ignore)
		}
	default:
		if !reflect.DeepEqual(want, got) {
			t.Fatalf("%s: want %#v, got %#v", path, want, got)
		}
	}
}

func unionKeys(a, b map[string]any) []string {
	set := map[string]bool{}
	for key := range a {
		set[key] = true
	}
	for key := range b {
		set[key] = true
	}
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func documentschemaCategories() documentschema.CategoriesConfig {
	return documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
		{ID: "invoices", Name: "Factures", Description: "", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{
			{ID: "sfr", Name: "SFR", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{}},
		}},
	}}
}
