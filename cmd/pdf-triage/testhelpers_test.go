package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/osopen"
)

// --- synthetic PDF ------------------------------------------------------------------------------

// writeSyntheticPDF writes a small, valid single-page PDF with a text content stream. It is the
// "text-layer PDF generated in-test" the e2e scan test feeds to the pipeline. The pipeline never
// parses these bytes in the test (pdf2w is faked), but the file is a real PDF so nothing has to
// special-case it.
func writeSyntheticPDF(t *testing.T, path, text string) {
	t.Helper()

	var buf bytes.Buffer
	buf.WriteString("%PDF-1.4\n")

	offsets := make([]int, 6)
	writeObject := func(number int, body string) {
		offsets[number] = buf.Len()
		fmt.Fprintf(&buf, "%d 0 obj\n%s\nendobj\n", number, body)
	}

	content := fmt.Sprintf("BT /F1 12 Tf 72 720 Td (%s) Tj ET", escapePDFText(text))
	writeObject(1, "<< /Type /Catalog /Pages 2 0 R >>")
	writeObject(2, "<< /Type /Pages /Kids [3 0 R] /Count 1 >>")
	writeObject(3, "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>")
	writeObject(4, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content))
	writeObject(5, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	xref := buf.Len()
	buf.WriteString("xref\n0 6\n0000000000 65535 f \n")
	for i := 1; i <= 5; i++ {
		fmt.Fprintf(&buf, "%010d 00000 n \n", offsets[i])
	}
	fmt.Fprintf(&buf, "trailer\n<< /Size 6 /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", xref)

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatalf("write synthetic PDF %s: %v", path, err)
	}
}

// escapePDFText escapes the characters that must be escaped inside a PDF literal string.
func escapePDFText(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `(`, `\(`, `)`, `\)`)
	return replacer.Replace(text)
}

// --- fake pdf2w service -------------------------------------------------------------------------

// fakePDF2W is a canned markdown-extract-service. It answers POST /convert only, records call
// count, and selects the canned response by the x-file-name header (percent-encoded by the
// real client), so one fake can serve the good file and the under-10-character file.
type fakePDF2W struct {
	mu sync.Mutex

	convertCalls int
	defaultText  string
	defaultMD    string
	textByFile   map[string]string
	markdownByF  map[string]string
}

func newFakePDF2W() *fakePDF2W {
	return &fakePDF2W{textByFile: map[string]string{}, markdownByF: map[string]string{}}
}

func (f *fakePDF2W) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.convertCalls
}

func (f *fakePDF2W) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/convert" {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.convertCalls++
	f.mu.Unlock()

	_, _ = io.Copy(io.Discard, r.Body)

	name := r.Header.Get("x-file-name")
	if decoded, err := url.PathUnescape(name); err == nil {
		name = decoded
	}

	f.mu.Lock()
	text, ok := f.textByFile[name]
	if !ok {
		text = f.defaultText
	}
	markdown, ok := f.markdownByF[name]
	if !ok {
		markdown = f.defaultMD
	}
	f.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"markdown": markdown,
		"text":     text,
		"numpages": 1,
	})
}

// --- fake Ollama service ------------------------------------------------------------------------

// fakeOllama answers the three endpoints infra/ollama calls: /api/tags (model list),
// /api/generate (health check, Step A and Step D all receive the canned classification JSON) and
// /api/embeddings.
type fakeOllama struct {
	mu sync.Mutex

	generateCalls int
	response      string
}

func newFakeOllama(classificationJSON string) *fakeOllama {
	return &fakeOllama{response: classificationJSON}
}

func (f *fakeOllama) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generateCalls
}

func (f *fakeOllama) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/tags":
		_, _ = io.WriteString(w, `{"models":[{"name":"qwen3.5:9b"}]}`)
	case "/api/generate":
		_, _ = io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.generateCalls++
		response := f.response
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"response": response, "done_reason": "stop"})
	case "/api/embeddings":
		_, _ = io.Copy(io.Discard, r.Body)
		_, _ = io.WriteString(w, `{"embedding":[]}`)
	default:
		http.NotFound(w, r)
	}
}

// classificationJSON is the canned Step A/Step D answer: a bank-statement-like synthetic document
// classified to bank/bnp_paribas with a summary. BNP Paribas also appears in the extracted text,
// so the subcategory grounding check accepts the auto-created branch (Golden Rule 5).
const classificationJSON = `{"thinking":"compte courant","titre":"Releve de compte BNP Paribas","registre":"00012345678","date":"2026-01-10","categorie":"bank","subcategorie":"bnp_paribas","summary":"Releve de compte BNP Paribas: solde 1 234,56 EUR au 10 janvier 2026, compte courant 00012345678.","tags":["bank","bnp_paribas"],"total_amount":"1234.56","vat_amount":"","siren":"","iban":"","expiry_date":"","contact_name":"","contact_email":"","contact_phone":"","contact_address":"","contact_website":""}`

// goodStatementText / goodStatementMarkdown are the canned pdf2w output for the good file.
const (
	goodStatementText = "Releve de compte BNP Paribas. Solde au 10 janvier 2026 : 1 234,56 EUR. " +
		"Compte courant 00012345678. Virement salaire recu. Operations carte et prelevement."
	goodStatementMarkdown = "# Releve de compte BNP Paribas\n\n" +
		"Solde au 10 janvier 2026 : 1 234,56 EUR\n\n" +
		"- Compte courant 00012345678\n" +
		"- Virement salaire recu\n" +
		"- Operations carte et prelevement\n"
)

// --- test application ---------------------------------------------------------------------------

// newTestApplication builds the production dependency graph against temp directories and injected
// no-op process runners, so no Explorer/Chrome/Ollama is ever started. Callers must set
// PDF_INPUT_DIR / PDF_OUTPUT_DIR / PDF2W_SERVICE_URL / OLLAMA_HOST (t.Setenv) before calling.
func newTestApplication(t *testing.T, baseDir, dataDir string) *application {
	t.Helper()

	app, err := newApplication(appOptions{
		BaseDir:          baseDir,
		DataDir:          dataDir,
		OllamaSpawnServe: func() error { return nil },
		OllamaSleep:      func(time.Duration) {},
		Spawner:          noopSpawner{},
		McpOpener:        noopMcpOpener{},
		McpRunner:        noopRunner{},
	})
	if err != nil {
		t.Fatalf("newApplication: %v", err)
	}
	t.Cleanup(app.Close)
	return app
}

// noopMcpOpener satisfies mcpserver.Opener without launching anything.
type noopMcpOpener struct{}

// RevealInFileManager returns an empty plan; tests never launch it.
func (noopMcpOpener) RevealInFileManager(string) osopen.Plan { return osopen.Plan{} }

// --- SSE collector ------------------------------------------------------------------------------

// sseCollector subscribes to a triage/events stream and exposes the parsed event `type` values in
// order.
type sseCollector struct {
	types  chan string
	cancel context.CancelFunc
}

// startSSECollector opens the SSE stream at path and starts parsing `data: {json}` frames.
func startSSECollector(t *testing.T, baseURL, path string) *sseCollector {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+path, nil)
	if err != nil {
		cancel()
		t.Fatalf("new SSE request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		t.Fatalf("open SSE stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("SSE status = %d, want 200", response.StatusCode)
	}

	collector := &sseCollector{types: make(chan string, 256), cancel: cancel}
	go func() {
		defer response.Body.Close()
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var event struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
				continue
			}
			select {
			case collector.types <- event.Type:
			default:
			}
		}
	}()
	t.Cleanup(cancel)
	return collector
}

// waitFor reads event types until target appears (inclusive), or fails the test on timeout.
func (c *sseCollector) waitFor(t *testing.T, target string, timeout time.Duration) []string {
	t.Helper()
	deadline := time.After(timeout)
	var seen []string
	for {
		select {
		case eventType := <-c.types:
			seen = append(seen, eventType)
			if eventType == target {
				return seen
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %s; saw %v", target, seen)
		}
	}
}

// --- HTTP helpers -------------------------------------------------------------------------------

// postJSON performs a JSON request against a test server and returns the recorder.
func postJSON(t *testing.T, client *http.Client, method, url string, body any) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(payload)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if client == nil {
		client = http.DefaultClient
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return response
}

// runScanRequest POSTs /api/triage/scan and returns the decoded body.
func runScanRequest(t *testing.T, baseURL string) map[string]any {
	t.Helper()
	response := postJSON(t, nil, http.MethodPost, baseURL+"/api/triage/scan", map[string]any{})
	defer response.Body.Close()
	payload, _ := io.ReadAll(response.Body)
	var body map[string]any
	_ = json.Unmarshal(payload, &body)
	return body
}

// --- misc --------------------------------------------------------------------------------------

// setTempSettingsEnv points the settings store at temp dirs and service URLs, and clears the two
// env overrides that would otherwise take precedence over the explicit options.
func setTempSettingsEnv(t *testing.T, inputDir, outputDir, pdf2wURL, ollamaHost string) {
	t.Helper()
	t.Setenv("PDF_TRIAGE_BASE_DIR", "")
	t.Setenv("PDF_TRIAGE_DATA_DIR", "")
	t.Setenv("PDF_INPUT_DIR", inputDir)
	t.Setenv("PDF_OUTPUT_DIR", outputDir)
	t.Setenv("PDF2W_SERVICE_URL", pdf2wURL)
	t.Setenv("OLLAMA_HOST", ollamaHost)
	t.Setenv("OLLAMA_MODEL", "qwen3.5:9b")
}

// startTestServer serves the app handler on an ephemeral 127.0.0.1 port.
func startTestServer(t *testing.T, app *application) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(app.handler)
	t.Cleanup(server.Close)
	return server
}

// fileExists reports whether path exists.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// countFiles walks dir and counts regular files.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			count++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return count
}
