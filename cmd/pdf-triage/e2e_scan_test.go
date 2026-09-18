package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/canonicalpath"
)

// TestE2EScanPipeline is the end-to-end scan test. It uses ONLY temp dirs and httptest fakes: a
// synthetic text-layer PDF, a fake pdf2w service returning canned markdown/text, a fake Ollama
// returning canned classification JSON, temp output dir and temp DB. It drives the real
// POST /api/triage/scan route and subscribes to the real httpapi SSE hub at
// GET /api/triage/events.
func TestE2EScanPipeline(t *testing.T) {
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

	// --- stage 1: a good synthetic bank statement is classified, archived and mirrored ----------
	good := filepath.Join(input, "releve_bnp.pdf")
	writeSyntheticPDF(t, good, "Releve de compte BNP Paribas, compte 00012345678")

	collector := startSSECollector(t, server.URL, "/api/triage/events")
	confirmSubscribed(t, server.URL, collector)

	body := runScanRequest(t, server.URL)
	if got := int(body["processedCount"].(float64)); got != 1 {
		t.Fatalf("processedCount = %d, want 1 (body=%v)", got, body)
	}
	seen := collector.waitFor(t, "SCAN_COMPLETED", 20*time.Second)
	assertScanEventOrder(t, seen, "SCAN_STARTED", "FILE_PROGRESS", "FILE_COMPLETED", "SCAN_COMPLETED")

	docs, err := app.db.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("documents = %d, want 1", len(docs))
	}
	if docs[0].Category != "bank" || docs[0].Subcategory != "bnp_paribas" {
		t.Fatalf("classified %q/%q, want bank/bnp_paribas", docs[0].Category, docs[0].Subcategory)
	}
	if !strings.Contains(docs[0].Summary, "BNP Paribas") {
		t.Fatalf("summary = %q, want it to mention BNP Paribas", docs[0].Summary)
	}

	subcategory := "bnp_paribas"
	date := "2026-01-10"
	title := "Releve de compte BNP Paribas"
	expectedArchive := canonicalpath.ComputeCanonicalPath(good, "bank", output, &subcategory, &date, &title)
	if !fileExists(expectedArchive) {
		t.Fatalf("archived file missing at canonical path %s", expectedArchive)
	}
	if fileExists(good) {
		t.Fatalf("source file %s still exists after a successful scan", good)
	}

	registryPath := app.settings.Config().JSONRegistryPath
	registryBytes, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatalf("JSON registry not written at %s: %v", registryPath, err)
	}
	if !strings.Contains(string(registryBytes), docs[0].Checksum) {
		t.Fatalf("JSON registry does not contain the document checksum %s", docs[0].Checksum)
	}

	// --- stage 2: a PDF with < 10 clean characters is BLOCKED (Golden Rule 3) -------------------
	sparse := filepath.Join(input, "sparse.pdf")
	writeSyntheticPDF(t, sparse, "abc")
	pdf2w.mu.Lock()
	pdf2w.textByFile["sparse.pdf"] = "abc"
	pdf2w.markdownByF["sparse.pdf"] = "abc"
	pdf2w.mu.Unlock()

	sparseCollector := startSSECollector(t, server.URL, "/api/triage/events")
	confirmSubscribed(t, server.URL, sparseCollector)
	sparseBody := runScanRequest(t, server.URL)
	sparseSeen := sparseCollector.waitFor(t, "SCAN_COMPLETED", 20*time.Second)
	if !containsType(sparseSeen, "FILE_FAILED") {
		t.Fatalf("stage 2 events = %v, want FILE_FAILED", sparseSeen)
	}
	if got := int(sparseBody["processedCount"].(float64)); got != 0 {
		t.Fatalf("stage 2 processedCount = %d, want 0", got)
	}

	blockedPath := filepath.Join(input, ".blocked_files", "sparse.pdf")
	if !fileExists(blockedPath) {
		t.Fatalf("under-10-char PDF was not kept under the input folder at %s", blockedPath)
	}
	if fileExists(sparse) {
		t.Fatalf("under-10-char PDF left at its original path %s instead of .blocked_files", sparse)
	}
	docs, err = app.db.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("documents after the blocked file = %d, want still 1 (no DB row for a no-text file)", len(docs))
	}
	blockedRows, err := app.db.GetAllBlockedFiles()
	if err != nil {
		t.Fatalf("GetAllBlockedFiles: %v", err)
	}
	if len(blockedRows) == 0 {
		t.Fatalf("no blocked_files row recorded for the under-10-char PDF")
	}

	// --- stage 3: Ollama down stops the scan before touching a file -----------------------------
	down := filepath.Join(input, "down.pdf")
	writeSyntheticPDF(t, down, "Another synthetic BNP Paribas statement")
	pdf2wCallsBefore := pdf2w.calls()
	ollamaServer.Close()

	downCollector := startSSECollector(t, server.URL, "/api/triage/events")
	confirmSubscribed(t, server.URL, downCollector)
	downBody := runScanRequest(t, server.URL)
	downSeen := downCollector.waitFor(t, "OLLAMA_DOWN", 20*time.Second)
	if !containsType(downSeen, "OLLAMA_DOWN") {
		t.Fatalf("stage 3 events = %v, want OLLAMA_DOWN", downSeen)
	}
	if downBody["ollamaDown"] != true {
		t.Fatalf("stage 3 body = %v, want ollamaDown true", downBody)
	}
	if !fileExists(down) {
		t.Fatalf("Ollama-down scan moved %s; it must move nothing", down)
	}
	if got := pdf2w.calls(); got != pdf2wCallsBefore {
		t.Fatalf("Ollama-down scan reached pdf2w %d extra time(s); the rule-based fallback must not run", got-pdf2wCallsBefore)
	}
	docs, err = app.db.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("documents after the Ollama-down scan = %d, want still 1", len(docs))
	}
}

// confirmSubscribed forces one broadcast through the real hub and waits for it, which proves the
// SSE subscription is registered before the scan starts (the handler flushes headers before it
// calls Hub.Subscribe, so a header-only probe is not enough).
func confirmSubscribed(t *testing.T, baseURL string, collector *sseCollector) {
	t.Helper()
	response := postJSON(t, nil, "POST", baseURL+"/api/triage/unlock", nil)
	response.Body.Close()
	collector.waitFor(t, "REGISTRY_UPDATED", 5*time.Second)
}

// assertScanEventOrder filters the raw event stream to the four scan events and asserts their
// relative order.
func assertScanEventOrder(t *testing.T, seen []string, ordered ...string) {
	t.Helper()
	position := map[string]int{}
	last := -1
	for _, want := range ordered {
		found := -1
		for index, eventType := range seen {
			if eventType == want {
				found = index
				break
			}
		}
		if found < 0 {
			t.Fatalf("event %s missing from %v", want, seen)
		}
		if found < last {
			t.Fatalf("event %s out of order in %v", want, seen)
		}
		position[want] = found
		last = found
	}
}

// containsType reports whether the event type is present.
func containsType(seen []string, target string) bool {
	for _, eventType := range seen {
		if eventType == target {
			return true
		}
	}
	return false
}
