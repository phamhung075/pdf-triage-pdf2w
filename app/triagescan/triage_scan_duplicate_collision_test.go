package triagescan

// Ported from pdf-triage's src/application/triage-scan-duplicate-collision.test.ts (2 cases).
//
// The TS suite mocked the settings module to temp dirs, mocked classify-document, pdf-extractor,
// ollama-client and json-registry, and used the REAL SQLite store so the insert-time
// `UNIQUE constraint failed: documents.checksum` was exercised end to end. The Go port uses the
// same shape: fakes for the IO seams, a real *store/database.Store over a t.TempDir() database,
// and no real project file (pdf_triage.db, __raws, __archive, registry.json) is ever touched.

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfextractor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// --- fakes ---------------------------------------------------------------------------------------

type fakeExtractor struct {
	result pdfextractor.ExtractedPDF
	err    error
	calls  int
}

func (f *fakeExtractor) ExtractPDFContent(filePath string) (pdfextractor.ExtractedPDF, error) {
	f.calls++
	return f.result, f.err
}

type fakeClassifier struct {
	fn    func(rawText, filename string) (documentschema.DocumentMetadata, error)
	calls int
}

func (f *fakeClassifier) ClassifyPDFText(rawText, filename, previousError string, now time.Time, doclingMarkdown string) (documentschema.DocumentMetadata, error) {
	f.calls++
	if f.fn != nil {
		return f.fn(rawText, filename)
	}
	return documentschema.DocumentMetadata{}, nil
}

type fakeOllama struct {
	ensure     bool
	embedCalls int
}

func (f *fakeOllama) EnsureOllamaModel(modelName string) bool { return f.ensure }

func (f *fakeOllama) GenerateEmbedding(text string) []float64 {
	f.embedCalls++
	return []float64{}
}

type fakeRegistry struct{ calls int }

func (f *fakeRegistry) SyncJSONRegistry() error {
	f.calls++
	return nil
}

type fakeRelocalizer struct {
	result          relocalize.RelocalizeResult
	err             error
	calls           int
	lastPath        string
	lastCategory    string
	lastSubcategory string
}

func (f *fakeRelocalizer) RelocalizeFileIfNeeded(filePath, category string, subcategory, dateStr, title *string) (relocalize.RelocalizeResult, error) {
	f.calls++
	f.lastPath = filePath
	f.lastCategory = category
	if subcategory != nil {
		f.lastSubcategory = *subcategory
	}
	return f.result, f.err
}

// --- harness -------------------------------------------------------------------------------------

type scanHarness struct {
	t           *testing.T
	scanner     *Scanner
	db          *database.Store
	inputDir    string
	outputDir   string
	extractor   *fakeExtractor
	classifier  *fakeClassifier
	ollama      *fakeOllama
	registry    *fakeRegistry
	relocalizer *fakeRelocalizer
}

func newScanHarness(t *testing.T) *scanHarness {
	t.Helper()
	root := t.TempDir()
	inputDir := filepath.Join(root, "__raws")
	outputDir := filepath.Join(root, "__archive")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatalf("mkdir input: %v", err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	store, err := database.Open(filepath.Join(root, "pdf_triage.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	h := &scanHarness{
		t:           t,
		db:          store,
		inputDir:    inputDir,
		outputDir:   outputDir,
		extractor:   &fakeExtractor{},
		classifier:  &fakeClassifier{},
		ollama:      &fakeOllama{ensure: true},
		registry:    &fakeRegistry{},
		relocalizer: &fakeRelocalizer{},
	}
	h.scanner = New(Deps{
		Config: settings.Config{
			InputDir:         inputDir,
			OutputRootDir:    outputDir,
			DBPath:           filepath.Join(root, "pdf_triage.db"),
			JSONRegistryPath: filepath.Join(root, "registry.json"),
			OllamaModel:      "qwen3.5:9b",
		},
		DB:         store,
		Extractor:  h.extractor,
		Classifier: h.classifier,
		Ollama:     h.ollama,
		Registry:   h.registry,
		// The photo path is not reached by the ported cases, so Converter stays nil, which the
		// pipeline treats exactly as "no conversion".
		Relocalizer:       h.relocalizer,
		FindBundleFolders: func(rootDir string) []string { return nil },
	})
	return h
}

// --- cases ---------------------------------------------------------------------------------------

func TestRunTriageScanChecksumCollisionAtInsertTime(t *testing.T) {
	h := newScanHarness(t)

	filePath := filepath.Join(h.inputDir, "race_duplicate.pdf")
	if err := os.WriteFile(filePath, []byte("dummy pdf bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	sharedChecksum := "chk-race-collision"
	h.extractor.result = pdfextractor.ExtractedPDF{
		Checksum: sharedChecksum,
		RawText:  "Some genuine long enough raw text content here for the test to accept.",
	}

	// Simulates another process/request inserting a document with this exact checksum during the
	// window between this file's pre-check (which found nothing) and classifyPDFText resolving.
	h.classifier.fn = func(rawText, filename string) (documentschema.DocumentMetadata, error) {
		if _, err := h.db.InsertDocumentRecord(database.NewDocument{
			Checksum:         sharedChecksum,
			Title:            "Facture SFR",
			Date:             "2026-01-15",
			Category:         "invoices",
			Subcategory:      "sfr",
			RawText:          "contenu original suffisamment long pour ne pas etre considere vide",
			OriginalFilename: "already_archived_elsewhere.pdf",
			OriginalPath:     "C:/never/used.pdf",
			Status:           "MOVED",
		}); err != nil {
			t.Fatalf("racing insert: %v", err)
		}
		return documentschema.DocumentMetadata{
			Titre:           "Race Duplicate Doc",
			Categorie:       "invoices",
			Subcategorie:    "sfr",
			Date:            "2024-01-01",
			MarkdownContent: "",
		}, nil
	}

	result, err := h.scanner.RunTriageScan(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("RunTriageScan: %v", err)
	}

	if len(result.Items) != 1 {
		t.Fatalf("items = %d, want 1", len(result.Items))
	}
	if got := result.Items[0].Status; got != "SKIPPED_DUPLICATE" {
		t.Fatalf("item status = %q, want SKIPPED_DUPLICATE", got)
	}
	if fileExists(filePath) {
		t.Fatalf("duplicate file %s was left in __raws", filePath)
	}

	docs, err := h.db.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("documents = %d, want 1", len(docs))
	}
}

func TestRunTriageScanDoesNotTouchFilesWhenOllamaDown(t *testing.T) {
	h := newScanHarness(t)

	filePath := filepath.Join(h.inputDir, "pending.pdf")
	if err := os.WriteFile(filePath, []byte("some pdf bytes"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	h.ollama.ensure = false

	var events []Event
	result, err := h.scanner.RunTriageScan(context.Background(), func(event Event) {
		events = append(events, event)
	}, nil)
	if err != nil {
		t.Fatalf("RunTriageScan: %v", err)
	}

	if !result.OllamaDown {
		t.Errorf("result.OllamaDown = false, want true")
	}
	if result.ScannedCount != 0 {
		t.Errorf("scannedCount = %d, want 0", result.ScannedCount)
	}
	if result.ProcessedCount != 0 {
		t.Errorf("processedCount = %d, want 0", result.ProcessedCount)
	}
	if len(result.Items) != 0 {
		t.Errorf("items = %d, want 0", len(result.Items))
	}

	hasOllamaDown := false
	hasScanCompleted := false
	for _, event := range events {
		switch event.Type {
		case EventOllamaDown:
			hasOllamaDown = true
		case EventScanCompleted:
			hasScanCompleted = true
		}
	}
	if !hasOllamaDown {
		t.Errorf("no OLLAMA_DOWN event; events = %v", events)
	}
	if hasScanCompleted {
		t.Errorf("unexpected SCAN_COMPLETED event; events = %v", events)
	}

	if h.extractor.calls != 0 {
		t.Errorf("extractor called %d times, want 0", h.extractor.calls)
	}
	if h.classifier.calls != 0 {
		t.Errorf("classifier called %d times, want 0", h.classifier.calls)
	}
	if h.ollama.embedCalls != 0 {
		t.Errorf("embedding called %d times, want 0", h.ollama.embedCalls)
	}
	if !fileExists(filePath) {
		t.Errorf("pending file %s was moved or deleted", filePath)
	}
}
