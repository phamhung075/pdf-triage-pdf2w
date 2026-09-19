package httpapi

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// These tests exercise the 30 s /api/system/stats directory-walk cache through the
// systemStatsWalk / systemStatsNow package seams. They never touch the real filesystem: the walker
// is a counter, so a test asserts exactly how many walks ran.

// fakeStatsWalker counts walks per directory and returns a fixed, recognizable dirStats.
type fakeStatsWalker struct {
	mu    sync.Mutex
	calls map[string]int
	delay time.Duration
}

func newFakeStatsWalker() *fakeStatsWalker {
	return &fakeStatsWalker{calls: map[string]int{}}
}

func (w *fakeStatsWalker) walk(dir string) dirStats {
	if w.delay > 0 {
		time.Sleep(w.delay)
	}
	w.mu.Lock()
	w.calls[dir]++
	w.mu.Unlock()
	return dirStats{
		Count: 3,
		Bytes: 300,
		FormatBreakdown: map[string]*formatStat{
			"pdf":   {Count: 1, Bytes: 100},
			"image": {Count: 1, Bytes: 100},
			"text":  {Count: 1, Bytes: 100},
			"word":  {Count: 0, Bytes: 0},
			"excel": {Count: 0, Bytes: 0},
		},
	}
}

func (w *fakeStatsWalker) callCount(dir string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.calls[dir]
}

// fakeStatsClock is an advanceable time source for the TTL check.
type fakeStatsClock struct {
	mu sync.Mutex
	at time.Time
}

func newFakeStatsClock() *fakeStatsClock {
	return &fakeStatsClock{at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeStatsClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *fakeStatsClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// installStatsSeams swaps the package seams for the test and restores them afterwards. Tests in a
// package run sequentially unless they opt into t.Parallel, which none here do.
func installStatsSeams(t *testing.T, walker func(string) dirStats, now func() time.Time) {
	t.Helper()
	prevWalk := systemStatsWalk
	prevNow := systemStatsNow
	systemStatsWalk = walker
	systemStatsNow = now
	t.Cleanup(func() {
		systemStatsWalk = prevWalk
		systemStatsNow = prevNow
	})
}

// statsRequest runs one GET /api/system/stats against the given server without t.Fatalf, so it is
// safe to call from the concurrency test's goroutines.
func statsRequest(s *server) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/system/stats", nil)
	rec := httptest.NewRecorder()
	s.handler.ServeHTTP(rec, req)
	return rec
}

func TestSystemStatsCacheServesRepeatCalls(t *testing.T) {
	env := newTestEnv()
	rawDir := t.TempDir()
	archiveDir := t.TempDir()
	env.settings.cfg.InputDir = rawDir
	env.settings.cfg.OutputRootDir = archiveDir

	walker := newFakeStatsWalker()
	clock := newFakeStatsClock()
	installStatsSeams(t, walker.walk, clock.now)

	srv := newServer(testDeps(env))

	first := statsRequest(srv)
	clock.advance(5 * time.Second)
	second := statsRequest(srv)

	if got := walker.callCount(rawDir); got != 1 {
		t.Fatalf("raws walk calls = %d, want 1 (within TTL)", got)
	}
	if got := walker.callCount(archiveDir); got != 1 {
		t.Fatalf("archive walk calls = %d, want 1 (within TTL)", got)
	}
	if first.Body.String() != second.Body.String() {
		t.Fatalf("repeat body differs:\n first=%s\nsecond=%s", first.Body.String(), second.Body.String())
	}
	if first.Code != http.StatusOK || second.Code != http.StatusOK {
		t.Fatalf("statuses = %d, %d; want 200", first.Code, second.Code)
	}
	// The cached values (not the real filesystem) must reach the JSON.
	body := decodeJSON(t, first)
	if raws := body["raws"].(map[string]any); raws["count"].(float64) != 3 || raws["bytes"].(float64) != 300 {
		t.Fatalf("raws = %v, want the fake walker's 3 files / 300 bytes", raws)
	}
}

func TestSystemStatsCacheExpiresAfterTTL(t *testing.T) {
	env := newTestEnv()
	rawDir := t.TempDir()
	archiveDir := t.TempDir()
	env.settings.cfg.InputDir = rawDir
	env.settings.cfg.OutputRootDir = archiveDir

	walker := newFakeStatsWalker()
	clock := newFakeStatsClock()
	installStatsSeams(t, walker.walk, clock.now)

	srv := newServer(testDeps(env))
	_ = statsRequest(srv)

	clock.advance(systemStatsCacheTTL - time.Second)
	_ = statsRequest(srv)
	if got := walker.callCount(rawDir); got != 1 {
		t.Fatalf("raws walk calls just before TTL = %d, want 1", got)
	}

	// The freshness check is `< TTL`, so advancing exactly to the TTL expires the entry.
	clock.advance(time.Second)
	_ = statsRequest(srv)
	if got := walker.callCount(rawDir); got != 2 {
		t.Fatalf("raws walk calls at TTL = %d, want 2", got)
	}
	if got := walker.callCount(archiveDir); got != 2 {
		t.Fatalf("archive walk calls at TTL = %d, want 2", got)
	}
}

func TestSystemStatsCacheMissesWhenDirsChange(t *testing.T) {
	env := newTestEnv()
	rawA := t.TempDir()
	rawB := t.TempDir()
	archiveA := t.TempDir()
	archiveB := t.TempDir()
	env.settings.cfg.InputDir = rawA
	env.settings.cfg.OutputRootDir = archiveA

	walker := newFakeStatsWalker()
	clock := newFakeStatsClock()
	installStatsSeams(t, walker.walk, clock.now)

	srv := newServer(testDeps(env))
	_ = statsRequest(srv)
	if walker.callCount(rawA) != 1 || walker.callCount(archiveA) != 1 {
		t.Fatalf("first call calls: rawA=%d archiveA=%d, want 1/1",
			walker.callCount(rawA), walker.callCount(archiveA))
	}

	// A new InputDir (as saved in Settings) invalidates the entry even though the TTL is fresh.
	env.settings.cfg.InputDir = rawB
	_ = statsRequest(srv)
	if walker.callCount(rawB) != 1 {
		t.Fatalf("rawB walk calls after InputDir change = %d, want 1", walker.callCount(rawB))
	}
	if walker.callCount(archiveA) != 2 {
		t.Fatalf("archiveA walk calls after InputDir change = %d, want 2", walker.callCount(archiveA))
	}

	// A new OutputRootDir invalidates it again.
	env.settings.cfg.OutputRootDir = archiveB
	_ = statsRequest(srv)
	if walker.callCount(archiveB) != 1 {
		t.Fatalf("archiveB walk calls after OutputRootDir change = %d, want 1", walker.callCount(archiveB))
	}
}

func TestSystemStatsCacheSingleFlightUnderConcurrency(t *testing.T) {
	env := newTestEnv()
	rawDir := t.TempDir()
	archiveDir := t.TempDir()
	env.settings.cfg.InputDir = rawDir
	env.settings.cfg.OutputRootDir = archiveDir

	walker := newFakeStatsWalker()
	walker.delay = 20 * time.Millisecond // widen the window so the goroutines really overlap
	clock := newFakeStatsClock()
	installStatsSeams(t, walker.walk, clock.now)

	srv := newServer(testDeps(env))

	const goroutines = 8
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			if rec := statsRequest(srv); rec.Code != http.StatusOK {
				t.Errorf("status = %d, want 200", rec.Code)
			}
		}()
	}
	wg.Wait()

	if got := walker.callCount(rawDir); got != 1 {
		t.Fatalf("raws walk calls under %d concurrent requests = %d, want 1", goroutines, got)
	}
	if got := walker.callCount(archiveDir); got != 1 {
		t.Fatalf("archive walk calls under %d concurrent requests = %d, want 1", goroutines, got)
	}
}
