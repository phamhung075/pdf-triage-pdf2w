// Scan/repair/clear mutation handlers and the shared in-memory scan gate, ported from
// web-server.ts:254-281 (#9 repair), :1345-1375 (#43 clear) and :1481-1514 (#46 scan).
//
// The three operations all serialize on the same two locks, in the same order:
//
//  1. the in-memory isAutoScanning guard, claimed ATOMICALLY by tryBeginScan. Part 1's unlock route
//     (triage.go) writes the same field under the same server.mu, so it cannot bypass the claim;
//  2. the cross-process app/scanlock (.scan.lock), for #46 and #43 acquired by THIS layer
//     synchronously, before any blocking work, and released when the run returns. #9 delegates its
//     file lock to app/repair's own Deps.Lock, which is the package's documented production mode.
//
// Ordering is the migration-design decision 7 invariant: claim the in-memory guard first so a
// concurrent manual scan/tick in the same process is rejected, then the file lock so another
// process cannot insert the same checksum. web-server.ts:1426-1434 documents the checksum-UNIQUE
// failure this prevents.
package httpapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/phamhung075/pdf-triage-pdf2w/app/clear"
	"github.com/phamhung075/pdf-triage-pdf2w/app/repair"
	"github.com/phamhung075/pdf-triage-pdf2w/app/scanlock"
	"github.com/phamhung075/pdf-triage-pdf2w/app/taskstate"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
)

// scanBusyMessage is the exact 409 body message the TS uses for every "already in progress" case
// (web-server.ts:257, :1348, :1484).
const scanBusyMessage = "A scan/repair/clear operation is already in progress. Try again shortly."

// tryBeginScan claims the shared in-memory scan guard ATOMICALLY: it checks and sets under one
// lock, so two concurrent callers can never both win. This is the fix for the TOCTOU window the
// upstream red test describes — a plain `if isAutoScanning { 409 }; isAutoScanning = true` has a
// gap between the read and the write. The manual routes use this form (no cooldown, exactly like
// the TS); the watcher uses tryBeginScanRespectingCooldown.
func (s *server) tryBeginScan() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isAutoScanning {
		return false
	}
	s.isAutoScanning = true
	return true
}

// tryBeginScanRespectingCooldown is tryBeginScan plus the watcher's 60-second manual-stop cooldown,
// checked and claimed under one lock so an unlock that lands between the two checks cannot be
// missed.
func (s *server) tryBeginScanRespectingCooldown(now int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.isAutoScanning {
		return false
	}
	if now < s.manualStopCooldownUntil {
		return false
	}
	s.isAutoScanning = true
	return true
}

// noopTasks is installed when no task manager is injected, so the handlers never call a method on
// a nil interface. It deliberately broadcasts nothing.
type noopTasks struct{}

func (noopTasks) StartTask(taskstate.TaskType, int, string)   {}
func (noopTasks) UpdateTaskProgress(taskstate.ProgressUpdate) {}
func (noopTasks) FinishTask(any, ...string)                   {}
func (noopTasks) FailTask(string)                             {}
func (noopTasks) ResetTaskState()                             {}
func (noopTasks) SetBroadcaster(taskstate.Broadcaster)        {}

// --- #9 POST /api/registry/repair ----------------------------------------------------------------

func (s *server) registryRepairHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	if !s.tryBeginScan() {
		writeError(w, http.StatusConflict, scanBusyMessage)
		return
	}
	defer s.endScan()

	tasks := d.Tasks
	if tasks == nil {
		tasks = noopTasks{}
	}
	tasks.StartTask(taskstate.TaskRepair, 0, "Initializing registry repair & relocalization...")

	if d.Repair == nil {
		tasks.FailTask("registry repair is not configured")
		writeError(w, http.StatusInternalServerError, "registry repair is not configured")
		return
	}

	result, err := d.Repair.RepairRegistry(func(event any) {
		applyRepairProgress(tasks, event)
		s.hub.Broadcast(event)
	})
	if err != nil {
		tasks.FailTask(err.Error())
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	// Ordering is load-bearing (web-server.ts:271-273): finishTask first, then REPAIR_COMPLETED,
	// then REGISTRY_UPDATED. repair.CompletedEvent builds the byte-identical `{type, ...result}`.
	tasks.FinishTask(result, fmt.Sprintf(
		"Registry repair completed. Repaired %d, updated %d, relocalized %d file(s).",
		result.RepairedCount, result.UpdatedCount, result.RelocalizedCount,
	))
	s.hub.Broadcast(repair.CompletedEvent(result))
	s.hub.Broadcast(map[string]any{"type": "REGISTRY_UPDATED", "action": "REPAIR"})

	writeJSON(w, http.StatusOK, struct {
		Message string `json:"message"`
		repair.Result
	}{Message: "Registry repair completed successfully", Result: result})
}

// applyRepairProgress ports the repair onProgress block (web-server.ts:263-270). repair emits typed
// structs through an `any` seam, so the type switch is the discriminator.
func applyRepairProgress(tasks DocumentWriteTasks, event any) {
	if tasks == nil {
		return
	}
	switch evt := event.(type) {
	case repair.RepairStartedEvent:
		empty := ""
		stage := "REPAIRING"
		message := evt.Message
		tasks.UpdateTaskProgress(taskstate.ProgressUpdate{
			ProcessedFiles: 0,
			TotalFiles:     &evt.TotalFiles,
			CurrentFile:    &empty,
			Stage:          &stage,
			Message:        &message,
		})
	case repair.FileProgressEvent:
		count := evt.ScannedCount
		if count == 0 {
			count = evt.ProcessedCount
		}
		update := taskstate.ProgressUpdate{ProcessedFiles: count}
		filename, stage, message := evt.Filename, evt.Stage, evt.Message
		update.CurrentFile = &filename
		update.Stage = &stage
		update.Message = &message
		tasks.UpdateTaskProgress(update)
	case repair.FileFailedEvent:
		count := 0
		filename, stage, message := evt.Filename, evt.Stage, evt.Message
		tasks.UpdateTaskProgress(taskstate.ProgressUpdate{
			ProcessedFiles: count,
			CurrentFile:    &filename,
			Stage:          &stage,
			Message:        &message,
		})
	}
}

// --- #43 DELETE /api/documents -------------------------------------------------------------------

func (s *server) clearDocumentsHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	if !s.tryBeginScan() {
		writeError(w, http.StatusConflict, scanBusyMessage)
		return
	}
	// acquireScanLockForRun releases the in-memory guard and reports false when the cross-process
	// lock is held, so the handler can answer 409 without leaving isAutoScanning stuck.
	releaseLock, ok := s.acquireScanLockForRun(d)
	if !ok {
		writeError(w, http.StatusConflict, scanBusyMessage)
		return
	}
	defer releaseLock()
	defer s.endScan()

	tasks := d.Tasks
	if tasks == nil {
		tasks = noopTasks{}
	}
	tasks.StartTask(taskstate.TaskClear, 0, "Initializing registry clear...")

	if d.Clear == nil {
		tasks.FailTask("registry clear is not configured")
		writeError(w, http.StatusInternalServerError, "registry clear is not configured")
		return
	}

	result, err := d.Clear.ClearRegistryAndMoveArchiveToRaws(func(evt clear.Event) {
		applyClearProgress(tasks, evt)
		s.hub.Broadcast(evt)
	})
	if err != nil {
		tasks.FailTask(err.Error())
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	tasks.FinishTask(result, fmt.Sprintf("Registry cleared. Moved %d PDF file(s) back to __raws.", result.CountMoved))
	s.hub.Broadcast(map[string]any{"type": "REGISTRY_UPDATED", "action": "CLEAR"})
	s.hub.Broadcast(map[string]any{"type": "CATEGORIES_UPDATED"})

	writeJSON(w, http.StatusOK, struct {
		Message string `json:"message"`
		clear.ClearResult
	}{
		Message:     fmt.Sprintf("Registry cleared successfully. Moved %d PDF file(s) from __archive back to __raws.", result.CountMoved),
		ClearResult: result,
	})
}

// applyClearProgress ports the clear onProgress block (web-server.ts:1355-1360).
func applyClearProgress(tasks DocumentWriteTasks, evt clear.Event) {
	if tasks == nil {
		return
	}
	switch evt.Type {
	case clear.EventClearStarted:
		empty := ""
		stage := clear.StageClearing
		message := evt.Message
		total := evt.TotalFiles
		tasks.UpdateTaskProgress(taskstate.ProgressUpdate{
			ProcessedFiles: 0,
			TotalFiles:     &total,
			CurrentFile:    &empty,
			Stage:          &stage,
			Message:        &message,
		})
	case clear.EventFileProgress:
		count := 0
		if evt.ScannedCount != nil {
			count = *evt.ScannedCount
		} else if evt.ProcessedCount != nil {
			count = *evt.ProcessedCount
		}
		total := evt.TotalFiles
		update := taskstate.ProgressUpdate{ProcessedFiles: count, TotalFiles: &total}
		if evt.Filename != nil {
			update.CurrentFile = evt.Filename
		}
		if evt.Stage != nil {
			update.Stage = evt.Stage
		}
		message := evt.Message
		update.Message = &message
		tasks.UpdateTaskProgress(update)
	}
}

// --- #46 POST /api/triage/scan -------------------------------------------------------------------

// triageScanHandler ports web-server.ts:1482-1514. The in-memory guard and the cross-process lock
// are both claimed synchronously, before RunTriageScan performs any blocking work; RunTriageScan
// then runs on the net/http per-request goroutine and its result is the response body, exactly as
// the TS route awaited and returned it. The abort flag (set by the unlock route on another request
// goroutine) is polled per file, which is what lets `Stop` interrupt the inline run.
func (s *server) triageScanHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	if !s.tryBeginScan() {
		writeError(w, http.StatusConflict, scanBusyMessage)
		return
	}
	releaseLock, ok := s.acquireScanLockForRun(d)
	if !ok {
		writeError(w, http.StatusConflict, scanBusyMessage)
		return
	}
	defer releaseLock()
	defer s.endScan()

	// A new scan clears the abort flag the unlock route may have left set.
	s.setAbort(false)

	tasks := d.Tasks
	if tasks == nil {
		tasks = noopTasks{}
	}
	tasks.StartTask(taskstate.TaskScan, 0, "Initializing triage scan...")

	if d.Scan == nil {
		tasks.FailTask("triage scan is not configured")
		writeError(w, http.StatusInternalServerError, "triage scan is not configured")
		return
	}

	result, err := d.Scan.RunTriageScan(r.Context(), func(evt triagescan.Event) {
		applyScanProgress(tasks, evt)
		s.hub.Broadcast(evt)
	}, s.abortRequested)
	if err != nil {
		tasks.FailTask(err.Error())
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if result.OllamaDown {
		message := result.Message
		if message == "" {
			message = "Ollama is down — start Ollama and re-scan."
		}
		tasks.FailTask(message)
		body := scanResultMap(result)
		body["message"] = message
		writeJSON(w, http.StatusOK, body)
		return
	}

	tasks.FinishTask(scanResultMap(result), fmt.Sprintf("Triage scan completed. Processed %d file(s).", result.ProcessedCount))
	body := scanResultMap(result)
	body["message"] = "Triage scan completed"
	writeJSON(w, http.StatusOK, body)
}

// acquireScanLockForRun claims the cross-process scan lock for a synchronous route run. When the
// lock is already held it releases the in-memory guard it would otherwise leak and returns
// (nil, false). A nil ScanLock is a deliberate no-op for tests and single-process callers.
func (s *server) acquireScanLockForRun(d DocumentWriteDeps) (func(), bool) {
	if d.ScanLock == nil {
		return func() {}, true
	}
	release, err := d.ScanLock.Acquire()
	if err != nil {
		s.endScan()
		var inProgress *scanlock.ScanInProgressError
		if errors.As(err, &inProgress) {
			d.logWarn("SCAN_LOCK", err.Error(), nil)
		}
		return nil, false
	}
	return release, true
}
