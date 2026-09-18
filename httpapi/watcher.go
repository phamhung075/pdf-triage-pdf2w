// The 10-second auto-watcher, ported from web-server.ts:1421-1479.
//
// Every tick:
//
//  1. CLAIMS the shared in-memory scan guard ATOMICALLY, including the 60-second manual-stop
//     cooldown, before any blocking work (tryBeginScanRespectingCooldown). This is the TOCTOU the
//     upstream red test calls out: the per-file blocked-check loop below performs a DB call per
//     incoming file, and a manual scan arriving mid-loop must see the guard already held. The Go
//     claim is a single mutex-protected check-and-set, so "at most one scan" holds under -race.
//  2. Acquires the cross-process app/scanlock (RunTriageScan does not take it), if one was injected.
//  3. Walks CONFIG.InputDir only (Golden Rule 1), skipping the output root and the hidden folders.
//  4. Skips a file whose blocked_files mtime/size are unchanged (Golden Rule 3), i.e. a PDF that is
//     still exactly the bytes that were blocked.
//  5. If any file is unblocked, clears the abort flag, starts a SCAN task and runs the scan.
//  6. Releases the file lock and the in-memory guard on every path.
//
// The ticker and clock are injected (Ticker, Interval, Now) so the tests drive ticks
// deterministically instead of sleeping 10 seconds.
package httpapi

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/taskstate"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfscanner"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// defaultWatcherInterval is the upstream `setInterval(..., 10000)`.
const defaultWatcherInterval = 10 * time.Second

// ScanGate is the in-memory scan guard the server owns. *Server satisfies it; the unexported
// methods are deliberately unreachable outside package httpapi, so no other type can claim to be
// the shared guard.
type ScanGate interface {
	tryBeginScanRespectingCooldown(nowMillis int64) bool
	endScan()
	setAbort(value bool)
	abortRequested() bool
}

// WatcherDeps is the watcher's injected collaborator set. Gate is the only required field; every
// other field has a production default.
type WatcherDeps struct {
	Gate ScanGate
	// Settings provides InputDir/OutputRootDir. Nil is the zero config.
	Settings Settings
	// DB is the store the default GetBlockedFile uses.
	DB DocumentWriteDB
	// GetPDFsRecursively defaults to pdfscanner.GetPDFsRecursively.
	GetPDFsRecursively func(dir string, ignoreDir ...string) []string
	// GetBlockedFile defaults to DB.GetBlockedFile; when neither is set every file is unblocked.
	GetBlockedFile func(originalPath string) (*database.BlockedFileRecord, error)
	// RunScan is the scan pipeline. Required for a tick to actually scan.
	RunScan ScanRunner
	// ScanLock is the cross-process lock; nil skips it.
	ScanLock ScanLocker
	// Tasks receives start/progress/finish/fail. Nil is a no-op.
	Tasks DocumentWriteTasks
	// Hub receives the SCAN_* SSE frames. Nil broadcasts nothing.
	Hub *Hub
	// Log is the AUTO_WATCHER logger; nil is silent.
	Log DocumentWriteLog
	// Now is the cooldown clock; nil is time.Now.
	Now func() time.Time
	// Ticker overrides the real 10 s ticker (tests drive ticks on this channel).
	Ticker <-chan time.Time
	// Interval is the real ticker period; 0 is defaultWatcherInterval.
	Interval time.Duration
}

// Watcher polls the incoming folder on a ticker. Create it with NewWatcher, run it with Start and
// stop it with Stop; RunTick runs one tick synchronously (the test seam).
type Watcher struct {
	deps WatcherDeps

	mu      sync.Mutex
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

// NewWatcher builds a watcher, applying the production defaults for every unset seam.
func NewWatcher(deps WatcherDeps) *Watcher {
	if deps.GetPDFsRecursively == nil {
		deps.GetPDFsRecursively = pdfscanner.GetPDFsRecursively
	}
	if deps.GetBlockedFile == nil && deps.DB != nil {
		db := deps.DB
		deps.GetBlockedFile = func(originalPath string) (*database.BlockedFileRecord, error) {
			return db.GetBlockedFile(originalPath)
		}
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Interval <= 0 {
		deps.Interval = defaultWatcherInterval
	}
	if deps.Tasks == nil {
		deps.Tasks = noopTasks{}
	}
	return &Watcher{deps: deps}
}

// Start launches the tick loop. A second Start while running is a no-op. The loop stops when ctx
// is done or Stop is called.
func (w *Watcher) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	if w.running {
		w.mu.Unlock()
		return
	}
	child, cancel := context.WithCancel(ctx)
	w.running = true
	w.cancel = cancel
	w.done = make(chan struct{})
	w.mu.Unlock()

	go w.loop(child)
}

// Stop cancels the loop and waits for it to return. It is safe to call when not running.
func (w *Watcher) Stop() {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	if done != nil {
		<-done
	}
	w.mu.Lock()
	w.running = false
	w.cancel = nil
	w.done = nil
	w.mu.Unlock()
}

// Running reports whether the loop is active.
func (w *Watcher) Running() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.running
}

func (w *Watcher) loop(ctx context.Context) {
	w.mu.Lock()
	done := w.done
	w.mu.Unlock()
	defer close(done)

	ticker := w.deps.Ticker
	var real *time.Ticker
	if ticker == nil {
		real = time.NewTicker(w.deps.Interval)
		defer real.Stop()
		ticker = real.C
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker:
			w.RunTick(ctx)
		}
	}
}

// RunTick performs one watcher tick synchronously. It is exported so tests (and a future scheduler)
// can drive a tick without a ticker.
func (w *Watcher) RunTick(ctx context.Context) {
	if w.deps.Gate == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}

	now := w.deps.Now()
	if !w.deps.Gate.tryBeginScanRespectingCooldown(now.UnixMilli()) {
		return
	}
	defer w.deps.Gate.endScan()

	if w.deps.ScanLock != nil {
		release, err := w.deps.ScanLock.Acquire()
		if err != nil {
			w.logWarn("AUTO_WATCHER", fmt.Sprintf("Auto-scan skipped: %s", err.Error()), nil)
			return
		}
		defer release()
	}

	cfg := w.config()
	incoming := w.deps.GetPDFsRecursively(cfg.InputDir, cfg.OutputRootDir)
	if len(incoming) == 0 {
		return
	}

	unblocked := make([]string, 0, len(incoming))
	for _, candidate := range incoming {
		info, err := os.Stat(candidate)
		if err != nil {
			unblocked = append(unblocked, candidate)
			continue
		}
		blocked, err := w.getBlockedFile(candidate)
		if err != nil {
			unblocked = append(unblocked, candidate)
			continue
		}
		if blocked == nil || blocked.MtimeMs != mtimeMsOf(info) || blocked.Size != info.Size() {
			unblocked = append(unblocked, candidate)
		}
	}
	if len(unblocked) == 0 {
		return
	}

	w.logInfo("AUTO_WATCHER", fmt.Sprintf("Auto-scan triggered: Found %d incoming PDF(s) in __raws", len(unblocked)), nil)

	if w.deps.RunScan == nil {
		return
	}

	w.deps.Gate.setAbort(false)
	tasks := w.deps.Tasks
	tasks.StartTask(taskstate.TaskScan, len(unblocked), fmt.Sprintf("Auto-scanning %d incoming PDF(s) in __raws...", len(unblocked)))

	result, err := w.deps.RunScan.RunTriageScan(ctx, func(evt triagescan.Event) {
		applyScanProgress(tasks, evt)
		if w.deps.Hub != nil {
			w.deps.Hub.Broadcast(evt)
		}
	}, w.deps.Gate.abortRequested)
	if err != nil {
		tasks.FailTask(err.Error())
		return
	}
	if result.OllamaDown {
		message := result.Message
		if message == "" {
			message = "Ollama is down — start Ollama and re-scan."
		}
		tasks.FailTask(message)
		return
	}
	tasks.FinishTask(scanResultMap(result), fmt.Sprintf("Auto-scan completed. Processed %d file(s).", result.ProcessedCount))
}

func (w *Watcher) config() (cfg struct {
	InputDir      string
	OutputRootDir string
}) {
	if w.deps.Settings == nil {
		return cfg
	}
	got := w.deps.Settings.Config()
	cfg.InputDir = got.InputDir
	cfg.OutputRootDir = got.OutputRootDir
	return cfg
}

func (w *Watcher) getBlockedFile(originalPath string) (*database.BlockedFileRecord, error) {
	if w.deps.GetBlockedFile == nil {
		return nil, nil
	}
	return w.deps.GetBlockedFile(originalPath)
}

func (w *Watcher) logInfo(module, message string, meta any) {
	if w.deps.Log != nil {
		w.deps.Log.Info(module, message, meta)
	}
}

func (w *Watcher) logWarn(module, message string, meta any) {
	if w.deps.Log != nil {
		w.deps.Log.Warn(module, message, meta)
	}
}

// mtimeMsOf is Node's `fs.Stats.mtimeMs`: milliseconds since the epoch as a float. It must match
// app/triagescan's helper so the unchanged-blocked-file comparison stays exact.
func mtimeMsOf(info os.FileInfo) float64 {
	return float64(info.ModTime().UnixNano()) / 1e6
}
