package httpapi

import (
	"net/http"
	"sync"

	"github.com/phamhung075/pdf-triage-pdf2w/app/taskstate"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

// fakeSettings is an in-memory Settings. UpdateConfig mirrors settings.Store's field application
// (non-empty wins) so PUT /api/config responses read back the new values.
type fakeSettings struct {
	cfg       settings.Config
	dataDir   string
	baseDir   string
	firstRun  bool
	updateErr error
	updates   []settings.UpdateSettings
	mu        sync.Mutex
}

func (f *fakeSettings) Config() settings.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}
func (f *fakeSettings) IsFirstRun() bool { return f.firstRun }
func (f *fakeSettings) DataDir() string  { return f.dataDir }
func (f *fakeSettings) BaseDir() string  { return f.baseDir }
func (f *fakeSettings) UpdateConfig(patch settings.UpdateSettings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.updates = append(f.updates, patch)
	if f.updateErr != nil {
		return f.updateErr
	}
	if patch.Language != nil && *patch.Language != "" {
		f.cfg.Language = *patch.Language
	}
	if patch.InputDir != nil && *patch.InputDir != "" {
		f.cfg.InputDir = *patch.InputDir
	}
	if patch.OutputRootDir != nil && *patch.OutputRootDir != "" {
		f.cfg.OutputRootDir = *patch.OutputRootDir
	}
	if patch.OllamaModel != nil && *patch.OllamaModel != "" {
		f.cfg.OllamaModel = *patch.OllamaModel
	}
	if patch.OllamaHost != nil && *patch.OllamaHost != "" {
		f.cfg.OllamaHost = *patch.OllamaHost
	}
	if patch.PersonalNameDenylist != nil {
		f.cfg.PersonalNameDenylist = patch.PersonalNameDenylist
	}
	return nil
}

type fakeDB struct {
	stats      database.CategoryStats
	statsErr   error
	blocked    []database.BlockedFileRecord
	blockedErr error
}

func (f *fakeDB) GetAllBlockedFiles() ([]database.BlockedFileRecord, error) {
	return f.blocked, f.blockedErr
}
func (f *fakeDB) GetCategorySubcategoryStats() (database.CategoryStats, error) {
	return f.stats, f.statsErr
}

type fakeCategories struct {
	cfg      documentschema.CategoriesConfig
	saved    [][]*documentschema.CategoryItem
	saveErr  error
	callback func()
}

func (f *fakeCategories) GetCategoriesConfig() documentschema.CategoriesConfig { return f.cfg }
func (f *fakeCategories) SaveCategoriesConfig(categories []*documentschema.CategoryItem) error {
	f.saved = append(f.saved, categories)
	if f.callback != nil {
		f.callback()
	}
	return f.saveErr
}
func (f *fakeCategories) SetOnCategoryCreatedCallback(cb func()) { f.callback = cb }

type fakeDecisions struct {
	records     []manualdecisions.Record
	update      *manualdecisions.Record
	updateErr   error
	lastID      int64
	lastPatch   manualdecisions.Patch
	updateCalls int
	deleted     bool
	deleteErr   error
	clearErr    error
	clearCalls  int
}

func (f *fakeDecisions) GetManualDecisions() []manualdecisions.Record { return f.records }
func (f *fakeDecisions) UpdateManualDecision(id int64, patch manualdecisions.Patch) (*manualdecisions.Record, error) {
	f.updateCalls++
	f.lastID = id
	f.lastPatch = patch
	return f.update, f.updateErr
}
func (f *fakeDecisions) DeleteManualDecision(id int64) (bool, error) {
	f.lastID = id
	return f.deleted, f.deleteErr
}
func (f *fakeDecisions) ClearManualDecisions() error {
	f.clearCalls++
	return f.clearErr
}

type fakeLogs struct {
	recent   []logger.LogEntry
	sessions []logger.DocumentLogSession
	mu       sync.Mutex
	subs     []func(logger.LogEntry)
}

func (f *fakeLogs) RecentLogs(limit int) []logger.LogEntry          { return f.recent }
func (f *fakeLogs) GroupedSessionLogs() []logger.DocumentLogSession { return f.sessions }
func (f *fakeLogs) Subscribe(fn func(logger.LogEntry)) func() {
	f.mu.Lock()
	f.subs = append(f.subs, fn)
	idx := len(f.subs) - 1
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		f.subs[idx] = nil
		f.mu.Unlock()
	}
}
func (f *fakeLogs) emit(entry logger.LogEntry) {
	f.mu.Lock()
	subs := append([]func(logger.LogEntry){}, f.subs...)
	f.mu.Unlock()
	for _, fn := range subs {
		if fn != nil {
			fn(entry)
		}
	}
}
func (f *fakeLogs) subscriberCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, fn := range f.subs {
		if fn != nil {
			count++
		}
	}
	return count
}

type fakeOllama struct {
	models    []string
	listErr   error
	health    ollama.ModelHealth
	listHosts []string
	healthFor []string
}

func (f *fakeOllama) ListModels(host string) ([]string, error) {
	f.listHosts = append(f.listHosts, host)
	return f.models, f.listErr
}
func (f *fakeOllama) CheckModelCanGenerate(model string, forceRefresh bool) ollama.ModelHealth {
	f.healthFor = append(f.healthFor, model)
	return f.health
}

type fakeOpener struct {
	launch    Launch
	directory bool
	reveal    bool
	chrome    bool
	chromeOK  bool
	paths     []string
}

func (f *fakeOpener) OpenDirectory(path string) (Launch, bool) {
	f.directory = true
	f.paths = append(f.paths, path)
	return Launch{Cmd: f.launch.Cmd, Args: []string{path}}, true
}
func (f *fakeOpener) RevealInFileManager(path string) (Launch, bool) {
	f.reveal = true
	f.paths = append(f.paths, path)
	return Launch{Cmd: f.launch.Cmd, Args: []string{path}}, true
}
func (f *fakeOpener) OpenInChrome(path string) (Launch, bool) {
	f.chrome = true
	f.paths = append(f.paths, path)
	return Launch{Cmd: f.launch.Cmd, Args: []string{path}}, f.chromeOK
}

type fakeSpawner struct {
	calls []Launch
}

func (f *fakeSpawner) Spawn(cmd string, args []string) {
	f.calls = append(f.calls, Launch{Cmd: cmd, Args: args})
}

type fakeWatcher struct {
	ch chan struct{}
}

func newFakeWatcher() *fakeWatcher { return &fakeWatcher{ch: make(chan struct{}, 1)} }
func (f *fakeWatcher) Subscribe() (<-chan struct{}, func()) {
	return f.ch, func() {}
}
func (f *fakeWatcher) emit() { f.ch <- struct{}{} }

// testEnv bundles the fakes plus the handler, so each test mutates only what it asserts.
type testEnv struct {
	settings  *fakeSettings
	db        *fakeDB
	cats      *fakeCategories
	decisions *fakeDecisions
	logs      *fakeLogs
	ollama    *fakeOllama
	opener    *fakeOpener
	spawner   *fakeSpawner
	watcher   *fakeWatcher
	tasks     *taskstate.Manager
	exits     []int
	startedCh chan struct{}
	handler   http.Handler
}

func newTestEnv() *testEnv {
	env := &testEnv{
		settings: &fakeSettings{
			cfg: settings.Config{
				Language:             "fr",
				InputDir:             "/tmp/raws",
				OutputRootDir:        "/tmp/archive",
				OllamaModel:          "qwen3.5:9b",
				OllamaHost:           "http://127.0.0.1:11434",
				DBPath:               "/tmp/does-not-exist.db",
				PersonalNameDenylist: []string{"secret"},
			},
			dataDir: "/tmp/data",
			baseDir: "/tmp/base",
		},
		db:        &fakeDB{},
		cats:      &fakeCategories{},
		decisions: &fakeDecisions{},
		logs:      &fakeLogs{},
		ollama:    &fakeOllama{},
		opener:    &fakeOpener{launch: Launch{Cmd: "xdg-open", Args: []string{"/tmp"}}, chromeOK: true},
		spawner:   &fakeSpawner{},
		watcher:   newFakeWatcher(),
		tasks:     taskstate.NewManager(),
		startedCh: make(chan struct{}, 1),
	}
	env.handler = NewServer(testDeps(env))
	return env
}

// testDeps builds a Deps from the env fakes so individual tests can rebuild the handler with
// overrides (e.g. RestartDelay, PublicDir).
func testDeps(env *testEnv) Deps {
	return Deps{
		Settings:        env.settings,
		DB:              env.db,
		Categories:      env.cats,
		ManualDecisions: env.decisions,
		Logs:            env.logs,
		Tasks:           env.tasks,
		Ollama:          env.ollama,
		Opener:          env.opener,
		Spawner:         env.spawner,
		StartOllama:     func() error { env.startedCh <- struct{}{}; return nil },
		Exit:            func(code int) { env.exits = append(env.exits, code) },
		LiveReload:      env.watcher,
	}
}

func intPtr(value int) *int { return &value }

// manualDecisionRecord is the store result the PUT decision tests return.
var manualDecisionRecord = manualdecisions.Record{
	ID:                 3,
	DocumentID:         1,
	Checksum:           "abc",
	OriginalFilename:   "x.pdf",
	Title:              "X",
	OldCategory:        "invoices",
	OldSubcategory:     "sfr",
	NewCategory:        "bank",
	NewSubcategory:     "societe_generale",
	UserFeedbackReason: "Manual user selection",
	RawTextSnippet:     "snip",
	RuleKeywords:       []string{"SG_CODE"},
	Enabled:            intPtr(0),
	CreatedAt:          "2026-01-01T00:00:00.000Z",
}
