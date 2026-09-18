// Triage SSE stream and unlock, ported from web-server.ts:1377-1419.
//
// GET  /api/triage/events  is the frozen triage event stream (`data: {event}\n\n`); every TASK_*,
// SCAN_*, FILE_*, OLLAMA_DOWN, REPAIR_*, CLEAR_*, CATEGORIES_UPDATED, REGISTRY_UPDATED,
// DECISIONS_UPDATED and DOCUMENTS_UPDATED event is fanned out through the Hub.
// POST /api/triage/unlock sets the scan-abort flag (polled per file by the scan loop), drops the
// in-memory scan guard, starts the 60-second manual-stop cooldown, resets task state, and broadcasts
// REGISTRY_UPDATED/UNLOCK.
package httpapi

import (
	"net/http"
	"time"
)

func (s *server) registerTriage(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/triage/events", s.triageEventsHandler)
	mux.HandleFunc("POST /api/triage/unlock", s.triageUnlockHandler)
}

func (s *server) triageEventsHandler(w http.ResponseWriter, r *http.Request) {
	setSSEHeaders(w)
	flush(w)

	frames, cancel := s.hub.Subscribe()
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return
		case frame := <-frames:
			if _, err := w.Write(frame); err != nil {
				return
			}
			flush(w)
		}
	}
}

func (s *server) triageUnlockHandler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	// Ask the running loop to stop at its next file boundary. Clearing isAutoScanning alone never
	// stopped anything — it only re-opened the door for a second concurrent scan.
	s.scanAbortRequested = true
	s.isAutoScanning = false
	// Suppress auto-watcher for 60 seconds so it doesn't immediately re-trigger.
	s.manualStopCooldownUntil = time.Now().UnixMilli() + 60_000
	s.mu.Unlock()

	s.deps.Tasks.ResetTaskState()
	s.hub.Broadcast(map[string]any{"type": "REGISTRY_UPDATED", "action": "UNLOCK"})
	writeJSON(w, 200, map[string]any{"message": "Operation lock forcefully cleared. Auto-watcher paused for 60s."})
}

// isScanning reports the shared in-memory scan guard (web-server.ts:84).
func (s *server) isScanning() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.isAutoScanning
}

// beginScan claims the shared guard synchronously. The later scan/repair/clear routes call it before
// any await, which is the ordering web-server.ts:1426-1434 documents as the fix for the
// checksum-UNIQUE concurrent-scan failure.
func (s *server) beginScan() {
	s.mu.Lock()
	s.isAutoScanning = true
	s.mu.Unlock()
}

// endScan releases the shared guard.
func (s *server) endScan() {
	s.mu.Lock()
	s.isAutoScanning = false
	s.mu.Unlock()
}

// abortRequested reports the per-file abort flag polled by the scan loop.
func (s *server) abortRequested() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.scanAbortRequested
}

// setAbort sets the per-file abort flag (a new scan clears it).
func (s *server) setAbort(value bool) {
	s.mu.Lock()
	s.scanAbortRequested = value
	s.mu.Unlock()
}

// withinManualStopCooldown reports whether the 60-second manual-stop cooldown is still active.
func (s *server) withinManualStopCooldown() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Now().UnixMilli() < s.manualStopCooldownUntil
}
