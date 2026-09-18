package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/phamhung075/pdf-triage-pdf2w/httpapi"
	"github.com/phamhung075/pdf-triage-pdf2w/mcpserver"
	"github.com/phamhung075/pdf-triage-pdf2w/visionlab"
)

// signalContext returns a context cancelled by SIGINT/SIGTERM, plus the stop function. This is the
// Go equivalent of the TS `process.on('SIGINT'/'SIGTERM')` release-and-exit handlers
// (web-server.ts:1553-1556): cancellation unwinds the server/watcher/DB cleanly, and every deferred
// lock release runs before the process exits.
func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// newStartupApplication builds the full graph for a subcommand and reports a fatal construction
// failure on stderr. The second return is false when construction failed.
func newStartupApplication() (*application, bool) {
	app, err := newApplication(appOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error in application: %v\n", err)
		return nil, false
	}
	return app, true
}

// runServeCommand is the `serve` subcommand (the default): HTTP + SSE + the 10-second auto-watcher.
func runServeCommand() int {
	app, ok := newStartupApplication()
	if !ok {
		return 1
	}
	defer app.Close()

	cfg := app.settings.Config()
	ctx, stop := signalContext()
	defer stop()

	// Banner lines preserved verbatim from src/index.ts:20 and web-server.ts:1572 (the latter is
	// printed by httpapi.Start once the listener is bound).
	fmt.Fprintln(os.Stdout, "Starting Web Dashboard & Triage API Server...")
	if app.firstRun {
		fmt.Fprintln(os.Stdout, "First run detected: no incoming/archive folders are configured yet — open the dashboard to finish setup.")
	}

	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	if err := app.serve(ctx, addr, httpapi.StartOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error in application: %v\n", err)
		return 1
	}
	return 0
}

// runScanCommand is the `scan` subcommand: a one-shot triage scan (`npm run scan`).
func runScanCommand() int {
	app, ok := newStartupApplication()
	if !ok {
		return 1
	}
	defer app.Close()

	ctx, stop := signalContext()
	defer stop()
	return runScanWith(app, ctx, os.Stdout, os.Stderr)
}

// runScanWith runs the one-shot scan against an already-built application. It holds app/scanlock
// for the whole run (RunTriageScan deliberately does not take it) and exits non-zero when Ollama is
// down, so shell callers can distinguish an outage from a successful scan.
func runScanWith(app *application, ctx context.Context, stdout, stderr io.Writer) int {
	fmt.Fprintln(stdout, "Starting standalone PDF triage scan...")

	release, err := app.scanLock.Acquire()
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer release()

	result, err := app.scanner.RunTriageScan(ctx, nil, nil)
	if err != nil {
		fmt.Fprintf(stderr, "Fatal error in application: %v\n", err)
		return 1
	}
	if result.OllamaDown {
		// No silent rule-based fallback: the scan stopped before touching a file.
		fmt.Fprintln(stderr, result.Message)
		return 2
	}

	encoded, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		fmt.Fprintf(stderr, "Fatal error in application: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "Triage finished: %s\n", encoded)
	return 0
}

// runMCPCommand is the `mcp` subcommand: MCP over stdio, with the streamable HTTP transport started
// per settings. mcpserver.Start handles the bearer token, the HTTP port and the EADDRINUSE
// degrade-to-stdio behavior.
func runMCPCommand() int {
	app, ok := newStartupApplication()
	if !ok {
		return 1
	}
	defer app.Close()

	ctx, stop := signalContext()
	defer stop()

	fmt.Fprintln(os.Stderr, "Starting MCP Server...")
	if err := mcpserver.Start(ctx, app.mcpDeps, mcpserver.StartOptions{}); err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error in application: %v\n", err)
		return 1
	}
	return 0
}

// runVisionLabCommand is the `vision-lab` subcommand: the standalone diagnostic server on
// VISION_LAB_PORT (`npm run vision:dev`).
func runVisionLabCommand() int {
	app, ok := newStartupApplication()
	if !ok {
		return 1
	}
	defer app.Close()

	cfg := app.settings.Config()
	ctx, stop := signalContext()
	defer stop()

	deps := visionlab.Deps{
		Stepper:   app.stepper,
		PublicDir: app.publicDir,
		Logger:    app.log,
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.VisionLabPort))
	if err := visionlab.Start(ctx, addr, deps); err != nil {
		fmt.Fprintf(os.Stderr, "Fatal error in application: %v\n", err)
		return 1
	}
	return 0
}
