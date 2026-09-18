package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/aichat"
	"github.com/phamhung075/pdf-triage-pdf2w/app/chatplanner"
	"github.com/phamhung075/pdf-triage-pdf2w/app/classify"
	"github.com/phamhung075/pdf-triage-pdf2w/app/clear"
	"github.com/phamhung075/pdf-triage-pdf2w/app/convertimage"
	"github.com/phamhung075/pdf-triage-pdf2w/app/imagetopdf"
	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/app/repair"
	"github.com/phamhung075/pdf-triage-pdf2w/app/scanlock"
	"github.com/phamhung075/pdf-triage-pdf2w/app/taskstate"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/chatquery"
	"github.com/phamhung075/pdf-triage-pdf2w/classification"
	"github.com/phamhung075/pdf-triage-pdf2w/httpapi"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/imageprocessor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/osopen"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/vision"
	"github.com/phamhung075/pdf-triage-pdf2w/mcpserver"
	"github.com/phamhung075/pdf-triage-pdf2w/prompt"
	"github.com/phamhung075/pdf-triage-pdf2w/store/categories"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/entitydictionary"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
	storepromptpersonalization "github.com/phamhung075/pdf-triage-pdf2w/store/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/store/taxonomyhints"
)

// appOptions carries the composition root's injectable inputs. Zero values are the production
// behavior; every process-launching or clock seam is overridable so tests never start
// Explorer/Chrome/Ollama and never depend on wall-clock timing.
type appOptions struct {
	// BaseDir is BASE_DIR. Empty means the process working directory, exactly like settings.ts.
	BaseDir string
	// DataDir is DATA_DIR. Empty means BaseDir.
	DataDir string

	// OllamaSpawnServe / OllamaSleep override the auto-spawn of `ollama serve` that
	// infra/ollama performs when the model is unreachable. Tests inject no-ops.
	OllamaSpawnServe func() error
	OllamaSleep      func(time.Duration)

	// Spawner overrides the OS launcher used by POST /api/open-location and /api/open-chrome.
	Spawner httpapi.Spawner
	// McpOpener / McpRunner override the MCP open_document_folder launcher.
	McpOpener mcpserver.Opener
	McpRunner mcpserver.Runner

	// Now overrides every injected clock (task state, scans, decisions).
	Now func() time.Time
}

// application owns every constructed collaborator, the wired HTTP handler, the server capture and
// the auto-watcher. Close releases the watcher and the database.
type application struct {
	settings  *settings.Store
	log       *logger.Logger
	db        *database.Store
	scanLock  *scanlock.Guard
	scanner   *triagescan.Scanner
	stepper   *imagetopdf.Stepper
	tasks     *taskstate.Manager
	handler   http.Handler
	server    *httpapi.Server
	watcher   *httpapi.Watcher
	mcpDeps   mcpserver.Deps
	firstRun  bool
	publicDir string

	closeOnce sync.Once
}

// findBaseDir resolves the project root containing public/ and settings/categories.
// It checks explicit, PDF_TRIAGE_BASE_DIR, working directory, and parent directories.
func findBaseDir(explicit string) string {
	if explicit != "" {
		return explicit
	}
	if v := os.Getenv("PDF_TRIAGE_BASE_DIR"); v != "" {
		return v
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	if hasPublicDir(wd) {
		return wd
	}
	parent := filepath.Clean(filepath.Join(wd, ".."))
	if hasPublicDir(parent) {
		return parent
	}
	grandparent := filepath.Clean(filepath.Join(wd, "..", ".."))
	if hasPublicDir(grandparent) {
		return grandparent
	}
	return wd
}

func hasPublicDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "public"))
	return err == nil && info.IsDir()
}

// newApplication builds the whole dependency graph. The settings store is created first because
// every path derives from it; a failure to create it (or to open the SQLite database) is fatal.
func newApplication(opts appOptions) (*application, error) {
	baseDir := findBaseDir(opts.BaseDir)

	settingsStore, err := settings.New(settings.Options{BaseDir: baseDir, DataDir: opts.DataDir})
	if err != nil {
		return nil, err
	}
	baseDir = settingsStore.BaseDir()
	cfg := settingsStore.Config()
	dataDir := settingsStore.DataDir()

	// ensureDirectoriesExist() runs at startup for every subcommand, exactly as src/index.ts:7 did.
	if err := settingsStore.EnsureDirectoriesExist(); err != nil {
		return nil, err
	}

	log := logger.New(logger.OptionsFromEnv(dataDir, os.Getenv))

	db, err := database.Open(cfg.DBPath)
	if err != nil {
		return nil, err
	}

	// --- stores -------------------------------------------------------------------------------
	categoriesStore := categories.New(cfg.CategoriesFile, cfg.CategoriesPrivateFile, os.Stderr)
	entityDictionaryStore := entitydictionary.New(entitydictionary.Options{FilePath: cfg.EntityDictionaryFile, Stderr: os.Stderr})
	hintsStore := taxonomyhints.New(taxonomyhints.Options{FilePath: cfg.TaxonomyHintsFile})
	decisionsStore := manualdecisions.New(db, manualdecisions.Options{DecisionsFile: cfg.ManualDecisionsFile, Logger: log})
	promptStore := storepromptpersonalization.New(storepromptpersonalization.Options{
		PromptsPrivateFile: cfg.PromptsPrivateFile,
		ManualDecisions:    decisionsStore,
		TaxonomyHints:      hintsStore,
		Stderr:             os.Stderr,
	})

	// --- infra adapters -----------------------------------------------------------------------
	ollamaClient := ollama.New(ollama.Config{
		BaseURL:    cfg.OllamaHost,
		Model:      cfg.OllamaModel,
		EmbedModel: cfg.OllamaEmbedModel,
		SpawnServe: opts.OllamaSpawnServe,
		Sleep:      opts.OllamaSleep,
		Logf:       func(format string, args ...any) { log.Warn("OLLAMA", fmt.Sprintf(format, args...), nil) },
	})
	visionClient := vision.NewClient(cfg.OllamaHost, cfg.OllamaVisionModel)
	stepper := imagetopdf.NewStepper(imagetopdf.DefaultDeps(visionClient, log))
	pdfExtractor := relocalize.NewPDFExtractor(cfg, log)
	registrySync := relocalize.JSONRegistrySync{DB: db, Path: cfg.JSONRegistryPath}

	// --- app layer ----------------------------------------------------------------------------
	classifyDeps := classify.Deps{
		Config: classify.Config{
			OllamaModel:          cfg.OllamaModel,
			OllamaHost:           cfg.OllamaHost,
			Language:             cfg.Language,
			PersonalNameDenylist: cfg.PersonalNameDenylist,
		},
		Ollama:                ollamaClient,
		Categories:            categoriesStore,
		EntityDictionary:      entityDictionaryStore,
		PromptPersonalization: promptStore,
		TaxonomyHints:         hintsStore,
		Prompts: prompt.Deps{
			Templates:       os.DirFS(cfg.PromptsDir),
			Personalization: promptStore.GetPromptPersonalization,
		},
		Log: log,
	}

	converter := convertimage.NewConverter(convertimage.Deps{
		InputDir: cfg.InputDir,
		Steps:    stepper,
		Extract:  pdfExtractor.ExtractPDFContent,
		EncodeJpeg: func(imageBuffer []byte, quality int) ([]byte, error) {
			return imageprocessor.EncodeJpeg(imageBuffer, quality)
		},
		ForDocument: func(filename string) convertimage.DocumentLogger {
			return log.ForDocument(filename)
		},
		Now: timeNowOr(opts.Now),
	})

	relocalizer := relocalize.Deps{
		Config:     cfg,
		DB:         db,
		Categories: categoriesStore,
		Extractor:  pdfExtractor,
		Classifier: classifyDeps,
		Registry:   registrySync,
		Decisions:  decisionsStore,
		Log:        log,
	}

	scanLock := scanlock.New(dataDir)

	repairDeps := repair.Deps{
		Config:                  cfg,
		Settings:                settingsStore,
		DB:                      db,
		Categories:              categoriesStore,
		Extractor:               pdfExtractor,
		Relocalizer:             relocalizer,
		Classifier:              classifyDeps,
		RuleBasedClassify:       classification.RuleBasedClassify,
		ExtractRuleBasedContact: classification.ExtractRuleBasedContact,
		GenerateEmbedding:       ollamaClient.GenerateEmbedding,
		EntityDictionary:        entityDictionaryStore,
		PromptPersonalization:   promptStore,
		Registry:                registrySync,
		Log:                     log,
		Lock:                    scanLock,
	}

	clearDeps := clear.Deps{
		Config:            cfg,
		DB:                db,
		Mover:             clear.NewRelocalizeMover(cfg, db),
		Registry:          clear.NewJSONRegistrySync(db, cfg.JSONRegistryPath),
		Log:               log,
		EnsureDirectories: settingsStore.EnsureDirectoriesExist,
	}

	aichatDeps := aichat.Deps{
		Store:  db,
		Ollama: ollamaClient,
		PlanQuery: func(userMessage string, now time.Time) (chatquery.StructuredQuery, error) {
			return chatplanner.PlanQuery(chatplanner.Deps{
				Ollama:     ollamaClient,
				Categories: categoriesStore.GetCategoriesConfig,
				Log:        log,
			}, userMessage, now), nil
		},
		Log: log,
	}

	scanner := triagescan.New(triagescan.Deps{
		Config:            cfg,
		ReloadConfig:      func() settings.Config { settingsStore.ReloadFromDisk(); return settingsStore.Config() },
		EnsureDirectories: settingsStore.EnsureDirectoriesExist,
		DB:                db,
		Extractor:         pdfExtractor,
		Converter:         converter,
		Classifier:        classifyDeps,
		Ollama:            ollamaClient,
		Relocalizer:       relocalizer,
		Registry:          triagescan.JSONRegistrySync{DB: db, Path: cfg.JSONRegistryPath},
		Log:               triagescan.AdaptLogger(log),
	})

	tasks := taskstate.NewManager()

	// --- HTTP ---------------------------------------------------------------------------------
	var captured *httpapi.Server
	spawner := httpapi.Spawner(detachedSpawner{})
	if opts.Spawner != nil {
		spawner = opts.Spawner
	}
	httpDeps := httpapi.Deps{
		Settings:        settingsStore,
		DB:              db,
		Categories:      categoriesStore,
		ManualDecisions: decisionsStore,
		Logs:            log,
		Tasks:           tasks,
		Ollama:          ollamaClient,
		Opener:          newOSLauncher(),
		Spawner:         spawner,
		StartOllama:     startOllamaServe,
		Exit:            os.Exit,
		PublicDir:       filepath.Join(baseDir, "public"),
		RouteGroups: []httpapi.RouteGroup{
			func(server *httpapi.Server) { captured = server },
			httpapi.DocumentReadRoutes(httpapi.DocumentReadDeps{
				Documents: db,
				Settings:  settingsStore,
				Opener:    newOSLauncher(),
				Spawner:   spawner,
				Mcp:       mcpToolLister{},
			}),
			httpapi.DocumentWriteRoutes(httpapi.DocumentWriteDeps{
				Settings:          settingsStore,
				DB:                db,
				Categories:        categoriesStore,
				ManualDecisions:   decisionsStore,
				Relocalizer:       relocalizer,
				Repair:            repairDeps,
				Clear:             clearDeps,
				Scan:              scanner,
				ScanLock:          scanLock,
				AIChat:            aichatDeps,
				Registry:          registrySync,
				Tasks:             tasks,
				Log:               log,
				EnsureDirectories: settingsStore.EnsureDirectoriesExist,
				Now:               timeNowOr(opts.Now),
			}),
		},
	}
	handler := httpapi.NewServer(httpDeps)

	// --- auto-watcher -------------------------------------------------------------------------
	// The server owns the SSE hub that GET /api/triage/events subscribes to. Sharing it with the
	// watcher (Server.Hub) makes an AUTO-scan's raw SCAN_*/FILE_* frames reach the same dashboard
	// clients as a manual scan; TASK_* already arrives through the shared taskstate.Manager
	// broadcaster installed by the route groups.
	var hub *httpapi.Hub
	if captured != nil {
		hub = captured.Hub()
	}
	watcher := httpapi.NewWatcher(httpapi.WatcherDeps{
		Gate:     captured,
		Settings: settingsStore,
		DB:       db,
		RunScan:  scanner,
		ScanLock: scanLock,
		Tasks:    tasks,
		Hub:      hub,
		Log:      log,
	})

	// --- MCP ----------------------------------------------------------------------------------
	mcpOpener := opts.McpOpener
	if mcpOpener == nil {
		mcpOpener = newRealMCPOpener()
	}
	mcpRunner := opts.McpRunner
	if mcpRunner == nil {
		mcpRunner = osopen.ExecRunner{}
	}
	mcpDeps := mcpserver.Deps{
		Config:      cfg,
		BaseDir:     baseDir,
		DB:          db,
		Categories:  categoriesStore,
		Relocalizer: relocalizer,
		FileLocator: mcpserver.RealFileLocator{Config: cfg},
		Registry:    mcpserver.JSONRegistrySyncer{DB: db, Path: cfg.JSONRegistryPath},
		Scanner:     scanner,
		ChatSearch:  mcpserver.ChatSearcher{Deps: aichatDeps},
		Opener:      mcpOpener,
		Runner:      mcpRunner,
		Decisions:   decisionsStore,
		Now:         timeNowOr(opts.Now),
	}

	return &application{
		settings:  settingsStore,
		log:       log,
		db:        db,
		scanLock:  scanLock,
		scanner:   scanner,
		stepper:   stepper,
		tasks:     tasks,
		handler:   handler,
		server:    captured,
		watcher:   watcher,
		mcpDeps:   mcpDeps,
		firstRun:  settingsStore.IsFirstRun(),
		publicDir: filepath.Join(baseDir, "public"),
	}, nil
}

// Close stops the auto-watcher and closes the SQLite handle. It is idempotent.
func (a *application) Close() {
	a.closeOnce.Do(func() {
		if a.watcher != nil {
			a.watcher.Stop()
		}
		if a.db != nil {
			_ = a.db.Close()
		}
	})
}

// logf writes a startup/lifecycle line to stderr, preserving the console.error destinations the TS
// startup path used.
func (a *application) logf(format string, args ...any) {
	fmt.Fprintf(os.Stdout, format+"\n", args...)
}

// serve runs the HTTP server plus the auto-watcher until ctx is cancelled, then stops the watcher.
// httpapi.Start owns the single-instance lock and the EADDRINUSE take-over; its deferred release
// runs when Start returns, so locks are freed on every exit path.
func (a *application) serve(ctx context.Context, addr string, startOptions httpapi.StartOptions) error {
	if startOptions.Handler == nil {
		startOptions.Handler = a.handler
	}
	if startOptions.DataDir == "" {
		startOptions.DataDir = a.settings.DataDir()
	}
	if startOptions.Logf == nil {
		startOptions.Logf = a.logf
	}
	if a.watcher != nil {
		a.watcher.Start(ctx)
	}
	err := httpapi.Start(ctx, addr, startOptions)
	if a.watcher != nil {
		a.watcher.Stop()
	}
	return err
}
