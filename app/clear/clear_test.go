package clear

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// These cases are ported from pdf-triage's src/application/clear-registry.test.ts (4 cases).
//
// Three cases are ported here: the tracked-document move + DB purge + registry sync (:90), the
// outside-OUTPUT_ROOT_DIR file that is left untouched while its row is still purged (:108), and the
// orphan-file move + empty-skeleton prune (:124).
//
// The fourth upstream case (:140, "propagates ScanInProgressError when the scan lock is held by
// another process") is deliberately NOT ported: this port does not acquire the scan lock — the
// caller does, as the package comment and the migration design's lock-ordering decision require.
// The lock-error contract is pinned by app/scanlock's own suite, and the propagation assertion
// belongs to the caller/composition-root suite that owns acquire/release.
//
// The TS suite redirected CONFIG through a whole-module settings mock and opened a temp DB. The Go
// equivalent is the h.cfg + *database.Store pair built by newHarness against a t.TempDir(); no real
// project file (pdf_triage.db, __raws, __archive, registry.json) is ever touched.
//
// Added cases (not upstream) pin the event payloads byte-for-byte, the nil-checksum move-back call
// the TS makes, the warn-and-continue behavior for a failed move, error propagation from PurgeAll
// and the registry sync, the two ensureDirectoriesExist call points, the required-Deps guard, and
// the production app/relocalize registry adapter.

// --- fakes ---------------------------------------------------------------------------------------

type fakeRegistry struct {
	calls int
	err   error
}

func (f *fakeRegistry) SyncJSONRegistry() error {
	f.calls++
	return f.err
}

// spyMover records every move-back call and delegates to the real app/relocalize helper.
type moverCall struct {
	filePath string
	checksum *string
}

type spyMover struct {
	real  FileMover
	calls []moverCall
}

func (s *spyMover) MoveBackToRaws(filePath string, checksum *string) (string, error) {
	s.calls = append(s.calls, moverCall{filePath: filePath, checksum: checksum})
	return s.real.MoveBackToRaws(filePath, checksum)
}

// failingMover fails every move, so the warn-and-continue paths can be exercised deterministically.
type failingMover struct {
	err   error
	calls int
}

func (f *failingMover) MoveBackToRaws(filePath string, checksum *string) (string, error) {
	f.calls++
	return "", f.err
}

// purgeFailingStore keeps the real reads and overrides only the wholesale purge.
type purgeFailingStore struct {
	*database.Store
	err error
}

func (p purgeFailingStore) PurgeAll() error { return p.err }

// --- harness -------------------------------------------------------------------------------------

type harness struct {
	t         *testing.T
	root      string
	inputDir  string
	outputDir string
	db        *database.Store
	cfg       settings.Config
	deps      Deps
	registry  *fakeRegistry
	events    []Event
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	inputDir := filepath.Join(root, "__raws")
	outputDir := filepath.Join(root, "__archive")
	dbPath := filepath.Join(root, "pdf_triage.db")
	// The folders are intentionally NOT pre-created: the function's ensureDirectories call must
	// create them, exactly as the TS settings mock did.
	db, err := database.Open(dbPath)
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	cfg := settings.Config{
		InputDir:         inputDir,
		OutputRootDir:    outputDir,
		DBPath:           dbPath,
		JSONRegistryPath: filepath.Join(root, "registry.json"),
	}

	h := &harness{
		t:         t,
		root:      root,
		inputDir:  inputDir,
		outputDir: outputDir,
		db:        db,
		cfg:       cfg,
		registry:  &fakeRegistry{},
	}
	h.deps = Deps{
		Config:   cfg,
		DB:       db,
		Mover:    NewRelocalizeMover(cfg, db),
		Registry: h.registry,
	}
	return h
}

func (h *harness) progress(ev Event) { h.events = append(h.events, ev) }

func (h *harness) run() (ClearResult, error) {
	h.t.Helper()
	return h.deps.ClearRegistryAndMoveArchiveToRaws(h.progress)
}

func (h *harness) sampleDoc(apply ...func(*database.NewDocument)) database.NewDocument {
	doc := database.NewDocument{
		Checksum:         "chk-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		Title:            "Facture SFR",
		Date:             "2026-01-15",
		Category:         "invoices",
		Subcategory:      "sfr",
		RawText:          "contenu",
		OriginalFilename: "facture.pdf",
		OriginalPath:     filepath.Join(h.root, "never_used.pdf"),
		Status:           "MOVED",
	}
	for _, f := range apply {
		f(&doc)
	}
	return doc
}

func (h *harness) insert(apply ...func(*database.NewDocument)) int64 {
	h.t.Helper()
	id, err := h.db.InsertDocumentRecord(h.sampleDoc(apply...))
	if err != nil {
		h.t.Fatalf("InsertDocumentRecord: %v", err)
	}
	return id
}

// --- filesystem helpers --------------------------------------------------------------------------

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
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

func derefString(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func derefInt(p *int) int {
	if p == nil {
		return -1
	}
	return *p
}

// --- ported cases --------------------------------------------------------------------------------

func TestClear_MovesTrackedDocumentPurgesDBAndSyncsRegistry(t *testing.T) {
	h := newHarness(t)

	archivedFile := filepath.Join(h.outputDir, "invoices", "sfr", "2026", "facture.pdf")
	writeFile(t, archivedFile, "dummy pdf bytes")
	id := h.insert(func(d *database.NewDocument) { d.NewPath = archivedFile })

	result, err := h.run()
	if err != nil {
		t.Fatalf("ClearRegistryAndMoveArchiveToRaws: %v", err)
	}

	if result.CountMoved != 1 {
		t.Fatalf("CountMoved = %d, want 1", result.CountMoved)
	}
	mustNotExist(t, archivedFile)
	mustExist(t, filepath.Join(h.inputDir, "facture.pdf"))

	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc != nil {
		t.Fatalf("document row %d still present, want purged", id)
	}
	docs, err := h.db.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("GetAllDocuments returned %d rows after purge, want 0", len(docs))
	}
	if h.registry.calls != 1 {
		t.Fatalf("SyncJSONRegistry calls = %d, want exactly 1", h.registry.calls)
	}

	if len(h.events) != 2 {
		t.Fatalf("events = %d, want CLEAR_STARTED + FILE_PROGRESS: %+v", len(h.events), h.events)
	}
	start := h.events[0]
	if start.Type != EventClearStarted || start.TotalFiles != 1 {
		t.Fatalf("CLEAR_STARTED = %+v", start)
	}
	if start.Message != "Clearing registry records and moving 1 physical file(s) back to __raws..." {
		t.Fatalf("CLEAR_STARTED message = %q", start.Message)
	}
	prog := h.events[1]
	if prog.Type != EventFileProgress {
		t.Fatalf("second event type = %q, want FILE_PROGRESS", prog.Type)
	}
	if derefString(prog.Filename) != "facture.pdf" {
		t.Fatalf("filename = %q, want facture.pdf", derefString(prog.Filename))
	}
	if derefInt(prog.ScannedCount) != 1 || derefInt(prog.ProcessedCount) != 1 {
		t.Fatalf("scanned/processed = %d/%d, want 1/1", derefInt(prog.ScannedCount), derefInt(prog.ProcessedCount))
	}
	if prog.TotalFiles != 1 {
		t.Fatalf("totalFiles = %d, want 1", prog.TotalFiles)
	}
	if derefString(prog.Stage) != StageClearing {
		t.Fatalf("stage = %q, want CLEARING", derefString(prog.Stage))
	}
	if prog.Message != "Moving file 1/1 back to __raws: Facture SFR" {
		t.Fatalf("FILE_PROGRESS message = %q", prog.Message)
	}
}

func TestClear_LeavesFileOutsideOutputRootButStillPurgesRow(t *testing.T) {
	h := newHarness(t)

	outsideFile := filepath.Join(h.root, "somewhere_else", "weird.pdf")
	writeFile(t, outsideFile, "dummy")
	id := h.insert(func(d *database.NewDocument) { d.NewPath = outsideFile })

	result, err := h.run()
	if err != nil {
		t.Fatalf("ClearRegistryAndMoveArchiveToRaws: %v", err)
	}

	if result.CountMoved != 0 {
		t.Fatalf("CountMoved = %d, want 0", result.CountMoved)
	}
	mustExist(t, outsideFile) // untouched

	doc, err := h.db.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if doc != nil {
		t.Fatalf("document row %d still present; the DB row must be purged unconditionally", id)
	}
	// The boundary check must not emit a FILE_PROGRESS for a file it will not move.
	if len(h.events) != 1 || h.events[0].Type != EventClearStarted {
		t.Fatalf("events = %+v, want a single CLEAR_STARTED", h.events)
	}
}

func TestClear_MovesOrphanFilesAndPrunesEmptySkeleton(t *testing.T) {
	h := newHarness(t)

	orphanFile := filepath.Join(h.outputDir, "health", "doctor_x", "2025", "orphan.pdf")
	writeFile(t, orphanFile, "dummy pdf bytes")

	result, err := h.run()
	if err != nil {
		t.Fatalf("ClearRegistryAndMoveArchiveToRaws: %v", err)
	}

	if result.CountMoved != 1 {
		t.Fatalf("CountMoved = %d, want 1", result.CountMoved)
	}
	mustNotExist(t, orphanFile)
	mustExist(t, filepath.Join(h.inputDir, "orphan.pdf"))
	// The now-empty category/subcategory/year skeleton under __archive should be pruned.
	mustNotExist(t, filepath.Join(h.outputDir, "health"))

	// No DB row existed, so only CLEAR_STARTED is emitted: the orphan walk never emits
	// FILE_PROGRESS in TS.
	if len(h.events) != 1 || h.events[0].Type != EventClearStarted || h.events[0].TotalFiles != 0 {
		t.Fatalf("events = %+v, want a single CLEAR_STARTED with totalFiles 0", h.events)
	}
}

// --- added cases ---------------------------------------------------------------------------------

func TestClear_EmptyRegistryStillEmitsClearStartedAndSyncs(t *testing.T) {
	h := newHarness(t)

	result, err := h.run()
	if err != nil {
		t.Fatalf("ClearRegistryAndMoveArchiveToRaws: %v", err)
	}
	if result.CountMoved != 0 {
		t.Fatalf("CountMoved = %d, want 0", result.CountMoved)
	}
	if h.registry.calls != 1 {
		t.Fatalf("SyncJSONRegistry calls = %d, want 1", h.registry.calls)
	}
	if len(h.events) != 1 {
		t.Fatalf("events = %+v, want a single CLEAR_STARTED", h.events)
	}
	if h.events[0] != NewClearStartedEvent(0) {
		t.Fatalf("event = %+v, want %+v", h.events[0], NewClearStartedEvent(0))
	}
}

func TestClear_MoverIsCalledWithNilChecksum(t *testing.T) {
	h := newHarness(t)

	archivedFile := filepath.Join(h.outputDir, "invoices", "sfr", "2026", "facture.pdf")
	writeFile(t, archivedFile, "dummy")
	h.insert(func(d *database.NewDocument) { d.NewPath = archivedFile })

	spy := &spyMover{real: h.deps.Mover}
	deps := h.deps
	deps.Mover = spy

	if _, err := deps.ClearRegistryAndMoveArchiveToRaws(h.progress); err != nil {
		t.Fatalf("ClearRegistryAndMoveArchiveToRaws: %v", err)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("mover calls = %d, want 1: %+v", len(spy.calls), spy.calls)
	}
	if spy.calls[0].checksum != nil {
		t.Fatalf("checksum = %v, want nil (the TS one-argument moveBackToRaws call)", *spy.calls[0].checksum)
	}
}

func TestClear_WarnsOnMoveFailureAndContinues(t *testing.T) {
	h := newHarness(t)

	first := filepath.Join(h.outputDir, "invoices", "sfr", "2026", "first.pdf")
	second := filepath.Join(h.outputDir, "invoices", "sfr", "2026", "second.pdf")
	writeFile(t, first, "one")
	writeFile(t, second, "two")
	h.insert(func(d *database.NewDocument) { d.OriginalFilename = "first.pdf"; d.NewPath = first })
	h.insert(func(d *database.NewDocument) { d.OriginalFilename = "second.pdf"; d.NewPath = second })

	mover := &failingMover{err: errors.New("move boom")}
	deps := h.deps
	deps.Mover = mover

	result, err := deps.ClearRegistryAndMoveArchiveToRaws(h.progress)
	if err != nil {
		t.Fatalf("a per-file move failure must not fail the run: %v", err)
	}
	if result.CountMoved != 0 {
		t.Fatalf("CountMoved = %d, want 0", result.CountMoved)
	}
	// Both files remain: the mover never moved them.
	mustExist(t, first)
	mustExist(t, second)
	// The DB is purged and the registry synced regardless.
	docs, err := h.db.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("GetAllDocuments returned %d rows after purge, want 0", len(docs))
	}
	if h.registry.calls != 1 {
		t.Fatalf("SyncJSONRegistry calls = %d, want 1", h.registry.calls)
	}
	// CLEAR_STARTED + one FILE_PROGRESS per tracked doc (the orphan walk emits none).
	if len(h.events) != 3 {
		t.Fatalf("events = %d, want 3: %+v", len(h.events), h.events)
	}
}

func TestClear_PurgeFailurePropagates(t *testing.T) {
	h := newHarness(t)

	deps := h.deps
	deps.DB = purgeFailingStore{Store: h.db, err: errors.New("purge boom")}

	_, err := deps.ClearRegistryAndMoveArchiveToRaws(h.progress)
	if err == nil || !strings.Contains(err.Error(), "purge boom") {
		t.Fatalf("err = %v, want the PurgeAll failure", err)
	}
}

func TestClear_RegistrySyncFailurePropagates(t *testing.T) {
	h := newHarness(t)
	h.registry.err = errors.New("sync boom")

	_, err := h.run()
	if err == nil || !strings.Contains(err.Error(), "sync boom") {
		t.Fatalf("err = %v, want the SyncJSONRegistry failure", err)
	}
}

func TestClear_EventJSONMatchesTSPayloads(t *testing.T) {
	startJSON, err := json.Marshal(NewClearStartedEvent(0))
	if err != nil {
		t.Fatalf("marshal CLEAR_STARTED: %v", err)
	}
	wantStart := `{"type":"CLEAR_STARTED","totalFiles":0,"message":"Clearing registry records and moving 0 physical file(s) back to __raws..."}`
	if string(startJSON) != wantStart {
		t.Fatalf("CLEAR_STARTED JSON:\n got %s\nwant %s", startJSON, wantStart)
	}

	progJSON, err := json.Marshal(NewFileProgressEvent("facture.pdf", 2, 5, "Facture SFR"))
	if err != nil {
		t.Fatalf("marshal FILE_PROGRESS: %v", err)
	}
	wantProg := `{"type":"FILE_PROGRESS","filename":"facture.pdf","scannedCount":2,"processedCount":2,"totalFiles":5,"stage":"CLEARING","message":"Moving file 2/5 back to __raws: Facture SFR"}`
	if string(progJSON) != wantProg {
		t.Fatalf("FILE_PROGRESS JSON:\n got %s\nwant %s", progJSON, wantProg)
	}
}

func TestClear_NeverRemovesOutputRootDir(t *testing.T) {
	h := newHarness(t)
	if err := os.MkdirAll(h.outputDir, 0o755); err != nil {
		t.Fatalf("mkdir output: %v", err)
	}
	emptySub := filepath.Join(h.outputDir, "emptycat")
	if err := os.MkdirAll(emptySub, 0o755); err != nil {
		t.Fatalf("mkdir empty sub: %v", err)
	}

	if _, err := h.run(); err != nil {
		t.Fatalf("ClearRegistryAndMoveArchiveToRaws: %v", err)
	}

	mustExist(t, h.outputDir) // __archive itself is never removed
	mustNotExist(t, emptySub) // an empty subdirectory is pruned
}

func TestClear_CallsEnsureDirectoriesAtStartAndEnd(t *testing.T) {
	h := newHarness(t)

	calls := 0
	deps := h.deps
	deps.EnsureDirectories = func() error {
		calls++
		return nil
	}

	if _, err := deps.ClearRegistryAndMoveArchiveToRaws(h.progress); err != nil {
		t.Fatalf("ClearRegistryAndMoveArchiveToRaws: %v", err)
	}
	if calls != 2 {
		t.Fatalf("ensureDirectories calls = %d, want 2 (start + post-orphan-walk)", calls)
	}
}

func TestClear_RequiresDBAndMover(t *testing.T) {
	var noDeps Deps
	if _, err := noDeps.ClearRegistryAndMoveArchiveToRaws(nil); err == nil {
		t.Fatal("expected an error when Deps.DB is nil")
	}

	h := newHarness(t)
	noMover := Deps{Config: h.cfg, DB: h.db}
	if _, err := noMover.ClearRegistryAndMoveArchiveToRaws(nil); err == nil {
		t.Fatal("expected an error when Deps.Mover is nil")
	}
}

func TestClear_ProductionRegistrySyncWritesMirror(t *testing.T) {
	h := newHarness(t)

	deps := h.deps
	deps.Registry = NewJSONRegistrySync(h.db, h.cfg.JSONRegistryPath)

	if _, err := deps.ClearRegistryAndMoveArchiveToRaws(h.progress); err != nil {
		t.Fatalf("ClearRegistryAndMoveArchiveToRaws: %v", err)
	}

	raw, err := os.ReadFile(h.cfg.JSONRegistryPath)
	if err != nil {
		t.Fatalf("read registry.json: %v", err)
	}
	var data struct {
		TotalCount int `json:"total_count"`
		Documents  []struct {
		} `json:"documents"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("registry.json is not valid JSON: %v\n%s", err, raw)
	}
	if data.TotalCount != 0 {
		t.Fatalf("registry total_count = %d, want 0 after a full purge", data.TotalCount)
	}
}

// Compile-time reminder that the production relocalize adapters still satisfy this package's seams
// (the assertions in clear.go cover this too; this keeps the test file self-documenting).
var (
	_ FileMover = relocalize.Deps{}
)
