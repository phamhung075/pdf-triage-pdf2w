package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestCanonicalPathHandler(t *testing.T) {
	root := `C:\test-archive`
	body, _ := json.Marshal(map[string]any{
		"originalPath":  `C:\raws\facture.pdf`,
		"category":      "invoices",
		"outputRootDir": root,
		"subcategory":   "sfr",
		"dateStr":       "2024-05-12",
		"title":         nil,
	})
	req := httptest.NewRequest(http.MethodPost, "/canonical-path", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	canonicalPathHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	// filepath.Join uses the build OS's separator for the segments it joins (Linux: '/'), while
	// outputRootDir's own embedded backslashes pass through untouched — matching exactly what
	// ComputeCanonicalPath itself produces, so this must be built the same way rather than as a
	// hardcoded literal (a literal all-backslash string would fail on a Linux build/CI runner).
	want := filepath.Join(root, "invoices", "sfr", "2024", "facture.pdf")
	if resp["canonicalPath"] != want {
		t.Fatalf("got %q, want %q", resp["canonicalPath"], want)
	}
}

func TestHealthHandler(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()
	healthHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}
