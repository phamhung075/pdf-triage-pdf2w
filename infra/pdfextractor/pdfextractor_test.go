package pdfextractor

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "extractor-test.pdf")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return p
}

func jsonServer(t *testing.T, paths *[]string, mu *sync.Mutex, status int, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*paths = append(*paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
}

// TS: pdf-extractor.test.ts "maps the pdf2w result onto the ExtractedPDF contract". The TS test
// mocks pdf2w-remote and clean-text-remote; the Go port drives the real infra/pdf2w client against
// an httptest server and cleans text in-process with the existing cleantext package.
func TestExtractPDFContentMapsResult(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := jsonServer(t, &paths, &mu, http.StatusOK,
		`{"checksum":"remote-checksum","markdown":"# Facture","text":"line one\n\n\n\nline two","numpages":3,"info":{"title":"Facture"}}`)
	defer srv.Close()

	filePath := writeTempFile(t, "actual bytes on disk")
	result, err := ExtractPDFContent(filePath, Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("ExtractPDFContent returned error: %v", err)
	}

	if result.Checksum != "remote-checksum" {
		t.Errorf("Checksum = %q, want %q", result.Checksum, "remote-checksum")
	}
	if result.RawText != "line one\n\nline two" {
		t.Errorf("RawText = %q, want %q (3+ newlines collapsed in-process)", result.RawText, "line one\n\nline two")
	}
	if result.Numpages != 3 {
		t.Errorf("Numpages = %d, want 3", result.Numpages)
	}
	if !reflect.DeepEqual(result.Info, map[string]any{"title": "Facture"}) {
		t.Errorf("Info = %#v, want map[title:Facture]", result.Info)
	}
	if result.Pdf2wMarkdown != "# Facture" {
		t.Errorf("Pdf2wMarkdown = %q, want %q", result.Pdf2wMarkdown, "# Facture")
	}

	// In-process cleaning: only /convert is hit; the old /clean-text HTTP hop is gone.
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(paths, []string{"/convert"}) {
		t.Errorf("requested paths = %v, want only [/convert] (cleaning is in-process)", paths)
	}
}

// TS: pdf-extractor.test.ts "recomputes the sha256 locally when the service reports no checksum".
func TestExtractPDFContentRecomputesChecksumWhenMissing(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := jsonServer(t, &paths, &mu, http.StatusOK,
		`{"checksum":"","markdown":"some extracted text","numpages":1,"info":{}}`)
	defer srv.Close()

	filePath := writeTempFile(t, "bytes-to-hash")
	result, err := ExtractPDFContent(filePath, Config{BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("ExtractPDFContent returned error: %v", err)
	}
	sum := sha256.Sum256([]byte("bytes-to-hash"))
	want := hex.EncodeToString(sum[:])
	if result.Checksum != want {
		t.Errorf("Checksum = %q, want %q", result.Checksum, want)
	}
}

// TS: pdf-extractor.test.ts "propagates a pdf2w failure unchanged — there is no in-process fallback".
func TestExtractPDFContentPropagatesFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	filePath := writeTempFile(t, "bytes")
	if _, err := ExtractPDFContent(filePath, Config{BaseURL: url}); err == nil {
		t.Fatal("ExtractPDFContent succeeded against an unreachable service; want a propagated error")
	}
}

// Added case: an HTTP error is propagated with the pdf2w error message, unchanged.
func TestExtractPDFContentPropagatesHTTPError(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := jsonServer(t, &paths, &mu, http.StatusInternalServerError,
		`{"error":"pdf2w exploded"}`)
	defer srv.Close()

	filePath := writeTempFile(t, "bytes")
	_, err := ExtractPDFContent(filePath, Config{BaseURL: srv.URL})
	if err == nil || err.Error() != "pdf2w service returned 500 Internal Server Error for 'extractor-test.pdf'" {
		t.Fatalf("err = %v, want the pdf2w non-OK error", err)
	}
}

// Added case: Config.Timeout reaches the pdf2w client's context deadline.
func TestExtractPDFContentPropagatesTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"markdown":"late"}`)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "bytes")
	if _, err := ExtractPDFContent(filePath, Config{BaseURL: srv.URL, Timeout: 50 * time.Millisecond}); err == nil {
		t.Fatal("ExtractPDFContent did not time out")
	}
}

// Added case: the logger hook is optional and receives the pdf2w provenance line without affecting
// the result.
func TestExtractPDFContentCallsOptionalLogger(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := jsonServer(t, &paths, &mu, http.StatusOK, `{"markdown":"hello world, this is long enough"}`)
	defer srv.Close()

	filePath := writeTempFile(t, "bytes")
	var logged []string
	logger := &recordingLogger{lines: &logged}
	if _, err := ExtractPDFContent(filePath, Config{BaseURL: srv.URL, Logger: logger}); err != nil {
		t.Fatalf("ExtractPDFContent returned error: %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "Extraction delegated to pdf2w") {
		t.Fatalf("logged = %v, want one pdf2w delegation line", logged)
	}
}

type recordingLogger struct {
	lines *[]string
}

func (l *recordingLogger) Info(moduleName, message string, meta any, filename ...string) {
	*l.lines = append(*l.lines, moduleName+" "+message)
}

// TS: micro-prompt-pipeline.test.ts "sanitizeDocumentNoise removes browser extension and PDF
// properties litter" (the pure function is exported from pdf-extractor.ts:25).
func TestSanitizeDocumentNoiseRemovesLitter(t *testing.T) {
	rawWithLitter := `[Propriétés Document: chrome-extension___mhjfbmdgcfjbbpaeojofohoefgiehjai_edge_pdf_index.html | Jane Doe]
[OCR Extracted Text]
QPtmp000123
BULLETIN DE SALAIRE
SAS GLOBEX CONSEIL`

	cleaned := SanitizeDocumentNoise(rawWithLitter)
	if strings.Contains(cleaned, "[Propriétés Document:") {
		t.Errorf("cleaned still contains the PDF properties litter: %q", cleaned)
	}
	if strings.Contains(cleaned, "[OCR Extracted Text]") {
		t.Errorf("cleaned still contains the OCR marker: %q", cleaned)
	}
	if strings.Contains(cleaned, "QPtmp000123") {
		t.Errorf("cleaned still contains the QPtmp litter: %q", cleaned)
	}
	if !strings.Contains(cleaned, "BULLETIN DE SALAIRE") {
		t.Errorf("cleaned lost the real content: %q", cleaned)
	}
	if !strings.Contains(cleaned, "SAS GLOBEX CONSEIL") {
		t.Errorf("cleaned lost the real content: %q", cleaned)
	}
}

// Added case: whitespace normalization — [ \t]+\n -> \n and \n{3,} -> \n\n, then trim; the JS
// regexes are /gim so the line-anchored strips are case-insensitive and multiline.
func TestSanitizeDocumentNoiseNormalizesWhitespaceAndCase(t *testing.T) {
	in := "line one   \nline two\t\n\n\n\n[ocr extracted text]\nqptmp999\n\n\n"
	want := "line one\nline two"
	if got := SanitizeDocumentNoise(in); got != want {
		t.Errorf("SanitizeDocumentNoise(%q) = %q, want %q", in, got, want)
	}
}

// Added case: empty input returns the empty string (TS `if (!text) return ”`).
func TestSanitizeDocumentNoiseEmpty(t *testing.T) {
	if got := SanitizeDocumentNoise(""); got != "" {
		t.Errorf("SanitizeDocumentNoise(\"\") = %q, want empty", got)
	}
}
