package repair

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/app/scanlock"
	"github.com/phamhung075/pdf-triage-pdf2w/classification"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfextractor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// These cases are ported from pdf-triage's src/application/repair-registry.test.ts (9 cases; the
// upstream suite is green at port time: `npx vitest run src/application/repair-registry.test.ts`
// -> 9 passed). The TS suite redirects CONFIG to temp dirs with a whole-module settings mock,
// mocks every remote collaborator, and opens a temp DB; the Go equivalent is the harness below,
// built on Deps + *database.Store + t.TempDir(). No real project file (pdf_triage.db, __raws,
// __archive, registry.json) is touched.
//
// Cases ADDED, not ported: the event payload stream (parity-pinned JSON, including the HTTP-layer
// CompletedEvent builder), the scan-lock propagation test, the canonical-category correction, the
// settings reload/ensure call, and the lock acquire/release bookkeeping.

// --- fakes ---------------------------------------------------------------------------------------

type fakeSettings struct {
	cfg       settings.Config
	reloads   int
	ensures   int
	ensureErr error
}

func (f *fakeSettings) ReloadFromDisk() { f.reloads++ }

func (f *fakeSettings) EnsureDirectoriesExist() error {
	f.ensures++
	return f.ensureErr
}

func (f *fakeSettings) Config() settings.Config { return f.cfg }

type fakeCategories struct {
	config documentschema.CategoriesConfig
	saves  int
}

func (f *fakeCategories) GetCategoriesConfig() documentschema.CategoriesConfig { return f.config }

func (f *fakeCategories) SaveCategoriesConfig(cats []*documentschema.CategoryItem) error {
	f.saves++
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

func (f *fakeExtractor) set(result pdfextractor.ExtractedPDF, err error) {
	f.result = result
	f.err = err
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

type fakeRegistry struct {
	calls int
	err   error
}

func (f *fakeRegistry) SyncJSONRegistry() error {
	f.calls++
	return f.err
}

type fakeEntityDictionary struct{}

func (fakeEntityDictionary) GetEntityDictionary() documentschema.EntityDictionary {
	return documentschema.EntityDictionary{}
}

type fakePromptPersonalization struct{}

func (fakePromptPersonalization) GetPromptPersonalization() promptpersonalization.PromptPersonalization {
	return promptpersonalization.EMPTY_PROMPT_PERSONALIZATION
}

type fakeLock struct {
	mu       sync.Mutex
	acquired int
	released int
	err      error
}

func (f *fakeLock) Acquire() (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	f.acquired++
	return func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.released++
	}, nil
}

// --- harness -------------------------------------------------------------------------------------

type ruleCall struct {
	rawText  string
	filename string
	denylist []string
}

type harness struct {
	t *testing.T

	deps Deps
	db   *database.Store

	inputDir  string
	outputDir string
	dataDir   string

	cats       *fakeCategories
	extractor  *fakeExtractor
	classifier *fakeClassifier
	registry   *fakeRegistry
	settings   *fakeSettings
	lock       *fakeLock

	ruleResult classification.RuleBasedClassifyResult
	ruleCalls  []ruleCall

	contactResult classification.RuleBasedContact
	contactCalls  []string

	embeddingResult []float64
	embeddingCalls  []string

	events []any
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	inputDir := filepath.Join(root, "__raws")
	outputDir := filepath.Join(root, "__archive")
	dataDir := filepath.Join(root, "base")
	for _, dir := range []string{inputDir, outputDir, dataDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	db, err := database.Open(filepath.Join(root, "pdf_triage.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := settings.Config{
		InputDir:            inputDir,
		OutputRootDir:       outputDir,
		DBPath:              filepath.Join(root, "pdf_triage.db"),
		JSONRegistryPath:    filepath.Join(root, "registry.json"),
		ManualDecisionsFile: filepath.Join(root, "manual_decisions.json"),
		OllamaModel:         "qwen3.5:9b",
	}

	h := &harness{
		t:          t,
		db:         db,
		inputDir:   inputDir,
		outputDir:  outputDir,
		dataDir:    dataDir,
		cats:       &fakeCategories{config: documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{}}},
		extractor:  &fakeExtractor{result: pdfextractor.ExtractedPDF{Checksum: "unused", RawText: "", Numpages: 1}},
		classifier: &fakeClassifier{},
		registry:   &fakeRegistry{},
		settings:   &fakeSettings{cfg: cfg},
		lock:       &fakeLock{},
	}
	h.deps = Deps{
		Config:                cfg,
		Settings:              h.settings,
		DB:                    db,
		Categories:            h.cats,
		Extractor:             h.extractor,
		Relocalizer:           relocalize.Deps{Config: cfg, DB: db, Categories: h.cats, Extractor: h.extractor, Classifier: h.classifier, Registry: h.registry},
		Classifier:            h.classifier,
		EntityDictionary:      fakeEntityDictionary{},
		PromptPersonalization: fakePromptPersonalization{},
		Registry:              h.registry,
		Lock:                  h.lock,
	}
	h.deps.RuleBasedClassify = func(rawText, filename string, dictionary documentschema.EntityDictionary, denylist []string, personalization ...promptpersonalization.PromptPersonalization) classification.RuleBasedClassifyResult {
		h.ruleCalls = append(h.ruleCalls, ruleCall{rawText: rawText, filename: filename, denylist: denylist})
		return h.ruleResult
	}
	h.deps.ExtractRuleBasedContact = func(rawText string) classification.RuleBasedContact {
		h.contactCalls = append(h.contactCalls, rawText)
		return h.contactResult
	}
	h.deps.GenerateEmbedding = func(text string) []float64 {
		h.embeddingCalls = append(h.embeddingCalls, text)
		return h.embeddingResult
	}
	return h
}

// onProgress records every event the use case emits.
func (h *harness) onProgress(event any) { h.events = append(h.events, event) }

func (h *harness) writeArchivedFile(relDir, filename string) string {
	h.t.Helper()
	dir := filepath.Join(h.outputDir, relDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir %s: %v", dir, err)
	}
	file := filepath.Join(dir, filename)
	if err := os.WriteFile(file, []byte("dummy pdf bytes"), 0o644); err != nil {
		h.t.Fatalf("write %s: %v", file, err)
	}
	return file
}

// sampleDoc mirrors the TS sampleDoc(): same defaults, with an original_path that cannot exist.
func (h *harness) sampleDoc(overrides ...func(*database.NewDocument)) database.NewDocument {
	doc := database.NewDocument{
		Checksum:         "chk-" + time.Now().Format("150405.000000000"),
		Title:            "Facture SFR",
		Registre:         "",
		Date:             "2026-01-15",
		Category:         "invoices",
		Subcategory:      "sfr",
		Summary:          "",
		Tags:             []string{},
		RawText:          "contenu original suffisamment long pour ne pas etre considere vide",
		OriginalFilename: "facture.pdf",
		OriginalPath:     "C:/never/used.pdf",
		Status:           "MOVED",
	}
	for _, apply := range overrides {
		apply(&doc)
	}
	return doc
}

func (h *harness) insert(doc database.NewDocument) int64 {
	h.t.Helper()
	id, err := h.db.InsertDocumentRecord(doc)
	if err != nil {
		h.t.Fatalf("InsertDocumentRecord: %v", err)
	}
	return id
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected %s to not exist, stat err = %v", path, err)
	}
}

// eventJSON is the SSE frame's payload encoder: compact JSON with HTML escaping disabled, which is
// what the httpapi Hub does (TS JSON.stringify writes `<`/`&` literally too).
func eventJSON(t *testing.T, v any) string {
	t.Helper()
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatalf("encode event: %v", err)
	}
	return strings.TrimRight(b.String(), "\n")
}

// --- ported cases: ghost purge & year-string normalization --------------------------------------

func TestRepairRegistryPurgesGhostRecordMissingOnDisk(t *testing.T) {
	h := newHarness(t)
	id := h.insert(h.sampleDoc(func(d *database.NewDocument) {
		d.Checksum = "ghost-checksum"
		d.NewPath = filepath.Join(h.outputDir, "invoices", "sfr", "2026", "gone.pdf")
		d.OriginalPath = "C:/gone/gone.pdf"
		d.OriginalFilename = "gone.pdf"
	}))

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	got, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if got != nil {
		t.Fatalf("ghost record still present: %+v", got)
	}
	if result.ScannedCount != 0 {
		t.Fatalf("ScannedCount = %d, want 0", result.ScannedCount)
	}
}

func TestRepairRegistryNormalizesYearStringSubcategory(t *testing.T) {
	h := newHarness(t)
	file := h.writeArchivedFile(filepath.Join("invoices", "2024"), "facture.pdf")
	id := h.insert(h.sampleDoc(func(d *database.NewDocument) {
		d.Checksum = "yearstring-doc"
		d.Category = "invoices"
		d.Subcategory = "2024"
		d.NewPath = file
	}))
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "yearstring-doc", RawText: "contenu suffisant", Numpages: 1}, nil)
	h.ruleResult = classification.RuleBasedClassifyResult{Categorie: "telecom", Subcategorie: "orange", Title: "t"}

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc.Category != "telecom" || doc.Subcategory != "orange" {
		t.Fatalf("doc = %s/%s, want telecom/orange", doc.Category, doc.Subcategory)
	}
	if result.UpdatedCount < 1 {
		t.Fatalf("UpdatedCount = %d, want >= 1", result.UpdatedCount)
	}
	if len(h.ruleCalls) != 1 {
		t.Fatalf("ruleBasedClassify calls = %d, want 1", len(h.ruleCalls))
	}
}

// --- ported cases: existing indexed documents ----------------------------------------------------

func TestRepairRegistryUpdatesStaleRawText(t *testing.T) {
	h := newHarness(t)
	file := h.writeArchivedFile(filepath.Join("invoices", "sfr", "2026"), "facture.pdf")
	id := h.insert(h.sampleDoc(func(d *database.NewDocument) {
		d.Checksum = "stale-text"
		d.NewPath = file
		d.RawText = ""
	}))
	const fresh = "Freshly re-extracted content, plenty of characters here"
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "stale-text", RawText: fresh, Numpages: 1}, nil)

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc.RawText != fresh {
		t.Fatalf("RawText = %q, want %q", doc.RawText, fresh)
	}
	if result.UpdatedCount < 1 {
		t.Fatalf("UpdatedCount = %d, want >= 1", result.UpdatedCount)
	}
	if len(h.ruleCalls) != 0 {
		t.Fatalf("ruleBasedClassify must not run for an already-specific document (calls=%d)", len(h.ruleCalls))
	}
}

func TestRepairRegistryReclassifiesGenericDocument(t *testing.T) {
	h := newHarness(t)
	file := h.writeArchivedFile(filepath.Join("personal", "general", "2026"), "doc.pdf")
	id := h.insert(h.sampleDoc(func(d *database.NewDocument) {
		d.Checksum = "generic-doc"
		d.Category = "personal"
		d.Subcategory = "general"
		d.NewPath = file
	}))
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "generic-doc", RawText: "contenu suffisant", Numpages: 1}, nil)
	h.ruleResult = classification.RuleBasedClassifyResult{Categorie: "health", Subcategorie: "doctor_x", Title: "t"}

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc.Category != "health" || doc.Subcategory != "doctor_x" {
		t.Fatalf("doc = %s/%s, want health/doctor_x", doc.Category, doc.Subcategory)
	}
	if result.RelocalizedCount < 1 {
		t.Fatalf("RelocalizedCount = %d, want >= 1", result.RelocalizedCount)
	}
	mustExist(t, filepath.Join(h.outputDir, "health", "doctor_x"))
}

func TestRepairRegistryMovesBackWhenRuleReclassificationIsGeneric(t *testing.T) {
	h := newHarness(t)
	file := h.writeArchivedFile(filepath.Join("personal", "general", "2026"), "doc.pdf")
	h.insert(h.sampleDoc(func(d *database.NewDocument) {
		d.Checksum = "still-generic"
		d.Category = "personal"
		d.Subcategory = "general"
		d.NewPath = file
	}))
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "still-generic", RawText: "contenu suffisant", Numpages: 1}, nil)
	h.ruleResult = classification.RuleBasedClassifyResult{Categorie: "other", Subcategorie: "general", Title: "t"}

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	if result.MovedToRawsCount < 1 {
		t.Fatalf("MovedToRawsCount = %d, want >= 1", result.MovedToRawsCount)
	}
	mustNotExist(t, file)
	mustExist(t, filepath.Join(h.inputDir, "doc.pdf"))
}

// --- ported cases: unindexed archived files ------------------------------------------------------

func TestRepairRegistryInsertsUnindexedFile(t *testing.T) {
	h := newHarness(t)
	h.writeArchivedFile(filepath.Join("some", "legacy", "path"), "unindexed.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "brand-new-checksum", RawText: "contenu suffisant pour classification", Numpages: 1}, nil)
	h.classifier.result = documentschema.DocumentMetadata{
		Titre: "Nouvelle Facture", Registre: "REF-9", Categorie: "utilities", Subcategorie: "edf",
		Date: "2026-03-01", Summary: "resume", Tags: []string{"edf"}, MarkdownContent: "# EDF",
	}

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	if result.RepairedCount != 1 {
		t.Fatalf("RepairedCount = %d, want 1", result.RepairedCount)
	}
	doc, err := h.db.GetDocumentByChecksum("brand-new-checksum")
	if err != nil {
		t.Fatalf("GetDocumentByChecksum: %v", err)
	}
	if doc == nil {
		t.Fatalf("inserted record not found")
	}
	if doc.Category != "utilities" || doc.Subcategory != "edf" || doc.Title != "Nouvelle Facture" {
		t.Fatalf("doc = %s/%s %q, want utilities/edf 'Nouvelle Facture'", doc.Category, doc.Subcategory, doc.Title)
	}
	if len(h.embeddingCalls) != 1 {
		t.Fatalf("generateEmbedding calls = %d, want 1", len(h.embeddingCalls))
	}
	mustExist(t, filepath.Join(h.outputDir, "utilities", "edf", "2026"))
}

func TestRepairRegistryMovesUnindexedGenericFileBackToRaws(t *testing.T) {
	h := newHarness(t)
	file := h.writeArchivedFile(filepath.Join("some", "legacy", "path"), "unindexed.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "generic-new-checksum", RawText: "contenu suffisant", Numpages: 1}, nil)
	h.classifier.result = documentschema.DocumentMetadata{
		Titre: "t", Categorie: "other", Subcategorie: "general",
	}

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	if result.MovedToRawsCount != 1 {
		t.Fatalf("MovedToRawsCount = %d, want 1", result.MovedToRawsCount)
	}
	if result.RepairedCount != 0 {
		t.Fatalf("RepairedCount = %d, want 0", result.RepairedCount)
	}
	mustNotExist(t, file)
	mustExist(t, filepath.Join(h.inputDir, "unindexed.pdf"))
}

// --- ported cases: missing/empty content guard ---------------------------------------------------

func TestRepairRegistryMovesEmptyContentBackWithoutClassifying(t *testing.T) {
	h := newHarness(t)
	h.writeArchivedFile(filepath.Join("invoices", "sfr", "2026"), "blank.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "blank-checksum", RawText: "", Numpages: 1}, nil)

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	if result.MovedToRawsCount != 1 {
		t.Fatalf("MovedToRawsCount = %d, want 1", result.MovedToRawsCount)
	}
	if len(h.classifier.calls) != 0 {
		t.Fatalf("classifyPDFText must not run for empty content (calls=%d)", len(h.classifier.calls))
	}
	if len(h.ruleCalls) != 0 {
		t.Fatalf("ruleBasedClassify must not run for empty content (calls=%d)", len(h.ruleCalls))
	}
	mustExist(t, filepath.Join(h.inputDir, "blank.pdf"))
}

// --- ported cases: summary counts ----------------------------------------------------------------

func TestRepairRegistryScannedCountEqualsArchivedFileCount(t *testing.T) {
	h := newHarness(t)
	h.writeArchivedFile("a", "one.pdf")
	h.writeArchivedFile("b", "two.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "c1", RawText: "", Numpages: 1}, nil)

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}
	if result.ScannedCount != 2 {
		t.Fatalf("ScannedCount = %d, want 2", result.ScannedCount)
	}
}

// --- added cases ---------------------------------------------------------------------------------

// The canonical-category correction is repair-registry.ts:123-136: a non-generic subcategory whose
// canonical owner is a different category moves the document (and its file) there. The TS suite
// does not cover it; the taxonomy package's own suite covers the ambiguity rule.
func TestRepairRegistryRelocalizesToCanonicalCategory(t *testing.T) {
	h := newHarness(t)
	h.cats.config = documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
		{ID: "telecom", Subcategories: []*documentschema.SubcategoryItem{{ID: "sfr"}}},
	}}
	file := h.writeArchivedFile(filepath.Join("other", "sfr", "2026"), "facture.pdf")
	id := h.insert(h.sampleDoc(func(d *database.NewDocument) {
		d.Checksum = "canonical-doc"
		d.Category = "other"
		d.Subcategory = "sfr"
		d.NewPath = file
	}))
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "canonical-doc", RawText: "contenu suffisant", Numpages: 1}, nil)

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc.Category != "telecom" {
		t.Fatalf("Category = %q, want telecom", doc.Category)
	}
	if result.RelocalizedCount < 1 {
		t.Fatalf("RelocalizedCount = %d, want >= 1", result.RelocalizedCount)
	}
	mustExist(t, filepath.Join(h.outputDir, "telecom", "sfr", "2026"))
}

// The in-band event stream, pinned to the exact TS payloads (repair-registry.ts:53 and :71). The
// blank-content path emits REPAIR_STARTED then FILE_PROGRESS; REPAIR_COMPLETED is the HTTP layer's
// broadcast (covered by TestCompletedEventMatchesTSPayload).
func TestRepairRegistryEmitsExactEventPayloads(t *testing.T) {
	h := newHarness(t)
	h.writeArchivedFile(filepath.Join("invoices", "sfr", "2026"), "blank.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "blank-checksum", RawText: "", Numpages: 1}, nil)

	result, err := h.deps.RepairRegistry(h.onProgress)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	if len(h.events) != 2 {
		t.Fatalf("events = %d (%#v), want 2", len(h.events), h.events)
	}
	want := []struct {
		name string
		got  any
		json string
	}{
		{
			name: "REPAIR_STARTED",
			got:  h.events[0],
			json: `{"type":"REPAIR_STARTED","totalFiles":1,"message":"Starting repair & relocalization of 1 archived PDF file(s)..."}`,
		},
		{
			name: "FILE_PROGRESS",
			got:  h.events[1],
			json: `{"type":"FILE_PROGRESS","filename":"blank.pdf","scannedCount":1,"processedCount":1,"stage":"REPAIRING","message":"Analyzing & repairing file 1/1: blank.pdf"}`,
		},
	}
	for _, tc := range want {
		if got := eventJSON(t, tc.got); got != tc.json {
			t.Fatalf("%s payload = %s, want %s", tc.name, got, tc.json)
		}
	}

	if got, ok := h.events[0].(RepairStartedEvent); !ok {
		t.Fatalf("event[0] = %T, want RepairStartedEvent", h.events[0])
	} else if got.TotalFiles != 1 {
		t.Fatalf("REPAIR_STARTED.TotalFiles = %d, want 1", got.TotalFiles)
	}
	if got, ok := h.events[1].(FileProgressEvent); !ok {
		t.Fatalf("event[1] = %T, want FileProgressEvent", h.events[1])
	} else if got.Stage != "REPAIRING" {
		t.Fatalf("FILE_PROGRESS.Stage = %q, want REPAIRING", got.Stage)
	}
	// The result the HTTP layer spreads into REPAIR_COMPLETED.
	if result.ScannedCount != 1 || result.MovedToRawsCount != 1 {
		t.Fatalf("result = %+v", result)
	}
}

// Ollama-down is the one per-file failure that becomes FILE_FAILED instead of a bare warning
// (repair-registry.ts:242-251). RepairRegistry is the emitter here; the payload is exact.
func TestRepairRegistryEmitsFileFailedWhenOllamaIsDown(t *testing.T) {
	h := newHarness(t)
	h.writeArchivedFile(filepath.Join("invoices", "sfr", "2026"), "down.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{}, &ollama.OllamaUnavailableError{Message: "connection refused"})

	result, err := h.deps.RepairRegistry(h.onProgress)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	if len(h.events) != 3 {
		t.Fatalf("events = %d (%#v), want 3", len(h.events), h.events)
	}
	failed, ok := h.events[2].(FileFailedEvent)
	if !ok {
		t.Fatalf("event[2] = %T, want FileFailedEvent", h.events[2])
	}
	const wantMessage = "⛔ Ollama is down — qwen3.5:9b unreachable. Start Ollama, then re-run Repair. Skipped: down.pdf"
	if failed.Filename != "down.pdf" || failed.Stage != "FAILED" || failed.Message != wantMessage {
		t.Fatalf("FILE_FAILED = %+v", failed)
	}
	wantJSON := `{"type":"FILE_FAILED","filename":"down.pdf","stage":"FAILED","message":"` + wantMessage + `"}`
	if got := eventJSON(t, failed); got != wantJSON {
		t.Fatalf("FILE_FAILED payload = %s, want %s", got, wantJSON)
	}
	// A skipped file is neither repaired nor moved; the completed event still reports the scan.
	if result.ScannedCount != 1 || result.RepairedCount != 0 || result.MovedToRawsCount != 0 {
		t.Fatalf("result = %+v", result)
	}
}

// CompletedEvent is the exported builder the HTTP adapter calls after RepairRegistry returns and
// finishTask ran (web-server.ts:272); its payload is byte-identical to the TS `{ type, ...result }`.
func TestCompletedEventMatchesTSPayload(t *testing.T) {
	result := Result{ScannedCount: 3, RepairedCount: 1, UpdatedCount: 2, RelocalizedCount: 1, MovedToRawsCount: 1}
	want := `{"type":"REPAIR_COMPLETED","scannedCount":3,"repairedCount":1,"updatedCount":2,"relocalizedCount":1,"movedToRawsCount":1}`
	if got := eventJSON(t, CompletedEvent(result)); got != want {
		t.Fatalf("CompletedEvent = %s, want %s", got, want)
	}
}

// The scan lock is the whole-run guard against a concurrent scan/repair/clear (app/scanlock). A
// second acquisition inside the same process is an in-progress error and must propagate unchanged,
// with nothing mutated.
func TestRepairRegistryPropagatesScanInProgress(t *testing.T) {
	h := newHarness(t)
	guard := scanlock.New(h.dataDir)
	h.deps.Lock = guard

	release, err := guard.Acquire()
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	defer release()

	h.writeArchivedFile(filepath.Join("invoices", "sfr", "2026"), "blocked.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "blocked", RawText: "", Numpages: 1}, nil)

	_, err = h.deps.RepairRegistry(nil)
	var inProgress *scanlock.ScanInProgressError
	if !errors.As(err, &inProgress) {
		t.Fatalf("err = %T %v, want *scanlock.ScanInProgressError", err, err)
	}
	// The file the run would have moved is untouched.
	mustExist(t, filepath.Join(h.outputDir, "invoices", "sfr", "2026", "blocked.pdf"))
	if len(h.events) != 0 {
		t.Fatalf("events emitted despite the lock error: %#v", h.events)
	}
}

// A successful run acquires and releases the injected lock exactly once, and refreshes the config
// first (reloadConfigFromDisk + ensureDirectoriesExist).
func TestRepairRegistryAcquiresLockAndRefreshesConfig(t *testing.T) {
	h := newHarness(t)
	h.writeArchivedFile(filepath.Join("invoices", "sfr", "2026"), "blank.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "blank-checksum", RawText: "", Numpages: 1}, nil)

	if _, err := h.deps.RepairRegistry(nil); err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}

	if h.lock.acquired != 1 || h.lock.released != 1 {
		t.Fatalf("lock acquired/released = %d/%d, want 1/1", h.lock.acquired, h.lock.released)
	}
	if h.settings.reloads != 1 || h.settings.ensures != 1 {
		t.Fatalf("settings reload/ensure = %d/%d, want 1/1", h.settings.reloads, h.settings.ensures)
	}
}

// The JSON mirror is synced once, after the file loop, even when every file was moved back.
func TestRepairRegistrySyncsJSONRegistry(t *testing.T) {
	h := newHarness(t)
	h.writeArchivedFile("a", "one.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "c1", RawText: "", Numpages: 1}, nil)

	if _, err := h.deps.RepairRegistry(nil); err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}
	if h.registry.calls != 1 {
		t.Fatalf("SyncJSONRegistry calls = %d, want 1", h.registry.calls)
	}
}

// The content guard uses JS trim semantics: a whitespace-only extraction is missing content and is
// moved back without classification (repair-registry.ts:85).
func TestRepairRegistryTreatsWhitespaceOnlyTextAsMissing(t *testing.T) {
	h := newHarness(t)
	h.writeArchivedFile(filepath.Join("invoices", "sfr", "2026"), "ws.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "ws-checksum", RawText: " \t\n\u00a0 ", Numpages: 1}, nil)

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}
	if result.MovedToRawsCount != 1 {
		t.Fatalf("MovedToRawsCount = %d, want 1", result.MovedToRawsCount)
	}
	if len(h.classifier.calls) != 0 {
		t.Fatalf("classifyPDFText must not run for whitespace-only content (calls=%d)", len(h.classifier.calls))
	}
}

// The unindexed branch passes pdf2w's deterministic Markdown to classifyPDFText when present
// (repair-registry.ts:83/:172-174) and omits it otherwise.
func TestRepairRegistryPassesDoclingMarkdownWhenPresent(t *testing.T) {
	h := newHarness(t)
	h.writeArchivedFile(filepath.Join("some", "legacy", "path"), "with-md.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{
		Checksum:      "with-md",
		RawText:       "contenu suffisant",
		Numpages:      1,
		Pdf2wMarkdown: "# Markdown",
	}, nil)
	h.classifier.result = documentschema.DocumentMetadata{Categorie: "utilities", Subcategorie: "edf", Titre: "t"}

	if _, err := h.deps.RepairRegistry(nil); err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}
	if len(h.classifier.calls) != 1 {
		t.Fatalf("classifyPDFText calls = %d, want 1", len(h.classifier.calls))
	}
	if got := h.classifier.calls[0].doclingMarkdown; got != "# Markdown" {
		t.Fatalf("doclingMarkdown = %q, want # Markdown", got)
	}
}

// A generic target is checked before the insert, so a rule that resolves to other/general never
// creates a row (Golden Rule 4) even for an unindexed file.
func TestRepairRegistryNeverInsertsForbiddenSubcategory(t *testing.T) {
	h := newHarness(t)
	h.writeArchivedFile(filepath.Join("some", "legacy", "path"), "forbidden.pdf")
	h.extractor.set(pdfextractor.ExtractedPDF{Checksum: "forbidden-checksum", RawText: "contenu suffisant", Numpages: 1}, nil)
	h.classifier.result = documentschema.DocumentMetadata{Titre: "t", Categorie: "other", Subcategorie: "divers"}

	result, err := h.deps.RepairRegistry(nil)
	if err != nil {
		t.Fatalf("RepairRegistry: %v", err)
	}
	if result.RepairedCount != 0 || result.MovedToRawsCount != 1 {
		t.Fatalf("repaired/movedToRaws = %d/%d, want 0/1", result.RepairedCount, result.MovedToRawsCount)
	}
	doc, err := h.db.GetDocumentByChecksum("forbidden-checksum")
	if err != nil {
		t.Fatalf("GetDocumentByChecksum: %v", err)
	}
	if doc != nil {
		t.Fatalf("a forbidden-subcategory row was inserted: %+v", doc)
	}
}
