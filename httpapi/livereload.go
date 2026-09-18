// GET /api/dev/livereload — live-reload SSE stream, ported from web-server.ts:88-114.
//
// TS registered the stream, pushed the response into liveReloadClients, and had
// `fs.watch(publicDir, { recursive: true })` write the literal `data: reload\n\n` to every client.
// Its purpose is a browser auto-refresh while editing the dashboard.
//
// Go has no stdlib recursive fs.watch, so the file-change source is a polling mtime/size watcher
// (PollingWatcher) behind the LiveReloadWatcher interface: the frozen wire behavior (`data: reload`,
// no JSON, one event per detected change) is unchanged, only the change-detection mechanism differs.
package httpapi

import (
	"hash/fnv"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

func (s *server) registerLiveReload(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/dev/livereload", s.liveReloadHandler)
}

func (s *server) liveReloadHandler(w http.ResponseWriter, r *http.Request) {
	watcher := s.deps.LiveReload
	if watcher == nil && s.deps.PublicDir != "" {
		if info, err := os.Stat(s.deps.PublicDir); err == nil && info.IsDir() {
			watcher = NewPollingWatcher(s.deps.PublicDir, time.Second)
		}
	}

	setSSEHeaders(w)
	flush(w)

	if watcher == nil {
		// public/ does not exist: TS registered no fs.watch, so a client may connect but never
		// receives a reload. Hold the stream open until the client goes away.
		<-r.Context().Done()
		return
	}

	changes, cancel := watcher.Subscribe()
	defer cancel()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-changes:
			if _, err := w.Write([]byte("data: reload\n\n")); err != nil {
				return
			}
			flush(w)
		}
	}
}

// setSSEHeaders applies the three headers every SSE route in web-server.ts sets.
func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

// flush writes response headers immediately so an SSE client's onopen fires before the first event,
// which is what Express's res.flushHeaders() did and what the ported supertest/curl tests rely on.
func flush(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// PollingWatcher is the stdlib-only replacement for fs.watch(publicDir, { recursive: true }). It
// computes a fingerprint of every file's path, size and mtime once per interval and emits one signal
// per observed change. The first fingerprint is captured at start so an already-populated public/
// does not fire a spurious reload the moment a client connects.
type PollingWatcher struct {
	dir      string
	interval time.Duration

	mu     sync.Mutex
	subs   map[int]chan struct{}
	nextID int

	startOnce sync.Once
	stopOnce  sync.Once
	stop      chan struct{}
}

// NewPollingWatcher returns a watcher over dir with the given poll interval.
func NewPollingWatcher(dir string, interval time.Duration) *PollingWatcher {
	if interval <= 0 {
		interval = time.Second
	}
	return &PollingWatcher{
		dir:      dir,
		interval: interval,
		subs:     map[int]chan struct{}{},
		stop:     make(chan struct{}),
	}
}

// Subscribe registers for change signals and lazily starts the poll goroutine.
func (p *PollingWatcher) Subscribe() (<-chan struct{}, func()) {
	p.startOnce.Do(func() { go p.run() })

	p.mu.Lock()
	id := p.nextID
	p.nextID++
	ch := make(chan struct{}, 1)
	p.subs[id] = ch
	p.mu.Unlock()

	cancel := func() {
		p.mu.Lock()
		delete(p.subs, id)
		p.mu.Unlock()
	}
	return ch, cancel
}

// Close stops the poll goroutine (tests only).
func (p *PollingWatcher) Close() {
	p.stopOnce.Do(func() { close(p.stop) })
}

func (p *PollingWatcher) run() {
	last := fingerprint(p.dir)
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-ticker.C:
			current := fingerprint(p.dir)
			if current == last {
				continue
			}
			last = current
			p.notify()
		}
	}
}

func (p *PollingWatcher) notify() {
	p.mu.Lock()
	subs := make([]chan struct{}, 0, len(p.subs))
	for _, ch := range p.subs {
		subs = append(subs, ch)
	}
	p.mu.Unlock()
	for _, ch := range subs {
		select {
		case ch <- struct{}{}:
		default:
			// A pending signal is already queued; one reload is enough.
		}
	}
}

// fingerprint hashes path/size/mtime for every regular file under dir. Read errors are folded into
// the hash rather than aborting, so a transiently unreadable file does not look like "no change".
func fingerprint(dir string) uint64 {
	h := fnv.New64a()
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			_, _ = h.Write([]byte("err:" + path))
			return nil
		}
		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		_, _ = h.Write([]byte(path))
		_, _ = h.Write([]byte{0})
		if info.IsDir() {
			return nil
		}
		_, _ = h.Write([]byte(info.ModTime().UTC().Format(time.RFC3339Nano)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(strconv.FormatInt(info.Size(), 10)))
		_, _ = h.Write([]byte{0})
		return nil
	})
	return h.Sum64()
}
