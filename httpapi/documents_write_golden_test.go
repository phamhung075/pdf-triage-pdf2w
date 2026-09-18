package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
)

// TestGoldenDocWriteContract replays the hand-derived golden fixtures for the mutation routes.
//
// Every expected body below is the exact TypeScript literal from web-server.ts (the 409 message at
// :257/:1348/:1484, the chat 400 at :933, the merge 400 at :893, the split 400 at :1037, the import
// 400 at :855, the rename 400 at :654, the forbidden-subcategory 400 at :1263, and the delete 404
// from relocalize-document.ts:348). They pin the frozen contract without a live TS server; the
// part-1 capture harness had already been deleted when this job ran, so these are derived from the
// source, not captured.
func TestGoldenDocWriteContract(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(*writeEnv)
		method string
		target string
		body   any
		raw    []byte
	}{
		{
			name: "docwrite_post-triage-scan-409",
			setup: func(env *writeEnv) {
				env.srv.tryBeginScan()
			},
			method: http.MethodPost, target: "/api/triage/scan",
		},
		{
			name: "docwrite_post-registry-repair-409",
			setup: func(env *writeEnv) {
				env.srv.tryBeginScan()
			},
			method: http.MethodPost, target: "/api/registry/repair",
		},
		{
			name: "docwrite_delete-documents-409",
			setup: func(env *writeEnv) {
				env.srv.tryBeginScan()
			},
			method: http.MethodDelete, target: "/api/documents",
		},
		{
			name:   "docwrite_post-chat-400",
			method: http.MethodPost, target: "/api/chat",
			body: map[string]any{"message": ""},
		},
		{
			name:   "docwrite_post-merge-400",
			method: http.MethodPost, target: "/api/pdf/merge",
			body: map[string]any{"filepaths": []any{}},
		},
		{
			name:   "docwrite_post-split-400",
			method: http.MethodPost, target: "/api/pdf/split",
			body: map[string]any{},
		},
		{
			name:   "docwrite_post-images-import-400",
			method: http.MethodPost, target: "/api/images/import?filename=evil.exe",
			raw: []byte("MZ"),
		},
		{
			name:   "docwrite_post-subcategories-rename-400",
			method: http.MethodPost, target: "/api/subcategories/rename",
			body: map[string]any{"category": "invoices"},
		},
		{
			name: "docwrite_put-document-400",
			setup: func(env *writeEnv) {
				env.db.docs[1] = samplePutDoc()
			},
			method: http.MethodPut, target: "/api/documents/1",
			body: map[string]any{"subcategory": "general"},
		},
		{
			name: "docwrite_delete-document-404",
			setup: func(env *writeEnv) {
				env.reloc.deleteFn = func(int64) (relocalize.DeleteResult, error) {
					return relocalize.DeleteResult{Success: false, Error: "Document not found"}, nil
				}
			},
			method: http.MethodDelete, target: "/api/documents/9",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			golden := loadGolden(t, tc.name)
			env := newWriteEnv()
			if tc.setup != nil {
				tc.setup(env)
			}

			var rec = doJSON(t, env.handler, tc.method, tc.target, tc.body, nil)
			if tc.raw != nil {
				rec = doRaw(t, env.handler, tc.target, tc.raw)
			}

			if rec.Code != golden.Status {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, golden.Status, rec.Body.String())
			}
			if got := rec.Header().Get("Content-Type"); got != golden.ContentType {
				t.Fatalf("Content-Type = %q, want %q", got, golden.ContentType)
			}
			var got any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode Go body %q: %v", rec.Body.String(), err)
			}
			assertGoldenEqual(t, "body", normalizeGolden(golden.Body), normalizeGolden(got), map[string]bool{})
		})
	}
}
