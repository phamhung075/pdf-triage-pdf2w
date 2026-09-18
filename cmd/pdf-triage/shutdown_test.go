package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/httpapi"
)

// TestGracefulShutdown starts serve on an ephemeral 127.0.0.1 port, cancels the context (the
// SIGINT/SIGTERM path), and asserts the server returns cleanly, the watcher stops and the
// single-instance lock is released.
func TestGracefulShutdown(t *testing.T) {
	baseDir := t.TempDir()
	dataDir := t.TempDir()
	inputDir := filepath.Join(dataDir, "input")
	outputDir := filepath.Join(dataDir, "archive")
	if err := os.MkdirAll(inputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PDF_TRIAGE_BASE_DIR", "")
	t.Setenv("PDF_TRIAGE_DATA_DIR", "")
	t.Setenv("PDF_INPUT_DIR", inputDir)
	t.Setenv("PDF_OUTPUT_DIR", outputDir)
	t.Setenv("PDF2W_SERVICE_URL", "http://127.0.0.1:1")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")

	app := newTestApplication(t, baseDir, dataDir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	listened := make(chan struct{})
	options := httpapi.StartOptions{
		Logf: func(string, ...any) {},
		Exit: func(int) {},
		Listen: func(network, address string) (net.Listener, error) {
			listener, err := net.Listen(network, address)
			if err == nil {
				close(listened)
			}
			return listener, err
		},
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- app.serve(ctx, "127.0.0.1:0", options)
	}()

	select {
	case <-listened:
	case <-time.After(10 * time.Second):
		t.Fatal("server did not start listening")
	}
	if !app.watcher.Running() {
		t.Fatal("auto-watcher is not running while the server is up")
	}

	cancel()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("serve returned %v after context cancellation, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after context cancellation")
	}

	if app.watcher.Running() {
		t.Fatal("auto-watcher still running after shutdown")
	}
	if _, err := os.Stat(filepath.Join(dataDir, ".server.lock")); !os.IsNotExist(err) {
		t.Fatalf("single-instance lock not released after shutdown (stat err = %v)", err)
	}
}
