// Golden-file contract replay for the document READ/EXPORT route group (part 2a).
//
// Each httpapi/testdata/golden/docread_*.json was captured from the REAL TypeScript
// createWebServer() mounted with supertest by the throwaway scratch/capture-docread-golden.test.ts
// harness (same mocks as web-server.test.ts). Every case replays the same request against this
// package's Go handler built through NewServer + DocumentReadRoutes and asserts the status,
// Content-Type, Content-Disposition and a structurally identical body.
//
// The export routes' date-stamped Content-Disposition values are normalized (YYYY-MM-DD -> <DATE>)
// on both sides, because the capture date and the replay date differ. The CSV body is compared as
// text, not parsed, so the UTF-8 BOM, the exact quoting and the CRLF separator are all pinned.
package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

type docReadGolden struct {
	Status      int    `json:"status"`
	ContentType string `json:"contentType"`
	Disposition string `json:"disposition"`
	Body        any    `json:"body"`
}

func loadDocReadGolden(t *testing.T, name string) docReadGolden {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "golden", name+".json"))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	var out docReadGolden
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("parse golden %s: %v", name, err)
	}
	return out
}

var docReadDateRe = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

func maskDocReadDate(value string) string {
	return docReadDateRe.ReplaceAllString(value, "<DATE>")
}

// docReadOptionalKeys are the document fields JSON.stringify drops when the column is NULL, but
// which Go's DocumentRecord always emits as "". Dropping empty values on BOTH sides keeps the
// golden comparison to the frozen present-value contract, exactly as golden_test.go's
// normalizeGolden does for its own optional keys.
var docReadOptionalKeys = map[string]bool{
	"total_amount": true, "vat_amount": true, "siren": true, "iban": true, "expiry_date": true,
	"contact_name": true, "contact_email": true, "contact_phone": true,
	"contact_address": true, "contact_website": true,
}

// normalizeDocReadGolden drops empty optional document fields on both sides.
func normalizeDocReadGolden(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := map[string]any{}
		for key, child := range v {
			if docReadOptionalKeys[key] && (child == nil || child == "") {
				continue
			}
			out[key] = normalizeDocReadGolden(child)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, child := range v {
			out = append(out, normalizeDocReadGolden(child))
		}
		return out
	default:
		return value
	}
}

// TestDocumentReadGolden replays every captured document READ/EXPORT exchange.
func TestDocumentReadGolden(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*docReadEnv)
		method string
		target string
		body   any
	}{
		{
			name: "docread_get-documents",
			setup: func(env *docReadEnv) {
				env.docs.docs = []database.DocumentRecord{docReadSample()}
			},
			method: http.MethodGet, target: "/api/documents",
		},
		{
			name: "docread_get-document",
			setup: func(env *docReadEnv) {
				doc := docReadSample()
				env.docs.byID[1] = &doc
			},
			method: http.MethodGet, target: "/api/documents/1",
		},
		{
			name:   "docread_get-document-404",
			method: http.MethodGet, target: "/api/documents/999",
		},
		{
			name: "docread_get-document-markdown",
			setup: func(env *docReadEnv) {
				doc := docReadSample(func(d *database.DocumentRecord) { d.Title = "Avis de Taxes Foncières" })
				env.docs.byID[1] = &doc
			},
			method: http.MethodGet, target: "/api/documents/1/markdown",
		},
		{
			name: "docread_get-documents-export-csv",
			setup: func(env *docReadEnv) {
				env.docs.docs = []database.DocumentRecord{docReadSample()}
			},
			method: http.MethodGet, target: "/api/documents/export/csv",
		},
		{
			name: "docread_get-mcp-status",
			setup: func(env *docReadEnv) {
				env.mcp.tools = []McpToolInfo{
					{Name: "search_documents", Description: "Search the registry"},
					{Name: "list_categories", Description: "List categories"},
				}
			},
			method: http.MethodGet, target: "/api/mcp/status",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			golden := loadDocReadGolden(t, tc.name)
			env := newDocReadEnv()
			if tc.setup != nil {
				tc.setup(env)
			}
			rec := doJSON(t, env.handler, tc.method, tc.target, tc.body, nil)

			if rec.Code != golden.Status {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, golden.Status, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != golden.ContentType {
				t.Fatalf("Content-Type = %q, want %q", got, golden.ContentType)
			}
			if golden.Disposition != "" {
				got := maskDocReadDate(rec.Header().Get("Content-Disposition"))
				want := maskDocReadDate(golden.Disposition)
				if got != want {
					t.Fatalf("Content-Disposition = %q, want %q", got, want)
				}
			}

			if wantText, ok := golden.Body.(string); ok {
				if got := rec.Body.String(); got != wantText {
					t.Fatalf("body = %q, want %q", got, wantText)
				}
				return
			}

			var got any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode Go body %q: %v", rec.Body.String(), err)
			}
			assertGoldenEqual(t, "body", normalizeDocReadGolden(golden.Body), normalizeDocReadGolden(got), nil)
		})
	}
}
