package pdf2w

import (
	"context"
	"errors"
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

// capturedRequest records the wire-level request the client sent, so a test can assert the exact
// bytes/fields the TypeScript SDK's fetch would have sent.
type capturedRequest struct {
	Method      string
	Path        string
	ContentType string
	FileName    string
	Body        []byte
}

func writeTempFile(t *testing.T, name, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}
	return p
}

func captureHandler(t *testing.T, captured *capturedRequest, mu *sync.Mutex, status int, body string) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		*captured = capturedRequest{
			Method:      r.Method,
			Path:        r.URL.Path,
			ContentType: r.Header.Get("Content-Type"),
			FileName:    r.Header.Get("x-file-name"),
			Body:        raw,
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// TS: pdf2w-remote.test.ts "posts the file and returns the extraction result". The TS suite mocks
// global.fetch; the Go port pins the same contract against a real httptest server and additionally
// asserts the request method, path, headers and raw body bytes.
func TestExtractContentPostsFileAndMapsResult(t *testing.T) {
	var mu sync.Mutex
	var captured capturedRequest
	srv := httptest.NewServer(captureHandler(t, &captured, &mu, http.StatusOK,
		`{"markdown":"# Facture","text":"Facture SFR","numpages":1,"checksum":"abc"}`))
	defer srv.Close()

	filePath := writeTempFile(t, "facture sfr.pdf", "%PDF-1.4 fake content for hashing")
	result, err := ExtractContent(filePath, srv.URL, 0)
	if err != nil {
		t.Fatalf("ExtractContent returned error: %v", err)
	}

	if result.RawText != "Facture SFR" {
		t.Errorf("RawText = %q, want %q", result.RawText, "Facture SFR")
	}
	if result.Pdf2wMarkdown != "# Facture" {
		t.Errorf("Pdf2wMarkdown = %q, want %q", result.Pdf2wMarkdown, "# Facture")
	}
	if result.Numpages != 1 {
		t.Errorf("Numpages = %d, want 1", result.Numpages)
	}
	if result.Checksum != "abc" {
		t.Errorf("Checksum = %q, want %q", result.Checksum, "abc")
	}

	mu.Lock()
	defer mu.Unlock()
	if captured.Method != http.MethodPost {
		t.Errorf("request method = %q, want POST", captured.Method)
	}
	if captured.Path != "/convert" {
		t.Errorf("request path = %q, want /convert", captured.Path)
	}
	if captured.ContentType != "application/octet-stream" {
		t.Errorf("content-type = %q, want application/octet-stream", captured.ContentType)
	}
	if captured.FileName != "facture%20sfr.pdf" {
		t.Errorf("x-file-name = %q, want facture%%20sfr.pdf", captured.FileName)
	}
	if string(captured.Body) != "%PDF-1.4 fake content for hashing" {
		t.Errorf("body = %q, want the raw file bytes", string(captured.Body))
	}
}

// TS: pdf2w-remote.test.ts "throws when the service is unreachable (no fallback)". Node's fetch
// rejects with an ECONNREFUSED Error; Go's net/http returns a *url.Error. The TS assertion is only
// that the call rejects, so the port asserts a non-nil error rather than a platform-specific string.
func TestExtractContentErrorsWhenServiceUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close()

	filePath := writeTempFile(t, "doc.pdf", "bytes")
	if _, err := ExtractContent(filePath, url, 0); err == nil {
		t.Fatal("ExtractContent succeeded against an unreachable service; want an error (no fallback)")
	}
}

// TS: pdf2w-remote.test.ts "throws when PDF2W_SERVICE_URL is not configured". The URL check runs
// before the file is read, exactly as in TS.
func TestExtractContentErrorsWhenURLNotConfigured(t *testing.T) {
	filePath := writeTempFile(t, "doc.pdf", "bytes")
	_, err := ExtractContent(filePath, "", 0)
	if err == nil || !strings.Contains(err.Error(), "PDF2W_SERVICE_URL is not configured") {
		t.Fatalf("err = %v, want it to contain %q", err, "PDF2W_SERVICE_URL is not configured")
	}
}

// Added case: the x-file-name header is encodeURIComponent(path.basename(filePath)) and a trailing
// slash on the base URL is stripped (TS `.replace(/\/+$/, ”)`).
func TestExtractContentEncodesFilenameAndTrimsBaseURL(t *testing.T) {
	var mu sync.Mutex
	var captured capturedRequest
	srv := httptest.NewServer(captureHandler(t, &captured, &mu, http.StatusOK, `{"markdown":"m"}`))
	defer srv.Close()

	filePath := writeTempFile(t, "fichier été (1)+#.pdf", "x")
	if _, err := ExtractContent(filePath, srv.URL+"///", 0); err != nil {
		t.Fatalf("ExtractContent returned error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if captured.Path != "/convert" {
		t.Errorf("request path = %q, want /convert", captured.Path)
	}
	// encodeURIComponent leaves ! ' ( ) * - . _ ~ alone; space->%20, +->%2B, #->%23, é->%C3%A9.
	want := "fichier%20%C3%A9t%C3%A9%20(1)%2B%23.pdf"
	if captured.FileName != want {
		t.Errorf("x-file-name = %q, want %q", captured.FileName, want)
	}
}

// Added case: non-2xx responses carry the status/statusText and the basename, matching the TS
// template `pdf2w service returned ${res.status} ${res.statusText} for '${basename}'`.
func TestExtractContentErrorsOnNonOKStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "facture.pdf", "bytes")
	_, err := ExtractContent(filePath, srv.URL, 0)
	want := "pdf2w service returned 500 Internal Server Error for 'facture.pdf'"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
}

// Added case: a JSON body without a markdown string is rejected with the exact TS message.
func TestExtractContentErrorsWhenMarkdownMissing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"text":"only text","numpages":2}`)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "doc.pdf", "bytes")
	_, err := ExtractContent(filePath, srv.URL, 0)
	if err == nil || err.Error() != "pdf2w service returned an unexpected response shape (missing markdown)" {
		t.Fatalf("err = %v, want the missing-markdown error", err)
	}
}

// Added case: the field-by-field mapping/fallbacks of the TS return object, including the JS-falsy
// and type guards (`typeof data.checksum === 'string'`, `data.numpages >= 1`, object info).
func TestExtractContentAppliesFieldFallbacks(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		checksum string
		rawText  string
		numpages int
		info     any
	}{
		{
			name:     "markdown only",
			body:     `{"markdown":"only markdown"}`,
			checksum: "",
			rawText:  "only markdown",
			numpages: 1,
			info:     map[string]any{},
		},
		{
			name:     "wrong types fall back",
			body:     `{"markdown":"m","checksum":123,"text":42,"numpages":"3","info":null}`,
			checksum: "",
			rawText:  "m",
			numpages: 1,
			info:     map[string]any{},
		},
		{
			name:     "zero numpages falls back",
			body:     `{"markdown":"m","numpages":0}`,
			checksum: "",
			rawText:  "m",
			numpages: 1,
			info:     map[string]any{},
		},
		{
			name:     "info object preserved",
			body:     `{"markdown":"m","info":{"title":"Facture","pages":3}}`,
			checksum: "",
			rawText:  "m",
			numpages: 1,
			info:     map[string]any{"title": "Facture", "pages": float64(3)},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			filePath := writeTempFile(t, "doc.pdf", "bytes")
			result, err := ExtractContent(filePath, srv.URL, 0)
			if err != nil {
				t.Fatalf("ExtractContent returned error: %v", err)
			}
			if result.Checksum != tc.checksum {
				t.Errorf("Checksum = %q, want %q", result.Checksum, tc.checksum)
			}
			if result.RawText != tc.rawText {
				t.Errorf("RawText = %q, want %q", result.RawText, tc.rawText)
			}
			if result.Numpages != tc.numpages {
				t.Errorf("Numpages = %d, want %d", result.Numpages, tc.numpages)
			}
			if !reflect.DeepEqual(result.Info, tc.info) {
				t.Errorf("Info = %#v, want %#v", result.Info, tc.info)
			}
		})
	}
}

// Added case: the PDF2W_SERVICE_TIMEOUT_MS behavior (`AbortSignal.timeout`) is implemented in Go
// with a context deadline; a slow service must abort.
func TestExtractContentTimesOutViaContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"markdown":"late"}`)
	}))
	defer srv.Close()

	filePath := writeTempFile(t, "doc.pdf", "bytes")
	_, err := ExtractContent(filePath, srv.URL, 50*time.Millisecond)
	if err == nil {
		t.Fatal("ExtractContent did not time out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want it to wrap context.DeadlineExceeded", err)
	}
}

// Added case: encodeURIComponent is a faithful port of the JS algorithm, not url.QueryEscape.
func TestEncodeURIComponent(t *testing.T) {
	cases := map[string]string{
		"simple.pdf":         "simple.pdf",
		"a b.pdf":            "a%20b.pdf",
		"!*'();:@&=+$,/?#[]": "!*'()%3B%3A%40%26%3D%2B%24%2C%2F%3F%23%5B%5D",
		"é":                  "%C3%A9",
		"~-_.":               "~-_.",
	}
	for in, want := range cases {
		if got := encodeURIComponent(in); got != want {
			t.Errorf("encodeURIComponent(%q) = %q, want %q", in, got, want)
		}
	}
}
