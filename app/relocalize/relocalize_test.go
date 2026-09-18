package relocalize

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfextractor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

// These cases are ported from pdf-triage's src/application/relocalize-document.test.ts (24 cases).
// The 16 cases for relocalizeFileIfNeeded (4), moveBackToRaws (3) and
// reclassifyAndRelocalizeDocument (9) live here. The other 8 — findActualFileOnDisk (5) and
// ensureCategoryAndSubcategoryExist (3) — are the shared guards and are already ported in
// app/guards/files_test.go and app/guards/categories_test.go; the migration design decision 6
// forbids reimplementing or re-porting a guard at a second call site, so they are not duplicated.
//
// The TS suite redirects CONFIG to temp dirs via a whole-module settings mock and opens a temp
// DB. The Go equivalent is the h.deps.Config + *database.Store pair built by newHarness against a
// t.TempDir(); no real project file (pdf_triage.db, __raws, __archive, manual_decisions.json,
// registry.json) is ever touched.
//
// Two cases are ADDED, not ported: deleteDocumentAndMoveToTrash has no upstream case in this file
// (it is covered here directly, Golden Rule 15), and renameAtomicNoOverwrite is exercised directly
// for the collision-suffix and concurrency behavior that the ported relocalize case only observes
// indirectly (see atomic_test.go).

// --- fakes ---------------------------------------------------------------------------------------

type fakeCategories struct {
	config documentschema.CategoriesConfig
	saves  int
	saved  []*documentschema.CategoryItem
}

func (f *fakeCategories) GetCategoriesConfig() documentschema.CategoriesConfig {
	return f.config
}

func (f *fakeCategories) SaveCategoriesConfig(cats []*documentschema.CategoryItem) error {
	f.saves++
	f.saved = cats
	f.config = documentschema.CategoriesConfig{Categories: cats}
	return nil
}

type fakeExtractor struct {
	result   pdfextractor.ExtractedPDF
	err      error
	calls    int
	lastPath string
}

func (f *fakeExtractor) ExtractPDFContent(filePath string) (pdfextractor.ExtractedPDF, error) {
	f.calls++
	f.lastPath = filePath
	return f.result, f.err
}

type classifyCall struct {
	rawText         string
	filename        string
	previousError   string
	doclingMarkdown string
	now             time.Time
}

type fakeClassifier struct {
	result documentschema.DocumentMetadata
	err    error
	calls  []classifyCall
}

func (f *fakeClassifier) ClassifyPDFText(rawText, filename, previousError string, now time.Time, doclingMarkdown string) (documentschema.DocumentMetadata, error) {
	f.calls = append(f.calls, classifyCall{
		rawText:         rawText,
		filename:        filename,
		previousError:   previousError,
		doclingMarkdown: doclingMarkdown,
		now:             now,
	})
	return f.result, f.err
}

func (f *fakeClassifier) lastCall(t *testing.T) classifyCall {
	t.Helper()
	if len(f.calls) == 0 {
		t.Fatalf("classifier was not called")
	}
	return f.calls[len(f.calls)-1]
}

type fakeRegistry struct {
	calls int
	err   error
}

func (f *fakeRegistry) SyncJSONRegistry() error {
	f.calls++
	return f.err
}

type fakeDecisions struct {
	records []manualdecisions.Record
}

func (f *fakeDecisions) RecordManualDecision(record manualdecisions.Record) {
	f.records = append(f.records, record)
}

// --- harness -------------------------------------------------------------------------------------

type harness struct {
	t          *testing.T
	deps       Deps
	db         *database.Store
	inputDir   string
	outputDir  string
	cats       *fakeCategories
	extractor  *fakeExtractor
	classifier *fakeClassifier
	registry   *fakeRegistry
	decisions  *fakeDecisions
}

func newHarness(t *testing.T) *harness {
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
	db, err := database.Open(filepath.Join(root, "pdf_triage.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	h := &harness{
		t:          t,
		db:         db,
		inputDir:   inputDir,
		outputDir:  outputDir,
		cats:       &fakeCategories{config: documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{}}},
		extractor:  &fakeExtractor{result: pdfextractor.ExtractedPDF{Checksum: "unused", RawText: "", Numpages: 1}},
		classifier: &fakeClassifier{},
		registry:   &fakeRegistry{},
		decisions:  &fakeDecisions{},
	}
	h.deps = Deps{
		Config: settings.Config{
			InputDir:            inputDir,
			OutputRootDir:       outputDir,
			DBPath:              filepath.Join(root, "pdf_triage.db"),
			JSONRegistryPath:    filepath.Join(root, "registry.json"),
			ManualDecisionsFile: filepath.Join(root, "manual_decisions.json"),
		},
		DB:         db,
		Categories: h.cats,
		Extractor:  h.extractor,
		Classifier: h.classifier,
		Registry:   h.registry,
		Decisions:  h.decisions,
	}
	return h
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to not exist, stat err = %v", path, err)
	}
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

// sampleDoc mirrors the TS sampleDoc(): same defaults, with an original_path that cannot exist.
func (h *harness) sampleDoc(overrides ...func(*database.NewDocument)) database.NewDocument {
	doc := database.NewDocument{
		Checksum:         "chk-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Title:            "Facture SFR",
		Registre:         "",
		Date:             "2026-01-15",
		Category:         "invoices",
		Subcategory:      "sfr",
		Summary:          "",
		RawText:          "contenu original",
		OriginalFilename: "facture.pdf",
		OriginalPath:     filepath.Join(h.inputDir, "never_used.pdf"),
		Status:           "MOVED",
	}
	for _, apply := range overrides {
		apply(&doc)
	}
	return doc
}

func (h *harness) insertDoc(overrides ...func(*database.NewDocument)) int64 {
	h.t.Helper()
	id, err := h.db.InsertDocumentRecord(h.sampleDoc(overrides...))
	if err != nil {
		h.t.Fatalf("InsertDocumentRecord: %v", err)
	}
	return id
}

func strPtr(s string) *string { return &s }

// --- relocalizeFileIfNeeded (4 ported cases) -----------------------------------------------------

func TestRelocalizeFileIfNeeded_MovesToCanonicalPath(t *testing.T) {
	h := newHarness(t)
	sourceFile := filepath.Join(h.inputDir, "loose", "facture.pdf")
	writeFile(t, sourceFile, "dummy bytes")

	result, err := h.deps.RelocalizeFileIfNeeded(sourceFile, "invoices", strPtr("sfr"), strPtr("2026-01-15"), nil)
	if err != nil {
		t.Fatalf("RelocalizeFileIfNeeded: %v", err)
	}

	expectedTarget := filepath.Join(h.outputDir, "invoices", "sfr", "2026", "facture.pdf")
	if result != (RelocalizeResult{NewPath: expectedTarget, Moved: true}) {
		t.Fatalf("result = %+v, want newPath %q moved true", result, expectedTarget)
	}
	mustExist(t, expectedTarget)
	mustNotExist(t, sourceFile)
}

func TestRelocalizeFileIfNeeded_AlreadyCanonicalLeavesFileInPlace(t *testing.T) {
	h := newHarness(t)
	canonicalDir := filepath.Join(h.outputDir, "invoices", "sfr", "2026")
	file := filepath.Join(canonicalDir, "facture.pdf")
	writeFile(t, file, "dummy bytes")

	result, err := h.deps.RelocalizeFileIfNeeded(file, "invoices", strPtr("sfr"), strPtr("2026-01-15"), nil)
	if err != nil {
		t.Fatalf("RelocalizeFileIfNeeded: %v", err)
	}

	if result != (RelocalizeResult{NewPath: file, Moved: false}) {
		t.Fatalf("result = %+v, want newPath %q moved false", result, file)
	}
	mustExist(t, file)
}

func TestRelocalizeFileIfNeeded_CleansUpEmptySourceDirectoryTree(t *testing.T) {
	h := newHarness(t)
	catDir := filepath.Join(h.outputDir, "old_cat", "old_sub")
	file := filepath.Join(catDir, "doc.pdf")
	writeFile(t, file, "dummy bytes")

	if _, err := h.deps.RelocalizeFileIfNeeded(file, "new_cat", strPtr("new_sub"), strPtr("2026-01-15"), nil); err != nil {
		t.Fatalf("RelocalizeFileIfNeeded: %v", err)
	}

	mustNotExist(t, catDir)
	mustNotExist(t, filepath.Join(h.outputDir, "old_cat"))
}

func TestRelocalizeFileIfNeeded_NeverOverwritesExistingTarget(t *testing.T) {
	h := newHarness(t)
	targetDir := filepath.Join(h.outputDir, "invoices", "sfr", "2026")
	existingTarget := filepath.Join(targetDir, "facture.pdf")
	writeFile(t, existingTarget, "PRE-EXISTING CONTENT — must survive")

	sourceFile := filepath.Join(h.inputDir, "loose", "facture.pdf")
	writeFile(t, sourceFile, "incoming content")

	result, err := h.deps.RelocalizeFileIfNeeded(sourceFile, "invoices", strPtr("sfr"), strPtr("2026-01-15"), nil)
	if err != nil {
		t.Fatalf("RelocalizeFileIfNeeded: %v", err)
	}

	if !result.Moved {
		t.Fatalf("result.Moved = false, want true")
	}
	if result.NewPath == existingTarget {
		t.Fatalf("result.NewPath must not be the pre-existing target %q", existingTarget)
	}
	if got := readFile(t, existingTarget); got != "PRE-EXISTING CONTENT — must survive" {
		t.Fatalf("pre-existing target content = %q", got)
	}
	if got := readFile(t, result.NewPath); got != "incoming content" {
		t.Fatalf("moved file content = %q", got)
	}
}

// --- moveBackToRaws (3 ported cases) -------------------------------------------------------------

func TestMoveBackToRaws_MovesIntoInputDir(t *testing.T) {
	h := newHarness(t)
	archivedDir := filepath.Join(h.outputDir, "invoices", "sfr", "2026")
	file := filepath.Join(archivedDir, "facture.pdf")
	writeFile(t, file, "dummy")

	newPath, err := h.deps.MoveBackToRaws(file, nil)
	if err != nil {
		t.Fatalf("MoveBackToRaws: %v", err)
	}

	if want := filepath.Join(h.inputDir, "facture.pdf"); newPath != want {
		t.Fatalf("newPath = %q, want %q", newPath, want)
	}
	mustExist(t, newPath)
	mustNotExist(t, file)
}

func TestMoveBackToRaws_DeletesMatchingDBRecord(t *testing.T) {
	h := newHarness(t)
	archivedDir := filepath.Join(h.outputDir, "invoices", "sfr", "2026")
	file := filepath.Join(archivedDir, "facture.pdf")
	writeFile(t, file, "dummy")

	id := h.insertDoc(func(d *database.NewDocument) { d.Checksum = "to-delete" })

	if _, err := h.deps.MoveBackToRaws(file, strPtr("to-delete")); err != nil {
		t.Fatalf("MoveBackToRaws: %v", err)
	}

	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc != nil {
		t.Fatalf("document row %d still present, want deleted", id)
	}
}

func TestMoveBackToRaws_CleansUpEmptySourceDirectoryTree(t *testing.T) {
	h := newHarness(t)
	archivedDir := filepath.Join(h.outputDir, "invoices", "sfr")
	file := filepath.Join(archivedDir, "facture.pdf")
	writeFile(t, file, "dummy")

	if _, err := h.deps.MoveBackToRaws(file, nil); err != nil {
		t.Fatalf("MoveBackToRaws: %v", err)
	}

	mustNotExist(t, archivedDir)
	mustNotExist(t, filepath.Join(h.outputDir, "invoices"))
}

// --- reclassifyAndRelocalizeDocument (9 ported cases) --------------------------------------------

func TestReclassify_DocumentNotFound(t *testing.T) {
	h := newHarness(t)

	result, err := h.deps.ReclassifyAndRelocalizeDocument(9999, nil, nil, nil)
	if err != nil {
		t.Fatalf("ReclassifyAndRelocalizeDocument: %v", err)
	}
	if result != (ReclassifyResult{Success: false, Error: "Document not found"}) {
		t.Fatalf("result = %+v, want the exact 'Document not found' failure", result)
	}
}

func TestReclassify_RejectsForbiddenExplicitSubcategoryWithoutTouchingDB(t *testing.T) {
	h := newHarness(t)
	id := h.insertDoc()

	result, err := h.deps.ReclassifyAndRelocalizeDocument(id, strPtr("invoices"), strPtr("general"), nil)
	if err != nil {
		t.Fatalf("ReclassifyAndRelocalizeDocument: %v", err)
	}
	if result.Success {
		t.Fatalf("result.Success = true, want false")
	}
	if !strings.Contains(result.Error, "Golden Rule #4") {
		t.Fatalf("error = %q, want it to contain 'Golden Rule #4'", result.Error)
	}
	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc == nil {
		t.Fatalf("row %d must be untouched by the forbidden-subcategory reject", id)
	}
}

func TestReclassify_PurgesStaleGhostRecord(t *testing.T) {
	h := newHarness(t)
	id := h.insertDoc(func(d *database.NewDocument) {
		d.NewPath = filepath.Join(h.outputDir, "gone.pdf")
		d.OriginalPath = filepath.Join(h.inputDir, "gone.pdf")
	})

	result, err := h.deps.ReclassifyAndRelocalizeDocument(id, nil, nil, nil)
	if err != nil {
		t.Fatalf("ReclassifyAndRelocalizeDocument: %v", err)
	}
	if !result.StaleCleaned || result.Success {
		t.Fatalf("result = %+v, want staleCleaned true and success false", result)
	}
	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc != nil {
		t.Fatalf("stale row %d still present, want purged", id)
	}
	if h.registry.calls != 1 {
		t.Fatalf("syncJSONRegistry calls = %d, want 1", h.registry.calls)
	}
}

func TestReclassify_ExplicitSelectionSkipsAIAndUpdatesDB(t *testing.T) {
	h := newHarness(t)
	archivedDir := filepath.Join(h.outputDir, "invoices", "sfr", "2026")
	file := filepath.Join(archivedDir, "facture.pdf")
	writeFile(t, file, "dummy")

	id := h.insertDoc(func(d *database.NewDocument) {
		d.NewPath = file
		d.Category = "invoices"
		d.Subcategory = "sfr"
	})

	result, err := h.deps.ReclassifyAndRelocalizeDocument(id, strPtr("telecom"), strPtr("orange"), nil)
	if err != nil {
		t.Fatalf("ReclassifyAndRelocalizeDocument: %v", err)
	}
	if !result.Success {
		t.Fatalf("result = %+v, want success", result)
	}
	if len(h.classifier.calls) != 0 {
		t.Fatalf("classifier calls = %d, want 0 (explicit selection skips AI)", len(h.classifier.calls))
	}
	if h.cats.saves == 0 {
		t.Fatalf("SaveCategoriesConfig was never called; ensureCategoryAndSubcategoryExist did not run")
	}

	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc.Category != "telecom" || doc.Subcategory != "orange" {
		t.Fatalf("doc = %s/%s, want telecom/orange", doc.Category, doc.Subcategory)
	}
	mustExist(t, filepath.Join(h.outputDir, "telecom", "orange", "2026", "facture.pdf"))
}

func TestReclassify_RerunsAIClassificationWithFeedbackReason(t *testing.T) {
	h := newHarness(t)
	archivedDir := filepath.Join(h.outputDir, "invoices", "sfr", "2026")
	file := filepath.Join(archivedDir, "facture.pdf")
	writeFile(t, file, "dummy")

	h.extractor.result = pdfextractor.ExtractedPDF{Checksum: "x", RawText: "Some genuinely re-extracted content here", Numpages: 1}
	h.classifier.result = documentschema.DocumentMetadata{
		Titre:           "Facture Orange Corrigee",
		Categorie:       "telecom",
		Subcategorie:    "orange",
		Date:            "2026-02-01",
		Summary:         "nouveau resume",
		MarkdownContent: "# Orange",
	}

	id := h.insertDoc(func(d *database.NewDocument) {
		d.NewPath = file
		d.Category = "invoices"
		d.Subcategory = "sfr"
	})

	const feedback = "wrong category, this is actually Orange telecom"
	result, err := h.deps.ReclassifyAndRelocalizeDocument(id, nil, nil, strPtr(feedback))
	if err != nil {
		t.Fatalf("ReclassifyAndRelocalizeDocument: %v", err)
	}

	call := h.classifier.lastCall(t)
	if call.rawText != "Some genuinely re-extracted content here" {
		t.Errorf("rawText = %q", call.rawText)
	}
	if call.filename != "facture.pdf" {
		t.Errorf("filename = %q", call.filename)
	}
	if call.previousError != feedback {
		t.Errorf("previousError = %q, want %q", call.previousError, feedback)
	}
	if call.doclingMarkdown != "" {
		t.Errorf("doclingMarkdown = %q, want empty (3-arg call shape)", call.doclingMarkdown)
	}
	if !call.now.IsZero() {
		t.Errorf("now = %v, want the zero time (TS default)", call.now)
	}

	if !result.Success {
		t.Fatalf("result = %+v, want success", result)
	}
	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc.Category != "telecom" || doc.Subcategory != "orange" || doc.Title != "Facture Orange Corrigee" {
		t.Fatalf("doc = %s/%s %q, want telecom/orange Facture Orange Corrigee", doc.Category, doc.Subcategory, doc.Title)
	}
}

func TestReclassify_KeepsStoredTextWhenReextractionDegraded(t *testing.T) {
	h := newHarness(t)
	archivedDir := filepath.Join(h.outputDir, "identity", "permis_conduire", "2026")
	file := filepath.Join(archivedDir, "permis.pdf")
	writeFile(t, file, "dummy")

	h.extractor.result = pdfextractor.ExtractedPDF{
		Checksum:    "x",
		RawText:     "3 > U NI NV me : = a LBD pa Se To",
		Numpages:    2,
		OCRDegraded: true,
	}
	h.classifier.result = documentschema.DocumentMetadata{Categorie: "identity", Subcategorie: "permis_conduire"}

	const stored = "PERMIS DE CONDUIRE 1. NOM 2. PRENOM 3. 01.01.1990"
	id := h.insertDoc(func(d *database.NewDocument) {
		d.NewPath = file
		d.OriginalFilename = "permis.pdf"
		d.Category = "identity"
		d.Subcategory = "permis_conduire"
		d.RawText = stored
	})

	if _, err := h.deps.ReclassifyAndRelocalizeDocument(id, nil, nil, nil); err != nil {
		t.Fatalf("ReclassifyAndRelocalizeDocument: %v", err)
	}

	call := h.classifier.lastCall(t)
	if call.rawText != stored {
		t.Errorf("rawText = %q, want the stored %q", call.rawText, stored)
	}
	if call.filename != "permis.pdf" {
		t.Errorf("filename = %q", call.filename)
	}
	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc.RawText != stored {
		t.Errorf("doc.RawText = %q, want the stored text preserved", doc.RawText)
	}
}

func TestReclassify_AcceptsDegradedReextractionWhenNoStoredText(t *testing.T) {
	h := newHarness(t)
	archivedDir := filepath.Join(h.outputDir, "invoices", "sfr", "2026")
	file := filepath.Join(archivedDir, "facture.pdf")
	writeFile(t, file, "dummy")

	h.extractor.result = pdfextractor.ExtractedPDF{
		Checksum:    "x",
		RawText:     "degraded but genuinely present text",
		Numpages:    1,
		OCRDegraded: true,
	}
	h.classifier.result = documentschema.DocumentMetadata{Categorie: "invoices", Subcategorie: "sfr"}

	id := h.insertDoc(func(d *database.NewDocument) {
		d.NewPath = file
		d.RawText = ""
	})

	if _, err := h.deps.ReclassifyAndRelocalizeDocument(id, nil, nil, nil); err != nil {
		t.Fatalf("ReclassifyAndRelocalizeDocument: %v", err)
	}

	call := h.classifier.lastCall(t)
	if call.rawText != "degraded but genuinely present text" {
		t.Errorf("rawText = %q, want the degraded fresh text", call.rawText)
	}
	if call.filename != "facture.pdf" {
		t.Errorf("filename = %q", call.filename)
	}
}

func TestReclassify_PersistsFreshRawTextWhenItWasAnalyzed(t *testing.T) {
	h := newHarness(t)
	archivedDir := filepath.Join(h.outputDir, "invoices", "sfr", "2026")
	file := filepath.Join(archivedDir, "facture.pdf")
	writeFile(t, file, "dummy")

	const fresh = "Some genuinely re-extracted content here"
	h.extractor.result = pdfextractor.ExtractedPDF{Checksum: "x", RawText: fresh, Numpages: 1, OCRDegraded: false}
	h.classifier.result = documentschema.DocumentMetadata{Categorie: "invoices", Subcategorie: "sfr"}

	id := h.insertDoc(func(d *database.NewDocument) {
		d.NewPath = file
		d.RawText = "contenu original"
	})

	if _, err := h.deps.ReclassifyAndRelocalizeDocument(id, nil, nil, nil); err != nil {
		t.Fatalf("ReclassifyAndRelocalizeDocument: %v", err)
	}

	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc.RawText != fresh {
		t.Errorf("doc.RawText = %q, want the fresh text %q", doc.RawText, fresh)
	}
}

func TestReclassify_FallsBackToStoredTextWhenReextractionTooShort(t *testing.T) {
	h := newHarness(t)
	archivedDir := filepath.Join(h.outputDir, "invoices", "sfr", "2026")
	file := filepath.Join(archivedDir, "facture.pdf")
	writeFile(t, file, "dummy")

	h.extractor.result = pdfextractor.ExtractedPDF{Checksum: "x", RawText: "", Numpages: 1}
	h.classifier.result = documentschema.DocumentMetadata{Categorie: "invoices", Subcategorie: "sfr"}

	const stored = "ORIGINAL STORED TEXT FROM FIRST SCAN"
	id := h.insertDoc(func(d *database.NewDocument) {
		d.NewPath = file
		d.RawText = stored
	})

	if _, err := h.deps.ReclassifyAndRelocalizeDocument(id, nil, nil, nil); err != nil {
		t.Fatalf("ReclassifyAndRelocalizeDocument: %v", err)
	}

	call := h.classifier.lastCall(t)
	if call.rawText != stored {
		t.Errorf("rawText = %q, want the stored fallback %q", call.rawText, stored)
	}
}

// --- deleteDocumentAndMoveToTrash (ADDED: no upstream case in the ported file) -------------------

func TestDeleteDocumentAndMoveToTrash_NotFound(t *testing.T) {
	h := newHarness(t)

	result, err := h.deps.DeleteDocumentAndMoveToTrash(9999)
	if err != nil {
		t.Fatalf("DeleteDocumentAndMoveToTrash: %v", err)
	}
	if result != (DeleteResult{Success: false, Error: "Document not found"}) {
		t.Fatalf("result = %+v, want the exact 'Document not found' failure", result)
	}
}

func TestDeleteDocumentAndMoveToTrash_MovesFilePreservingBytesAndDeletesRow(t *testing.T) {
	h := newHarness(t)
	file := filepath.Join(h.outputDir, "invoices", "sfr", "2026", "facture.pdf")
	writeFile(t, file, "precious bytes")

	id := h.insertDoc(func(d *database.NewDocument) { d.NewPath = file })

	result, err := h.deps.DeleteDocumentAndMoveToTrash(id)
	if err != nil {
		t.Fatalf("DeleteDocumentAndMoveToTrash: %v", err)
	}
	if !result.Success {
		t.Fatalf("result = %+v, want success", result)
	}

	// Golden Rule 15: the file is MOVED to __raws/.delete_files, never deleted.
	trashPath := filepath.Join(h.inputDir, ".delete_files", "facture.pdf")
	if got := readFile(t, trashPath); got != "precious bytes" {
		t.Fatalf("trashed content = %q, want the original bytes", got)
	}
	mustNotExist(t, file)

	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc != nil {
		t.Fatalf("document row %d still present, want deleted", id)
	}
	if h.registry.calls != 1 {
		t.Fatalf("syncJSONRegistry calls = %d, want 1", h.registry.calls)
	}
}

func TestDeleteDocumentAndMoveToTrash_MissingFileStillUnregisters(t *testing.T) {
	h := newHarness(t)
	id := h.insertDoc(func(d *database.NewDocument) {
		d.NewPath = filepath.Join(h.outputDir, "gone.pdf")
		d.OriginalPath = filepath.Join(h.inputDir, "gone.pdf")
	})

	result, err := h.deps.DeleteDocumentAndMoveToTrash(id)
	if err != nil {
		t.Fatalf("DeleteDocumentAndMoveToTrash: %v", err)
	}
	if !result.Success {
		t.Fatalf("result = %+v, want success (a missing file does not block un-registering)", result)
	}
	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc != nil {
		t.Fatalf("document row %d still present, want deleted", id)
	}
}
