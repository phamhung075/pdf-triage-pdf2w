package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// TestTryBeginScanIsAtomic is the unit-level half of the TOCTOU proof: many goroutines race for
// the guard and exactly one wins.
func TestTryBeginScanIsAtomic(t *testing.T) {
	env := newWriteEnv()
	const workers = 64
	var wins atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if env.srv.tryBeginScan() {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if got := wins.Load(); got != 1 {
		t.Fatalf("guard winners = %d, want exactly 1", got)
	}
}

// TestWatcherAndManualScanAtMostOne hammers a watcher tick and a manual POST /api/triage/scan
// concurrently and proves at most one scan is ever in flight. Run with `go test -race` this also
// proves the shared guard itself is race-free.
func TestWatcherAndManualScanAtMostOne(t *testing.T) {
	env := newWriteEnv()
	gate := newScanGate()
	var concurrent, maxConcurrent, total atomic.Int64
	entered := make(chan struct{}, 1)

	env.scanR.run = func(context.Context, func(triagescan.Event), func() bool) (triagescan.Result, error) {
		now := concurrent.Add(1)
		for {
			old := maxConcurrent.Load()
			if now <= old || maxConcurrent.CompareAndSwap(old, now) {
				break
			}
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		gate.wait()
		concurrent.Add(-1)
		total.Add(1)
		return triagescan.Result{}, nil
	}

	w := watcherFor(env, WatcherDeps{
		Gate:               env.srv,
		GetPDFsRecursively: func(string, ...string) []string { return []string{"/raws/a.pdf"} },
		GetBlockedFile:     func(string) (*database.BlockedFileRecord, error) { return nil, nil },
		RunScan:            env.scanR,
	})

	const iterations = 40
	for i := 0; i < iterations; i++ {
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			w.RunTick(context.Background())
		}()
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/triage/scan", nil)
			env.handler.ServeHTTP(rec, req)
		}()

		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatalf("iteration %d: no scan started", i)
		}
		gate.open()
		wg.Wait()
		gate.reset()
	}
	if got := maxConcurrent.Load(); got != 1 {
		t.Fatalf("max concurrent scans = %d, want 1", got)
	}
	if got := total.Load(); got < iterations {
		t.Fatalf("total scans = %d, want at least %d", got, iterations)
	}
	if env.srv.isScanning() {
		t.Fatal("guard left claimed after the hammer")
	}
}
