package httpapi

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/scanlock"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// watcherFor builds a watcher whose gate is the env's server.
func watcherFor(env *writeEnv, deps WatcherDeps) *Watcher {
	if deps.Gate == nil {
		deps.Gate = env.srv
	}
	if deps.Settings == nil {
		deps.Settings = env.base.settings
	}
	if deps.Log == nil {
		deps.Log = env.log
	}
	if deps.Tasks == nil {
		deps.Tasks = env.base.tasks
	}
	if deps.Hub == nil {
		deps.Hub = env.srv.hub
	}
	return NewWatcher(deps)
}

func TestWatcherKicksOffScanForUnblockedPDFs(t *testing.T) {
	env := newWriteEnv()
	var scans int
	w := watcherFor(env, WatcherDeps{
		GetPDFsRecursively: func(string, ...string) []string { return []string{"/raws/incoming.pdf"} },
		GetBlockedFile:     func(string) (*database.BlockedFileRecord, error) { return nil, nil },
		RunScan: &fakeScanRunner{run: func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			scans++
			return triagescan.Result{ProcessedCount: 1, Items: []triagescan.ResultItem{}}, nil
		}},
	})

	w.RunTick(context.Background())

	if scans != 1 {
		t.Fatalf("scans = %d, want 1", scans)
	}
	if env.srv.isScanning() {
		t.Fatal("guard was not released")
	}
	if state := env.base.tasks.State(); state.IsRunning || state.Stage != "COMPLETED" {
		t.Fatalf("task state = %+v", state)
	}
}

func TestWatcherWalksOnlyTheInputDir(t *testing.T) {
	env := newWriteEnv()
	env.base.settings.cfg.InputDir = "/managed/__raws"
	env.base.settings.cfg.OutputRootDir = "/managed/__archive"

	var gotDir string
	var gotIgnore []string
	w := watcherFor(env, WatcherDeps{
		GetPDFsRecursively: func(dir string, ignoreDir ...string) []string {
			gotDir = dir
			gotIgnore = ignoreDir
			return nil
		},
	})
	w.RunTick(context.Background())

	if gotDir != "/managed/__raws" {
		t.Fatalf("walked dir = %q, want the input dir (Golden Rule 1)", gotDir)
	}
	if len(gotIgnore) != 1 || gotIgnore[0] != "/managed/__archive" {
		t.Fatalf("ignore dirs = %v, want the output root", gotIgnore)
	}
}

func TestWatcherDoesNothingWithNoPDFs(t *testing.T) {
	env := newWriteEnv()
	w := watcherFor(env, WatcherDeps{
		GetPDFsRecursively: func(string, ...string) []string { return nil },
		RunScan: &fakeScanRunner{run: func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			t.Fatal("scan must not run with no PDFs")
			return triagescan.Result{}, nil
		}},
	})
	w.RunTick(context.Background())
}

func TestWatcherSkipsUnchangedBlockedFile(t *testing.T) {
	env := newWriteEnv()
	dir := t.TempDir()
	blockedPath := filepath.Join(dir, "blocked.pdf")
	if err := os.WriteFile(blockedPath, []byte("blocked-bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(blockedPath)
	if err != nil {
		t.Fatal(err)
	}

	env.db.blocked[blockedPath] = &database.BlockedFileRecord{
		OriginalPath: blockedPath,
		MtimeMs:      mtimeMsOf(info),
		Size:         info.Size(),
	}

	w := watcherFor(env, WatcherDeps{
		GetPDFsRecursively: func(string, ...string) []string { return []string{blockedPath} },
		DB:                 env.db,
		RunScan: &fakeScanRunner{run: func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			t.Fatal("scan must not run for an unchanged blocked file")
			return triagescan.Result{}, nil
		}},
	})
	w.RunTick(context.Background())

	// A changed size must unblock it.
	env.db.blocked[blockedPath].Size = info.Size() + 1
	scans := 0
	w2 := watcherFor(env, WatcherDeps{
		GetPDFsRecursively: func(string, ...string) []string { return []string{blockedPath} },
		DB:                 env.db,
		RunScan: &fakeScanRunner{run: func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			scans++
			return triagescan.Result{}, nil
		}},
	})
	w2.RunTick(context.Background())
	if scans != 1 {
		t.Fatalf("changed blocked file was not retried: scans = %d", scans)
	}
}

func TestWatcherDoesNotOverlapManualScan(t *testing.T) {
	env := newWriteEnv()
	if !env.srv.tryBeginScan() {
		t.Fatal("could not seed the guard")
	}
	w := watcherFor(env, WatcherDeps{
		GetPDFsRecursively: func(string, ...string) []string { return []string{"/raws/a.pdf"} },
		RunScan: &fakeScanRunner{run: func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			t.Fatal("watcher tick must skip while a manual scan holds the guard")
			return triagescan.Result{}, nil
		}},
	})
	w.RunTick(context.Background())
}

// TestWatcherClaimsGuardBeforeBlockedCheck is the Go-side TOCTOU proof: while a tick is parked
// inside the per-file blocked-file check, a manual scan must be rejected because the guard was
// claimed synchronously before the check.
func TestWatcherClaimsGuardBeforeBlockedCheck(t *testing.T) {
	env := newWriteEnv()
	dir := t.TempDir()
	pdfPath := filepath.Join(dir, "incoming.pdf")
	if err := os.WriteFile(pdfPath, []byte("pdf"), 0o600); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	w := watcherFor(env, WatcherDeps{
		GetPDFsRecursively: func(string, ...string) []string { return []string{pdfPath} },
		GetBlockedFile: func(string) (*database.BlockedFileRecord, error) {
			close(entered)
			<-release
			return nil, nil
		},
		RunScan: &fakeScanRunner{run: func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			return triagescan.Result{}, nil
		}},
	})

	go func() {
		w.RunTick(context.Background())
		close(done)
	}()

	<-entered
	if env.srv.tryBeginScan() {
		t.Fatal("manual scan claimed the guard while the tick was inside its blocked-file check")
	}
	close(release)
	<-done
	if env.srv.isScanning() {
		t.Fatal("guard was not released")
	}
}

func TestWatcherRespectsManualStopCooldown(t *testing.T) {
	env := newWriteEnv()
	scans := 0
	w := watcherFor(env, WatcherDeps{
		GetPDFsRecursively: func(string, ...string) []string { return []string{"/raws/a.pdf"} },
		GetBlockedFile:     func(string) (*database.BlockedFileRecord, error) { return nil, nil },
		RunScan: &fakeScanRunner{run: func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			scans++
			return triagescan.Result{}, nil
		}},
	})

	env.srv.mu.Lock()
	env.srv.manualStopCooldownUntil = time.Now().UnixMilli() + 60_000
	env.srv.mu.Unlock()

	w.RunTick(context.Background())
	if scans != 0 {
		t.Fatalf("scan ran inside the cooldown: scans = %d", scans)
	}

	env.srv.mu.Lock()
	env.srv.manualStopCooldownUntil = 0
	env.srv.mu.Unlock()

	w.RunTick(context.Background())
	if scans != 1 {
		t.Fatalf("scan did not run after the cooldown: scans = %d", scans)
	}
}

func TestWatcherReleasesGuardOnScanError(t *testing.T) {
	env := newWriteEnv()
	w := watcherFor(env, WatcherDeps{
		GetPDFsRecursively: func(string, ...string) []string { return []string{"/raws/a.pdf"} },
		GetBlockedFile:     func(string) (*database.BlockedFileRecord, error) { return nil, nil },
		RunScan: &fakeScanRunner{run: func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			return triagescan.Result{}, errors.New("scan boom")
		}},
	})
	w.RunTick(context.Background())
	if env.srv.isScanning() {
		t.Fatal("guard was not released after a scan error")
	}
	if env.base.tasks.State().Stage != "FAILED" {
		t.Fatalf("task state = %+v", env.base.tasks.State())
	}
}

func TestWatcherReleasesGuardOnScanLockContention(t *testing.T) {
	env := newWriteEnv()
	env.lock.err = &scanlock.ScanInProgressError{HolderPID: 7}
	w := watcherFor(env, WatcherDeps{
		ScanLock:           env.lock,
		GetPDFsRecursively: func(string, ...string) []string { return []string{"/raws/a.pdf"} },
		GetBlockedFile:     func(string) (*database.BlockedFileRecord, error) { return nil, nil },
		RunScan: &fakeScanRunner{run: func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			t.Fatal("scan must not run when the cross-process lock is held")
			return triagescan.Result{}, nil
		}},
	})
	w.RunTick(context.Background())
	if env.srv.isScanning() {
		t.Fatal("guard was not released after lock contention")
	}
}

func TestWatcherStartStopWithInjectedTicker(t *testing.T) {
	env := newWriteEnv()
	ticker := make(chan time.Time, 4)
	var scans int
	var mu sync.Mutex
	w := watcherFor(env, WatcherDeps{
		Ticker:             ticker,
		GetPDFsRecursively: func(string, ...string) []string { return []string{"/raws/a.pdf"} },
		GetBlockedFile:     func(string) (*database.BlockedFileRecord, error) { return nil, nil },
		RunScan: &fakeScanRunner{run: func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
			mu.Lock()
			scans++
			mu.Unlock()
			return triagescan.Result{}, nil
		}},
	})

	w.Start(context.Background())
	if !w.Running() {
		t.Fatal("watcher did not start")
	}
	ticker <- time.Now()
	waitFor(t, "watcher tick", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return scans == 1
	})
	w.Stop()
	if w.Running() {
		t.Fatal("watcher did not stop")
	}
	w.Stop() // idempotent
}

// scanGate is a per-iteration release latch the race test uses to hold a scan open. It uses a
// condition variable plus a released flag so an open() that races ahead of the winner's wait()
// still releases it.
type scanGate struct {
	mu       sync.Mutex
	cond     *sync.Cond
	released bool
}

func newScanGate() *scanGate {
	g := &scanGate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

func (g *scanGate) wait() {
	g.mu.Lock()
	for !g.released {
		g.cond.Wait()
	}
	g.mu.Unlock()
}

func (g *scanGate) open() {
	g.mu.Lock()
	g.released = true
	g.cond.Broadcast()
	g.mu.Unlock()
}

func (g *scanGate) reset() {
	g.mu.Lock()
	g.released = false
	g.mu.Unlock()
}
