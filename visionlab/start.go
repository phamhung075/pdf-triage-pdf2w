// Server startup: the Vision Lab's own port and EADDRINUSE takeover-once, ported from
// vision-lab-server.ts:65-98.
//
// Unlike the main web server, the Vision Lab has no single-instance lock file: startVisionLabServer
// simply attempted to listen and, on EADDRINUSE, killed whatever held the port once and retried
// with takeover disabled. Start is the Go equivalent. Every seam — the listener, the port killer,
// the sleep, process exit and the log sink — is injected through Deps so tests never bind a real
// port, kill a real process, or exit the test binary.
package visionlab

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/pidlock"
)

// withDefaults fills Start's seams with their real implementations. Handler-shaping fields are left
// untouched.
func (d Deps) withDefaults() Deps {
	if d.Listen == nil {
		d.Listen = net.Listen
	}
	if d.KillProcessOnPort == nil {
		d.KillProcessOnPort = pidlock.KillProcessOnPort
	}
	if d.Exit == nil {
		d.Exit = os.Exit
	}
	if d.Sleep == nil {
		d.Sleep = time.Sleep
	}
	if d.Logf == nil {
		d.Logf = func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) }
	}
	return d
}

// Start serves NewApp(deps) on addr until ctx is cancelled. A port already in use is taken over once
// (kill + 500 ms + retry) before Exit(1), matching vision-lab-server.ts:72-98. It returns the fatal
// listen error, or nil after a clean context-driven shutdown.
func Start(ctx context.Context, addr string, deps Deps) error {
	deps = deps.withDefaults()
	return attemptListen(ctx, addr, deps, true)
}

// attemptListen is the TS attemptListen. It recurses only for the single takeover retry, whose
// allowTakeover is false so a port still unavailable after the kill fails fast instead of looping.
func attemptListen(ctx context.Context, addr string, deps Deps, allowTakeover bool) error {
	listener, err := deps.Listen("tcp", addr)
	if err != nil {
		port, _ := portOf(addr)
		if !isAddrInUse(err) {
			deps.Logf("Vision Lab server failed to start: %v", err)
			deps.Exit(1)
			return err
		}
		if !allowTakeover {
			deps.Logf("Port %d is still in use after a takeover attempt — exiting.", port)
			deps.Exit(1)
			return err
		}
		deps.Logf("Port %d is already in use — attempting to take over from the previous instance...", port)
		if _, ok := portOf(addr); !ok || !deps.KillProcessOnPort(port) {
			deps.Logf("Port %d is in use, but no process could be found to free it.", port)
			deps.Exit(1)
			return err
		}
		deps.Logf("Killed the process holding port %d. Retrying...", port)
		deps.Sleep(500 * time.Millisecond)
		return attemptListen(ctx, addr, deps, false)
	}

	server := &http.Server{Handler: NewApp(deps)}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	deps.Logf("Vision Lab server running at http://%s", addr)
	deps.Logf("Diagnostic page: http://%s/test-image-to-pdf.html", addr)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		return nil
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			deps.Logf("Vision Lab server failed: %v", err)
			deps.Exit(1)
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

// portOf extracts the numeric port from a host:port address.
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
