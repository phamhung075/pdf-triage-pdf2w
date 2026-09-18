// Document/action mutation routes ("httpapi part 2b"), ported from
// src/infrastructure/http/web-server.ts. This is the second half of the HTTP surface: every route
// that MUTATES documents, files or the taxonomy, plus the 10-second auto-watcher. Part 1
// (server.go) owns the read-only routes, the SSE hub and the startup path; this file adds the
// mutation half through the RouteGroup seam part 1 established, without editing it.
//
// Routes registered here (inventory 2026-09-18-go-backend-inventory.md §3 row numbers):
//
//	#9  POST   /api/registry/repair           registryRepairHandler
//	#24 POST   /api/subcategories/rename      renameSubcategoryHandler
//	#27 POST   /api/images/import             importImageHandler
//	#28 POST   /api/pdf/merge                 mergePDFHandler
//	#29 POST   /api/chat                      chatHandler
//	#33 POST   /api/pdf/split                 splitPDFHandler
//	#40 DELETE /api/documents/{id}            deleteDocumentHandler
//	#41 PUT    /api/documents/{id}            putDocumentHandler
//	#42 POST   /api/documents/{id}/relocalize relocalizeDocumentHandler
//	#43 DELETE /api/documents                 clearDocumentsHandler
//	#46 POST   /api/triage/scan               triageScanHandler
//
// # Wiring
//
// The routes are a RouteGroup: Deps.RouteGroups (or routeGroupHooks) calls
//
//	DocumentWriteRoutes(DocumentWriteDeps{...})
//
// once per NewServer. The group closes over its own collaborator set, so none of these types leak
// into part 1's Deps. The handlers are methods on *server because they must share the in-memory
// isAutoScanning / manualStopCooldownUntil / scanAbortRequested guard that triage.go's unlock route
// already owns (migration-design decision 7); the watcher shares the same guard through the
// ScanGate interface in watcher.go.
//
// # Golden Rules pinned by this group
//
//	1  scan scope: the watcher and the scan route only ever walk CONFIG.InputDir (__raws).
//	3  no-text block: the watcher's blocked-file mtime/size skip keeps a blocked PDF in __raws.
//	4  forbidden subcategory: PUT /api/documents/{id} rejects an explicit general/other/divers/year.
//	5  pre-move auto-create: PUT and rename call app/guards.EnsureCategoryAndSubcategoryExist
//	   BEFORE any physical move.
//	9  sequential scan: RunTriageScan owns the per-file stream; this layer only serializes runs.
//	10 SSE on every mutation: every handler broadcasts exactly the events web-server.ts did.
//	15 clear semantics: DELETE /api/documents moves archive files back to __raws (app/clear).
//	16 no synthetic DDD: this is a thin adapter over the ported application packages.
//	18 feedback teaches AI: PUT records a manual decision when the edit re-classifies, and
//	   POST relocalize forwards `reason` as previousError to the classifier.
//
// # The upstream TOCTOU red test, pinned and reconciled
//
// The upstream case `does not let a manual scan start while the tick is still inside its async
// blocked-file-check loop` is RED in the full suite at port time:
//
//	npx vitest run src/infrastructure/http/web-server.test.ts
//	  -> 53 tests, 2 failed; this case expects 409, gets 200
//	npx vitest run src/infrastructure/http/web-server.test.ts -t "TOCTOU"
//	  -> 1 passed
//
// It is green in isolation and red only with certain predecessors in the same file (e.g.
// "does nothing" + "skips files" + "does not overlap with an in-flight manual scan" + this case
// fails; any single predecessor + this case passes), so the red is order/isolation dependent.
// web-server.ts:1434 already assigns `isAutoScanning = true` before the first `await`, so the
// production handler ordering the test intends to pin is present; the 200 comes from the shared
// per-file mock being reached by a timer callback from an earlier app instance before the current
// app's tick runs, which is a test-isolation artifact rather than a reproducible production scan.
//
// The contradiction is recorded, not silently "fixed": the Go port does not depend on that
// ordering. It replaces the plain `if (isAutoScanning) { 409 }; isAutoScanning = true` with an
// atomic check-and-set (tryBeginScan / tryBeginScanRespectingCooldown, documents_write_scan.go),
// and TestWatcherAndManualScanAtMostOne hammers a tick and a manual scan concurrently under
// `go test -race` and asserts at most one scan ever runs. That is a strictly stronger guarantee
// than the upstream assertion, and it holds regardless of timer scheduling.
//
// # Reported TS-vs-Go gaps
//
//  1. Raw image import body size. #27 is `express.raw({limit:'64mb'})` upstream, but part 1's
//     jsonBodyMiddleware (server.go) reads EVERY request body once with a 100 kB cap before any
//     handler runs and answers 413 above it. The import handler itself enforces the 64 MB cap
//     (maxImportBytes) and is fully tested against it, but through the composed handler a real
//     upload larger than 100 kB is rejected by that middleware first. Fixing this needs part 1's
//     middleware to skip the raw route (the only edit this job is not allowed to make); the gap is
//     reported to the orchestrator rather than silently worked around. See importImageHandler.
//  2. triagescan.Result has no JSON tags. The frozen REST/SSE field names (scannedCount, ...) are
//     produced by scanResultMap instead of marshaling the struct, the same treatment part 1 gave
//     app/taskstate (server.go gap 3).
//  3. relocalize DeleteResult/ReclassifyResult have no JSON tags either; deleteResultMap /
//     reclassifyResultMap emit the TS object literals' keys and omit absent optional fields.
//  4. taxonomy.MergeSubcategoryInTaxonomy reads a narrow taxonomy.Category view; the store holds
//     the richer documentschema.CategoryItem. mergeSubcategoryInDocumentschemaConfig projects,
//     delegates to the one shared implementation, then writes the changed subcategory set back by
//     pointer identity, so description/name_fr/nested subcategories survive the round trip
//     (web-server.ts:668 called the domain function on the full objects).
package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/aichat"
	"github.com/phamhung075/pdf-triage-pdf2w/app/clear"
	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/app/repair"
	"github.com/phamhung075/pdf-triage-pdf2w/app/taskstate"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

// DocumentWriteDB is the narrow document-store surface these routes use. *store/database.Store
// satisfies it. GetBlockedFile serves the watcher's unchanged-blocked-file skip.
type DocumentWriteDB interface {
	GetDocumentByID(id int64) (*database.DocumentRecord, error)
	UpdateDocumentRecord(id any, updates database.DocumentUpdates) (bool, error)
	GetAllDocuments() ([]database.DocumentRecord, error)
	GetBlockedFile(originalPath string) (*database.BlockedFileRecord, error)
}

// ManualDecisionRecorder is the write half of the manual-decisions store. *manualdecisions.Store
// satisfies it. GET/PUT/DELETE list management stays in part 1's ManualDecisions interface.
type ManualDecisionRecorder interface {
	RecordManualDecision(record manualdecisions.Record)
}

// DocumentWriteRelocalizer is the app/relocalize seam. relocalize.Deps satisfies it.
type DocumentWriteRelocalizer interface {
	RelocalizeFileIfNeeded(filePath, category string, subcategory, dateStr, title *string) (relocalize.RelocalizeResult, error)
	ReclassifyAndRelocalizeDocument(id int64, explicitCategory, explicitSubcategory, userFeedbackReason *string) (relocalize.ReclassifyResult, error)
	DeleteDocumentAndMoveToTrash(id int64) (relocalize.DeleteResult, error)
}

// RepairRunner is the app/repair seam. repair.Deps satisfies it.
type RepairRunner interface {
	RepairRegistry(onProgress repair.ProgressFunc) (repair.Result, error)
}

// ClearRunner is the app/clear seam. clear.Deps satisfies it. The caller owns the cross-process
// scan lock (app/clear's package comment), which clearDocumentsHandler acquires.
type ClearRunner interface {
	ClearRegistryAndMoveArchiveToRaws(onProgress func(clear.Event)) (clear.ClearResult, error)
}

// ScanRunner is the app/triagescan seam. *triagescan.Scanner satisfies it. RunTriageScan does NOT
// take the scan lock; the caller does (triageScanHandler / Watcher.RunTick).
type ScanRunner interface {
	RunTriageScan(ctx context.Context, onProgress func(triagescan.Event), shouldAbort func() bool) (triagescan.Result, error)
}

// ScanLocker is the app/scanlock seam. *scanlock.Guard satisfies it.
type ScanLocker interface {
	Acquire() (release func(), err error)
}

// DocumentWriteTasks extends part 1's TaskState read surface with the mutation verbs. The same
// *taskstate.Manager must back both, or task events would not reach the SSE hub; DocumentWriteRoutes
// installs the hub broadcaster itself so a caller cannot forget.
type DocumentWriteTasks interface {
	StartTask(taskType taskstate.TaskType, totalFiles int, message string)
	UpdateTaskProgress(update taskstate.ProgressUpdate)
	FinishTask(result any, message ...string)
	FailTask(errorMessage string)
	ResetTaskState()
	SetBroadcaster(broadcaster taskstate.Broadcaster)
}

// DocumentWriteLog is the slice of *infra/logger.Logger these routes log through (IMPORT,
// PDF_UTIL, AUTO_WATCHER, DELETE modules). Nil is silent.
type DocumentWriteLog interface {
	Info(moduleName, message string, meta any, filename ...string)
	Warn(moduleName, message string, meta any, filename ...string)
	Error(moduleName, message string, meta any, filename ...string)
}

// DocumentWriteDeps is the injected collaborator set for the mutation route group plus the
// watcher. Every field is optional in tests; production wiring sets all of them.
type DocumentWriteDeps struct {
	Settings        Settings
	DB              DocumentWriteDB
	Categories      Categories
	ManualDecisions ManualDecisionRecorder
	Relocalizer     DocumentWriteRelocalizer
	Repair          RepairRunner
	Clear           ClearRunner
	Scan            ScanRunner
	// ScanLock is the cross-process lock route #46 and #43 acquire synchronously. Nil skips the
	// acquisition (tests, or a caller that deliberately does not want cross-process serialization).
	ScanLock ScanLocker
	// AIChat is app/aichat.Deps; its Store and Ollama fields must be set for a real answer.
	AIChat aichat.Deps
	// Registry is the JSON-mirror writer, relocalize.JSONRegistrySync in production.
	Registry relocalize.RegistrySyncer
	// Tasks is the task-state machine. When nil, the group falls back to Deps.Tasks if it satisfies
	// DocumentWriteTasks.
	Tasks DocumentWriteTasks
	Log   DocumentWriteLog
	// EnsureDirectories is ensureDirectoriesExist(), called by image import before writing. Nil
	// falls back to settings.EnsureDirectoriesExist(Settings.Config()).
	EnsureDirectories func() error
	// Now is the clock for chat and cooldown math. Nil is time.Now.
	Now func() time.Time
}

// DocumentWriteRoutes returns the RouteGroup that registers every mutation route on the server's
// mux. The returned closure installs the hub broadcaster on the task manager, then registers the
// handlers.
func DocumentWriteRoutes(d DocumentWriteDeps) RouteGroup {
	return func(s *Server) {
		if d.Tasks == nil {
			if tasks, ok := s.deps.Tasks.(DocumentWriteTasks); ok {
				d.Tasks = tasks
			}
		}
		if d.Tasks != nil {
			// The task manager must broadcast through THIS server's hub, exactly as part 1's
			// newServer installs it for its own task manager.
			d.Tasks.SetBroadcaster(func(evt taskstate.Event) {
				s.hub.Broadcast(taskEventJSON{Type: evt.Type, TaskState: toTaskStateJSON(evt.TaskState)})
			})
		}
		s.registerDocumentWrite(d)
	}
}

// registerDocumentWrite adds the mutation routes. Go 1.22 ServeMux matches by method+path, so the
// two DELETE /api/documents forms and the /{id}/relocalize subpath coexist without order tricks.
func (s *server) registerDocumentWrite(d DocumentWriteDeps) {
	s.mux.HandleFunc("POST /api/registry/repair", func(w http.ResponseWriter, r *http.Request) {
		s.registryRepairHandler(d, w, r)
	})
	s.mux.HandleFunc("POST /api/subcategories/rename", func(w http.ResponseWriter, r *http.Request) {
		s.renameSubcategoryHandler(d, w, r)
	})
	s.mux.HandleFunc("POST /api/images/import", func(w http.ResponseWriter, r *http.Request) {
		s.importImageHandler(d, w, r)
	})
	s.mux.HandleFunc("POST /api/pdf/merge", func(w http.ResponseWriter, r *http.Request) {
		s.mergePDFHandler(d, w, r)
	})
	s.mux.HandleFunc("POST /api/pdf/split", func(w http.ResponseWriter, r *http.Request) {
		s.splitPDFHandler(d, w, r)
	})
	s.mux.HandleFunc("POST /api/chat", func(w http.ResponseWriter, r *http.Request) {
		s.chatHandler(d, w, r)
	})
	s.mux.HandleFunc("DELETE /api/documents/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.deleteDocumentHandler(d, w, r)
	})
	s.mux.HandleFunc("PUT /api/documents/{id}", func(w http.ResponseWriter, r *http.Request) {
		s.putDocumentHandler(d, w, r)
	})
	s.mux.HandleFunc("POST /api/documents/{id}/relocalize", func(w http.ResponseWriter, r *http.Request) {
		s.relocalizeDocumentHandler(d, w, r)
	})
	s.mux.HandleFunc("DELETE /api/documents", func(w http.ResponseWriter, r *http.Request) {
		s.clearDocumentsHandler(d, w, r)
	})
	s.mux.HandleFunc("POST /api/triage/scan", func(w http.ResponseWriter, r *http.Request) {
		s.triageScanHandler(d, w, r)
	})
}

// nowOr returns d.Now() or time.Now when no clock was injected.
func (d DocumentWriteDeps) nowOr() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// config returns the settings snapshot, zero when no Settings was injected.
func (d DocumentWriteDeps) config() settings.Config {
	if d.Settings == nil {
		return settings.Config{}
	}
	return d.Settings.Config()
}

// ensureDirectories ports ensureDirectoriesExist(): create the managed folders before a write.
func (d DocumentWriteDeps) ensureDirectories() error {
	if d.EnsureDirectories != nil {
		return d.EnsureDirectories()
	}
	if d.Settings == nil {
		return nil
	}
	return settings.EnsureDirectoriesExist(d.Settings.Config())
}

// syncRegistry runs the JSON mirror when one is injected.
func (d DocumentWriteDeps) syncRegistry() error {
	if d.Registry == nil {
		return nil
	}
	return d.Registry.SyncJSONRegistry()
}

// logInfo / logWarn / logError are nil-safe.
func (d DocumentWriteDeps) logInfo(module, message string, meta any) {
	if d.Log != nil {
		d.Log.Info(module, message, meta)
	}
}

func (d DocumentWriteDeps) logWarn(module, message string, meta any) {
	if d.Log != nil {
		d.Log.Warn(module, message, meta)
	}
}

func (d DocumentWriteDeps) logError(module, message string, meta any) {
	if d.Log != nil {
		d.Log.Error(module, message, meta)
	}
}

// scanResultMap mirrors the TS runTriageScan return object's JSON keys. triagescan.Result has no
// json tags (gap 2), so the mapping is explicit. `items` is always present (the TS initializes
// `const items = []`); OllamaDown/Message are omitted unless set, matching the TS return literal.
func scanResultMap(result triagescan.Result) map[string]any {
	items := result.Items
	if items == nil {
		items = []triagescan.ResultItem{}
	}
	out := map[string]any{
		"scannedCount":   result.ScannedCount,
		"processedCount": result.ProcessedCount,
		"skippedCount":   result.SkippedCount,
		"items":          items,
	}
	if result.OllamaDown {
		out["ollamaDown"] = true
	}
	if result.Message != "" {
		out["message"] = result.Message
	}
	return out
}

// applyScanProgress ports the onProgress block shared by /api/triage/scan and the auto-watcher
// (web-server.ts:1491-1498, :1456-1463).
func applyScanProgress(tasks DocumentWriteTasks, evt triagescan.Event) {
	if tasks == nil {
		return
	}
	switch evt.Type {
	case triagescan.EventScanStarted:
		empty := ""
		stage := "SCANNING"
		message := evt.Message
		tasks.UpdateTaskProgress(taskstate.ProgressUpdate{
			ProcessedFiles: 0,
			TotalFiles:     evt.TotalFiles,
			CurrentFile:    &empty,
			Stage:          &stage,
			Message:        &message,
		})
	case triagescan.EventFileProgress, triagescan.EventFileCompleted, triagescan.EventFileFailed:
		count := 0
		if evt.ScannedCount != nil {
			count = *evt.ScannedCount
		} else if evt.ProcessedCount != nil {
			count = *evt.ProcessedCount
		}
		update := taskstate.ProgressUpdate{ProcessedFiles: count, TotalFiles: evt.TotalFiles}
		if evt.Filename != "" {
			filename := evt.Filename
			update.CurrentFile = &filename
		}
		if evt.Stage != "" {
			stage := string(evt.Stage)
			update.Stage = &stage
		}
		if evt.Message != "" {
			message := evt.Message
			update.Message = &message
		}
		tasks.UpdateTaskProgress(update)
	}
}

// documentWithTags ports the GET/PUT response document shape: the raw record with `tags` replaced
// by the parsed array (web-server.ts:1320 `{...updatedDoc, tags: safeParseJSON(...)}`).
func documentWithTags(doc *database.DocumentRecord) map[string]any {
	if doc == nil {
		return nil
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	out["tags"] = parseTagsArray(doc.Tags)
	return out
}

// parseTagsArray is safeParseJSON(str, []).
func parseTagsArray(raw string) []any {
	out := []any{}
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil || out == nil {
		return []any{}
	}
	return out
}

// reclassifyResultMap ports the reclassifyAndRelocalizeDocument return object; relocalize's Go
// struct carries no json tags (gap 3), so absent optional fields stay absent.
func reclassifyResultMap(result relocalize.ReclassifyResult) map[string]any {
	out := map[string]any{"success": result.Success}
	if result.StaleCleaned {
		out["staleCleaned"] = true
	}
	if result.Error != "" {
		out["error"] = result.Error
	}
	if result.Message != "" {
		out["message"] = result.Message
	}
	if result.Document != nil {
		out["document"] = result.Document
	}
	return out
}

// deleteResultMap ports the deleteDocumentAndMoveToTrash return object.
func deleteResultMap(result relocalize.DeleteResult) map[string]any {
	out := map[string]any{"success": result.Success}
	if result.Error != "" {
		out["error"] = result.Error
	}
	if result.Message != "" {
		out["message"] = result.Message
	}
	return out
}

// firstNonEmptyString is the Go form of the TS `a || b || fallback` chain used for the target
// category/subcategory/date: an empty string is falsy and falls through.
func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

// derefOr is `ptr ?? ""`; it makes the optional UpdateDocumentInput fields usable in
// firstNonEmptyString chains.
func derefOr(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
