package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func doRaw(t *testing.T, h http.Handler, target string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// --- #27 POST /api/images/import -----------------------------------------------------------------

func TestImportImageRoute(t *testing.T) {
	t.Run("rejects an unsupported extension before writing", func(t *testing.T) {
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir
		rec := doRaw(t, env.handler, "/api/images/import?filename=evil.exe", []byte("MZ"))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if body["error"] != "Unsupported image type '.exe'. Accepted: .png, .jpg, .jpeg, .webp, .bmp, .tiff" {
			t.Fatalf("error = %v", body["error"])
		}
	})

	t.Run("rejects an empty body", func(t *testing.T) {
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir
		rec := doRaw(t, env.handler, "/api/images/import?filename=photo.png", nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if body := decodeJSON(t, rec); body["error"] != "Empty image body." {
			t.Fatalf("error = %v", body["error"])
		}
	})

	t.Run("writes with exclusive-create and never overwrites", func(t *testing.T) {
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir

		rec := doRaw(t, env.handler, "/api/images/import?filename=photo.png", []byte("first"))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		first := decodeJSON(t, rec)
		if first["filename"] != "photo.png" {
			t.Fatalf("filename = %v", first["filename"])
		}
		if data, _ := os.ReadFile(filepath.Join(dir, "photo.png")); string(data) != "first" {
			t.Fatalf("file content = %q", data)
		}

		rec = doRaw(t, env.handler, "/api/images/import?filename=photo.png", []byte("second"))
		if rec.Code != http.StatusOK {
			t.Fatalf("second status = %d", rec.Code)
		}
		second := decodeJSON(t, rec)
		if second["filename"] != "photo_1.png" {
			t.Fatalf("second filename = %v", second["filename"])
		}
		if data, _ := os.ReadFile(filepath.Join(dir, "photo.png")); string(data) != "first" {
			t.Fatalf("original was overwritten: %q", data)
		}
	})

	t.Run("enforces the 64 MB handler cap", func(t *testing.T) {
		if testing.Short() {
			t.Skip("allocates 64 MB")
		}
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir

		req := httptest.NewRequest(http.MethodPost, "/api/images/import?filename=big.png", bytes.NewReader(make([]byte, maxImportBytes+1)))
		rec := httptest.NewRecorder()
		env.srv.importImageHandler(env.deps(), rec, req)
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", rec.Code)
		}
		if entries, _ := os.ReadDir(dir); len(entries) != 0 {
			t.Fatalf("imported despite oversize body: %d entries", len(entries))
		}
	})

	t.Run("composed middleware still caps at 100 kB (reported gap 1)", func(t *testing.T) {
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir
		rec := doRaw(t, env.handler, "/api/images/import?filename=big.png", make([]byte, 101*1024))
		if rec.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want the part-1 middleware 413", rec.Code)
		}
	})
}

// --- #28 POST /api/pdf/merge ---------------------------------------------------------------------

func TestMergePDFRoute(t *testing.T) {
	t.Run("requires at least two filepaths", func(t *testing.T) {
		env := newWriteEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/pdf/merge", map[string]any{"filepaths": []any{"a.pdf"}}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("rejects a path outside the managed directories", func(t *testing.T) {
		env := newWriteEnv()
		env.base.settings.cfg.InputDir = t.TempDir()
		env.base.settings.cfg.OutputRootDir = t.TempDir()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/pdf/merge", map[string]any{
			"filepaths": []any{"/etc/passwd", "/etc/hosts"},
		}, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("404 when a managed file is missing", func(t *testing.T) {
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir
		env.base.settings.cfg.OutputRootDir = dir
		rec := doJSON(t, env.handler, http.MethodPost, "/api/pdf/merge", map[string]any{
			"filepaths": []any{dir + "/missing-a.pdf", dir + "/missing-b.pdf"},
		}, nil)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("merges two PDFs into the input folder", func(t *testing.T) {
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir
		env.base.settings.cfg.OutputRootDir = dir + "/archive"
		a := filepath.Join(dir, "a.pdf")
		b := filepath.Join(dir, "b.pdf")
		writeTestPDF(t, a, 1)
		writeTestPDF(t, b, 1)

		rec := doJSON(t, env.handler, http.MethodPost, "/api/pdf/merge", map[string]any{
			"filepaths": []any{a, b}, "outputFilename": "combined.pdf",
		}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		target, _ := body["targetPath"].(string)
		if filepath.Base(target) != "combined.pdf" {
			t.Fatalf("targetPath = %v", target)
		}
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("merged file missing: %v", err)
		}
	})

	t.Run("defaults the output name and sanitizes it", func(t *testing.T) {
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir
		env.base.settings.cfg.OutputRootDir = dir + "/archive"
		a := filepath.Join(dir, "a.pdf")
		b := filepath.Join(dir, "b.pdf")
		writeTestPDF(t, a, 1)
		writeTestPDF(t, b, 1)

		rec := doJSON(t, env.handler, http.MethodPost, "/api/pdf/merge", map[string]any{
			"filepaths": []any{a, b}, "outputFilename": "my report!",
		}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		target, _ := decodeJSON(t, rec)["targetPath"].(string)
		if filepath.Base(target) != "my_report_.pdf" {
			t.Fatalf("targetPath = %q", target)
		}
	})
}

// --- #33 POST /api/pdf/split ---------------------------------------------------------------------

func TestSplitPDFRoute(t *testing.T) {
	t.Run("rejects a missing filepath", func(t *testing.T) {
		env := newWriteEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/pdf/split", map[string]any{}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("rejects a single-page PDF", func(t *testing.T) {
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir
		env.base.settings.cfg.OutputRootDir = dir + "/archive"
		p := filepath.Join(dir, "one.pdf")
		writeTestPDF(t, p, 1)
		rec := doJSON(t, env.handler, http.MethodPost, "/api/pdf/split", map[string]any{"filepath": p}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("writes one file per page into the input folder", func(t *testing.T) {
		env := newWriteEnv()
		dir := t.TempDir()
		env.base.settings.cfg.InputDir = dir
		env.base.settings.cfg.OutputRootDir = dir + "/archive"
		p := filepath.Join(dir, "multi.pdf")
		writeTestPDF(t, p, 3)

		rec := doJSON(t, env.handler, http.MethodPost, "/api/pdf/split", map[string]any{"filepath": p}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		created, _ := body["createdFiles"].([]any)
		if len(created) != 3 {
			t.Fatalf("createdFiles = %v", body["createdFiles"])
		}
		for i, name := range created {
			want := "multi_page_" + strconv.Itoa(i+1) + ".pdf"
			if name != want {
				t.Fatalf("created[%d] = %v, want %s", i, name, want)
			}
			if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
				t.Fatalf("split file missing: %v", err)
			}
		}
	})

	t.Run("rejects a path outside the managed directories", func(t *testing.T) {
		env := newWriteEnv()
		env.base.settings.cfg.InputDir = t.TempDir()
		env.base.settings.cfg.OutputRootDir = t.TempDir()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/pdf/split", map[string]any{"filepath": "/etc/passwd"}, nil)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

// --- #29 POST /api/chat --------------------------------------------------------------------------

func TestChatRoute(t *testing.T) {
	t.Run("rejects an empty message", func(t *testing.T) {
		env := newWriteEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/chat", map[string]any{"message": "   "}, nil)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d", rec.Code)
		}
		if body := decodeJSON(t, rec); body["error"] != "Message text is required." {
			t.Fatalf("error = %v", body["error"])
		}
	})

	t.Run("returns an answer and matched documents", func(t *testing.T) {
		env := newWriteEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/chat", map[string]any{"message": "show my invoices", "history": []any{}}, nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := decodeJSON(t, rec)
		if _, ok := body["answer"]; !ok {
			t.Fatalf("answer missing: %v", body)
		}
		if _, ok := body["matchedDocuments"]; !ok {
			t.Fatalf("matchedDocuments missing: %v", body)
		}
	})
}
