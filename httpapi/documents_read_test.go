package httpapi

import (
	"archive/zip"
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

	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// docReadStore is an in-memory DocumentStore. byID is consulted first; a missing key yields nil,
// which the routes treat as "Document not found".
type docReadStore struct {
	docs    []database.DocumentRecord
	docsErr error
	byID    map[int64]*database.DocumentRecord
	byIDErr error
}

func (s *docReadStore) GetAllDocuments() ([]database.DocumentRecord, error) {
	return s.docs, s.docsErr
}

func (s *docReadStore) GetDocumentByID(id int64) (*database.DocumentRecord, error) {
	if s.byIDErr != nil {
		return nil, s.byIDErr
	}
	if s.byID == nil {
		return nil, nil
	}
	return s.byID[id], nil
}

// docReadMcp is an in-memory McpToolLister.
type docReadMcp struct {
	tools []McpToolInfo
	err   error
}

func (m *docReadMcp) ListTools() ([]McpToolInfo, error) { return m.tools, m.err }

// docReadEnv bundles the group's fakes and the handler. The handler is the REAL NewServer with only
// this route group installed, so ServeMux pattern resolution is exercised, not bypassed.
type docReadEnv struct {
	settings *fakeSettings
	docs     *docReadStore
	opener   *fakeOpener
	spawner  *fakeSpawner
	mcp      *docReadMcp
	deps     DocumentReadDeps
	handler  http.Handler
}

func newDocReadEnv() *docReadEnv {
	env := &docReadEnv{
		settings: &fakeSettings{
			cfg: settings.Config{
				InputDir:      "/tmp/docread-raws",
				OutputRootDir: "/tmp/docread-archive",
			},
		},
		docs:    &docReadStore{byID: map[int64]*database.DocumentRecord{}},
		opener:  &fakeOpener{launch: Launch{Cmd: "xdg-open"}},
		spawner: &fakeSpawner{},
		mcp:     &docReadMcp{},
	}
	env.deps = DocumentReadDeps{
		Documents: env.docs,
		Settings:  env.settings,
		Opener:    env.opener,
		Spawner:   env.spawner,
		Mcp:       env.mcp,
	}
	env.handler = NewServer(Deps{Settings: env.settings, RouteGroups: []RouteGroup{DocumentReadRoutes(env.deps)}})
	return env
}

// docReadSample mirrors web-server.test.ts's sampleDoc(), including the JSON-encoded tags.
func docReadSample(overrides ...func(*database.DocumentRecord)) database.DocumentRecord {
	doc := database.DocumentRecord{
		ID:               1,
		Checksum:         "abc123",
		FileType:         "PDF",
		SourceImagePath:  "",
		Title:            "Facture SFR Janvier",
		Registre:         "REF-001",
		Date:             "2026-01-15",
		Category:         "invoices",
		Subcategory:      "sfr",
		Summary:          "Facture mensuelle SFR pour janvier",
		Tags:             `["facture","sfr"]`,
		RawText:          "Contenu complet de la facture SFR de janvier 2026",
		MarkdownContent:  "# Facture SFR",
		OriginalFilename: "facture.pdf",
		OriginalPath:     "C:/pdf-triage-test/__raws/facture.pdf",
		NewPath:          "",
		Embedding:        "[]",
		Status:           "COMPLETED",
		CreatedAt:        "2026-01-15T10:00:00.000Z",
		UpdatedAt:        "2026-01-15T10:00:00.000Z",
	}
	for _, override := range overrides {
		override(&doc)
	}
	return doc
}

func docReadJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return out
}

// newDocReadManagerDir returns a fresh (inputDir, outputDir) pair so FindActualFileOnDisk cannot
// wander into shared temp state.
func newDocReadManagerDir(t *testing.T) (string, string) {
	t.Helper()
	return t.TempDir(), t.TempDir()
}

// TestListDocuments ports the four GET /api/documents cases in web-server.test.ts.
func TestListDocuments(t *testing.T) {
	t.Run("returns the formatted, truncated document list", func(t *testing.T) {
		env := newDocReadEnv()
		env.docs.docs = []database.DocumentRecord{docReadSample()}

		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents", nil, nil)
		if rec.Code != 200 {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		body := docReadJSON(t, rec)
		if body["total"].(float64) != 1 {
			t.Fatalf("total = %v, want 1", body["total"])
		}
		first := body["documents"].([]any)[0].(map[string]any)
		for key, want := range map[string]any{
			"id": float64(1), "title": "Facture SFR Janvier",
			"category": "invoices", "subcategory": "sfr",
		} {
			if first[key] != want {
				t.Fatalf("%s = %#v, want %#v", key, first[key], want)
			}
		}
		tags := first["tags"].([]any)
		if len(tags) != 2 || tags[0] != "facture" || tags[1] != "sfr" {
			t.Fatalf("tags = %#v", tags)
		}
	})

	t.Run("truncates raw_text to 800 UTF-16 code units and defaults subcategory", func(t *testing.T) {
		env := newDocReadEnv()
		doc := docReadSample(func(d *database.DocumentRecord) {
			d.RawText = strings.Repeat("x", 900)
			d.Subcategory = ""
		})
		env.docs.docs = []database.DocumentRecord{doc}

		first := docReadJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/documents", nil, nil))["documents"].([]any)[0].(map[string]any)
		if got := first["raw_text"].(string); len(got) != 800 {
			t.Fatalf("raw_text length = %d, want 800", len(got))
		}
		if first["subcategory"] != "general" {
			t.Fatalf("subcategory = %v, want general", first["subcategory"])
		}
	})

	t.Run("filters by category and subcategory query params, case-insensitively", func(t *testing.T) {
		env := newDocReadEnv()
		env.docs.docs = []database.DocumentRecord{
			docReadSample(func(d *database.DocumentRecord) { d.ID = 1; d.Category = "invoices"; d.Subcategory = "sfr" }),
			docReadSample(func(d *database.DocumentRecord) {
				d.ID = 2
				d.Category = "invoices"
				d.Subcategory = "edf"
				d.Checksum = "c2"
			}),
			docReadSample(func(d *database.DocumentRecord) {
				d.ID = 3
				d.Category = "health"
				d.Subcategory = "general"
				d.Checksum = "c3"
			}),
		}

		body := docReadJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/documents?category=INVOICES&subcategory=SFR", nil, nil))
		if body["total"].(float64) != 1 {
			t.Fatalf("total = %v, want 1", body["total"])
		}
		if id := body["documents"].([]any)[0].(map[string]any)["id"].(float64); id != 1 {
			t.Fatalf("id = %v, want 1", id)
		}
	})

	t.Run("filters by the free-text search query across title/summary/tags/raw_text", func(t *testing.T) {
		env := newDocReadEnv()
		env.docs.docs = []database.DocumentRecord{
			docReadSample(func(d *database.DocumentRecord) { d.ID = 1; d.Title = "Facture SFR" }),
			docReadSample(func(d *database.DocumentRecord) {
				d.ID = 2
				d.Title = "Bulletin de Salaire"
				d.Summary = "Paie mensuelle"
				d.Checksum = "c2"
			}),
		}

		body := docReadJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/documents?q=salaire", nil, nil))
		if body["total"].(float64) != 1 {
			t.Fatalf("total = %v, want 1", body["total"])
		}
		if id := body["documents"].([]any)[0].(map[string]any)["id"].(float64); id != 2 {
			t.Fatalf("id = %v, want 2", id)
		}
	})

	t.Run("returns 500 with the error message when the DB layer throws", func(t *testing.T) {
		env := newDocReadEnv()
		env.docs.docsErr = fmt.Errorf("DB unavailable")

		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents", nil, nil)
		if rec.Code != 500 || docReadJSON(t, rec)["error"] != "DB unavailable" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestGetDocumentByID ports the two GET /api/documents/:id cases.
func TestGetDocumentByID(t *testing.T) {
	t.Run("returns the full document with tags parsed", func(t *testing.T) {
		env := newDocReadEnv()
		doc := docReadSample()
		env.docs.byID[1] = &doc

		body := docReadJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/documents/1", nil, nil))
		if body["title"] != "Facture SFR Janvier" {
			t.Fatalf("title = %v", body["title"])
		}
		tags := body["tags"].([]any)
		if len(tags) != 2 || tags[0] != "facture" || tags[1] != "sfr" {
			t.Fatalf("tags = %#v", tags)
		}
	})

	t.Run("returns 404 when the document does not exist", func(t *testing.T) {
		env := newDocReadEnv()
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/999", nil, nil)
		if rec.Code != 404 || docReadJSON(t, rec)["error"] != "Document not found" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestDocumentMarkdown ports the four GET /api/documents/:id/markdown cases.
func TestDocumentMarkdown(t *testing.T) {
	t.Run("downloads markdown_content as a .md with a sanitized filename", func(t *testing.T) {
		env := newDocReadEnv()
		doc := docReadSample(func(d *database.DocumentRecord) { d.Title = "Facture / SFR: Janvier?" })
		env.docs.byID[1] = &doc

		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/1/markdown", nil, nil)
		if rec.Code != 200 {
			t.Fatalf("status = %d", rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
			t.Fatalf("Content-Type = %q", ct)
		}
		if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "Facture _ SFR_ Janvier_.md") {
			t.Fatalf("Content-Disposition = %q", cd)
		}
		if rec.Body.String() != "# Facture SFR" {
			t.Fatalf("body = %q", rec.Body.String())
		}
	})

	t.Run("RFC-6266-encodes an accented title instead of emitting raw UTF-8", func(t *testing.T) {
		env := newDocReadEnv()
		doc := docReadSample(func(d *database.DocumentRecord) { d.Title = "Avis de Taxes Foncières" })
		env.docs.byID[1] = &doc

		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/1/markdown", nil, nil)
		cd := rec.Header().Get("Content-Disposition")
		if !strings.Contains(cd, `filename="Avis de Taxes Fonci_res.md"`) {
			t.Fatalf("ASCII fallback missing from %q", cd)
		}
		wantEncoded := "filename*=UTF-8''" + "Avis%20de%20Taxes%20Fonci%C3%A8res.md"
		if !strings.Contains(cd, wantEncoded) {
			t.Fatalf("filename* missing: got %q, want substring %q", cd, wantEncoded)
		}
	})

	t.Run("falls back to raw_text when markdown_content is empty", func(t *testing.T) {
		env := newDocReadEnv()
		doc := docReadSample(func(d *database.DocumentRecord) { d.MarkdownContent = "" })
		env.docs.byID[1] = &doc

		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/1/markdown", nil, nil)
		if rec.Body.String() != "Contenu complet de la facture SFR de janvier 2026" {
			t.Fatalf("body = %q", rec.Body.String())
		}
	})

	t.Run("returns 404 when the document does not exist", func(t *testing.T) {
		env := newDocReadEnv()
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/999/markdown", nil, nil)
		if rec.Code != 404 || docReadJSON(t, rec)["error"] != "Document not found" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestExportMarkdown ports the two GET /api/documents/export/markdown cases and proves the export
// literal still wins over the {id}/markdown wildcard.
func TestExportMarkdown(t *testing.T) {
	t.Run("bundles every document's markdown into one ZIP with no on-disk file required", func(t *testing.T) {
		env := newDocReadEnv()
		env.docs.docs = []database.DocumentRecord{
			docReadSample(func(d *database.DocumentRecord) { d.ID = 1; d.Title = "Facture SFR"; d.MarkdownContent = "# SFR" }),
			docReadSample(func(d *database.DocumentRecord) { d.ID = 2; d.Title = "Facture EDF"; d.MarkdownContent = "# EDF" }),
		}

		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/export/markdown", nil, nil)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" {
			t.Fatalf("status=%d content-type=%q", rec.Code, rec.Header().Get("Content-Type"))
		}
		body := rec.Body.Bytes()
		if sig := docReadUint32LE(body, len(body)-22); sig != 0x06054b50 {
			t.Fatalf("EOCD signature = %#x", sig)
		}
		if count := docReadUint16LE(body, len(body)-22+8); count != 2 {
			t.Fatalf("entry count = %d, want 2", count)
		}
		names, contents := docReadUnzip(t, body)
		if fmt.Sprint(names) != "[Facture SFR.md Facture EDF.md]" {
			t.Fatalf("names = %v", names)
		}
		if contents["Facture SFR.md"] != "# SFR" || contents["Facture EDF.md"] != "# EDF" {
			t.Fatalf("contents = %v", contents)
		}
	})

	t.Run("dedupes filenames for documents sharing the same title by suffixing the doc id", func(t *testing.T) {
		env := newDocReadEnv()
		env.docs.docs = []database.DocumentRecord{
			docReadSample(func(d *database.DocumentRecord) {
				d.ID = 1
				d.Title = "Accusé de réception"
				d.MarkdownContent = "# A"
			}),
			docReadSample(func(d *database.DocumentRecord) {
				d.ID = 2
				d.Title = "Accusé de réception"
				d.MarkdownContent = "# B"
			}),
		}

		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/export/markdown", nil, nil)
		names, _ := docReadUnzip(t, rec.Body.Bytes())
		if fmt.Sprint(names) != "[Accusé de réception.md Accusé de réception_2.md]" {
			t.Fatalf("names = %v", names)
		}
	})

	t.Run("export literal resolves before the {id}/markdown wildcard", func(t *testing.T) {
		env := newDocReadEnv()
		doc := docReadSample()
		env.docs.byID[1] = &doc
		env.docs.docs = []database.DocumentRecord{doc}

		exportRec := doJSON(t, env.handler, http.MethodGet, "/api/documents/export/markdown", nil, nil)
		if ct := exportRec.Header().Get("Content-Type"); ct != "application/zip" {
			t.Fatalf("export/markdown Content-Type = %q, want application/zip", ct)
		}
		singleRec := doJSON(t, env.handler, http.MethodGet, "/api/documents/1/markdown", nil, nil)
		if ct := singleRec.Header().Get("Content-Type"); !strings.Contains(ct, "text/markdown") {
			t.Fatalf("/1/markdown Content-Type = %q, want text/markdown", ct)
		}
	})
}

// TestDocumentFileAndFileByPath ports the four upstream file-serving cases, pinning the known-red
// 404-vs-403 file-by-path case at its ACTUAL 403.
func TestDocumentFileAndFileByPath(t *testing.T) {
	t.Run("returns 404 if document ID does not exist", func(t *testing.T) {
		env := newDocReadEnv()
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/999/file", nil, nil)
		if rec.Code != 404 {
			t.Fatalf("status = %d, want 404", rec.Code)
		}
	})

	t.Run("returns 404 for a nonexistent path inside the managed archive", func(t *testing.T) {
		env := newDocReadEnv()
		inputDir, outputDir := newDocReadManagerDir(t)
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir

		missing := filepath.Join(outputDir, "nonexistent.pdf")
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/file-by-path?path="+missing, nil, nil)
		if rec.Code != 404 || !strings.Contains(docReadJSON(t, rec)["error"].(string), "missing on disk") {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("pins the known-red drive-letter candidate at the actual 403 (upstream expects 404)", func(t *testing.T) {
		// web-server.test.ts:946 expects 404 for this candidate, but the live TS guard (and this
		// port, via app/guards.ResolveManagedPath) rejects a drive-letter path when the managed roots
		// are not WSL mount paths. See app/guards/path.go's contradiction write-up.
		env := newDocReadEnv()
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/file-by-path?path=C:/pdf-triage-test/__archive/nonexistent.pdf", nil, nil)
		if rec.Code != 403 {
			t.Fatalf("status = %d, want 403 (the pinned actual verdict)", rec.Code)
		}
		if !strings.Contains(docReadJSON(t, rec)["error"].(string), "outside the managed") {
			t.Fatalf("body = %s", rec.Body.String())
		}
	})

	t.Run("rejects a path outside the managed directories", func(t *testing.T) {
		env := newDocReadEnv()
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/file-by-path?path=/etc/passwd", nil, nil)
		if rec.Code != 403 {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("rejects a traversal attempt that escapes the managed root", func(t *testing.T) {
		env := newDocReadEnv()
		inputDir, outputDir := newDocReadManagerDir(t)
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir

		traversal := filepath.Join(outputDir, "..", "..", "etc", "hosts")
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/file-by-path?path="+traversal, nil, nil)
		if rec.Code != 403 {
			t.Fatalf("status = %d, want 403", rec.Code)
		}
	})

	t.Run("serves an existing managed PDF inline", func(t *testing.T) {
		env := newDocReadEnv()
		inputDir, outputDir := newDocReadManagerDir(t)
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir

		pdfPath := filepath.Join(outputDir, "doc.pdf")
		payload := []byte("%PDF-1.4 fake")
		if err := os.WriteFile(pdfPath, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/file-by-path?path="+pdfPath, nil, nil)
		if rec.Code != 200 {
			t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
			t.Fatalf("Content-Type = %q", ct)
		}
		if cd := rec.Header().Get("Content-Disposition"); cd != "inline" {
			t.Fatalf("Content-Disposition = %q", cd)
		}
		if !bytes.Equal(rec.Body.Bytes(), payload) {
			t.Fatalf("body = %q", rec.Body.String())
		}
	})

	t.Run("serves the document file from new_path and falls back to original_path", func(t *testing.T) {
		inputDir, outputDir := newDocReadManagerDir(t)

		newFile := filepath.Join(outputDir, "new.pdf")
		if err := os.WriteFile(newFile, []byte("new-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		env := newDocReadEnv()
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir
		doc := docReadSample(func(d *database.DocumentRecord) {
			d.NewPath = newFile
			d.OriginalPath = filepath.Join(inputDir, "orig.pdf")
		})
		env.docs.byID[1] = &doc
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/1/file", nil, nil)
		if rec.Code != 200 || rec.Body.String() != "new-bytes" || rec.Header().Get("Content-Disposition") != "inline" {
			t.Fatalf("status=%d cd=%q body=%q", rec.Code, rec.Header().Get("Content-Disposition"), rec.Body.String())
		}

		env2 := newDocReadEnv()
		env2.settings.cfg.InputDir = inputDir
		env2.settings.cfg.OutputRootDir = outputDir
		origFile := filepath.Join(inputDir, "orig.pdf")
		if err := os.WriteFile(origFile, []byte("orig-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		doc2 := docReadSample(func(d *database.DocumentRecord) { d.NewPath = ""; d.OriginalPath = origFile })
		env2.docs.byID[1] = &doc2
		rec2 := doJSON(t, env2.handler, http.MethodGet, "/api/documents/1/file", nil, nil)
		if rec2.Code != 200 || rec2.Body.String() != "orig-bytes" {
			t.Fatalf("fallback status=%d body=%q", rec2.Code, rec2.Body.String())
		}
	})

	t.Run("returns 404 when the document file is missing on disk", func(t *testing.T) {
		env := newDocReadEnv()
		doc := docReadSample(func(d *database.DocumentRecord) {
			d.NewPath = filepath.Join(t.TempDir(), "gone.pdf")
			d.OriginalPath = filepath.Join(t.TempDir(), "gone2.pdf")
		})
		env.docs.byID[1] = &doc
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/1/file", nil, nil)
		if rec.Code != 404 || docReadJSON(t, rec)["error"] != "PDF file missing on disk" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestOpenFolder ports the two upstream POST /api/documents/:id/open-folder cases.
func TestOpenFolder(t *testing.T) {
	t.Run("opens the file manager via spawn with an argument array", func(t *testing.T) {
		inputDir, outputDir := newDocReadManagerDir(t)
		archiveFile := filepath.Join(outputDir, "facture & cie.pdf")
		if err := os.WriteFile(archiveFile, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}

		env := newDocReadEnv()
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir
		doc := docReadSample(func(d *database.DocumentRecord) { d.NewPath = archiveFile })
		env.docs.byID[1] = &doc

		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/1/open-folder", nil, nil)
		if rec.Code != 200 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if !env.opener.reveal {
			t.Fatal("RevealInFileManager was not called")
		}
		if len(env.spawner.calls) != 1 {
			t.Fatalf("spawn calls = %d, want 1", len(env.spawner.calls))
		}
		// The file path must reach the spawner as an argv entry, never a shell string.
		found := false
		for _, arg := range env.spawner.calls[0].Args {
			if strings.Contains(arg, "facture & cie.pdf") {
				found = true
			}
		}
		if !found {
			t.Fatalf("spawn args %v do not contain the file path", env.spawner.calls[0].Args)
		}
		if docReadJSON(t, rec)["filePath"] != archiveFile {
			t.Fatalf("filePath = %v", docReadJSON(t, rec)["filePath"])
		}
	})

	t.Run("returns 404 when the document file is missing on disk", func(t *testing.T) {
		env := newDocReadEnv()
		inputDir, outputDir := newDocReadManagerDir(t)
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir
		doc := docReadSample(func(d *database.DocumentRecord) {
			d.NewPath = ""
			d.OriginalPath = ""
			d.OriginalFilename = ""
		})
		env.docs.byID[1] = &doc

		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/1/open-folder", nil, nil)
		if rec.Code != 404 || docReadJSON(t, rec)["error"] != "Document file not found on disk" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestSourceImage covers GET /api/documents/:id/source-image, which has no upstream HTTP case.
func TestSourceImage(t *testing.T) {
	t.Run("returns 404 when the document does not exist", func(t *testing.T) {
		env := newDocReadEnv()
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/9/source-image", nil, nil)
		if rec.Code != 404 || docReadJSON(t, rec)["error"] != "Document not found" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 when no source image was retained", func(t *testing.T) {
		env := newDocReadEnv()
		doc := docReadSample(func(d *database.DocumentRecord) { d.SourceImagePath = "" })
		env.docs.byID[1] = &doc
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/1/source-image", nil, nil)
		if rec.Code != 404 || docReadJSON(t, rec)["error"] != "This document has no retained source image." {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("rejects a source image outside the managed directories", func(t *testing.T) {
		env := newDocReadEnv()
		doc := docReadSample(func(d *database.DocumentRecord) { d.SourceImagePath = "C:/outside/photo.png" })
		env.docs.byID[1] = &doc
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/1/source-image", nil, nil)
		if rec.Code != 403 || docReadJSON(t, rec)["error"] != "Source image is outside the managed directories." {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("returns 404 when the managed source image is gone", func(t *testing.T) {
		env := newDocReadEnv()
		inputDir, outputDir := newDocReadManagerDir(t)
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir
		doc := docReadSample(func(d *database.DocumentRecord) { d.SourceImagePath = filepath.Join(outputDir, "gone.png") })
		env.docs.byID[1] = &doc
		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/1/source-image", nil, nil)
		if rec.Code != 404 || docReadJSON(t, rec)["error"] != "Source image is no longer on disk." {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("serves a retained PNG with the extension MIME type", func(t *testing.T) {
		env := newDocReadEnv()
		inputDir, outputDir := newDocReadManagerDir(t)
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir
		photo := filepath.Join(outputDir, "photo.PNG")
		if err := os.WriteFile(photo, []byte("png-bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
		doc := docReadSample(func(d *database.DocumentRecord) { d.SourceImagePath = photo })
		env.docs.byID[1] = &doc

		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/1/source-image", nil, nil)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || rec.Body.String() != "png-bytes" {
			t.Fatalf("status=%d ct=%q body=%q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
	})
}

// TestSourceImageMIME pins the TS extension table (web-server.ts:1098-1104).
func TestSourceImageMIME(t *testing.T) {
	cases := map[string]string{
		"a.png": "image/png", "a.PNG": "image/png",
		"a.webp": "image/webp", "a.bmp": "image/bmp",
		"a.tiff": "image/tiff", "a.tif": "image/tiff",
		"a.jpg": "image/jpeg", "a.jpeg": "image/jpeg", "a": "image/jpeg",
	}
	for path, want := range cases {
		if got := sourceImageMIME(path); got != want {
			t.Errorf("sourceImageMIME(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestPackageZip covers POST /api/documents/package-zip, which has no upstream HTTP case.
func TestPackageZip(t *testing.T) {
	t.Run("rejects a missing or empty docIds array", func(t *testing.T) {
		env := newDocReadEnv()
		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/package-zip", map[string]any{"docIds": []any{}}, nil)
		if rec.Code != 400 || docReadJSON(t, rec)["error"] != "docIds array is required and cannot be empty." {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		rec = doJSON(t, env.handler, http.MethodPost, "/api/documents/package-zip", map[string]any{}, nil)
		if rec.Code != 400 {
			t.Fatalf("missing docIds status = %d, want 400", rec.Code)
		}
	})

	t.Run("returns 404 when none of the requested files exist on disk", func(t *testing.T) {
		env := newDocReadEnv()
		inputDir, outputDir := newDocReadManagerDir(t)
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir
		doc := docReadSample(func(d *database.DocumentRecord) {
			d.NewPath = ""
			d.OriginalPath = ""
			d.OriginalFilename = ""
		})
		env.docs.byID[1] = &doc

		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/package-zip", map[string]any{"docIds": []any{1}}, nil)
		if rec.Code != 404 || docReadJSON(t, rec)["error"] != "None of the requested document files exist on disk." {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("packages a document file under its sanitized title", func(t *testing.T) {
		env := newDocReadEnv()
		inputDir, outputDir := newDocReadManagerDir(t)
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir
		pdfPath := filepath.Join(outputDir, "raw.pdf")
		if err := os.WriteFile(pdfPath, []byte("pdf-payload"), 0o600); err != nil {
			t.Fatal(err)
		}
		doc := docReadSample(func(d *database.DocumentRecord) { d.NewPath = pdfPath; d.Title = "Facture / SFR" })
		env.docs.byID[1] = &doc

		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/package-zip", map[string]any{"docIds": []any{1, "bogus"}, "zipName": "my/dossier:2026"}, nil)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/zip" {
			t.Fatalf("status=%d ct=%q body=%s", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
		}
		if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="my_dossier_2026"` {
			t.Fatalf("Content-Disposition = %q", cd)
		}
		names, contents := docReadUnzip(t, rec.Body.Bytes())
		if fmt.Sprint(names) != "[Facture _ SFR.pdf]" {
			t.Fatalf("names = %v", names)
		}
		if contents["Facture _ SFR.pdf"] != "pdf-payload" {
			t.Fatalf("contents = %v", contents)
		}
	})

	t.Run("uses the default download name when zipName is omitted", func(t *testing.T) {
		env := newDocReadEnv()
		inputDir, outputDir := newDocReadManagerDir(t)
		env.settings.cfg.InputDir = inputDir
		env.settings.cfg.OutputRootDir = outputDir
		pdfPath := filepath.Join(outputDir, "raw.pdf")
		if err := os.WriteFile(pdfPath, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		doc := docReadSample(func(d *database.DocumentRecord) { d.NewPath = pdfPath })
		env.docs.byID[1] = &doc

		rec := doJSON(t, env.handler, http.MethodPost, "/api/documents/package-zip", map[string]any{"docIds": []any{1}}, nil)
		if cd := rec.Header().Get("Content-Disposition"); cd != `attachment; filename="dossier_documents_package.zip"` {
			t.Fatalf("Content-Disposition = %q", cd)
		}
	})
}

// TestMCPStatus covers GET /api/mcp/status through the injected lister.
func TestMCPStatus(t *testing.T) {
	t.Run("reports the active tools", func(t *testing.T) {
		env := newDocReadEnv()
		env.mcp.tools = []McpToolInfo{{Name: "search_documents", Description: "Search"}, {Name: "list_categories", Description: "List"}}
		body := docReadJSON(t, doJSON(t, env.handler, http.MethodGet, "/api/mcp/status", nil, nil))
		if body["status"] != "active" || body["connected"] != true || body["toolsCount"].(float64) != 2 {
			t.Fatalf("body = %v", body)
		}
		tools := body["tools"].([]any)
		if tools[0].(map[string]any)["name"] != "search_documents" {
			t.Fatalf("tools = %v", tools)
		}
	})

	t.Run("folds a lister error into a 500 error envelope", func(t *testing.T) {
		env := newDocReadEnv()
		env.mcp.err = fmt.Errorf("mcp unavailable")
		rec := doJSON(t, env.handler, http.MethodGet, "/api/mcp/status", nil, nil)
		body := docReadJSON(t, rec)
		if rec.Code != 500 || body["status"] != "error" || body["connected"] != false || body["error"] != "mcp unavailable" {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
	})
}

// TestExportCSV pins the UTF-8 BOM, exact quoting and CRLF row joining.
func TestExportCSV(t *testing.T) {
	t.Run("writes a BOM, the header and quoted rows with CRLF and no trailing newline", func(t *testing.T) {
		env := newDocReadEnv()
		env.docs.docs = []database.DocumentRecord{docReadSample()}

		rec := doJSON(t, env.handler, http.MethodGet, "/api/documents/export/csv", nil, nil)
		if rec.Code != 200 || rec.Header().Get("Content-Type") != "text/csv; charset=utf-8" {
			t.Fatalf("status=%d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
		}
		body := rec.Body.String()
		if !strings.HasPrefix(body, "\uFEFF") {
			t.Fatalf("body does not start with a UTF-8 BOM: %q", body)
		}
		lines := strings.Split(body, "\r\n")
		if len(lines) != 2 {
			t.Fatalf("line count = %d, want 2 (no trailing newline)\n%q", len(lines), body)
		}
		if !strings.HasPrefix(lines[0], "\uFEFF\"ID\",\"Checksum\",\"Title\"") {
			t.Fatalf("header = %q", lines[0])
		}
		if !strings.Contains(lines[1], `"Facture SFR Janvier"`) || !strings.Contains(lines[1], `"REF-001"`) {
			t.Fatalf("row = %q", lines[1])
		}
		if strings.HasSuffix(body, "\r\n") {
			t.Fatalf("body has a trailing newline")
		}
	})

	t.Run("doubles embedded quotes and flattens summary newlines", func(t *testing.T) {
		env := newDocReadEnv()
		doc := docReadSample(func(d *database.DocumentRecord) {
			d.Title = `A "quoted" title`
			d.Summary = "line one\r\nline two\nline three"
		})
		env.docs.docs = []database.DocumentRecord{doc}

		body := doJSON(t, env.handler, http.MethodGet, "/api/documents/export/csv", nil, nil).Body.String()
		if !strings.Contains(body, `A ""quoted"" title`) {
			t.Fatalf("embedded quotes not doubled: %q", body)
		}
		if !strings.Contains(body, "line one line two line three") {
			t.Fatalf("summary newlines not flattened: %q", body)
		}
	})
}

// TestDocumentReadRoutesDoNotPanic is a direct regression guard for the ServeMux conflict question:
// building the group registers /api/documents/export/{csv,markdown} alongside /{id} and /{id}/markdown.
func TestDocumentReadRoutesDoNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("registering the document READ route group panicked: %v", r)
		}
	}()
	env := newDocReadEnv()
	if env.handler == nil {
		t.Fatal("handler is nil")
	}
}

func docReadUint32LE(b []byte, offset int) uint32 {
	return uint32(b[offset]) | uint32(b[offset+1])<<8 | uint32(b[offset+2])<<16 | uint32(b[offset+3])<<24
}

func docReadUint16LE(b []byte, offset int) uint16 {
	return uint16(b[offset]) | uint16(b[offset+1])<<8
}

func docReadUnzip(t *testing.T, data []byte) ([]string, map[string]string) {
	t.Helper()
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("open zip: %v", err)
	}
	names := make([]string, 0, len(reader.File))
	contents := map[string]string{}
	for _, file := range reader.File {
		names = append(names, file.Name)
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("open entry %q: %v", file.Name, err)
		}
		body, err := io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			t.Fatalf("read entry %q: %v", file.Name, err)
		}
		contents[file.Name] = string(body)
	}
	return names, contents
}
