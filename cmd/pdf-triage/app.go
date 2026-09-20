package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	"github.com/phamhung075/pdf-triage-pdf2w/infra/aiprovider"
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
	// EnsurePdf2w overrides the auto-spawn of pdf2md-server when unreachable.
	EnsurePdf2w func(url string) error
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
	aiManager *aiprovider.Manager
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
		dir := explicit
		for {
			if hasPublicDir(dir) {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if v := os.Getenv("PDF_TRIAGE_BASE_DIR"); v != "" {
		dir := v
		for {
			if hasPublicDir(dir) {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	dir := wd
	for {
		if hasPublicDir(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return wd
}

func hasPublicDir(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "public"))
	return err == nil && info.IsDir()
}

// aiConfigFrom is the single mapping from the settings-layer Config to the aiprovider.Config. It is
// used at boot, on every scan reload and on every successful PUT /api/config, so a newly selected
// AI Engine reaches the shared Manager without duplication.
func aiConfigFrom(c settings.Config) aiprovider.Config {
	return aiprovider.Config{
		AIProvider:       c.AIProvider,
		CloudProvider:    c.CloudProvider,
		OllamaHost:       c.OllamaHost,
		OllamaModel:      c.OllamaModel,
		GoogleAPIKey:     c.GoogleAPIKey,
		GoogleModel:      c.GoogleModel,
		GoogleBaseURL:    c.GoogleBaseURL,
		AnthropicAPIKey:  c.AnthropicAPIKey,
		AnthropicModel:   c.AnthropicModel,
		AnthropicBaseURL: c.AnthropicBaseURL,
		DeepSeekAPIKey:   c.DeepSeekAPIKey,
		DeepSeekModel:    c.DeepSeekModel,
		DeepSeekBaseURL:  c.DeepSeekBaseURL,
		OpenAIAPIKey:     c.OpenAIAPIKey,
		OpenAIModel:      c.OpenAIModel,
		OpenAIBaseURL:    c.OpenAIBaseURL,
	}
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

	if cfg.PDF2WServiceURL == "" {
		cfg.PDF2WServiceURL = "http://127.0.0.1:3984"
	}
	if opts.EnsurePdf2w != nil {
		_ = opts.EnsurePdf2w(cfg.PDF2WServiceURL)
	} else if opts.DataDir == "" && opts.Spawner == nil {
		ensurePdf2wService(cfg.PDF2WServiceURL, baseDir, log)
	}

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
	aiManager := aiprovider.NewManager(aiConfigFrom(cfg), ollamaClient)

	visionClient := vision.NewClient(cfg.OllamaHost, cfg.OllamaVisionModel)
	stepper := imagetopdf.NewStepper(imagetopdf.DefaultDeps(visionClient, log))
	pdfExtractor := relocalize.NewPDFExtractor(cfg, log)
	registrySync := relocalize.JSONRegistrySync{DB: db, Path: cfg.JSONRegistryPath}

	cloudModelFor := func(c settings.Config) string {
		canonical, ok := aiprovider.NormalizeCloud(c.CloudProvider)
		if !ok {
			if strings.TrimSpace(c.CloudProvider) != "" {
				return ""
			}
			canonical = "google"
		}
		switch canonical {
		case "deepseek":
			if c.DeepSeekModel != "" {
				return c.DeepSeekModel
			}
			return "deepseek-flash"
		case "google":
			if c.GoogleModel != "" {
				return c.GoogleModel
			}
			return "gemini-3.8-flash"
		case "claude":
			if c.AnthropicModel != "" {
				return c.AnthropicModel
			}
			return "claude-3-7-sonnet-20250219"
		case "openai":
			if c.OpenAIModel != "" {
				return c.OpenAIModel
			}
			return "gpt-4o-mini"
		}
		return ""
	}

	// --- app layer ----------------------------------------------------------------------------
	classifyDeps := classify.Deps{
		Config: classify.Config{
			OllamaModel:          cfg.OllamaModel,
			OllamaHost:           cfg.OllamaHost,
			Language:             cfg.Language,
			PersonalNameDenylist: cfg.PersonalNameDenylist,
			AIProvider:           cfg.AIProvider,
			CloudProvider:        cfg.CloudProvider,
			CloudModel:           cloudModelFor(cfg),
			GetConfig: func() classify.Config {
				c := settingsStore.Config()
				return classify.Config{
					OllamaModel:          c.OllamaModel,
					OllamaHost:           c.OllamaHost,
					Language:             c.Language,
					PersonalNameDenylist: c.PersonalNameDenylist,
					AIProvider:           c.AIProvider,
					CloudProvider:        c.CloudProvider,
					CloudModel:           cloudModelFor(c),
				}
			},
		},
		Ollama:                aiManager,
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
		Ollama: aiManager,
		PlanQuery: func(userMessage string, now time.Time) (chatquery.StructuredQuery, error) {
			return chatplanner.PlanQuery(chatplanner.Deps{
				Ollama:     aiManager,
				Categories: categoriesStore.GetCategoriesConfig,
				Log:        log,
			}, userMessage, now), nil
		},
		Log: log,
	}

	scanner := triagescan.New(triagescan.Deps{
		Config: cfg,
		ReloadConfig: func() settings.Config {
			settingsStore.ReloadFromDisk()
			newCfg := settingsStore.Config()
			if newCfg.PDF2WServiceURL == "" {
				newCfg.PDF2WServiceURL = "http://127.0.0.1:3984"
			}
			aiManager.UpdateConfig(aiConfigFrom(newCfg))
			return newCfg
		},
		EnsureDirectories: settingsStore.EnsureDirectoriesExist,
		DB:                db,
		Extractor:         pdfExtractor,
		Converter:         converter,
		Classifier:        classifyDeps,
		Ollama:            aiManager,
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
		Ollama:          aiManager,
		Opener:          newOSLauncher(),
		Spawner:         spawner,
		StartOllama:     startOllamaServe,
		Exit:            os.Exit,
		PublicDir:       filepath.Join(baseDir, "public"),
		// Saving Settings must retarget the shared Manager immediately: chat, /api/ollama/status and
		// classification all read it, and the scan reload is too late for a Save-then-chat flow.
		OnConfigChanged: func(c settings.Config) {
			aiManager.UpdateConfig(aiConfigFrom(c))
		},
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
		aiManager: aiManager,
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

func isLocalURL(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "0.0.0.0"
}

func ensurePdf2wService(serviceURL, baseDir string, log *logger.Logger) {
	if serviceURL == "" || !isLocalURL(serviceURL) {
		return
	}

	client := &http.Client{Timeout: 300 * time.Millisecond}
	healthURL := strings.TrimRight(serviceURL, "/") + "/health"
	resp, err := client.Get(healthURL)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return
		}
	}

	candidates := []string{
		os.Getenv("PDF2MD_SERVER_BIN"),
		filepath.Join(baseDir, "..", "markdown-extract-service", "public", "server", "bin", "pdf2md-server"),
		filepath.Join(baseDir, "bin", "pdf2md-server"),
	}

	var binPath string
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			binPath = c
			break
		}
	}

	if binPath == "" {
		if lp, err := exec.LookPath("pdf2md-server"); err == nil {
			binPath = lp
		}
	}

	if binPath == "" {
		if log != nil {
			log.Warn("PDF2W", "pdf2w extraction service is unreachable and pdf2md-server binary not found", nil)
		}
		return
	}

	port := "3984"
	if parsed, err := url.Parse(serviceURL); err == nil && parsed.Port() != "" {
		port = parsed.Port()
	}

	cmd := exec.Command(binPath)
	cmd.Env = append(os.Environ(), "PORT="+port)
	if err := cmd.Start(); err != nil {
		if log != nil {
			log.Warn("PDF2W", fmt.Sprintf("Failed to spawn %s: %v", binPath, err), nil)
		}
		return
	}
	go func() { _ = cmd.Wait() }()

	if log != nil {
		log.Info("PDF2W", fmt.Sprintf("Spawned %s on port %s", binPath, port), nil)
	}

	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		resp, err := client.Get(healthURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				if log != nil {
					log.Info("PDF2W", "pdf2w extraction service is ready", nil)
				}
				return
			}
		}
	}
}
