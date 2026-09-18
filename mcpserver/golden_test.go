package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/app/scanlock"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// The golden files are captured from the TypeScript module by the throwaway
// scratch/capture-mcp-golden.test.ts script, driving the real mcp-server.ts over a temp SQLite DB
// with the I/O-heavy collaborators mocked the same way mcp-server.test.ts does. These tests assert
// the Go port is contract-identical.

const goldenDir = "testdata/golden"

func TestGoldenToolListDeepEqual(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(goldenDir, "mcp_tools.json"))
	if err != nil {
		t.Fatalf("read tools golden: %v", err)
	}
	var golden struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("unmarshal tools golden: %v", err)
	}

	gotTools := ToolDefinitions()
	if len(gotTools) != len(golden.Tools) {
		t.Fatalf("tool count = %d, want %d", len(gotTools), len(golden.Tools))
	}
	for i, want := range golden.Tools {
		got := gotTools[i]
		if got.Name != want.Name {
			t.Fatalf("tool[%d].name = %q, want %q", i, got.Name, want.Name)
		}
		if got.Description != want.Description {
			t.Fatalf("tool %s description = %q, want %q", got.Name, got.Description, want.Description)
		}

		var wantSchema any
		if err := json.Unmarshal(want.InputSchema, &wantSchema); err != nil {
			t.Fatalf("unmarshal %s golden schema: %v", got.Name, err)
		}
		gotRaw, err := json.Marshal(got.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", got.Name, err)
		}
		var gotSchema any
		if err := json.Unmarshal(gotRaw, &gotSchema); err != nil {
			t.Fatalf("unmarshal %s schema: %v", got.Name, err)
		}
		if !reflect.DeepEqual(gotSchema, wantSchema) {
			t.Fatalf("tool %s inputSchema mismatch:\n got: %v\nwant: %v", got.Name, gotSchema, wantSchema)
		}
	}
}

type goldenCall struct {
	Tool    string         `json:"tool"`
	Args    map[string]any `json:"args"`
	IsError bool           `json:"isError"`
	Text    string         `json:"text"`
	JSON    any            `json:"json"`
}

func TestGoldenHandleToolCalls(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(goldenDir, "mcp_handle_calls.json"))
	if err != nil {
		t.Fatalf("read handle-calls golden: %v", err)
	}
	var calls []goldenCall
	if err := json.Unmarshal(raw, &calls); err != nil {
		t.Fatalf("unmarshal handle-calls golden: %v", err)
	}
	if len(calls) == 0 {
		t.Fatal("empty handle-calls golden")
	}

	h := newHarness(t)
	idA := insertGoldenDocuments(t, h)

	tempDir := t.TempDir()
	tempFile := filepath.Join(tempDir, "mcp-golden-doc.pdf")
	if err := os.WriteFile(tempFile, []byte("dummy pdf bytes"), 0o644); err != nil {
		t.Fatalf("write temp file: %v", err)
	}

	h.categories.config = documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
		{ID: "invoices", Name: "Invoices", Description: "", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{}},
	}}
	// Only doc A resolves to a file, exactly like the scratch capture's mock.
	h.locator.byID = map[int64]string{idA: tempFile}

	docs, err := h.store.GetAllDocuments()
	if err != nil {
		t.Fatalf("GetAllDocuments: %v", err)
	}
	h.chat.docs = docs
	successResult := triagescan.Result{
		ScannedCount:   3,
		ProcessedCount: 2,
		SkippedCount:   1,
		Items: []triagescan.ResultItem{{
			Filename:    "a.pdf",
			DocID:       idA,
			Title:       "Facture SFR Janvier",
			Category:    "invoices",
			Subcategory: "sfr",
			NewPath:     "/archive/invoices/sfr/2026/a.pdf",
			Status:      "COMPLETED",
		}},
	}
	// The scratch capture used mockResolvedValue(success) + two mockRejectedValueOnce calls, which
	// fire in this exact order.
	h.scanner.sequence = []scanResponse{
		{result: successResult},
		{err: &scanlock.ScanInProgressError{HolderPID: 4242}},
		{err: errors.New("disk exploded")},
	}

	handler := NewHandler(h.deps)
	normalize := func(s string) string {
		s = strings.ReplaceAll(s, h.baseDir, "<BASE>")
		s = strings.ReplaceAll(s, tempDir, "<TMP>")
		return s
	}

	for i, entry := range calls {
		result := handler.CallTool(context.Background(), entry.Tool, entry.Args)
		if result.IsError != entry.IsError {
			t.Fatalf("call %d (%s): isError = %v, want %v (text=%q)", i, entry.Tool, result.IsError, entry.IsError, resultText(t, result))
		}
		gotText := resultText(t, result)

		// The Zod validation message embeds the full issue list; the Go validator returns its own
		// equivalent message. Only the stable prefix is contractual.
		if entry.Tool == "update_document_metadata" && strings.HasPrefix(entry.Text, "Error: invalid arguments") {
			if !strings.HasPrefix(gotText, "Error: invalid arguments — ") {
				t.Fatalf("call %d: text = %q, want prefix %q", i, gotText, "Error: invalid arguments — ")
			}
			continue
		}

		if entry.JSON != nil {
			var gotJSON any
			if err := json.Unmarshal([]byte(gotText), &gotJSON); err != nil {
				t.Fatalf("call %d (%s): unmarshal Go text %q: %v", i, entry.Tool, gotText, err)
			}
			gotJSON = normalizeJSON(gotJSON, h.baseDir, tempDir)
			if !reflect.DeepEqual(gotJSON, entry.JSON) {
				t.Fatalf("call %d (%s) payload mismatch:\n got: %#v\nwant: %#v", i, entry.Tool, gotJSON, entry.JSON)
			}
			continue
		}

		if got := normalize(gotText); got != entry.Text {
			t.Fatalf("call %d (%s) text mismatch:\n got: %q\nwant: %q", i, entry.Tool, got, entry.Text)
		}
	}
}

// normalizeJSON replaces the volatile host paths in a parsed tool payload with the same
// placeholders the scratch capture used.
func normalizeJSON(v any, baseDir, tempDir string) any {
	switch x := v.(type) {
	case string:
		x = strings.ReplaceAll(x, baseDir, "<BASE>")
		x = strings.ReplaceAll(x, tempDir, "<TMP>")
		return x
	case []any:
		for i := range x {
			x[i] = normalizeJSON(x[i], baseDir, tempDir)
		}
		return x
	case map[string]any:
		for k := range x {
			x[k] = normalizeJSON(x[k], baseDir, tempDir)
		}
		return x
	}
	return v
}

// goldenDocBase mirrors the scratch capture's `base` document exactly.
func goldenDocBase() database.NewDocument {
	return database.NewDocument{
		Title:            "Facture SFR Janvier",
		Registre:         "REF-001",
		Date:             "2026-01-15",
		Category:         "invoices",
		Subcategory:      "sfr",
		Summary:          "Facture mensuelle SFR pour janvier",
		Tags:             []string{"facture", "sfr"},
		RawText:          "Contenu complet de la facture SFR de janvier 2026",
		MarkdownContent:  "# Facture SFR",
		OriginalFilename: "facture.pdf",
		OriginalPath:     "C:/raws/facture.pdf",
		NewPath:          "",
		Embedding:        []float64{0.1, 0.2, 0.3},
		Status:           "COMPLETED",
		TotalAmount:      "42.00",
		ContactName:      "SFR",
		ContactEmail:     "contact@sfr.fr",
		ContactPhone:     "0102030405",
	}
}

func insertGoldenDocuments(t *testing.T, h *harness) int64 {
	t.Helper()
	base := goldenDocBase()
	a := base
	a.Checksum = "a"
	idA := h.insertDoc(t, a)

	b := base
	b.Checksum = "b"
	b.Title = "Facture EDF Fevrier"
	b.Registre = "REF-002"
	b.Subcategory = "edf"
	b.Summary = "Facture electricite"
	b.OriginalFilename = "edf.png"
	b.OriginalPath = "C:/raws/edf.png"
	b.TotalAmount = "99.00"
	h.insertDoc(t, b)
	return idA
}
