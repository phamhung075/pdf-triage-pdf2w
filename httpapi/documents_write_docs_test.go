package httpapi

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// samplePutDoc returns a document with no on-disk file, so the PUT route stays off the physical
// relocalize branch unless a test gives it a real NewPath.
func samplePutDoc() *database.DocumentRecord {
	return &database.DocumentRecord{
		ID: 1, Checksum: "abc123", Title: "Facture SFR", Category: "invoices", Subcategory: "sfr",
		Date: "2026-01-15", Summary: "old", Tags: `["facture","sfr"]`, RawText: "raw",
		OriginalFilename: "facture.pdf", Status: "COMPLETED",
	}
}

// --- #41 PUT /api/documents/{id} -----------------------------------------------------------------

func TestPutDocumentRoute(t *testing.T) {
	t.Run("updates metadata, syncs the registry and returns parsed tags", func(t *testing.T) {
		env := newWriteEnv()
		env.db.docs[1] = samplePutDoc()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/documents/1", map[string]any{"summary": "Updated summary"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		doc := body["document"].(map[string]any)
		tags, ok := doc["tags"].([]any)
		if !ok || len(tags) != 2 {
			t.Fatalf("tags = %#v", doc["tags"])
		}
		if env.registry.calls != 1 {
			t.Fatalf("registry sync calls = %d", env.registry.calls)
		}
		update, _ := env.db.lastUpdate()
		if update.updates.Summary == nil || *update.updates.Summary != "Updated summary" {
			t.Fatalf("update = %+v", update.updates)
		}
	})

	t.Run("rejects an explicit forbidden subcategory before writing", func(t *testing.T) {
		env := newWriteEnv()
		env.db.docs[1] = samplePutDoc()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/documents/1", map[string]any{"subcategory": "general"}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
		if len(env.db.updateCall) != 0 {
			t.Fatalf("update ran despite the forbidden subcategory")
		}
	})

	t.Run("rejects a year-string subcategorie alias", func(t *testing.T) {
		env := newWriteEnv()
		env.db.docs[1] = samplePutDoc()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/documents/1", map[string]any{"subcategorie": "2026"}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("404 when the document does not exist", func(t *testing.T) {
		env := newWriteEnv()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/documents/999", map[string]any{"summary": "x"}, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if body := decodeJSON(t, rec); body["error"] != "Document not found or update failed" {
			t.Fatalf("error = %v", body["error"])
		}
	})

	t.Run("records a manual decision when the edit re-classifies (Golden Rule 18)", func(t *testing.T) {
		env := newWriteEnv()
		env.db.docs[1] = samplePutDoc()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/documents/1", map[string]any{"category": "bank", "subcategory": "bnp_paribas"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if env.decisions.count() != 1 {
			t.Fatalf("decisions = %d, want 1", env.decisions.count())
		}
		if got := env.decisions.records[0].UserFeedbackReason; got != "Manual user selection (Edit modal)" {
			t.Fatalf("reason = %q", got)
		}
	})

	t.Run("does not record a decision for a non-classification edit", func(t *testing.T) {
		env := newWriteEnv()
		env.db.docs[1] = samplePutDoc()
		rec := doJSON(t, env.handler, http.MethodPut, "/api/documents/1", map[string]any{"summary": "only the summary"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if env.decisions.count() != 0 {
			t.Fatalf("decisions = %d, want 0", env.decisions.count())
		}
	})

	t.Run("ensures the branch exists before relocalizing (Golden Rule 5)", func(t *testing.T) {
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir
		env.base.settings.cfg.OutputRootDir = dir + "/archive"
		file := dir + "/facture.pdf"
		if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		doc := samplePutDoc()
		doc.NewPath = file
		env.db.docs[1] = doc
		env.reloc.relocalizeFn = func(string, string, *string, *string, *string) (relocalize.RelocalizeResult, error) {
			return relocalize.RelocalizeResult{NewPath: dir + "/archive/bank/bnp_paribas/facture.pdf", Moved: true}, nil
		}
		rec := doJSON(t, env.handler, http.MethodPut, "/api/documents/1", map[string]any{"category": "bank", "subcategory": "bnp_paribas"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		saveIdx := env.order.indexOf("save")
		relocalizeIdx := env.order.indexOf("relocalize")
		if saveIdx == -1 || relocalizeIdx == -1 || saveIdx > relocalizeIdx {
			t.Fatalf("order = %v, want save before relocalize", env.order.steps)
		}
	})

	t.Run("does not relocalize when the document has no new_path (upstream case 15)", func(t *testing.T) {
		env := newWriteEnv()
		env.db.docs[1] = samplePutDoc() // NewPath is empty: the physical branch must be skipped.
		rec := doJSON(t, env.handler, http.MethodPut, "/api/documents/1", map[string]any{"category": "bank", "subcategory": "bnp_paribas"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if env.reloc.relocalizeCalls != 0 {
			t.Fatalf("relocalize called %d times, want 0", env.reloc.relocalizeCalls)
		}
	})

	t.Run("broadcasts REGISTRY_UPDATED/EDIT over SSE (upstream case 19)", func(t *testing.T) {
		env := newWriteEnv()
		env.db.docs[1] = samplePutDoc()
		_, reader, closeFn := connectTriageSSE(t, env)
		defer closeFn()

		rec := doJSON(t, env.handler, http.MethodPut, "/api/documents/1", map[string]any{"summary": "edited"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		frame := readSSEFrame(t, reader)
		if !strings.Contains(frame, `"type":"REGISTRY_UPDATED"`) ||
			!strings.Contains(frame, `"action":"EDIT"`) ||
			!strings.Contains(frame, `"docId":1`) {
			t.Fatalf("frame = %q", frame)
		}
	})
}

// --- #40 DELETE /api/documents/{id} --------------------------------------------------------------

func TestDeleteDocumentRoute(t *testing.T) {
	t.Run("success broadcasts DOCUMENTS_UPDATED", func(t *testing.T) {
		env := newWriteEnv()
		_, reader, closeFn := connectTriageSSE(t, env)
		defer closeFn()
		rec := doJSON(t, env.handler, http.MethodDelete, "/api/documents/1", nil, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		body := decodeJSON(t, rec)
		if body["success"] != true {
			t.Fatalf("body = %v", body)
		}
		types := frameTypes(t, readFrames(t, reader, 1))
		if len(types) != 1 || types[0] != "DOCUMENTS_UPDATED" {
			t.Fatalf("frames = %v", types)
		}
	})

	t.Run("404 when the document does not exist", func(t *testing.T) {
		env := newWriteEnv()
		env.reloc.deleteFn = func(int64) (relocalize.DeleteResult, error) {
			return relocalize.DeleteResult{Success: false, Error: "Document not found"}, nil
		}
		rec := doJSON(t, env.handler, http.MethodDelete, "/api/documents/9", nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
		if body := decodeJSON(t, rec); body["error"] != "Document not found" {
			t.Fatalf("error = %v", body["error"])
		}
	})
}

// --- #42 POST /api/documents/{id}/relocalize -----------------------------------------------------

func TestRelocalizeDocumentRoute(t *testing.T) {
	t.Run("success broadcasts REGISTRY_UPDATED/RELOCALIZE and CATEGORIES_UPDATED", func(t *testing.T) {
		env := newWriteEnv()
		_, reader, closeFn := connectTriageSSE(t, env)
		defer closeFn()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/1/relocalize", map[string]any{"category": "bank"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		types := frameTypes(t, readFrames(t, reader, 2))
		assertOrder(t, types, "REGISTRY_UPDATED", "CATEGORIES_UPDATED")
	})

	t.Run("forwards reason as the classifier feedback (Golden Rule 18)", func(t *testing.T) {
		env := newWriteEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/1/relocalize", map[string]any{"reason": "this is a bank statement"}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if env.reloc.lastReason == nil || *env.reloc.lastReason != "this is a bank statement" {
			t.Fatalf("reason = %v", env.reloc.lastReason)
		}
	})

	t.Run("stale-cleaned failure maps to 404", func(t *testing.T) {
		env := newWriteEnv()
		env.reloc.reclassifyFn = func(int64, *string, *string, *string) (relocalize.ReclassifyResult, error) {
			return relocalize.ReclassifyResult{Success: false, StaleCleaned: true, Error: "missing on disk"}, nil
		}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/1/relocalize", nil, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
		if body := decodeJSON(t, rec); body["staleCleaned"] != true {
			t.Fatalf("body = %v", body)
		}
	})

	t.Run("handled failure maps to 400", func(t *testing.T) {
		env := newWriteEnv()
		env.reloc.reclassifyFn = func(int64, *string, *string, *string) (relocalize.ReclassifyResult, error) {
			return relocalize.ReclassifyResult{Success: false, Error: "nope"}, nil
		}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/1/relocalize", nil, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("500 on an unexpected error", func(t *testing.T) {
		env := newWriteEnv()
		env.reloc.reclassifyFn = func(int64, *string, *string, *string) (relocalize.ReclassifyResult, error) {
			return relocalize.ReclassifyResult{}, errors.New("boom")
		}
		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/1/relocalize", nil, nil)
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}
