package httpapi

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/app/aichat"
	"github.com/phamhung075/pdf-triage-pdf2w/app/clear"
	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/app/repair"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

// --- fakes --------------------------------------------------------------------------------------

type fakeWriteDB struct {
	mu         sync.Mutex
	docs       map[int64]*database.DocumentRecord
	all        []database.DocumentRecord
	blocked    map[string]*database.BlockedFileRecord
	getErr     error
	updateOK   bool
	updateErr  error
	updateCall []fakeUpdateCall
}

type fakeUpdateCall struct {
	id      any
	updates database.DocumentUpdates
}

func newFakeWriteDB() *fakeWriteDB {
	return &fakeWriteDB{docs: map[int64]*database.DocumentRecord{}, blocked: map[string]*database.BlockedFileRecord{}, updateOK: true}
}

func (f *fakeWriteDB) GetDocumentByID(id int64) (*database.DocumentRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.docs[id], nil
}

func (f *fakeWriteDB) UpdateDocumentRecord(id any, updates database.DocumentUpdates) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updateCall = append(f.updateCall, fakeUpdateCall{id: id, updates: updates})
	if f.updateErr != nil {
		return false, f.updateErr
	}
	return f.updateOK, nil
}

func (f *fakeWriteDB) GetAllDocuments() ([]database.DocumentRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.all, nil
}

func (f *fakeWriteDB) GetBlockedFile(originalPath string) (*database.BlockedFileRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blocked[originalPath], nil
}

func (f *fakeWriteDB) lastUpdate() (fakeUpdateCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.updateCall) == 0 {
		return fakeUpdateCall{}, false
	}
	return f.updateCall[len(f.updateCall)-1], true
}

type fakeWriteDecisions struct {
	mu      sync.Mutex
	records []manualdecisions.Record
}

func (f *fakeWriteDecisions) RecordManualDecision(record manualdecisions.Record) {
	f.mu.Lock()
	f.records = append(f.records, record)
	f.mu.Unlock()
}

func (f *fakeWriteDecisions) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.records)
}

// fakeRelocalizer covers the three app/relocalize entry points and records the arguments the
// routes forward, which is how the Golden Rule 18 reason-forwarding test asserts.
type fakeRelocalizer struct {
	mu sync.Mutex

	relocalizeFn func(filePath, category string, subcategory, dateStr, title *string) (relocalize.RelocalizeResult, error)
	reclassifyFn func(id int64, category, subcategory, reason *string) (relocalize.ReclassifyResult, error)
	deleteFn     func(id int64) (relocalize.DeleteResult, error)

	relocalizeCalls int
	reclassifyCalls int
	deleteCalls     int
	lastReason      *string
	lastCategory    string
	lastSubcategory *string
	order           *orderRecorder
}

func newFakeRelocalizer(order *orderRecorder) *fakeRelocalizer {
	return &fakeRelocalizer{
		order: order,
		relocalizeFn: func(string, string, *string, *string, *string) (relocalize.RelocalizeResult, error) {
			return relocalize.RelocalizeResult{}, nil
		},
		reclassifyFn: func(int64, *string, *string, *string) (relocalize.ReclassifyResult, error) {
			return relocalize.ReclassifyResult{Success: true, Message: "ok"}, nil
		},
		deleteFn: func(int64) (relocalize.DeleteResult, error) {
			return relocalize.DeleteResult{Success: true, Message: "deleted"}, nil
		},
	}
}

func (f *fakeRelocalizer) RelocalizeFileIfNeeded(filePath, category string, subcategory, dateStr, title *string) (relocalize.RelocalizeResult, error) {
	f.mu.Lock()
	f.relocalizeCalls++
	f.lastCategory = category
	f.lastSubcategory = subcategory
	fn := f.relocalizeFn
	order := f.order
	f.mu.Unlock()
	order.add("relocalize")
	return fn(filePath, category, subcategory, dateStr, title)
}

func (f *fakeRelocalizer) ReclassifyAndRelocalizeDocument(id int64, category, subcategory, reason *string) (relocalize.ReclassifyResult, error) {
	f.mu.Lock()
	f.reclassifyCalls++
	f.lastReason = reason
	fn := f.reclassifyFn
	f.mu.Unlock()
	return fn(id, category, subcategory, reason)
}

func (f *fakeRelocalizer) DeleteDocumentAndMoveToTrash(id int64) (relocalize.DeleteResult, error) {
	f.mu.Lock()
	f.deleteCalls++
	fn := f.deleteFn
	f.mu.Unlock()
	return fn(id)
}

type fakeRepairRunner struct {
	mu  sync.Mutex
	run func(onProgress repair.ProgressFunc) (repair.Result, error)
}

func (f *fakeRepairRunner) RepairRegistry(onProgress repair.ProgressFunc) (repair.Result, error) {
	f.mu.Lock()
	fn := f.run
	f.mu.Unlock()
	if fn == nil {
		return repair.Result{}, nil
	}
	return fn(onProgress)
}

type fakeClearRunner struct {
	mu  sync.Mutex
	run func(onProgress func(clear.Event)) (clear.ClearResult, error)
}

func (f *fakeClearRunner) ClearRegistryAndMoveArchiveToRaws(onProgress func(clear.Event)) (clear.ClearResult, error) {
	f.mu.Lock()
	fn := f.run
	f.mu.Unlock()
	if fn == nil {
		return clear.ClearResult{}, nil
	}
	return fn(onProgress)
}

type fakeScanRunner struct {
	mu  sync.Mutex
	run func(ctx context.Context, onProgress func(triagescan.Event), shouldAbort func() bool) (triagescan.Result, error)
}

func (f *fakeScanRunner) RunTriageScan(ctx context.Context, onProgress func(triagescan.Event), shouldAbort func() bool) (triagescan.Result, error) {
	f.mu.Lock()
	fn := f.run
	f.mu.Unlock()
	if fn == nil {
		return triagescan.Result{}, nil
	}
	return fn(ctx, onProgress, shouldAbort)
}

type fakeScanLocker struct {
	mu           sync.Mutex
	err          error
	acquireCalls int
	releaseCalls int
}

func (f *fakeScanLocker) Acquire() (func(), error) {
	f.mu.Lock()
	f.acquireCalls++
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return func() {
		f.mu.Lock()
		f.releaseCalls++
		f.mu.Unlock()
	}, nil
}

type fakeRegistry struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (f *fakeRegistry) SyncJSONRegistry() error {
	f.mu.Lock()
	f.calls++
	err := f.err
	f.mu.Unlock()
	return err
}

type fakeWriteLog struct {
	mu    sync.Mutex
	infos []string
}

func (f *fakeWriteLog) Info(moduleName, message string, meta any, filename ...string) {
	f.mu.Lock()
	f.infos = append(f.infos, moduleName+":"+message)
	f.mu.Unlock()
}
func (f *fakeWriteLog) Warn(string, string, any, ...string)  {}
func (f *fakeWriteLog) Error(string, string, any, ...string) {}

type fakeAIChatStore struct {
	docs []database.DocumentRecord
	err  error
}

func (f *fakeAIChatStore) GetAllDocuments() ([]database.DocumentRecord, error) {
	return f.docs, f.err
}
func (f *fakeAIChatStore) SearchDocumentsFts(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
	return nil, nil
}

// orderedCategories records the pre-move save on the shared order recorder so a test can prove the
// Golden Rule 5 guard runs before the physical move.
type orderedCategories struct {
	*fakeCategories
	order *orderRecorder
}

func (o *orderedCategories) SaveCategoriesConfig(categories []*documentschema.CategoryItem) error {
	o.order.add("save")
	return o.fakeCategories.SaveCategoriesConfig(categories)
}

// orderRecorder records cross-fake call order.
type orderRecorder struct {
	mu    sync.Mutex
	steps []string
}

func (o *orderRecorder) add(step string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.steps = append(o.steps, step)
	o.mu.Unlock()
}

func (o *orderRecorder) indexOf(step string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	for i, got := range o.steps {
		if got == step {
			return i
		}
	}
	return -1
}

// --- test environment ---------------------------------------------------------------------------

type writeEnv struct {
	base      *testEnv
	db        *fakeWriteDB
	cats      *orderedCategories
	decisions *fakeWriteDecisions
	reloc     *fakeRelocalizer
	repairR   *fakeRepairRunner
	clearR    *fakeClearRunner
	scanR     *fakeScanRunner
	lock      *fakeScanLocker
	registry  *fakeRegistry
	log       *fakeWriteLog
	aiStore   *fakeAIChatStore
	order     *orderRecorder
	srv       *server
	handler   http.Handler
}

func newWriteEnv() *writeEnv {
	base := newTestEnv()
	order := &orderRecorder{}
	env := &writeEnv{
		base:      base,
		db:        newFakeWriteDB(),
		cats:      &orderedCategories{fakeCategories: &fakeCategories{}, order: order},
		decisions: &fakeWriteDecisions{},
		reloc:     newFakeRelocalizer(order),
		repairR:   &fakeRepairRunner{},
		clearR:    &fakeClearRunner{},
		scanR:     &fakeScanRunner{},
		lock:      &fakeScanLocker{},
		registry:  &fakeRegistry{},
		log:       &fakeWriteLog{},
		aiStore:   &fakeAIChatStore{},
		order:     order,
	}
	deps := testDeps(base)
	deps.RouteGroups = []RouteGroup{DocumentWriteRoutes(env.deps())}
	env.srv = newServer(deps)
	env.handler = env.srv.handler
	return env
}

func (e *writeEnv) deps() DocumentWriteDeps {
	return DocumentWriteDeps{
		Settings:          e.base.settings,
		DB:                e.db,
		Categories:        e.cats,
		ManualDecisions:   e.decisions,
		Relocalizer:       e.reloc,
		Repair:            e.repairR,
		Clear:             e.clearR,
		Scan:              e.scanR,
		ScanLock:          e.lock,
		AIChat:            aichat.Deps{Store: e.aiStore, Log: e.log},
		Registry:          e.registry,
		Tasks:             e.base.tasks,
		Log:               e.log,
		EnsureDirectories: func() error { return nil },
	}
}

// --- minimal PDF builder ------------------------------------------------------------------------

// writeTestPDF writes a valid minimal N-page PDF to path. pdfcpu (merge/split) validates the xref,
// so the offsets are computed rather than faked.
func writeTestPDF(t *testing.T, path string, pages int) {
	t.Helper()
	data := minimalPDF(pages)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write test pdf: %v", err)
	}
}

func minimalPDF(pages int) []byte {
	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")

	offsets := make([]int, 2+2*pages+1)
	object := func(id int, body string) {
		offsets[id] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", id, body)
	}

	kids := make([]string, 0, pages)
	for i := 0; i < pages; i++ {
		kids = append(kids, fmt.Sprintf("%d 0 R", 3+2*i))
	}
	object(1, "<< /Type /Catalog /Pages 2 0 R >>")
	object(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", joinStrings(kids, " "), pages))
	for i := 0; i < pages; i++ {
		pageID := 3 + 2*i
		contentID := 4 + 2*i
		object(pageID, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 200 200] /Contents %d 0 R >>", contentID))
		object(contentID, "<< /Length 0 >>\nstream\n\nendstream")
	}

	total := 2 + 2*pages
	xrefOffset := buf.Len()
	fmt.Fprintf(&buf, "xref\n0 %d\n", total+1)
	buf.WriteString("0000000000 65535 f \n")
	for id := 1; id <= total; id++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[id])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", total+1, xrefOffset)
	return buf.Bytes()
}

func joinStrings(values []string, sep string) string {
	out := ""
	for i, value := range values {
		if i > 0 {
			out += sep
		}
		out += value
	}
	return out
}
