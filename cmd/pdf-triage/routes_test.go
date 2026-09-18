package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/aichat"
	"github.com/phamhung075/pdf-triage-pdf2w/app/clear"
	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/app/repair"
	"github.com/phamhung075/pdf-triage-pdf2w/app/taskstate"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/chatquery"
	"github.com/phamhung075/pdf-triage-pdf2w/httpapi"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/categories"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

// --- fakes for the route-completeness server ----------------------------------------------------

type fakeOllamaClient struct{}

func (fakeOllamaClient) ListModels(string) ([]string, error) { return []string{"qwen3.5:9b"}, nil }
func (fakeOllamaClient) CheckModelCanGenerate(string, bool) ollama.ModelHealth {
	return ollama.ModelHealth{OK: true}
}

type fakeTextChat struct{}

func (fakeTextChat) RequestTextChatCompletion(string, string) (ollama.TextCompletion, error) {
	return ollama.TextCompletion{Response: "canned answer"}, nil
}

type fakeRelocalizer struct{}

func (fakeRelocalizer) RelocalizeFileIfNeeded(filePath, _ string, _, _, _ *string) (relocalize.RelocalizeResult, error) {
	return relocalize.RelocalizeResult{NewPath: filePath}, nil
}

func (fakeRelocalizer) ReclassifyAndRelocalizeDocument(int64, *string, *string, *string) (relocalize.ReclassifyResult, error) {
	return relocalize.ReclassifyResult{Success: true}, nil
}

func (fakeRelocalizer) DeleteDocumentAndMoveToTrash(int64) (relocalize.DeleteResult, error) {
	return relocalize.DeleteResult{Success: true}, nil
}

type fakeRepair struct{}

func (fakeRepair) RepairRegistry(repair.ProgressFunc) (repair.Result, error) {
	return repair.Result{}, nil
}

type fakeClear struct{}

func (fakeClear) ClearRegistryAndMoveArchiveToRaws(func(clear.Event)) (clear.ClearResult, error) {
	return clear.ClearResult{}, nil
}

type fakeScan struct{}

func (fakeScan) RunTriageScan(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
	return triagescan.Result{}, nil
}

// fakeManualDecisions satisfies both httpapi.ManualDecisions and the write-side
// ManualDecisionRecorder, so every decision route answers non-404.
type fakeManualDecisions struct{}

func (fakeManualDecisions) GetManualDecisions() []manualdecisions.Record { return nil }
func (fakeManualDecisions) UpdateManualDecision(id int64, _ manualdecisions.Patch) (*manualdecisions.Record, error) {
	return &manualdecisions.Record{ID: id}, nil
}
func (fakeManualDecisions) DeleteManualDecision(int64) (bool, error)    { return true, nil }
func (fakeManualDecisions) ClearManualDecisions() error                 { return nil }
func (fakeManualDecisions) RecordManualDecision(manualdecisions.Record) {}

// routeEnv is the fully wired handler plus the seed identifiers the route table needs.
type routeEnv struct {
	handler             http.Handler
	db                  *database.Store
	settings            *settings.Store
	cfg                 settings.Config
	docID               int64
	docPath             string
	sourceImagePath     string
	inputDir, outputDir string
	baseDir, dataDir    string
}

// newRouteEnv builds the full httpapi server with real temp stores and the fakes above. It seeds
// one document whose file and source image exist on disk, so the file-serving routes answer 200
// rather than a handler-level 404.
func newRouteEnv(t *testing.T) *routeEnv {
	t.Helper()

	baseDir := t.TempDir()
	dataDir := t.TempDir()
	inputDir := filepath.Join(dataDir, "input")
	outputDir := filepath.Join(dataDir, "archive")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PDF_TRIAGE_BASE_DIR", "")
	t.Setenv("PDF_TRIAGE_DATA_DIR", "")
	t.Setenv("PDF_INPUT_DIR", inputDir)
	t.Setenv("PDF_OUTPUT_DIR", outputDir)

	settingsStore, err := settings.New(settings.Options{BaseDir: baseDir, DataDir: dataDir})
	if err != nil {
		t.Fatalf("settings.New: %v", err)
	}
	if err := settingsStore.EnsureDirectoriesExist(); err != nil {
		t.Fatalf("EnsureDirectoriesExist: %v", err)
	}
	cfg := settingsStore.Config()

	log := logger.New(logger.DefaultOptions(filepath.Join(dataDir, "logs")))
	db, err := database.Open(cfg.DBPath)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	categoriesStore := categories.New(cfg.CategoriesFile, cfg.CategoriesPrivateFile, io.Discard)
	decisions := fakeManualDecisions{}
	tasks := taskstate.NewManager()

	docPath := filepath.Join(outputDir, "bank", "bnp_paribas", "2026", "releve.pdf")
	if err := os.MkdirAll(filepath.Dir(docPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(docPath, []byte("%PDF-1.4 route test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sourceImagePath := filepath.Join(dataDir, "source.png")
	if err := os.WriteFile(sourceImagePath, []byte("png"), 0o644); err != nil {
		t.Fatal(err)
	}

	docID, err := db.InsertDocumentRecord(database.NewDocument{
		Checksum:         "route-test-checksum-1",
		Title:            "Releve de compte",
		Registre:         "R1",
		Date:             "2026-01-10",
		Category:         "bank",
		Subcategory:      "bnp_paribas",
		Summary:          "a summary",
		Tags:             []string{"bank"},
		RawText:          "Releve de compte BNP Paribas avec assez de texte propre.",
		MarkdownContent:  "# Releve\n\ncontenu",
		OriginalFilename: "releve.pdf",
		OriginalPath:     docPath,
		NewPath:          docPath,
		FileType:         "PDF",
		SourceImagePath:  sourceImagePath,
		Status:           "MOVED",
	})
	if err != nil {
		t.Fatalf("InsertDocumentRecord: %v", err)
	}

	aichatDeps := aichat.Deps{
		Store:  db,
		Ollama: fakeTextChat{},
		PlanQuery: func(string, time.Time) (chatquery.StructuredQuery, error) {
			return chatquery.StructuredQuery{Keywords: []string{"releve"}}, nil
		},
		Log: log,
	}

	spawner := noopSpawner{}
	handler := httpapi.NewServer(httpapi.Deps{
		Settings:        settingsStore,
		DB:              db,
		Categories:      categoriesStore,
		ManualDecisions: decisions,
		Logs:            log,
		Tasks:           tasks,
		Ollama:          fakeOllamaClient{},
		Opener:          newOSLauncher(),
		Spawner:         spawner,
		StartOllama:     func() error { return nil },
		Exit:            func(int) {},
		PublicDir:       filepath.Join(baseDir, "public"),
		RestartDelay:    time.Millisecond,
		RouteGroups: []httpapi.RouteGroup{
			httpapi.DocumentReadRoutes(httpapi.DocumentReadDeps{
				Documents: db,
				Settings:  settingsStore,
				Opener:    newOSLauncher(),
				Spawner:   spawner,
				Mcp:       mcpToolLister{},
			}),
			httpapi.DocumentWriteRoutes(httpapi.DocumentWriteDeps{
				Settings:          settingsStore,
				DB:                db,
				Categories:        categoriesStore,
				ManualDecisions:   decisions,
				Relocalizer:       fakeRelocalizer{},
				Repair:            fakeRepair{},
				Clear:             fakeClear{},
				Scan:              fakeScan{},
				AIChat:            aichatDeps,
				Registry:          relocalize.JSONRegistrySync{DB: db, Path: cfg.JSONRegistryPath},
				Tasks:             tasks,
				Log:               log,
				EnsureDirectories: settingsStore.EnsureDirectoriesExist,
			}),
		},
	})

	return &routeEnv{
		handler:         handler,
		db:              db,
		settings:        settingsStore,
		cfg:             cfg,
		docID:           docID,
		docPath:         docPath,
		sourceImagePath: sourceImagePath,
		inputDir:        inputDir,
		outputDir:       outputDir,
		baseDir:         baseDir,
		dataDir:         dataDir,
	}
}

// --- the route-completeness table ---------------------------------------------------------------

// TestAllRoutesRegistered asserts every one of the 46 routes in the migration inventory's §3 table
// answers with something other than a ServeMux 404/405 for its documented method. The `row` field
// is the inventory row number (docs/superpowers/specs/2026-09-18-go-backend-inventory.md §3).
//
// Rows 1, 16 and 44 are SSE streams and are checked separately (a streaming handler never returns
// a normal body). Row 27 needs a raw body and is checked separately too.
func TestAllRoutesRegistered(t *testing.T) {
	env := newRouteEnv(t)

	cases := []struct {
		row          int
		method, path string
		body         any
	}{
		{2, http.MethodPost, "/api/open-location", map[string]any{"targetPath": env.inputDir}},
		{3, http.MethodPost, "/api/open-chrome", map[string]any{"targetPath": env.docPath}},
		{4, http.MethodGet, "/api/ollama/status", nil},
		{5, http.MethodGet, "/api/ollama/models", nil},
		{6, http.MethodPost, "/api/ollama/start", nil},
		{7, http.MethodPost, "/api/server/restart", nil},
		{8, http.MethodGet, "/api/triage/status", nil},
		{9, http.MethodPost, "/api/registry/repair", nil},
		{10, http.MethodGet, "/api/config/setup-state", nil},
		{11, http.MethodGet, "/api/config", nil},
		{12, http.MethodGet, "/api/system/stats", nil},
		{13, http.MethodPut, "/api/config", map[string]any{
			"input_dir": env.inputDir, "output_root_dir": env.outputDir,
			"ollama_model": "qwen3.5:9b", "ollama_host": "http://127.0.0.1:11434",
		}},
		{14, http.MethodGet, "/api/logs/recent", nil},
		{15, http.MethodGet, "/api/logs/sessions", nil},
		{17, http.MethodGet, "/api/categories", nil},
		{18, http.MethodGet, "/api/blocked-files", nil},
		{19, http.MethodGet, "/api/manual-decisions", nil},
		{20, http.MethodPut, "/api/manual-decisions/1", map[string]any{"enabled": true}},
		{21, http.MethodDelete, "/api/manual-decisions/1", nil},
		{22, http.MethodDelete, "/api/manual-decisions", nil},
		{23, http.MethodPut, "/api/categories", map[string]any{"categories": []any{
			map[string]any{"id": "invoices", "name": "Factures", "aliases": []string{}, "subcategories": []any{}},
		}}},
		{24, http.MethodPost, "/api/subcategories/rename", map[string]any{
			"category": "bank", "oldSubcategory": "old", "newSubcategory": "new",
		}},
		{25, http.MethodGet, "/api/documents", nil},
		{26, http.MethodGet, "/api/documents/export/csv", nil},
		{28, http.MethodPost, "/api/pdf/merge", map[string]any{"filepaths": []string{}, "outputFilename": "x.pdf"}},
		{29, http.MethodPost, "/api/chat", map[string]any{"message": "bonjour"}},
		{30, http.MethodGet, "/api/mcp/status", nil},
		{31, http.MethodPost, "/api/documents/package-zip", map[string]any{"docIds": []any{env.docID}, "zipName": "x.zip"}},
		{32, http.MethodPost, fmt.Sprintf("/api/documents/%d/open-folder", env.docID), nil},
		{33, http.MethodPost, "/api/pdf/split", map[string]any{}},
		{34, http.MethodGet, fmt.Sprintf("/api/documents/%d/source-image", env.docID), nil},
		{35, http.MethodGet, "/api/documents/file-by-path?path=" + url.QueryEscape(env.docPath), nil},
		{36, http.MethodGet, fmt.Sprintf("/api/documents/%d", env.docID), nil},
		{37, http.MethodGet, fmt.Sprintf("/api/documents/%d/file", env.docID), nil},
		{38, http.MethodGet, "/api/documents/export/markdown", nil},
		{39, http.MethodGet, fmt.Sprintf("/api/documents/%d/markdown", env.docID), nil},
		{40, http.MethodDelete, fmt.Sprintf("/api/documents/%d", env.docID), nil},
		{41, http.MethodPut, fmt.Sprintf("/api/documents/%d", env.docID), map[string]any{"title": "Updated title"}},
		{42, http.MethodPost, fmt.Sprintf("/api/documents/%d/relocalize", env.docID), map[string]any{
			"category": "bank", "subcategory": "bnp_paribas",
		}},
		{43, http.MethodDelete, "/api/documents", nil},
		{45, http.MethodPost, "/api/triage/unlock", nil},
		{46, http.MethodPost, "/api/triage/scan", nil},
	}

	for _, testCase := range cases {
		testCase := testCase
		t.Run(fmt.Sprintf("row_%d_%s_%s", testCase.row, testCase.method, testCase.path), func(t *testing.T) {
			status := serveRoute(t, env.handler, testCase.method, testCase.path, testCase.body)
			if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
				t.Fatalf("inventory row %d: %s %s -> %d; route is not registered", testCase.row, testCase.method, testCase.path, status)
			}
		})
	}

	// Row 1 GET /api/dev/livereload.
	t.Run("row_1_GET_/api/dev/livereload", func(t *testing.T) {
		if status := sseRouteStatus(t, env.handler, "/api/dev/livereload"); status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			t.Fatalf("inventory row 1 -> %d; route is not registered", status)
		}
	})

	// Row 16 GET /api/logs/stream.
	t.Run("row_16_GET_/api/logs/stream", func(t *testing.T) {
		if status := sseRouteStatus(t, env.handler, "/api/logs/stream"); status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			t.Fatalf("inventory row 16 -> %d; route is not registered", status)
		}
	})

	// Row 27 POST /api/images/import (raw body, ?filename).
	t.Run("row_27_POST_/api/images/import", func(t *testing.T) {
		status := serveRawRoute(t, env.handler, http.MethodPost, "/api/images/import?filename=photo.png", []byte("png-bytes"))
		if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			t.Fatalf("inventory row 27 -> %d; route is not registered", status)
		}
	})

	// Row 44 GET /api/triage/events.
	t.Run("row_44_GET_/api/triage/events", func(t *testing.T) {
		if status := sseRouteStatus(t, env.handler, "/api/triage/events"); status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			t.Fatalf("inventory row 44 -> %d; route is not registered", status)
		}
	})
}

// serveRoute performs one request through the composed handler.
func serveRoute(t *testing.T, handler http.Handler, method, target string, body any) int {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	request := httptest.NewRequest(method, target, reader)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code
}

// serveRawRoute performs one raw-body request through the composed handler.
func serveRawRoute(t *testing.T, handler http.Handler, method, target string, body []byte) int {
	t.Helper()
	request := httptest.NewRequest(method, target, bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder.Code
}

// sseRouteStatus opens an SSE route on a real ephemeral server and returns its response status.
// The request context is cancelled when the helper returns, which unblocks the streaming handler.
func sseRouteStatus(t *testing.T, handler http.Handler, target string) int {
	t.Helper()
	server := httptest.NewServer(handler)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+target, nil)
	if err != nil {
		t.Fatalf("new SSE request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		// A timeout after headers is impossible here: Do returns once headers arrive. A failure
		// means the route never answered, which is a registration failure.
		t.Fatalf("GET %s: %v", target, err)
	}
	defer response.Body.Close()
	return response.StatusCode
}
