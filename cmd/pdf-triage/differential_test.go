package main

// differential_test.go — CUTOVER DIFFERENTIAL CHECK (spec:
// docs/superpowers/specs/2026-09-18-go-backend-migration-design.md Decision 5;
// docs/superpowers/specs/2026-09-18-http-parity-audit.md).
//
// This test is SKIPPED unless both env vars point at run-scoped artifacts produced by the
// throwaway TypeScript fixture harness:
//
//	PDF_TRIAGE_DIFF_DB_GO       path to the Go-side byte copy of pdf_triage.db
//	PDF_TRIAGE_DIFF_TS_FIXTURE  path to the TS-side JSON fixture
//
// It builds the real application through the cmd/pdf-triage composition root over the Go copy
// (fresh empty base/data/input/output temp dirs, synthetic committed taxonomy), serves it on an
// httptest 127.0.0.1:0 listener, replays every request in the fixture, and compares status,
// content-type, content-disposition and body after a shared normalization (temp-dir paths,
// timestamps, DB size). The committed file contains no real data; everything is read from the
// fixture and the DB copy at run time. It never touches the live pdf_triage.db.
//
// The test prints only route names, counts and diff CLASSES (JSON pointer + kind), never values.

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

type diffFixture struct {
	CategoriesJSON string        `json:"categoriesJson"`
	SampleIDs      []int64       `json:"sampleIds"`
	MarkdownIDs    []int64       `json:"markdownIds"`
	Requests       []diffRequest `json:"requests"`
}

type diffRequest struct {
	Name               string          `json:"name"`
	Method             string          `json:"method"`
	Path               string          `json:"path"`
	Kind               string          `json:"kind"`
	Status             int             `json:"status"`
	ContentType        string          `json:"contentType"`
	ContentDisposition string          `json:"contentDisposition"`
	Body               json.RawMessage `json:"body"`
	BodyB64            string          `json:"bodyB64"`
	Entries            []zipEntry      `json:"entries"`
}

type zipEntry struct {
	Name   string `json:"name"`
	Sha256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

type pathReplacement struct{ from, to string }

var diffTimestampRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:?\d{2})?`)

func TestDifferentialParity(t *testing.T) {
	dbPath := os.Getenv("PDF_TRIAGE_DIFF_DB_GO")
	fixturePath := os.Getenv("PDF_TRIAGE_DIFF_TS_FIXTURE")
	if dbPath == "" || fixturePath == "" {
		t.Skip("set PDF_TRIAGE_DIFF_DB_GO and PDF_TRIAGE_DIFF_TS_FIXTURE to run the cutover differential check")
	}

	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx diffFixture
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}

	// Fresh EMPTY base/data/input/output dirs, under the fixture's own scratch directory, so no
	// server ever sees the operator's __raws/__archive. Removed when the test ends.
	runRoot := filepath.Join(filepath.Dir(fixturePath), "go-run")
	t.Cleanup(func() { _ = os.RemoveAll(runRoot) })
	baseDir := filepath.Join(runRoot, "base")
	dataDir := filepath.Join(runRoot, "data")
	inputDir := filepath.Join(runRoot, "raws")
	outputDir := filepath.Join(runRoot, "archive")
	for _, dir := range []string{baseDir, dataDir, inputDir, outputDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(baseDir, "categories.json"), []byte(fx.CategoriesJSON), 0o644); err != nil {
		t.Fatalf("write synthetic categories.json: %v", err)
	}

	// Point the composition root at the Go DB copy and fresh empty dirs. Clearing the BASE/DATA
	// overrides lets the explicit Options values win, exactly as the existing test helpers do.
	t.Setenv("PDF_TRIAGE_BASE_DIR", "")
	t.Setenv("PDF_TRIAGE_DATA_DIR", "")
	t.Setenv("PDF_INPUT_DIR", inputDir)
	t.Setenv("PDF_OUTPUT_DIR", outputDir)
	t.Setenv("PDF_DB_PATH", dbPath)
	t.Setenv("PDF_REGISTRY_PATH", filepath.Join(dataDir, "registry.json"))
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")
	t.Setenv("OLLAMA_MODEL", "qwen3.5:9b")
	t.Setenv("PDF2W_SERVICE_URL", "http://127.0.0.1:1")

	app := newTestApplication(t, baseDir, dataDir)
	server := startTestServer(t, app)

	replacements := []pathReplacement{
		{inputDir, "<INPUT>"},
		{outputDir, "<OUTPUT>"},
		{baseDir, "<BASE>"},
		{dataDir, "<DATA>"},
	}
	sort.Slice(replacements, func(i, j int) bool { return len(replacements[i].from) > len(replacements[j].from) })

	type tally struct{ pass, diff int }
	byFamily := map[string]*tally{}
	familyOrder := []string{}
	record := func(family string, passed bool) {
		tl, ok := byFamily[family]
		if !ok {
			tl = &tally{}
			byFamily[family] = tl
			familyOrder = append(familyOrder, family)
		}
		if passed {
			tl.pass++
		} else {
			tl.diff++
		}
	}

	for _, req := range fx.Requests {
		req := req
		t.Run(req.Name, func(t *testing.T) {
			diffs := runOneDiff(t, server, req, replacements)
			record(diffFamily(req.Name), len(diffs) == 0)
			for _, d := range diffs {
				t.Errorf("DIFF %s: %s", req.Name, d)
			}
		})
	}

	totalPass, totalDiff := 0, 0
	var b strings.Builder
	fmt.Fprintf(&b, "DIFFERENTIAL SUMMARY\n")
	for _, fam := range familyOrder {
		tl := byFamily[fam]
		fmt.Fprintf(&b, "  %-22s PASS=%d DIFF=%d\n", fam, tl.pass, tl.diff)
		totalPass += tl.pass
		totalDiff += tl.diff
	}
	fmt.Fprintf(&b, "  %-22s PASS=%d DIFF=%d\n", "TOTAL", totalPass, totalDiff)
	t.Log("\n" + b.String())
}

// diffFamily maps a fixture request name to its route family.
func diffFamily(name string) string {
	switch {
	case strings.HasPrefix(name, "document_markdown_"):
		return "GET /:id/markdown"
	case name == "documents_all" || strings.HasPrefix(name, "documents_"):
		return "GET /api/documents"
	case name == "document_missing" || strings.HasPrefix(name, "document_"):
		return "GET /api/documents/:id"
	case name == "export_csv":
		return "GET export/csv"
	case name == "export_markdown":
		return "GET export/markdown"
	default:
		return name
	}
}

// runOneDiff issues one fixture request and returns a list of diff descriptors (empty = pass).
func runOneDiff(t *testing.T, server *httptest.Server, req diffRequest, repls []pathReplacement) []string {
	t.Helper()

	httpReq, err := http.NewRequest(req.Method, server.URL+req.Path, nil)
	if err != nil {
		return []string{"request-build: " + err.Error()}
	}
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return []string{"request-send: " + err.Error()}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return []string{"body-read: " + err.Error()}
	}

	var diffs []string
	if resp.StatusCode != req.Status {
		diffs = append(diffs, fmt.Sprintf("/status got=%d want=%d", resp.StatusCode, req.Status))
	}
	gotCT := resp.Header.Get("Content-Type")
	if gotCT != req.ContentType {
		diffs = append(diffs, fmt.Sprintf("/content-type got-class=%s want-class=%s", contentTypeClass(gotCT), contentTypeClass(req.ContentType)))
	}
	if req.ContentDisposition != "" {
		gotCD := resp.Header.Get("Content-Disposition")
		if gotCD != req.ContentDisposition {
			diffs = append(diffs, "/content-disposition value")
		}
	}

	switch req.Kind {
	case "json":
		var want any
		if err := json.Unmarshal(req.Body, &want); err != nil {
			return append(diffs, "fixture-body-decode: "+err.Error())
		}
		var got any
		if err := json.Unmarshal(body, &got); err != nil {
			return append(diffs, "go-body-decode: "+err.Error())
		}
		gotJSON := normalizeDeep(got, repls)
		wantJSON := normalizeDeep(want, repls)
		if req.Name == "system_stats" {
			// The two DB copies are distinct files whose on-disk size reflects WAL/checkpoint
			// timing, not behavior; both harnesses zero this field (see the TS generator).
			zeroDatabaseSize(gotJSON)
			zeroDatabaseSize(wantJSON)
		}
		diffs = append(diffs, compareValue(gotJSON, wantJSON, "")...)
	case "bytes":
		want, err := decodeB64(req.BodyB64)
		if err != nil {
			return append(diffs, "fixture-bytes-decode: "+err.Error())
		}
		gotBytes := body
		if !bytes.Equal(gotBytes, want) {
			diffs = append(diffs, fmt.Sprintf("/body-bytes len got=%d want=%d first-mismatch=%d", len(gotBytes), len(want), firstByteMismatch(gotBytes, want)))
		}
	case "text":
		want, err := decodeB64String(req.Body)
		if err != nil {
			return append(diffs, "fixture-text-decode: "+err.Error())
		}
		gotText := normalizeString(string(body), repls)
		wantText := normalizeString(want, repls)
		if gotText != wantText {
			diffs = append(diffs, fmt.Sprintf("/body-text len got=%d want=%d", len(gotText), len(wantText)))
		}
	case "zip":
		gotEntries, err := zipEntries(body)
		if err != nil {
			return append(diffs, "go-zip-decode: "+err.Error())
		}
		if len(gotEntries) != len(req.Entries) {
			diffs = append(diffs, fmt.Sprintf("/zip-entries count got=%d want=%d", len(gotEntries), len(req.Entries)))
			break
		}
		for i := range gotEntries {
			if gotEntries[i].Name != req.Entries[i].Name {
				diffs = append(diffs, fmt.Sprintf("/zip-entries/%d/name", i))
			} else if gotEntries[i].Sha256 != req.Entries[i].Sha256 {
				diffs = append(diffs, fmt.Sprintf("/zip-entries/%d/sha256", i))
			}
		}
	default:
		diffs = append(diffs, "unknown-kind: "+req.Kind)
	}

	if len(diffs) > 12 {
		diffs = append(diffs[:12], fmt.Sprintf("...(+%d more)", len(diffs)-12))
	}
	return diffs
}

// --- normalization (mirrors the TS harness) -----------------------------------------------------

func normalizeDeep(value any, repls []pathReplacement) any {
	switch v := value.(type) {
	case string:
		return normalizeString(v, repls)
	case []any:
		out := make([]any, len(v))
		for i := range v {
			out[i] = normalizeDeep(v[i], repls)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, item := range v {
			out[k] = normalizeDeep(item, repls)
		}
		return out
	default:
		return value
	}
}

func normalizeString(s string, repls []pathReplacement) string {
	s = diffTimestampRe.ReplaceAllString(s, "<TS>")
	for _, r := range repls {
		s = strings.ReplaceAll(s, r.from, r.to)
	}
	return s
}

// zeroDatabaseSize canonicalizes the /api/system/stats database size, which is a property of the
// particular DB copy on disk rather than of server behavior.
func zeroDatabaseSize(v any) {
	m, ok := v.(map[string]any)
	if !ok {
		return
	}
	if _, present := m["database"]; present {
		m["database"] = map[string]any{"bytes": float64(0), "sizeFormatted": "0 B"}
	}
}

// --- deep comparison ----------------------------------------------------------------------------

// compareValue returns structural diff descriptors (JSON-pointer + class), never values.
func compareValue(got, want any, path string) []string {
	if reflect.DeepEqual(got, want) {
		return nil
	}
	switch g := got.(type) {
	case map[string]any:
		w, ok := want.(map[string]any)
		if !ok {
			return []string{fmt.Sprintf("%s/type got=%T want=%T", path, got, want)}
		}
		var diffs []string
		keys := map[string]bool{}
		for k := range g {
			keys[k] = true
		}
		for k := range w {
			keys[k] = true
		}
		ordered := make([]string, 0, len(keys))
		for k := range keys {
			ordered = append(ordered, k)
		}
		sort.Strings(ordered)
		for _, k := range ordered {
			gv, gok := g[k]
			wv, wok := w[k]
			switch {
			case gok && !wok:
				diffs = append(diffs, path+"/"+k+"/extra-in-go")
			case !gok && wok:
				diffs = append(diffs, path+"/"+k+"/missing-in-go")
			default:
				diffs = append(diffs, compareValue(gv, wv, path+"/"+k)...)
			}
		}
		return diffs
	case []any:
		w, ok := want.([]any)
		if !ok {
			return []string{fmt.Sprintf("%s/type got=%T want=%T", path, got, want)}
		}
		if len(g) != len(w) {
			return []string{fmt.Sprintf("%s/array-length got=%d want=%d", path, len(g), len(w))}
		}
		var diffs []string
		for i := range g {
			diffs = append(diffs, compareValue(g[i], w[i], fmt.Sprintf("%s/%d", path, i))...)
		}
		return diffs
	default:
		return []string{fmt.Sprintf("%s/value got-type=%T want-type=%T", path, got, want)}
	}
}

func contentTypeClass(ct string) string {
	if ct == "" {
		return "none"
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		return ct[:i]
	}
	return ct
}

func firstByteMismatch(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

func decodeB64(s string) ([]byte, error) {
	if s == "" {
		return []byte{}, nil
	}
	return base64.StdEncoding.DecodeString(s)
}

func decodeB64String(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", err
	}
	return s, nil
}

func zipEntries(data []byte) ([]zipEntry, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, err
	}
	out := make([]zipEntry, 0, len(reader.File))
	for _, f := range reader.File {
		rc, err := f.Open()
		if err != nil {
			return nil, err
		}
		content, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(content)
		out = append(out, zipEntry{Name: f.Name, Sha256: hex.EncodeToString(sum[:]), Bytes: len(content)})
	}
	return out, nil
}
