package httpapi

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/app/clear"
	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/app/repair"
	"github.com/phamhung075/pdf-triage-pdf2w/app/scanlock"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// frameEventType extracts the `type` field from a raw SSE frame.
func frameEventType(t *testing.T, frame string) string {
	t.Helper()
	decoded := decodeFrameString(t, frame)
	m, ok := decoded.(map[string]any)
	if !ok {
		return ""
	}
	value, _ := m["type"].(string)
	return value
}

// connectTriageSSE opens the triage stream and waits until the server has registered the client.
func connectTriageSSE(t *testing.T, env *writeEnv) (*httptest.Server, *bufio.Reader, func()) {
	t.Helper()
	server := httptest.NewServer(env.handler)
	resp, err := http.Get(server.URL + "/api/triage/events")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "triage subscriber", func() bool { return env.srv.hub.ClientCount() == 1 })
	return server, bufio.NewReader(resp.Body), func() {
		resp.Body.Close()
		server.Close()
	}
}

// --- #9 POST /api/registry/repair ----------------------------------------------------------------

func TestRegistryRepairRoute(t *testing.T) {
	t.Run("409 while a scan is already in progress", func(t *testing.T) {
		env := newWriteEnv()
		if !env.srv.tryBeginScan() {
			t.Fatal("could not seed the scanning guard")
		}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/registry/repair", nil, nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d, want 409", rec.Code)
		}
		if body := decodeJSON(t, rec); body["error"] != scanBusyMessage {
			t.Fatalf("error = %v", body["error"])
		}
	})

	t.Run("completes, broadcasts REPAIR_COMPLETED after finishTask, and releases the guard", func(t *testing.T) {
		env := newWriteEnv()
		env.repairR.run = func(onProgress repair.ProgressFunc) (repair.Result, error) {
			onProgress(repair.RepairStartedEvent{Type: repair.EventRepairStarted, TotalFiles: 2, Message: "repairing"})
			onProgress(repair.FileProgressEvent{Type: repair.EventFileProgress, Filename: "a.pdf", ScannedCount: 1, ProcessedCount: 1, Stage: "REPAIRING", Message: "working"})
			return repair.Result{ScannedCount: 2, RepairedCount: 1, UpdatedCount: 1, RelocalizedCount: 1, MovedToRawsCount: 1}, nil
		}

		_, reader, closeFn := connectTriageSSE(t, env)
		defer closeFn()

		rec := doJSON(t, env.handler, http.MethodPost, "/api/registry/repair", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if body["message"] != "Registry repair completed successfully" {
			t.Fatalf("message = %v", body["message"])
		}
		if body["repairedCount"] != float64(1) || body["scannedCount"] != float64(2) {
			t.Fatalf("result = %v", body)
		}

		// TASK_STARTED, TASK_PROGRESS(repair started), REPAIR_STARTED, TASK_PROGRESS(file),
		// FILE_PROGRESS, TASK_FINISHED, REPAIR_COMPLETED, REGISTRY_UPDATED.
		types := frameTypes(t, readFrames(t, reader, 8))
		assertOrder(t, types, "TASK_FINISHED", "REPAIR_COMPLETED")
		assertOrder(t, types, "REPAIR_COMPLETED", "REGISTRY_UPDATED")
		if !containsString(types, "FILE_PROGRESS") {
			t.Fatalf("frames = %v", types)
		}
		if env.srv.isScanning() {
			t.Fatal("guard was not released")
		}
	})

	t.Run("500 and guard release on failure", func(t *testing.T) {
		env := newWriteEnv()
		env.repairR.run = func(repair.ProgressFunc) (repair.Result, error) {
			return repair.Result{}, errors.New("disk error")
		}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/registry/repair", nil, nil)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
		if env.srv.isScanning() {
			t.Fatal("guard was not released after failure")
		}
		env.repairR.run = func(repair.ProgressFunc) (repair.Result, error) { return repair.Result{}, nil }
		if rec := doJSON(t, env.handler, http.MethodPost, "/api/registry/repair", nil, nil); rec.Code != http.StatusOK {
			t.Fatalf("second status = %d", rec.Code)
		}
	})
}

// --- #43 DELETE /api/documents -------------------------------------------------------------------

func TestClearDocumentsRoute(t *testing.T) {
	t.Run("409 while a scan is already in progress", func(t *testing.T) {
		env := newWriteEnv()
		if !env.srv.tryBeginScan() {
			t.Fatal("could not seed the scanning guard")
		}
		rec := doJSON(t, env.handler, http.MethodDelete, "/api/documents", nil, nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("success broadcasts REGISTRY_UPDATED + CATEGORIES_UPDATED", func(t *testing.T) {
		env := newWriteEnv()
		env.clearR.run = func(onProgress func(clear.Event)) (clear.ClearResult, error) {
			onProgress(clear.NewClearStartedEvent(2))
			onProgress(clear.NewFileProgressEvent("a.pdf", 1, 2, "A"))
			return clear.ClearResult{CountMoved: 3}, nil
		}
		_, reader, closeFn := connectTriageSSE(t, env)
		defer closeFn()

		rec := doJSON(t, env.handler, http.MethodDelete, "/api/documents", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if body["countMoved"] != float64(3) {
			t.Fatalf("countMoved = %v", body["countMoved"])
		}
		if !strings.Contains(body["message"].(string), "Moved 3 PDF file(s)") {
			t.Fatalf("message = %v", body["message"])
		}
		types := frameTypes(t, readFrames(t, reader, 8))
		assertOrder(t, types, "TASK_FINISHED", "REGISTRY_UPDATED")
		assertOrder(t, types, "REGISTRY_UPDATED", "CATEGORIES_UPDATED")
		if env.srv.isScanning() {
			t.Fatal("guard was not released")
		}
	})

	t.Run("cross-process lock held releases the guard and answers 409", func(t *testing.T) {
		env := newWriteEnv()
		env.lock.err = &scanlock.ScanInProgressError{HolderPID: 123}
		rec := doJSON(t, env.handler, http.MethodDelete, "/api/documents", nil, nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d", rec.Code)
		}
		if env.srv.isScanning() {
			t.Fatal("guard was not released after lock contention")
		}
	})

	t.Run("500 on failure and lock/guard release", func(t *testing.T) {
		env := newWriteEnv()
		env.clearR.run = func(func(clear.Event)) (clear.ClearResult, error) {
			return clear.ClearResult{}, errors.New("boom")
		}
		rec := doJSON(t, env.handler, http.MethodDelete, "/api/documents", nil, nil)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
		if env.lock.releaseCalls != 1 {
			t.Fatalf("lock releases = %d, want 1", env.lock.releaseCalls)
		}
		if env.srv.isScanning() {
			t.Fatal("guard was not released")
		}
	})
}

// --- #46 POST /api/triage/scan -------------------------------------------------------------------

func TestTriageScanRoute(t *testing.T) {
	t.Run("runs a scan and returns its result", func(t *testing.T) {
		env := newWriteEnv()
		env.scanR.run = func(_ context.Context, onProgress func(triagescan.Event), shouldAbort func() bool) (triagescan.Result, error) {
			if shouldAbort() {
				t.Error("abort flag should have been cleared before the scan")
			}
			onProgress(triagescan.Event{Type: triagescan.EventScanStarted, Message: "start"})
			return triagescan.Result{ScannedCount: 2, ProcessedCount: 2, Items: []triagescan.ResultItem{}}, nil
		}
		env.srv.setAbort(true)
		rec := doJSON(t, env.handler, http.MethodPost, "/api/triage/scan", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if body["message"] != "Triage scan completed" {
			t.Fatalf("message = %v", body["message"])
		}
		if body["scannedCount"] != float64(2) || body["processedCount"] != float64(2) {
			t.Fatalf("body = %v", body)
		}
		if _, ok := body["items"]; !ok {
			t.Fatalf("items missing: %v", body)
		}
		if env.lock.acquireCalls != 1 || env.lock.releaseCalls != 1 {
			t.Fatalf("lock acquire/release = %d/%d", env.lock.acquireCalls, env.lock.releaseCalls)
		}
		if env.srv.isScanning() {
			t.Fatal("guard was not released")
		}
	})

	t.Run("409 when already scanning", func(t *testing.T) {
		env := newWriteEnv()
		if !env.srv.tryBeginScan() {
			t.Fatal("could not seed the scanning guard")
		}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/triage/scan", nil, nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("ollama down fails the task and echoes the reminder", func(t *testing.T) {
		env := newWriteEnv()
		env.scanR.run = func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			return triagescan.Result{OllamaDown: true, Message: "⛔ Ollama is down"}, nil
		}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/triage/scan", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		body := decodeJSON(t, rec)
		if body["ollamaDown"] != true || body["message"] != "⛔ Ollama is down" {
			t.Fatalf("body = %v", body)
		}
		if env.base.tasks.State().Stage != "FAILED" {
			t.Fatalf("task stage = %q, want FAILED", env.base.tasks.State().Stage)
		}
	})

	t.Run("500 on scan error and guard release", func(t *testing.T) {
		env := newWriteEnv()
		env.scanR.run = func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			return triagescan.Result{}, errors.New("scan boom")
		}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/triage/scan", nil, nil)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
		if env.srv.isScanning() {
			t.Fatal("guard was not released")
		}
	})

	t.Run("cross-process lock held releases the guard and answers 409", func(t *testing.T) {
		env := newWriteEnv()
		env.lock.err = &scanlock.ScanInProgressError{HolderPID: 1}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/triage/scan", nil, nil)
		if rec.Code != http.StatusConflict {
			t.Fatalf("status = %d", rec.Code)
		}
		if env.srv.isScanning() {
			t.Fatal("guard was not released after lock contention")
		}
	})
}

// --- #24 POST /api/subcategories/rename ----------------------------------------------------------

func TestRenameSubcategoryRoute(t *testing.T) {
	t.Run("missing parameters", func(t *testing.T) {
		env := newWriteEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/subcategories/rename", map[string]any{"category": "invoices"}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("unchanged name short-circuits", func(t *testing.T) {
		env := newWriteEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/subcategories/rename", map[string]any{
			"category": "invoices", "oldSubcategory": "SFR", "newSubcategory": "sfr",
		}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		body := decodeJSON(t, rec)
		if body["count"] != float64(0) {
			t.Fatalf("body = %v", body)
		}
	})

	t.Run("merges the taxonomy and relocalizes matching documents", func(t *testing.T) {
		env := newWriteEnv()
		env.cats.cfg = documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{{
			ID: "invoices", Name: "Factures", Description: "keep me",
			Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{
				{ID: "old_entity", Name: "Old", Aliases: []string{"x"}, Subcategories: []*documentschema.SubcategoryItem{}},
				{ID: "new_entity", Name: "New", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{}},
			},
		}}}
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir
		file := dir + "/doc.pdf"
		if err := os.WriteFile(file, []byte("pdf-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		env.db.all = []database.DocumentRecord{{
			ID: 1, Category: "invoices", Subcategory: "old_entity", Date: "2026-01-15",
			OriginalFilename: "doc.pdf", OriginalPath: file, NewPath: file,
		}}
		env.reloc.relocalizeFn = func(string, string, *string, *string, *string) (relocalize.RelocalizeResult, error) {
			return relocalize.RelocalizeResult{NewPath: dir + "/new_entity/doc.pdf"}, nil
		}

		_, reader, closeFn := connectTriageSSE(t, env)
		defer closeFn()

		rec := doJSON(t, env.handler, http.MethodPost, "/api/subcategories/rename", map[string]any{
			"category": "invoices", "oldSubcategory": "old_entity", "newSubcategory": "new_entity",
		}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if body["count"] != float64(1) {
			t.Fatalf("count = %v", body["count"])
		}
		// The merge removed the source and kept the rich target fields.
		saved := env.cats.saved[0]
		subs := saved[0].Subcategories
		if len(subs) != 1 || subs[0].ID != "new_entity" {
			t.Fatalf("saved subcategories = %+v", subs)
		}
		if saved[0].Description != "keep me" {
			t.Fatalf("category description lost: %q", saved[0].Description)
		}
		if env.registry.calls == 0 {
			t.Fatal("JSON registry was not synced")
		}
		types := frameTypes(t, readFrames(t, reader, 2))
		assertOrder(t, types, "REGISTRY_UPDATED", "CATEGORIES_UPDATED")
	})
}

func readFrames(t *testing.T, reader *bufio.Reader, count int) []string {
	t.Helper()
	frames := make([]string, 0, count)
	for i := 0; i < count; i++ {
		frames = append(frames, readSSEFrame(t, reader))
	}
	return frames
}

func frameTypes(t *testing.T, frames []string) []string {
	t.Helper()
	types := make([]string, 0, len(frames))
	for _, frame := range frames {
		types = append(types, frameEventType(t, frame))
	}
	return types
}

func assertOrder(t *testing.T, types []string, before, after string) {
	t.Helper()
	beforeIdx, afterIdx := -1, -1
	for i, value := range types {
		if value == before && beforeIdx == -1 {
			beforeIdx = i
		}
		if value == after && afterIdx == -1 {
			afterIdx = i
		}
	}
	if beforeIdx == -1 || afterIdx == -1 || beforeIdx > afterIdx {
		t.Fatalf("expected %s before %s in %v", before, after, types)
	}
}
