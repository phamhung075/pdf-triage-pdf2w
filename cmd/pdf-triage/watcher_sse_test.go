package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/httpapi"
)

// TestWatcherSSEBroadcastsScanFrames pins the auto-watcher's raw scan frames to the SSE hub that
// dashboard clients subscribe to. Before Server.Hub the composition root wired WatcherDeps.Hub =
// nil, so an AUTO-scan's SCAN_STARTED / FILE_PROGRESS / FILE_COMPLETED / SCAN_COMPLETED frames never
// reached GET /api/triage/events; only manual scans and TASK_* did.
//
// It uses ONLY temp dirs and httptest fakes, drives exactly one watcher tick through the
// Watcher.RunTick test seam (no 10 s sleep), and asserts a manual POST /api/triage/scan still
// produces the same frames (regression).
func TestWatcherSSEBroadcastsScanFrames(t *testing.T) {
	base := t.TempDir()
	data := t.TempDir()
	input := filepath.Join(base, "incoming")
	output := filepath.Join(base, "archive")
	if err := os.MkdirAll(input, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(output, 0o755); err != nil {
		t.Fatal(err)
	}

	pdf2w := newFakePDF2W()
	pdf2w.defaultText = goodStatementText
	pdf2w.defaultMD = goodStatementMarkdown
	pdf2wServer := httptest.NewServer(pdf2w)
	defer pdf2wServer.Close()

	ollama := newFakeOllama(classificationJSON)
	ollamaServer := httptest.NewServer(ollama)
	defer ollamaServer.Close()

	setTempSettingsEnv(t, input, output, pdf2wServer.URL, ollamaServer.URL)
	app := newTestApplication(t, base, data)
	server := startTestServer(t, app)

	collector := startSSECollector(t, server.URL, "/api/triage/events")
	waitForSubscriber(t, app.server.Hub())

	// --- stage 1: ONE watcher tick must stream the auto-scan's raw frames ----------------------
	auto := filepath.Join(input, "auto_watcher.pdf")
	writeSyntheticPDF(t, auto, "Releve de compte BNP Paribas, compte 00012345678")

	app.watcher.RunTick(context.Background())
	autoSeen := collector.waitFor(t, "SCAN_COMPLETED", 20*time.Second)
	assertScanEventOrder(t, autoSeen, "SCAN_STARTED", "FILE_PROGRESS", "FILE_COMPLETED", "SCAN_COMPLETED")

	// --- stage 2: a manual scan still streams the same frames (regression) ---------------------
	// Distinct bytes: an identical PDF would be caught by the checksum guard as a duplicate.
	manual := filepath.Join(input, "manual_scan.pdf")
	writeSyntheticPDF(t, manual, "Releve de compte BNP Paribas, compte 00998877665")

	body := runScanRequest(t, server.URL)
	if got := int(body["processedCount"].(float64)); got != 1 {
		t.Fatalf("manual processedCount = %d, want 1 (body=%v)", got, body)
	}
	manualSeen := collector.waitFor(t, "SCAN_COMPLETED", 20*time.Second)
	assertScanEventOrder(t, manualSeen, "SCAN_STARTED", "FILE_PROGRESS", "FILE_COMPLETED", "SCAN_COMPLETED")
}

// waitForSubscriber blocks until at least one SSE client is registered on the hub. The events route
// flushes headers BEFORE it calls Hub.Subscribe, so a client whose Do has returned is not yet
// guaranteed to be subscribed; polling ClientCount closes that window without a fixed sleep.
func waitForSubscriber(t *testing.T, hub *httpapi.Hub) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hub.ClientCount() > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no SSE client subscribed to the hub within 5s")
}
