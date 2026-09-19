package httpapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/taskstate"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

func doJSON(t *testing.T, h http.Handler, method, target string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	req := httptest.NewRequest(method, target, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return out
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func readSSEFrame(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	var frame strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE frame: %v (got %q)", err, frame.String())
		}
		if line == "\n" {
			return frame.String()
		}
		frame.WriteString(line)
	}
}

// TestNoCORSHeader ports web-server.test.ts "does not send a wide-open Access-Control-Allow-Origin".
func TestNoCORSHeader(t *testing.T) {
	env := newTestEnv()
	rec := doJSON(t, env.handler, http.MethodGet, "/api/triage/status", nil, map[string]string{"Origin": "https://evil.example.com"})
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "" {
		t.Fatalf("Access-Control-Allow-Origin = %q, want empty", got)
	}
}

// TestGetCategories ports the two GET /api/categories cases in web-server.test.ts.
func TestGetCategories(t *testing.T) {
	t.Run("merges DB-derived counts and injects DB-only subcategories", func(t *testing.T) {
		env := newTestEnv()
		env.cats.cfg = documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
			{ID: "invoices", Name: "Factures", Description: "", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{
				{ID: "sfr", Name: "SFR", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{}},
			}},
		}}
		env.db.stats = database.CategoryStats{
			Total:             5,
			CategoryCounts:    map[string]int{"invoices": 5},
			SubcategoryCounts: map[string]map[string]int{"invoices": {"sfr": 3, "edf": 2}},
		}

		rec := doJSON(t, env.handler, http.MethodGet, "/api/categories", nil, nil)
		if rec.Code != 200 {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		body := decodeJSON(t, rec)
		if body["totalDocuments"].(float64) != 5 {
			t.Fatalf("totalDocuments = %v, want 5", body["totalDocuments"])
		}
		invoices := findCategory(t, body, "invoices")
		if invoices["count"].(float64) != 5 {
			t.Fatalf("invoices.count = %v, want 5", invoices["count"])
		}
		subs := invoices["subcategories"].([]any)
		sfr := findSubcategory(t, subs, "sfr")
		if sfr["count"].(float64) != 3 {
			t.Fatalf("sfr.count = %v, want 3", sfr["count"])
		}
		edf := findSubcategory(t, subs, "edf")
		if edf == nil || edf["count"].(float64) != 2 {
			t.Fatalf("injected edf = %v, want count 2", edf)
		}
		if _, hasSubcategories := edf["subcategories"]; hasSubcategories {
			t.Fatalf("injected DB-only subcategory must not carry a subcategories key: %v", edf)
		}
	})

	t.Run("excludes general and bare year strings from injected subcategories", func(t *testing.T) {
		env := newTestEnv()
		env.cats.cfg = documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
			{ID: "invoices", Name: "Factures", Description: "", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{}},
		}}
		env.db.stats = database.CategoryStats{
			Total:             2,
			CategoryCounts:    map[string]int{"invoices": 2},
			SubcategoryCounts: map[string]map[string]int{"invoices": {"general": 1, "2026": 1}},
		}
		rec := doJSON(t, env.handler, http.MethodGet, "/api/categories", nil, nil)
		invoices := findCategory(t, decodeJSON(t, rec), "invoices")
		if subs := invoices["subcategories"].([]any); len(subs) != 0 {
			t.Fatalf("subcategories = %v, want none", subs)
		}
	})

	t.Run("injected DB-only subcategories keep the TS Object.keys order", func(t *testing.T) {
		env := newTestEnv()
		env.cats.cfg = documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
			{ID: "invoices", Name: "Factures", Description: "", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{}},
		}}
		env.db.stats = database.CategoryStats{
			Total:             6,
			CategoryCounts:    map[string]int{"invoices": 6},
			SubcategoryCounts: map[string]map[string]int{"invoices": {"zeta": 1, "alpha": 2, "mike": 3, "general": 1, "2026": 1}},
		}
		rec := doJSON(t, env.handler, http.MethodGet, "/api/categories", nil, nil)
		invoices := findCategory(t, decodeJSON(t, rec), "invoices")
		var ids []string
		for _, raw := range invoices["subcategories"].([]any) {
			ids = append(ids, raw.(map[string]any)["id"].(string))
		}
		if strings.Join(ids, ",") != "alpha,mike,zeta" {
			t.Fatalf("injected order = %v, want alpha,mike,zeta", ids)
		}
	})

	t.Run("integer-like injected ids keep the JS numeric-first order", func(t *testing.T) {
		env := newTestEnv()
		env.cats.cfg = documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
			{ID: "invoices", Name: "Factures", Description: "", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{}},
		}}
		env.db.stats = database.CategoryStats{
			Total:             3,
			CategoryCounts:    map[string]int{"invoices": 3},
			SubcategoryCounts: map[string]map[string]int{"invoices": {"10": 1, "2": 1, "alpha": 1}},
		}
		rec := doJSON(t, env.handler, http.MethodGet, "/api/categories", nil, nil)
		invoices := findCategory(t, decodeJSON(t, rec), "invoices")
		var ids []string
		for _, raw := range invoices["subcategories"].([]any) {
			ids = append(ids, raw.(map[string]any)["id"].(string))
		}
		if strings.Join(ids, ",") != "2,10,alpha" {
			t.Fatalf("injected order = %v, want 2,10,alpha", ids)
		}
	})

	t.Run("configured subcategories omit the subcategories key when the source omitted it", func(t *testing.T) {
		env := newTestEnv()
		env.cats.cfg = documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
			{ID: "invoices", Name: "Factures", Description: "", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{
				{ID: "sfr", Name: "SFR", Aliases: []string{}}, // nil Subcategories: TS `{...sub}` drops the key
				{ID: "edf", Name: "EDF", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{}},
			}},
		}}
		rec := doJSON(t, env.handler, http.MethodGet, "/api/categories", nil, nil)
		invoices := findCategory(t, decodeJSON(t, rec), "invoices")
		subs := invoices["subcategories"].([]any)
		if sfr := findSubcategory(t, subs, "sfr"); func() bool { _, ok := sfr["subcategories"]; return ok }() {
			t.Fatalf("sfr gained a subcategories key: %v", sfr)
		}
		edf := findSubcategory(t, subs, "edf")
		if _, ok := edf["subcategories"]; !ok {
			t.Fatalf("edf lost its explicit empty subcategories key: %v", edf)
		}
	})
}

func findCategory(t *testing.T, body map[string]any, id string) map[string]any {
	t.Helper()
	for _, raw := range body["categories"].([]any) {
		cat := raw.(map[string]any)
		if cat["id"] == id {
			return cat
		}
	}
	t.Fatalf("category %q not found in %v", id, body["categories"])
	return nil
}

func findSubcategory(t *testing.T, subs []any, id string) map[string]any {
	t.Helper()
	for _, raw := range subs {
		sub := raw.(map[string]any)
		if sub["id"] == id {
			return sub
		}
	}
	return nil
}

// TestGetBlockedFiles ports the three GET /api/blocked-files cases.
func TestGetBlockedFiles(t *testing.T) {
	t.Run("returns the total count and list", func(t *testing.T) {
		env := newTestEnv()
		env.db.blocked = []database.BlockedFileRecord{
			{OriginalPath: "/raws/a.pdf", Filename: "a.pdf", Reason: "no_text", Message: "Blocked: No text extracted from PDF.", MtimeMs: 1, Size: 10, BlockedAt: "2026-08-04T12:00:00.000Z"},
			{OriginalPath: "/raws/b.pdf", Filename: "b.pdf", Reason: "no_specific_subcategory", Message: "Blocked: no specific subcategory detected.", MtimeMs: 2, Size: 20, BlockedAt: "2026-08-04T13:00:00.000Z"},
		}
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/blocked-files", nil, nil))
		if body["total"].(float64) != 2 || len(body["files"].([]any)) != 2 {
			t.Fatalf("body = %v", body)
		}
	})

	t.Run("returns an empty list when nothing is blocked", func(t *testing.T) {
		env := newTestEnv()
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/blocked-files", nil, nil))
		if body["total"].(float64) != 0 {
			t.Fatalf("total = %v, want 0", body["total"])
		}
		if files, ok := body["files"].([]any); !ok || len(files) != 0 {
			t.Fatalf("files = %#v, want []", body["files"])
		}
	})

	t.Run("returns 500 when the DB layer throws", func(t *testing.T) {
		env := newTestEnv()
		env.db.blockedErr = fmt.Errorf("DB unavailable")
		rec := doJSON(t, env.handler, http.MethodGet, "/api/blocked-files", nil, nil)
		if rec.Code != 500 || decodeJSON(t, rec)["error"] != "DB unavailable" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestManualDecisions ports the list/update/delete/clear cases.
func TestManualDecisions(t *testing.T) {
	t.Run("lists the recorded decisions with a total", func(t *testing.T) {
		env := newTestEnv()
		env.decisions.records = []manualdecisions.Record{
			{ID: 3, DocumentID: 1, OriginalFilename: "STMT_CHK_101.pdf", NewCategory: "bank", NewSubcategory: "bnp_paribas", Enabled: intPtr(1)},
		}
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/manual-decisions", nil, nil))
		if body["total"].(float64) != 1 {
			t.Fatalf("total = %v, want 1", body["total"])
		}
	})

	t.Run("normalizes keywords and enabled, and returns success", func(t *testing.T) {
		env := newTestEnv()
		env.decisions.update = &manualDecisionRecord
		rec := doJSON(t, env.handler, http.MethodPut, "/api/manual-decisions/3", map[string]any{
			"new_subcategory": "societe_generale",
			"rule_keywords":   []any{"SG_CODE", "sg_code", "  ", "BNP"},
			"enabled":         false,
		}, nil)
		if rec.Code != 200 {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if env.decisions.updateCalls != 1 || env.decisions.lastID != 3 {
			t.Fatalf("update called %d times id=%d", env.decisions.updateCalls, env.decisions.lastID)
		}
		patch := env.decisions.lastPatch
		if patch.NewSubcategory == nil || *patch.NewSubcategory != "societe_generale" {
			t.Fatalf("new_subcategory = %v", patch.NewSubcategory)
		}
		if patch.RuleKeywords == nil || strings.Join(*patch.RuleKeywords, ",") != "SG_CODE,sg_code,BNP" {
			t.Fatalf("rule_keywords = %v", patch.RuleKeywords)
		}
		if patch.Enabled == nil || *patch.Enabled != 0 {
			t.Fatalf("enabled = %v", patch.Enabled)
		}
		if decodeJSON(t, rec)["success"] != true {
			t.Fatalf("success not true")
		}
	})

	t.Run("rejects a forbidden target subcategory without calling the store", func(t *testing.T) {
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/manual-decisions/3", map[string]any{"new_subcategory": "divers"}, nil)
		if rec.Code != 400 {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if !strings.Contains(decodeJSON(t, rec)["error"].(string), "not a valid subcategory") {
			t.Fatalf("error = %v", decodeJSON(t, rec)["error"])
		}
		if env.decisions.updateCalls != 0 {
			t.Fatalf("store called %d times, want 0", env.decisions.updateCalls)
		}
	})

	t.Run("rejects a generic target category without calling the store", func(t *testing.T) {
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/manual-decisions/3", map[string]any{"new_category": "general"}, nil)
		if rec.Code != 400 {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
		if env.decisions.updateCalls != 0 {
			t.Fatalf("store called %d times, want 0", env.decisions.updateCalls)
		}
	})

	t.Run("returns 404 when the decision does not exist", func(t *testing.T) {
		env := newTestEnv()
		env.decisions.update = nil
		rec := doJSON(t, env.handler, http.MethodPut, "/api/manual-decisions/999", map[string]any{"enabled": true}, nil)
		if rec.Code != 404 || decodeJSON(t, rec)["error"] != "Decision not found" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("deletes a single decision", func(t *testing.T) {
		env := newTestEnv()
		env.decisions.deleted = true
		rec := doJSON(t, env.handler, http.MethodDelete, "/api/manual-decisions/3", nil, nil)
		if rec.Code != 200 || decodeJSON(t, rec)["success"] != true || env.decisions.lastID != 3 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 when deleting a missing decision", func(t *testing.T) {
		env := newTestEnv()
		env.decisions.deleted = false
		rec := doJSON(t, env.handler, http.MethodDelete, "/api/manual-decisions/999", nil, nil)
		if rec.Code != 404 {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("clears every decision", func(t *testing.T) {
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodDelete, "/api/manual-decisions", nil, nil)
		if rec.Code != 200 || env.decisions.clearCalls != 1 || decodeJSON(t, rec)["success"] != true {
			t.Fatalf("status=%d calls=%d body=%s", rec.Code, env.decisions.clearCalls, rec.Body.String())
		}
	})

	t.Run("rejects an invalid decision id", func(t *testing.T) {
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/manual-decisions/abc", map[string]any{"enabled": true}, nil)
		if rec.Code != 400 || decodeJSON(t, rec)["error"] != "Invalid decision id" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestOpenChrome ports the three POST /api/open-chrome cases.
func TestOpenChrome(t *testing.T) {
	t.Run("returns 404 if the target file path does not exist", func(t *testing.T) {
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/open-chrome", map[string]any{"targetPath": "/definitely/missing.pdf"}, nil)
		if rec.Code != 404 || !strings.Contains(decodeJSON(t, rec)["error"].(string), "File path does not exist") {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("launches Chrome with the file path as an argv entry and never a shell string", func(t *testing.T) {
		env := newTestEnv()
		dir := t.TempDir()
		target := filepath.Join(dir, "facture & cie.pdf")
		if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/open-chrome", map[string]any{"targetPath": target}, nil)
		if rec.Code != 200 {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if len(env.spawner.calls) != 1 {
			t.Fatalf("spawn calls = %d, want 1", len(env.spawner.calls))
		}
		spawn := env.spawner.calls[0]
		found := false
		for _, arg := range spawn.Args {
			if strings.Contains(arg, "facture & cie.pdf") {
				found = true
			}
		}
		if !found {
			t.Fatalf("spawn args %v do not contain the file path", spawn.Args)
		}
	})

	t.Run("returns 400 if targetPath is missing", func(t *testing.T) {
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/open-chrome", map[string]any{}, nil)
		if rec.Code != 400 {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	})

	t.Run("returns 500 when Chrome is not found", func(t *testing.T) {
		env := newTestEnv()
		env.opener.chromeOK = false
		target := filepath.Join(t.TempDir(), "a.pdf")
		_ = os.WriteFile(target, []byte("x"), 0o600)
		rec := doJSON(t, env.handler, http.MethodPost, "/api/open-chrome", map[string]any{"targetPath": target}, nil)
		if rec.Code != 500 || decodeJSON(t, rec)["error"] != "Chrome executable not found" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestOpenLocation ports POST /api/open-location's three branches.
func TestOpenLocation(t *testing.T) {
	t.Run("opens an existing directory", func(t *testing.T) {
		env := newTestEnv()
		dir := t.TempDir()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/open-location", map[string]any{"targetPath": dir}, nil)
		body := decodeJSON(t, rec)
		if rec.Code != 200 || body["message"] != "Windows Explorer opened" || body["path"] != dir {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if !env.opener.directory {
			t.Fatal("OpenDirectory was not called")
		}
	})

	t.Run("opens the parent directory when the target is missing but the parent exists", func(t *testing.T) {
		env := newTestEnv()
		parent := t.TempDir()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/open-location", map[string]any{"targetPath": filepath.Join(parent, "missing")}, nil)
		body := decodeJSON(t, rec)
		if rec.Code != 200 || body["message"] != "Opened parent directory" || body["path"] != parent {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 when neither the path nor its parent exists", func(t *testing.T) {
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/open-location", map[string]any{"targetPath": "/no/such/dir/here"}, nil)
		if rec.Code != 404 {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})
}

// TestConfigRoutes ports the GET/PUT /api/config and GET /api/config/setup-state behaviours.
func TestConfigRoutes(t *testing.T) {
	t.Run("GET /api/config returns the six config keys", func(t *testing.T) {
		env := newTestEnv()
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/config", nil, nil))
		for _, key := range []string{"language", "input_dir", "output_root_dir", "ollama_model", "ollama_host", "personal_name_denylist"} {
			if _, ok := body[key]; !ok {
				t.Fatalf("missing key %q in %v", key, body)
			}
		}
	})

	t.Run("PUT /api/config validates and updates", func(t *testing.T) {
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/config", map[string]any{
			"language": "en", "input_dir": "/tmp/new-raws", "output_root_dir": "/tmp/new-archive",
			"ollama_model": "qwen3.5:9b", "ollama_host": "http://127.0.0.1:11434",
		}, nil)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if len(env.settings.updates) != 1 {
			t.Fatalf("updateConfig calls = %d, want 1", len(env.settings.updates))
		}
		body := decodeJSON(t, rec)
		config := body["config"].(map[string]any)
		if config["input_dir"] != "/tmp/new-raws" {
			t.Fatalf("config = %v", config)
		}
	})

	t.Run("PUT /api/config rejects a non-qwen3.5 model", func(t *testing.T) {
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/config", map[string]any{
			"input_dir": "/tmp/raws", "output_root_dir": "/tmp/archive",
			"ollama_model": "llama3", "ollama_host": "http://127.0.0.1:11434",
		}, nil)
		if rec.Code != 400 || !strings.Contains(decodeJSON(t, rec)["error"].(string), "qwen3.5:9b") {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("setup-state reports configured=false and the friendly defaults on first run", func(t *testing.T) {
		env := newTestEnv()
		env.settings.firstRun = true
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/config/setup-state", nil, nil))
		if body["configured"] != false {
			t.Fatalf("configured = %v, want false", body["configured"])
		}
		if body["dataDir"] != "/tmp/data" {
			t.Fatalf("dataDir = %v", body["dataDir"])
		}
	})
}

// TestSystemStats ports GET /api/system/stats bucketing.
func TestSystemStats(t *testing.T) {
	env := newTestEnv()
	rawDir := t.TempDir()
	_ = os.WriteFile(filepath.Join(rawDir, "a.pdf"), []byte("1234567890"), 0o600)
	_ = os.WriteFile(filepath.Join(rawDir, "b.png"), []byte("12345"), 0o600)
	env.settings.cfg.InputDir = rawDir
	env.settings.cfg.OutputRootDir = filepath.Join(t.TempDir(), "missing")

	body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/system/stats", nil, nil))
	raws := body["raws"].(map[string]any)
	if raws["count"].(float64) != 2 || raws["bytes"].(float64) != 15 || raws["sizeFormatted"] != "15 B" {
		t.Fatalf("raws = %v", raws)
	}
	breakdown := body["formatBreakdown"].(map[string]any)
	if breakdown["pdf"].(map[string]any)["count"].(float64) != 1 {
		t.Fatalf("breakdown = %v", breakdown)
	}
	if breakdown["image"].(map[string]any)["bytes"].(float64) != 5 {
		t.Fatalf("breakdown = %v", breakdown)
	}
}

// TestLogsRoutes ports the recent/sessions JSON and the stream INIT/LOG framing.
func TestLogsRoutes(t *testing.T) {
	t.Run("recent returns { logs }", func(t *testing.T) {
		env := newTestEnv()
		env.logs.recent = []logger.LogEntry{{ID: 1, Timestamp: "2026-01-15T10:00:00.000Z", Level: logger.LevelInfo, ModuleName: "TEST", Message: "hello", Line: "line\n"}}
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/logs/recent", nil, nil))
		if len(body["logs"].([]any)) != 1 {
			t.Fatalf("logs = %v", body["logs"])
		}
	})

	t.Run("non-numeric limit follows parseInt NaN semantics (whole buffer)", func(t *testing.T) {
		// TS getRecentLogs(NaN) is logBuffer.slice(-NaN) === slice(0) === every buffered entry, so
		// `?limit=abc` must not fall back to the 300 default. The real logger is used so the
		// response body length is the observable proof, not just the limit forwarded.
		env := newTestEnv()
		realLogger := logger.New(logger.Options{LogDir: t.TempDir(), MaxBuffer: 10, Stdout: io.Discard, Stderr: io.Discard})
		for i := 0; i < 5; i++ {
			realLogger.Info("TEST", "entry", nil)
		}
		deps := testDeps(env)
		deps.Logs = realLogger
		env.handler = NewServer(deps)

		all := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/logs/recent?limit=abc", nil, nil))
		if got := len(all["logs"].([]any)); got != 5 {
			t.Fatalf("NaN limit returned %d logs, want all 5", got)
		}
		truncated := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/logs/recent?limit=2", nil, nil))
		if got := len(truncated["logs"].([]any)); got != 2 {
			t.Fatalf("limit=2 returned %d logs, want 2", got)
		}
	})

	t.Run("sessions returns { total, sessions }", func(t *testing.T) {
		env := newTestEnv()
		env.logs.sessions = []logger.DocumentLogSession{{Filename: "a.pdf", StartedAt: "t0", UpdatedAt: "t1", LogsCount: 2, Status: "COMPLETED"}}
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/logs/sessions", nil, nil))
		if body["total"].(float64) != 1 || len(body["sessions"].([]any)) != 1 {
			t.Fatalf("body = %v", body)
		}
	})

	t.Run("stream writes INIT then LOG frames", func(t *testing.T) {
		env := newTestEnv()
		env.logs.recent = []logger.LogEntry{{ID: 1, Timestamp: "t", Level: logger.LevelInfo, ModuleName: "T", Message: "init", Line: "l\n"}}
		server := httptest.NewServer(env.handler)
		defer server.Close()

		resp, err := http.Get(server.URL + "/api/logs/stream")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		first := readSSEFrame(t, reader)
		if !strings.HasPrefix(first, "data: ") || !strings.Contains(first, `"type":"INIT"`) {
			t.Fatalf("first frame = %q", first)
		}
		waitFor(t, "log subscriber", func() bool { return env.logs.subscriberCount() > 0 })
		env.logs.emit(logger.LogEntry{ID: 2, Timestamp: "t2", Level: logger.LevelWarn, ModuleName: "T", Message: "tick", Filename: "a.pdf", Line: "l2\n"})
		second := readSSEFrame(t, reader)
		if !strings.Contains(second, `"type":"LOG"`) || !strings.Contains(second, `"message":"tick"`) {
			t.Fatalf("second frame = %q", second)
		}
	})
}

// TestOllamaRoutes ports the status/models/start behaviours.
func TestOllamaRoutes(t *testing.T) {
	t.Run("status online with matching model", func(t *testing.T) {
		env := newTestEnv()
		env.ollama.models = []string{"qwen3.5:9b", "nomic-embed-text"}
		env.ollama.health = ollama.ModelHealth{OK: true}
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/ollama/status", nil, nil))
		if body["online"] != true || body["modelsCount"].(float64) != 2 || body["modelExists"] != true || body["modelCanGenerate"] != true {
			t.Fatalf("body = %v", body)
		}
		if _, hasError := body["modelError"]; hasError {
			t.Fatalf("modelError should be absent when healthy: %v", body)
		}
	})

	t.Run("status offline folds the error into a 200", func(t *testing.T) {
		env := newTestEnv()
		env.ollama.listErr = fmt.Errorf("connection refused")
		rec := doJSON(t, env.handler, http.MethodGet, "/api/ollama/status", nil, nil)
		body := decodeJSON(t, rec)
		if rec.Code != 200 || body["online"] != false || body["modelsCount"].(float64) != 0 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("models uses the ?host= override", func(t *testing.T) {
		env := newTestEnv()
		env.ollama.models = []string{"qwen3.5:9b"}
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/ollama/models?host=http://example:1", nil, nil))
		if body["online"] != true || env.ollama.listHosts[0] != "http://example:1" {
			t.Fatalf("body=%v hosts=%v", body, env.ollama.listHosts)
		}
	})

	t.Run("start responds immediately and launches", func(t *testing.T) {
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/ollama/start", nil, nil)
		if rec.Code != 200 || decodeJSON(t, rec)["message"] != "Ollama serve launch initiated" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		select {
		case <-env.startedCh:
		case <-time.After(2 * time.Second):
			t.Fatal("StartOllama was not called")
		}
	})
}

// TestAITestLatency pins the dashboard contract: POST /api/ai/test must always include an
// integer latency_ms (>= 0), which ModalsManager reads as "Connected (Nms)". The local
// provider path is exercised so the existing fakeOllama stands in for the network.
func TestAITestLatency(t *testing.T) {
	assertLatency := func(t *testing.T, body map[string]any) {
		t.Helper()
		raw, ok := body["latency_ms"]
		if !ok {
			t.Fatalf("latency_ms missing from response: %v", body)
		}
		n, ok := raw.(float64)
		if !ok {
			t.Fatalf("latency_ms = %v (%T), want a JSON number", raw, raw)
		}
		if n < 0 || n != float64(int64(n)) {
			t.Fatalf("latency_ms = %v, want a non-negative integer", raw)
		}
	}

	t.Run("local success", func(t *testing.T) {
		env := newTestEnv()
		env.ollama.health = ollama.ModelHealth{OK: true}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/ai/test", map[string]string{"provider": "local"}, nil)
		body := decodeJSON(t, rec)
		if rec.Code != 200 || body["ok"] != true {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertLatency(t, body)
	})

	t.Run("local failure", func(t *testing.T) {
		env := newTestEnv()
		env.ollama.health = ollama.ModelHealth{OK: false, Error: "model not found"}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/ai/test", map[string]string{"provider": "local"}, nil)
		body := decodeJSON(t, rec)
		if rec.Code != 200 || body["ok"] != false {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertLatency(t, body)
	})

	// Cloud providers are exercised against an in-process httptest server (loopback only).
	t.Run("cloud success", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"pong"}]}}]}`))
		}))
		defer upstream.Close()
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/ai/test", map[string]string{
			"provider": "google", "api_key": "test-key", "model": "gemini-2.5-flash", "base_url": upstream.URL,
		}, nil)
		body := decodeJSON(t, rec)
		if rec.Code != 200 || body["ok"] != true {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertLatency(t, body)
	})

	t.Run("cloud failure", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		}))
		defer upstream.Close()
		env := newTestEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/ai/test", map[string]string{
			"provider": "google", "api_key": "test-key", "model": "gemini-2.5-flash", "base_url": upstream.URL,
		}, nil)
		body := decodeJSON(t, rec)
		if rec.Code != 200 || body["ok"] != false {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		assertLatency(t, body)
	})
}

// TestServerRestart ports POST /api/server/restart: respond first, then exit.
func TestServerRestart(t *testing.T) {
	env := newTestEnv()
	deps := testDeps(env)
	deps.RestartDelay = time.Millisecond
	exitCh := make(chan int, 1)
	deps.Exit = func(code int) { exitCh <- code }
	env.handler = NewServer(deps)

	rec := doJSON(t, env.handler, http.MethodPost, "/api/server/restart", nil, nil)
	if rec.Code != 200 || decodeJSON(t, rec)["message"] != "Server restarting..." {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	select {
	case code := <-exitCh:
		if code != 0 {
			t.Fatalf("exit code = %d, want 0", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("restart did not call Exit")
	}
}

// TestTriageStatusAndUnlock covers routes #8 and #45 and the ported task-state behaviours.
func TestTriageStatusAndUnlock(t *testing.T) {
	t.Run("status returns the live task state", func(t *testing.T) {
		env := newTestEnv()
		env.tasks.StartTask(taskstate.TaskRepair, 50, "Starting repair...")
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/triage/status", nil, nil))
		if body["isRunning"] != true || body["type"] != "REPAIR" || body["totalFiles"].(float64) != 50 {
			t.Fatalf("body = %v", body)
		}
	})

	t.Run("status reflects progress and failure", func(t *testing.T) {
		env := newTestEnv()
		env.tasks.StartTask(taskstate.TaskScan, 10, "Scanning...")
		env.tasks.UpdateTaskProgress(taskstate.ProgressUpdate{ProcessedFiles: 5})
		env.tasks.FailTask("Disk error")
		body := decodeJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/triage/status", nil, nil))
		if body["isRunning"] != false || body["stage"] != "FAILED" || body["error"] != "Disk error" {
			t.Fatalf("body = %v", body)
		}
	})

	t.Run("unlock aborts, cools down, resets task state, and responds", func(t *testing.T) {
		env := newTestEnv()
		srv := newServer(testDeps(env))
		env.handler = srv.handler
		srv.beginScan()
		srv.setAbort(false)
		env.tasks.StartTask(taskstate.TaskScan, 5, "Scanning...")
		server := httptest.NewServer(env.handler)
		defer server.Close()

		resp, err := http.Get(server.URL + "/api/triage/events")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		reader := bufio.NewReader(resp.Body)
		waitFor(t, "triage subscriber", func() bool { return srv.hub.ClientCount() == 1 })

		rec := doJSON(t, env.handler, http.MethodPost, "/api/triage/unlock", nil, nil)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if !srv.abortRequested() {
			t.Fatal("scanAbortRequested not set")
		}
		if srv.isScanning() {
			t.Fatal("isAutoScanning not cleared")
		}
		if !srv.withinManualStopCooldown() {
			t.Fatal("manual-stop cooldown not set")
		}
		state := env.tasks.State()
		if state.IsRunning || state.Type != taskstate.TaskIdle || state.Message != "System unlocked" {
			t.Fatalf("task state = %+v", state)
		}

		// The stream must carry TASK_FINISHED (from resetTaskState) then REGISTRY_UPDATED/UNLOCK.
		var frames []string
		for i := 0; i < 2; i++ {
			frames = append(frames, readSSEFrame(t, reader))
		}
		joined := strings.Join(frames, "\n")
		if !strings.Contains(joined, `"type":"TASK_FINISHED"`) || !strings.Contains(joined, `"type":"REGISTRY_UPDATED"`) || !strings.Contains(joined, `"action":"UNLOCK"`) {
			t.Fatalf("frames = %v", frames)
		}
	})
}

// TestSSEBroadcastOnDecisionMutation verifies DECISIONS_UPDATED reaches triage/events clients.
func TestSSEBroadcastOnDecisionMutation(t *testing.T) {
	env := newTestEnv()
	srv := newServer(testDeps(env))
	env.handler = srv.handler
	env.decisions.update = &manualDecisionRecord
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
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	frame := readSSEFrame(t, reader)
	if !strings.Contains(frame, `"type":"DECISIONS_UPDATED"`) || !strings.Contains(frame, `"action":"UPDATE"`) || !strings.Contains(frame, `"decisionId":3`) {
		t.Fatalf("frame = %q", frame)
	}
}

// TestLiveReload ports GET /api/dev/livereload's raw `data: reload` framing.
func TestLiveReload(t *testing.T) {
	env := newTestEnv()
	server := httptest.NewServer(env.handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/dev/livereload")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
	reader := bufio.NewReader(resp.Body)
	env.watcher.emit()
	frame := readSSEFrame(t, reader)
	if strings.TrimRight(frame, "\n") != "data: reload" {
		t.Fatalf("frame = %q, want %q", frame, "data: reload")
	}
}

// TestStaticNoStore verifies the public/ mount is conditional and sets Cache-Control: no-store.
func TestStaticNoStore(t *testing.T) {
	env := newTestEnv()
	publicDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(publicDir, "index.html"), []byte("<html>ok</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	deps := testDeps(env)
	deps.PublicDir = publicDir
	handler := NewServer(deps)

	rec := doJSON(t, handler, http.MethodGet, "/index.html", nil, nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("status=%d cache=%q", rec.Code, rec.Header().Get("Cache-Control"))
	}
}

// TestResolveManagedPathWrapsGuard verifies the server wrapper delegates to app/guards.
func TestResolveManagedPathWrapsGuard(t *testing.T) {
	env := newTestEnv()
	env.settings.cfg.InputDir = "/managed/raws"
	env.settings.cfg.OutputRootDir = "/managed/archive"
	srv := newServer(testDeps(env))

	got, violation := srv.resolveManagedPath("/managed/raws/a.pdf")
	if violation != nil || got != "/managed/raws/a.pdf" {
		t.Fatalf("inside path: got=%q violation=%v", got, violation)
	}
	if _, violation := srv.resolveManagedPath("/etc/passwd"); violation == nil || violation.HTTPStatus != 403 {
		t.Fatalf("outside path: violation = %v", violation)
	}
}
