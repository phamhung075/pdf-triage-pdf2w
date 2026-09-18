// Server startup: single-instance lock and port takeover, ported from
// web-server.ts:1537-1595.
//
// The TS startWebServer(port) acquired DATA_DIR/.server.lock (refusing to start when another live
// PID owned it) and then attempted to listen; on EADDRINUSE it killed the process holding the port
// once and retried with takeover disabled. Start is the Go equivalent. Every seam — the listener,
// the lock primitives, the port killer, the sleep and process exit — is injected so tests never bind
// a real port, kill a real process, or exit the test binary.
package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/pidlock"
)

// StartOptions carries Start's handler plus its injectable seams. Zero values fall back to the real
// implementations (net.Listen, infra/pidlock, os.Exit, time.Sleep) and os.Stderr logging.
type StartOptions struct {
	// Handler is the http.Handler to serve. Required.
	Handler http.Handler
	// DataDir holds .server.lock. The lock is per data directory, exactly as web-server.ts:1537-1541
	// explains.
	DataDir string
	// LockFile overrides DATA_DIR/.server.lock (tests).
	LockFile string

	Logf                 func(format string, args ...any)
	Listen               func(network, address string) (net.Listener, error)
	ReadActiveLockHolder func(lockFilePath string) (int, bool)
	AcquireProcessLock   func(lockFilePath string) (func(), error)
	KillProcessOnPort    func(port int) bool
	Exit                 func(code int)
	Sleep                func(d time.Duration)
}

func (o StartOptions) withDefaults() StartOptions {
	if o.Logf == nil {
		o.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}
	if o.Listen == nil {
		o.Listen = net.Listen
	}
	if o.ReadActiveLockHolder == nil {
		o.ReadActiveLockHolder = pidlock.ReadActiveLockHolder
	}
	if o.AcquireProcessLock == nil {
		o.AcquireProcessLock = pidlock.AcquireProcessLock
	}
	if o.KillProcessOnPort == nil {
		o.KillProcessOnPort = pidlock.KillProcessOnPort
	}
	if o.Exit == nil {
		o.Exit = os.Exit
	}
	if o.Sleep == nil {
		o.Sleep = time.Sleep
	}
	return o
}

// Start acquires the single-instance lock for opts.DataDir, then serves opts.Handler on addr until
// ctx is cancelled. A port already in use is taken over once (kill + 500 ms + retry) before Exit(1).
// It returns the fatal listen error, or nil after a clean context-driven shutdown.
func Start(ctx context.Context, addr string, opts StartOptions) error {
	opts = opts.withDefaults()

	lockFile := opts.LockFile
	if lockFile == "" {
		lockFile = filepath.Join(opts.DataDir, ".server.lock")
	}

	// Prevent two instances from running their auto-watchers concurrently against the same
	// __raws/__archive files.
	if holder, active := opts.ReadActiveLockHolder(lockFile); active {
		opts.Logf("Another instance of this server is already running (PID %d). Refusing to start a second instance — stop it first, or delete %s if it's stale.", holder, lockFile)
		err := fmt.Errorf("another instance is already running (PID %d)", holder)
		opts.Exit(1)
		return err
	}

	release, err := opts.AcquireProcessLock(lockFile)
	if err != nil {
		return err
	}
	defer release()

	return listenLoop(ctx, addr, opts, true)
}

// listenLoop is attemptListen. It is recursive only for the single takeover retry, whose
// allowTakeover is false so a port still unavailable after the kill fails fast.
func listenLoop(ctx context.Context, addr string, opts StartOptions, allowTakeover bool) error {
	listener, err := opts.Listen("tcp", addr)
	if err != nil {
		if !isAddrInUse(err) {
			opts.Logf("Web server failed to start: %v", err)
			opts.Exit(1)
			return err
		}
		if !allowTakeover {
			opts.Logf("Port %s is still in use after a takeover attempt — exiting.", addr)
			opts.Exit(1)
			return err
		}
		opts.Logf("Port %s is already in use — attempting to take over from the previous instance...", addr)
		port, ok := portOf(addr)
		if !ok || !opts.KillProcessOnPort(port) {
			opts.Logf("Port %s is in use, but no process could be found to free it.", addr)
			opts.Exit(1)
			return err
		}
		opts.Logf("Killed the process holding port %s. Retrying...", addr)
		opts.Sleep(500 * time.Millisecond)
		return listenLoop(ctx, addr, opts, false)
	}

	server := &http.Server{Handler: opts.Handler}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	opts.Logf("Web Dashboard is running at http://%s [Hot Reload Active 🔥]", addr)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			opts.Logf("Web server failed: %v", err)
			opts.Exit(1)
			return err
		}
		return nil
	}
}

// isAddrInUse recognises EADDRINUSE across platforms without importing platform-specific constants
// beyond syscall's portable Errno, plus a message fallback for wrapped errors.
func isAddrInUse(err error) bool {
	if errors.Is(err, syscall.EADDRINUSE) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "address already in use")
}

func portOf(addr string) (int, bool) {
	_, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, false
	}
	port, err := net.LookupPort("tcp", portText)
	if err != nil {
		return 0, false
	}
	return port, true
}
