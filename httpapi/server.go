// Package httpapi is a Go port of pdf-triage's REST/SSE HTTP surface, whose behavioral source of
// truth is src/infrastructure/http/web-server.ts (1,595 lines). This is "part 1": it covers every
// route that does NOT mutate documents, the shared SSE broadcast hub, the static public/ mount and
// the single-instance/port-takeover startup path. The document, chat, merge/split, import, export,
// scan, repair, clear and MCP-status routes are owned by a later job and are deliberately left
// unregistered; each is added by calling one more register* function from NewServer.
//
// Ported routes (inventory 2026-09-18-go-backend-inventory.md §3 row numbers):
//
//	#1  GET    /api/dev/livereload          liveReloadHandlers
//	#2  POST   /api/open-location           openLocationHandlers
//	#3  POST   /api/open-chrome             openChromeHandlers
//	#4  GET    /api/ollama/status           ollamaStatusHandler
//	#5  GET    /api/ollama/models           ollamaModelsHandler
//	#6  POST   /api/ollama/start            ollamaStartHandler
//	#7  POST   /api/server/restart          serverRestartHandler
//	#8  GET    /api/triage/status           triageStatusHandler
//	#10 GET    /api/config/setup-state      setupStateHandler
//	#11 GET    /api/config                  getConfigHandler
//	#12 GET    /api/system/stats            systemStatsHandler
//	#13 PUT    /api/config                  putConfigHandler
//	#14 GET    /api/logs/recent             logsRecentHandler
//	#15 GET    /api/logs/sessions           logsSessionsHandler
//	#16 GET    /api/logs/stream             logsStreamHandler
//	#17 GET    /api/categories              getCategoriesHandler
//	#18 GET    /api/blocked-files           blockedFilesHandler
//	#19 GET    /api/manual-decisions        manualDecisionsHandlers (list)
//	#20 PUT    /api/manual-decisions/:id    manualDecisionsHandlers (update)
//	#21 DELETE /api/manual-decisions/:id    manualDecisionsHandlers (delete one)
//	#22 DELETE /api/manual-decisions        manualDecisionsHandlers (clear all)
//	#23 PUT    /api/categories              putCategoriesHandler
//	#44 GET    /api/triage/events           triageEventsHandler
//	#45 POST   /api/triage/unlock           triageUnlockHandler
//
// Server-level behavior ported from the same file:
//
//   - express.json() becomes jsonBodyMiddleware (100 kB cap, expressed body decoded per handler).
//   - express.static(publicDir, { Cache-Control: no-store }) becomes the "/" handler registered
//     last, with the same no-store header and the same "only when public/ exists" condition.
//   - NO CORS and NO auth, exactly as web-server.ts:56-59 explains; the port adds neither.
//   - DATA_DIR/.server.lock single-instance lock and EADDRINUSE takeover live in start.go.
//   - resolveManagedPath (web-server.ts:38-45) is delegated to app/guards.ResolveManagedPath, the
//     one shared implementation (design decision 6 of the migration spec); resolveManagedPath below
//     is the thin server-side wrapper every path-taking route in a later job calls.
//
// # TS-vs-Go gaps resolved to match TS, and reported interface gaps
//
//  1. Express matches routes in registration order; Go 1.22 ServeMux matches by specificity. Every
//     route in this job has a distinct method+path, so specificity and registration order agree.
//     The one place the TS file documents order dependence (/api/documents/export/markdown before
//     /api/documents/:id/markdown, web-server.ts:1181-1184) is a documents route owned by the later
//     job.
//  2. Express returns 404 for a known path with an unregistered method; Go's ServeMux returns 405
//     with an Allow header when another method is registered for the same pattern. The port keeps
//     Go's 405 (a strictly more informative response) and documents it; no ported test depends on
//     the 404.
//  3. The task-state singleton in task-state.ts has camelCase JSON keys; app/taskstate's
//     ActiveTaskState has no `json` tags. httpapi therefore serializes task state through
//     taskStateJSON instead of marshaling the package struct, keeping the frozen SSE/JSON keys.
//  4. infra/logger.LogEntry and DocumentLogSession likewise carry no `json` tags; logs.go maps them
//     to logEntryJSON / logSessionJSON so the response keys stay camelCase and optional fields
//     (filename?, meta?, category?, subcategory?, decisionReason?) stay omittable, exactly as
//     JSON.stringify drops `undefined`.
//  5. infra/ollama.Client exposes CheckModelCanGenerate but not its internal list(); the OllamaClient
//     interface below therefore declares ListModels(host). REPORTED GAP: infra/ollama needs an
//     exported List/ListModels to wire the real client.
//  6. app/guards.ResolveManagedPath takes inputDir/outputDir explicitly; the server wrapper passes
//     CONFIG.INPUT_DIR / CONFIG.OUTPUT_ROOT_DIR, matching web-server.ts:41.
//  7. Zod's validation-error body is a pretty-printed JSON issue array; documentschema returns plain
//     Go errors with the same human message. The port keeps the `{ "error": <string> }` shape and the
//     400 status; the golden test compares structure and the human message, not the Zod framing.
//
// # Upstream tests
//
// `npx vitest run src/infrastructure/http/web-server.test.ts src/infrastructure/http/task-state.test.ts`
// at port time: web-server.test.ts -> 53 tests, 51 passed, 2 failed; task-state.test.ts -> 3 passed.
// The two red cases and their disposition:
//
//   - "does not let a manual scan start while the tick is still inside its async blocked-file-check
//     loop (TOCTOU ...)": the in-memory auto-watcher. The watcher tick is explicitly owned by the
//     later job (it is not one of this job's routes), so the case is not ported here; the guard
//     ordering it protects is preserved by beginScan(), which claims isAutoScanning synchronously.
//   - "returns 404 if file path by path query does not exist": route #35, also a later job. The
//     behavior is pinned at its ACTUAL verdict (403) by app/guards.ResolveManagedPath and its test,
//     and this package's resolveManagedPath test asserts the same 403.
//
// Every applicable case from web-server.test.ts is ported in routes_test.go; the task-state cases
// are already ported in app/taskstate and are re-asserted at the HTTP layer (#8 / #45).
package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/guards"
	"github.com/phamhung075/pdf-triage-pdf2w/app/taskstate"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

// maxJSONBody mirrors express.json()'s default 100 kB limit. A body over it is rejected before any
// handler runs, exactly as the Express middleware rejected it.
const maxJSONBody = 100 * 1024

// Settings is the narrow config/dir surface the routes consume. *settings.Store satisfies it.
type Settings interface {
	Config() settings.Config
	IsFirstRun() bool
	DataDir() string
	BaseDir() string
	UpdateConfig(patch settings.UpdateSettings) error
}

// Database is the narrow SQLite surface this job's routes use. *database.Store satisfies it.
type Database interface {
	GetAllBlockedFiles() ([]database.BlockedFileRecord, error)
	GetCategorySubcategoryStats() (database.CategoryStats, error)
}

// Categories is the taxonomy store surface. *categories.Store satisfies it.
type Categories interface {
	GetCategoriesConfig() documentschema.CategoriesConfig
	SaveCategoriesConfig(categories []*documentschema.CategoryItem) error
	SetOnCategoryCreatedCallback(cb func())
}

// ManualDecisions is the human-decisions store surface. *manualdecisions.Store satisfies it.
type ManualDecisions interface {
	GetManualDecisions() []manualdecisions.Record
	UpdateManualDecision(id int64, patch manualdecisions.Patch) (*manualdecisions.Record, error)
	DeleteManualDecision(id int64) (bool, error)
	ClearManualDecisions() error
}

// Logs is the logger surface the log routes and the injectable seam use. *logger.Logger satisfies it.
type Logs interface {
	RecentLogs(limit int) []logger.LogEntry
	GroupedSessionLogs() []logger.DocumentLogSession
	Subscribe(fn func(logger.LogEntry)) func()
}

// TaskState is the task-state surface. *taskstate.Manager satisfies it.
type TaskState interface {
	State() taskstate.ActiveTaskState
	SetBroadcaster(broadcaster taskstate.Broadcaster)
	ResetTaskState()
}

// OllamaClient is the Ollama surface the status/models routes need. The real *ollama.Client
// satisfies CheckModelCanGenerate but NOT ListModels (its list() is unexported); see the package
// comment, gap 5.
type OllamaClient interface {
	ListModels(host string) ([]string, error)
	CheckModelCanGenerate(model string, forceRefresh bool) ollama.ModelHealth
}

// Launch is the { cmd, args } shape os-open.ts returns and the two open routes spawn. It is defined
// here (rather than importing infra/osopen, which is being ported concurrently) so httpapi never
// depends on that package's in-progress API.
type Launch struct {
	Cmd  string
	Args []string
}

// Opener is the injected launcher. The real infra/osopen port is adapted to it at wiring time; tests
// inject a fake so no Explorer/Chrome process is ever launched.
type Opener interface {
	OpenDirectory(path string) (Launch, bool)
	RevealInFileManager(path string) (Launch, bool)
	OpenInChrome(path string) (Launch, bool)
}

// Spawner launches a resolved command. The default is processSpawner (os/exec, detached); tests
// inject a recorder.
type Spawner interface {
	Spawn(cmd string, args []string)
}

// LiveReloadWatcher is the polling mtime watcher behind GET /api/dev/livereload. The default is
// NewPollingWatcher; tests inject a fake emitting on demand.
type LiveReloadWatcher interface {
	Subscribe() (<-chan struct{}, func())
}

// Deps is the injected collaborator set. No package globals are consulted: every route reaches its
// collaborators through here, and NewServer installs the one broadcaster callback the frozen TS
// contract requires.
type Deps struct {
	Settings        Settings
	DB              Database
	Categories      Categories
	ManualDecisions ManualDecisions
	Logs            Logs
	Tasks           TaskState
	Ollama          OllamaClient
	Opener          Opener
	Spawner         Spawner
	// StartOllama launches `ollama serve` for POST /api/ollama/start. It is fire-and-forget, exactly
	// like the TS exec() whose callback only logs a warning.
	StartOllama func() error
	// Exit is process.exit for POST /api/server/restart. Tests inject a recorder; the response is
	// written before Exit is called.
	Exit func(int)
	// LiveReload overrides the default polling watcher.
	LiveReload LiveReloadWatcher
	// PublicDir is BASE_DIR/public. Static serving and the livereload watcher are only registered
	// when it exists, matching web-server.ts:71 and :108.
	PublicDir string
	// RestartDelay is how long POST /api/server/restart waits before calling Exit, matching the TS
	// setTimeout(..., 500). Tests set it to a small value; 0 means the 500 ms default.
	RestartDelay time.Duration
	// RouteGroups are the per-server route-group registrations. Each is called once per NewServer
	// with the server under construction, so it can register its routes on s.mux. A group closes
	// over its own dependency struct, keeping that group's collaborators out of Deps; nil entries
	// are skipped.
	RouteGroups []RouteGroup
}

// server carries the mutable route guards that web-server.ts held as closure variables:
// isAutoScanning (web-server.ts:84), manualStopCooldownUntil (:86) and scanAbortRequested (:1397).
type server struct {
	deps    Deps
	hub     *Hub
	handler http.Handler
	// mux is the route table. routeGroupHooks receive the server after the built-in register*
	// calls and add their routes here.
	mux *http.ServeMux
	// isAutoScanning is the shared guard read/written by POST /api/triage/unlock here and by the
	// later job's scan/repair/clear routes. It is atomic because Go handlers are concurrent.
	isAutoScanning bool
	// manualStopCooldownUntil is a Unix-millis deadline; 0 means "no cooldown".
	manualStopCooldownUntil int64
	// scanAbortRequested is polled per file by runTriageScan (ported in the later job).
	scanAbortRequested bool
	// mu guards the three flags above.
	mu sync.RWMutex
}

// NewServer builds the http.Handler for every route this job owns. Collaborators come from deps;
// route groups are registered one function each so the later job extends this by adding calls.
func NewServer(deps Deps) http.Handler {
	return newServer(deps).handler
}

// newServer builds the server plus its handler. Tests use it to reach the in-memory route guards.
func newServer(deps Deps) *server {
	s := &server{deps: deps, hub: NewHub()}

	if deps.Tasks != nil {
		deps.Tasks.SetBroadcaster(func(evt taskstate.Event) {
			s.hub.Broadcast(taskEventJSON{Type: evt.Type, TaskState: toTaskStateJSON(evt.TaskState)})
		})
	}
	if deps.Categories != nil {
		deps.Categories.SetOnCategoryCreatedCallback(func() {
			s.hub.Broadcast(map[string]any{"type": "CATEGORIES_UPDATED"})
		})
	}

	mux := http.NewServeMux()
	s.mux = mux
	s.registerLiveReload(mux)
	s.registerOpenLocation(mux)
	s.registerOpenChrome(mux)
	s.registerOllama(mux)
	s.registerSystem(mux)
	s.registerLogs(mux)
	s.registerCategories(mux)
	s.registerBlockedFiles(mux)
	s.registerManualDecisions(mux)
	s.registerTriage(mux)

	// Route-group hooks register after the built-in groups, in slice order, so a new route-group
	// file adds itself from its init function without editing this function.
	for _, hook := range routeGroupHooks {
		hook(s)
	}

	// Route groups injected through Deps register after the hooks, one call each, so a caller can
	// add routes with their own collaborators without editing Deps. Nil entries are skipped.
	for _, group := range deps.RouteGroups {
		if group != nil {
			group(s)
		}
	}

	// Static public/ is mounted last and only when the directory exists, exactly like
	// web-server.ts:71-80. "/" is the least specific pattern, so it never shadows an API route.
	if deps.PublicDir != "" {
		if info, err := os.Stat(deps.PublicDir); err == nil && info.IsDir() {
			mux.Handle("/", noStore(staticHandler(deps.PublicDir)))
		}
	}

	s.handler = jsonBodyMiddleware(mux)
	return s
}

// staticHandler serves dir like express.static(dir): "/" maps to index.html and a request for
// "/index.html" returns the file rather than Go's http.FileServer 301 redirect to "./".
func staticHandler(dir string) http.Handler {
	root := http.Dir(dir)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upath := r.URL.Path
		if !strings.HasPrefix(upath, "/") {
			upath = "/" + upath
		}
		name := strings.TrimPrefix(path.Clean(upath), "/")
		if name == "" || name == "." {
			name = "index.html"
		}
		file, err := root.Open(name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer file.Close()
		info, err := file.Stat()
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		http.ServeContent(w, r, name, info.ModTime(), file)
	})
}

// noStore sets Cache-Control: no-store on every static response. The TS WHY comment, preserved
// verbatim (web-server.ts:72-76):
//
// This is a local dev tool, not a CDN-scale site — never let the browser cache
// app.js/style.css/index.html on disk. The static ?v=18 query param in index.html
// relied on someone remembering to bump it on every edit (nobody did, twice, in one
// session), so a stale tab could silently keep running pre-fix JS after a server
// restart picked up new code. no-store removes the failure mode outright.
func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		next.ServeHTTP(w, r)
	})
}

// bodyCtxKey stores the raw request body read by jsonBodyMiddleware.
type bodyCtxKey struct{}

// jsonBodyMiddleware is express.json(): it reads the JSON body once (up to maxJSONBody) so handlers
// can parse it with decodeBody or documentschema.Parse*. Malformed JSON is rejected with the same
// 400 class the Express parser produced.
func jsonBodyMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			data, err := io.ReadAll(io.LimitReader(r.Body, maxJSONBody+1))
			if err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
			if len(data) > maxJSONBody {
				writeError(w, http.StatusRequestEntityTooLarge, "request entity too large")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), bodyCtxKey{}, data))
		}
		next.ServeHTTP(w, r)
	})
}

// bodyBytes returns the raw body captured by jsonBodyMiddleware. An absent/empty body yields nil,
// which documentschema parsers treat as an empty object.
func bodyBytes(r *http.Request) []byte {
	raw, _ := r.Context().Value(bodyCtxKey{}).([]byte)
	return raw
}

// decodeBody unmarshals the captured body into v. An empty body leaves v untouched (Express's
// express.json() sets req.body = {} and the routes' `req.body || {}` then reads defaults).
func decodeBody(r *http.Request, v any) error {
	raw := bodyBytes(r)
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	return dec.Decode(v)
}

// writeJSON serializes v exactly as Express res.json does: no HTML escaping and no trailing
// newline, with Content-Type application/json; charset=utf-8.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := marshalNoHTMLEscape(v)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"failed to serialize response"}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError is the `res.status(code).json({ error: message })` shape every catch block uses.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

// marshalNoHTMLEscape is JSON.stringify: encoding/json escapes <, > and & by default, Express does
// not. Trailing newline added by Encoder is trimmed so the bytes match res.json.
func marshalNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// resolveManagedPath wraps app/guards.ResolveManagedPath with the server's managed roots. The TS
// WHY comment lives with the guard implementation (app/guards/path.go), which this package calls
// rather than reimplements (migration design §6).
func (s *server) resolveManagedPath(candidate any) (string, *guards.GuardViolation) {
	text, ok := candidate.(string)
	if !ok {
		return "", guards.PathOutsideManagedDirectoriesViolation()
	}
	cfg := s.deps.Settings.Config()
	return guards.ResolveManagedPath(text, cfg.InputDir, cfg.OutputRootDir)
}

// taskStateJSON mirrors task-state.ts's ActiveTaskState JSON keys. app/taskstate's struct has no
// `json` tags, so the mapping is explicit (package comment, gap 3).
type taskStateJSON struct {
	IsRunning      bool               `json:"isRunning"`
	Type           taskstate.TaskType `json:"type"`
	TotalFiles     int                `json:"totalFiles"`
	ProcessedFiles int                `json:"processedFiles"`
	Percent        int                `json:"percent"`
	CurrentFile    string             `json:"currentFile"`
	Stage          string             `json:"stage"`
	Message        string             `json:"message"`
	StartedAt      *string            `json:"startedAt"`
	Result         any                `json:"result"`
	Error          *string            `json:"error"`
}

// taskEventJSON is `{ type, taskState }`, the payload TS broadcast on every TASK_* event.
type taskEventJSON struct {
	Type      string        `json:"type"`
	TaskState taskStateJSON `json:"taskState"`
}

func toTaskStateJSON(st taskstate.ActiveTaskState) taskStateJSON {
	return taskStateJSON{
		IsRunning:      st.IsRunning,
		Type:           st.Type,
		TotalFiles:     st.TotalFiles,
		ProcessedFiles: st.ProcessedFiles,
		Percent:        st.Percent,
		CurrentFile:    st.CurrentFile,
		Stage:          st.Stage,
		Message:        st.Message,
		StartedAt:      st.StartedAt,
		Result:         st.Result,
		Error:          st.Error,
	}
}
