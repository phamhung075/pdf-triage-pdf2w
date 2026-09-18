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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/app/scanlock"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/osopen"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// These cases are the Go port of src/infrastructure/mcp/mcp-server.test.ts (24 cases, green at
// port time: `npx vitest run src/infrastructure/mcp/mcp-server.test.ts` -> 24 passed). The
// TypeScript suite's vi.mock() collaborators map onto the fakes in fakes_test.go; the document
// store is real (a t.TempDir() SQLite file), exactly as the TS suite uses a real temp DB.

type harness struct {
	store       *database.Store
	deps        Deps
	categories  *fakeCategories
	relocalizer *fakeRelocalizer
	locator     *fakeFileLocator
	registry    *fakeRegistry
	scanner     *fakeScanner
	chat        *fakeChatSearch
	opener      *fakeOpener
	runner      *fakeRunner
	decisions   *fakeDecisions
	baseDir     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store, err := database.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("database.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	baseDir := t.TempDir()
	h := &harness{
		store:       store,
		categories:  &fakeCategories{},
		relocalizer: &fakeRelocalizer{},
		locator:     &fakeFileLocator{byID: map[int64]string{}},
		registry:    &fakeRegistry{},
		scanner:     &fakeScanner{},
		chat:        &fakeChatSearch{},
		opener:      &fakeOpener{plan: osopen.Plan{Cmd: "fake-open"}},
		runner:      &fakeRunner{},
		decisions:   &fakeDecisions{},
		baseDir:     baseDir,
	}
	h.deps = Deps{
		DB:          store,
		Categories:  h.categories,
		Relocalizer: h.relocalizer,
		FileLocator: h.locator,
		Registry:    h.registry,
		Scanner:     h.scanner,
		ChatSearch:  h.chat,
		Opener:      h.opener,
		Runner:      h.runner,
		Decisions:   h.decisions,
		BaseDir:     baseDir,
		Now:         func() time.Time { return time.UnixMilli(1_700_000_000_000) },
	}
	return h
}

func (h *harness) handler() *Handler { return NewHandler(h.deps) }

func (h *harness) call(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	return h.handler().CallTool(context.Background(), name, args)
}

func (h *harness) insertDoc(t *testing.T, doc database.NewDocument) int64 {
	t.Helper()
	id, err := h.store.InsertDocumentRecord(doc)
	if err != nil {
		t.Fatalf("InsertDocumentRecord: %v", err)
	}
	return id
}

func sampleDoc() database.NewDocument {
	return database.NewDocument{
		Checksum:         "chk",
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
	}
}

func resultText(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if r == nil {
		t.Fatal("nil result")
	}
	if len(r.Content) != 1 {
		t.Fatalf("content length = %d, want 1", len(r.Content))
	}
	tc, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content[0] = %T, want *mcp.TextContent", r.Content[0])
	}
	return tc.Text
}

func mustUnmarshal[T any](t *testing.T, s string) T {
	t.Helper()
	var out T
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatalf("unmarshal %q: %v", s, err)
	}
	return out
}

func TestListMcpTools(t *testing.T) {
	h := newHarness(t)
	tools := h.handler().ListTools().Tools

	names := make([]string, len(tools))
	byName := map[string]*mcp.Tool{}
	for i, tool := range tools {
		names[i] = tool.Name
		byName[tool.Name] = tool
	}
	wantNames := []string{
		"search_documents",
		"get_full_document_text",
		"update_document_metadata",
		"trigger_triage",
		"list_categories",
		"prepare_dossier",
		"get_document_markdown",
		"open_document_folder",
		"package_documents",
	}
	if !reflect.DeepEqual(names, wantNames) {
		t.Fatalf("tool names = %v, want %v", names, wantNames)
	}

	for _, name := range []string{"get_full_document_text", "update_document_metadata", "get_document_markdown", "open_document_folder"} {
		tool := byName[name]
		if tool == nil {
			t.Fatalf("missing tool %q", name)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", name, err)
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("unmarshal %s schema: %v", name, err)
		}
		if !reflect.DeepEqual(schema.Required, []string{"docId"}) {
			t.Fatalf("%s required = %v, want [docId]", name, schema.Required)
		}
	}
}

func TestSearchDocuments(t *testing.T) {
	h := newHarness(t)

	docA := sampleDoc()
	docA.Checksum = "a"
	h.insertDoc(t, docA)

	docB := sampleDoc()
	docB.Checksum = "b"
	docB.Title = "Facture EDF Fevrier"
	docB.Subcategory = "edf"
	docB.Summary = "Facture electricite"
	h.insertDoc(t, docB)

	docC := sampleDoc()
	docC.Checksum = "c"
	docC.Title = "Bulletin Salaire"
	docC.Category = "bulletin_salaire"
	docC.Subcategory = "employer_x"
	docC.Summary = "Paie mensuelle"
	h.insertDoc(t, docC)

	byCategory := mustUnmarshal[searchPayload](t, resultText(t, h.call(t, "search_documents", map[string]any{"category": "INVOICES"})))
	if byCategory.Count != 2 {
		t.Fatalf("by category count = %d, want 2", byCategory.Count)
	}

	bySub := mustUnmarshal[searchPayload](t, resultText(t, h.call(t, "search_documents", map[string]any{"subcategory": "SFR"})))
	if bySub.Count != 1 || bySub.Results[0].Title != "Facture SFR Janvier" {
		t.Fatalf("by subcategory = %+v", bySub)
	}

	byQuery := mustUnmarshal[searchPayload](t, resultText(t, h.call(t, "search_documents", map[string]any{"query": "ELECTRICITE"})))
	if byQuery.Count != 1 || byQuery.Results[0].Title != "Facture EDF Fevrier" {
		t.Fatalf("by query = %+v", byQuery)
	}
}

func TestSearchDocumentsLimit(t *testing.T) {
	h := newHarness(t)
	for _, checksum := range []string{"a", "b", "c"} {
		doc := sampleDoc()
		doc.Checksum = checksum
		h.insertDoc(t, doc)
	}

	parsed := mustUnmarshal[searchPayload](t, resultText(t, h.call(t, "search_documents", map[string]any{"limit": 2})))
	if parsed.Count != 2 {
		t.Fatalf("count = %d, want 2", parsed.Count)
	}
}

func TestSearchDocumentsNoFilters(t *testing.T) {
	h := newHarness(t)
	for _, checksum := range []string{"a", "b"} {
		doc := sampleDoc()
		doc.Checksum = checksum
		h.insertDoc(t, doc)
	}

	parsed := mustUnmarshal[searchPayload](t, resultText(t, h.call(t, "search_documents", map[string]any{})))
	if parsed.Count != 2 {
		t.Fatalf("count = %d, want 2", parsed.Count)
	}
}

func TestGetFullDocumentText(t *testing.T) {
	h := newHarness(t)
	doc := sampleDoc()
	doc.RawText = "The full extracted body text."
	id := h.insertDoc(t, doc)

	parsed := mustUnmarshal[fullTextPayload](t, resultText(t, h.call(t, "get_full_document_text", map[string]any{"docId": id})))
	if parsed.ID != id || parsed.RawText != "The full extracted body text." {
		t.Fatalf("payload = %+v", parsed)
	}
}

func TestGetFullDocumentTextNotFound(t *testing.T) {
	h := newHarness(t)
	result := h.call(t, "get_full_document_text", map[string]any{"docId": 9999})
	if !result.IsError {
		t.Fatal("expected isError")
	}
	if !strings.Contains(resultText(t, result), "Document ID 9999 not found") {
		t.Fatalf("text = %q", resultText(t, result))
	}
}

func TestUpdateDocumentMetadataRejectsBadDocID(t *testing.T) {
	h := newHarness(t)
	cases := []any{1.5, 0, -3}
	for _, docID := range cases {
		result := h.call(t, "update_document_metadata", map[string]any{"docId": docID, "title": "x"})
		if !result.IsError {
			t.Fatalf("docId %v: expected isError", docID)
		}
		if !strings.Contains(resultText(t, result), "docId must be a positive integer") {
			t.Fatalf("docId %v: text = %q", docID, resultText(t, result))
		}
	}
}

func TestUpdateDocumentMetadataRejectsInvalidArguments(t *testing.T) {
	h := newHarness(t)
	result := h.call(t, "update_document_metadata", map[string]any{"docId": 1, "category": 123})
	if !result.IsError {
		t.Fatal("expected isError")
	}
	if !strings.Contains(resultText(t, result), "invalid arguments") {
		t.Fatalf("text = %q", resultText(t, result))
	}
}

func TestUpdateDocumentMetadataRejectsForbiddenSubcategory(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		key   string
		value string
	}{
		{"subcategory", "general"},
		{"subcategory", "2024"},
		{"subcategorie", "divers"},
	}
	for _, tc := range cases {
		result := h.call(t, "update_document_metadata", map[string]any{"docId": 1, tc.key: tc.value})
		if !result.IsError {
			t.Fatalf("%s=%s: expected isError", tc.key, tc.value)
		}
		if !strings.Contains(resultText(t, result), "Golden Rule #4") {
			t.Fatalf("%s=%s: text = %q", tc.key, tc.value, resultText(t, result))
		}
	}
}

func TestUpdateDocumentMetadataNotFound(t *testing.T) {
	h := newHarness(t)
	result := h.call(t, "update_document_metadata", map[string]any{"docId": 9999, "subcategory": "sfr"})
	if !result.IsError {
		t.Fatal("expected isError")
	}
	if !strings.Contains(resultText(t, result), "Document ID 9999 not found") {
		t.Fatalf("text = %q", resultText(t, result))
	}
}

func TestUpdateDocumentMetadataNonPathFieldsOnly(t *testing.T) {
	h := newHarness(t)
	doc := sampleDoc()
	doc.NewPath = ""
	id := h.insertDoc(t, doc)

	result := h.call(t, "update_document_metadata", map[string]any{"docId": id, "summary": "Updated summary only"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	if !strings.Contains(resultText(t, result), "Successfully updated metadata for document ID 1") {
		t.Fatalf("text = %q", resultText(t, result))
	}

	updated, err := h.store.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if updated.Summary != "Updated summary only" {
		t.Fatalf("summary = %q", updated.Summary)
	}
	if updated.Title != "Facture SFR Janvier" {
		t.Fatalf("title = %q, want untouched", updated.Title)
	}
	if len(h.relocalizer.calls) != 0 {
		t.Fatalf("relocalizer calls = %d, want 0", len(h.relocalizer.calls))
	}
	if h.registry.calls != 1 {
		t.Fatalf("registry calls = %d, want 1", h.registry.calls)
	}
}

func TestUpdateDocumentMetadataFrenchAliases(t *testing.T) {
	h := newHarness(t)
	doc := sampleDoc()
	doc.NewPath = ""
	id := h.insertDoc(t, doc)

	result := h.call(t, "update_document_metadata", map[string]any{"docId": id, "categorie": "utilities", "subcategorie": "edf"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	updated, err := h.store.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if updated.Category != "utilities" || updated.Subcategory != "edf" {
		t.Fatalf("category/subcategory = %q/%q", updated.Category, updated.Subcategory)
	}
}

func TestUpdateDocumentMetadataRelocalizesPhysicalFile(t *testing.T) {
	h := newHarness(t)

	tempFile := filepath.Join(t.TempDir(), "mcp-relocalize-test.pdf")
	if err := os.WriteFile(tempFile, []byte("dummy pdf bytes"), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}

	doc := sampleDoc()
	doc.NewPath = tempFile
	id := h.insertDoc(t, doc)

	mockedNewPath := filepath.Join(t.TempDir(), "organized", "telecom", "orange", "2026", "facture.pdf")
	h.relocalizer.result = relocalize.RelocalizeResult{NewPath: mockedNewPath, Moved: true}

	result := h.call(t, "update_document_metadata", map[string]any{"docId": id, "category": "telecom", "subcategory": "orange"})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}

	// Golden Rule 5: ensureCategoryAndSubcategoryExist ran with telecom/orange.
	if len(h.categories.saved) != 1 {
		t.Fatalf("SaveCategoriesConfig calls = %d, want 1", len(h.categories.saved))
	}
	if !categoryConfigHas(h.categories.saved[0], "telecom", "orange") {
		t.Fatalf("saved categories missing telecom/orange: %+v", h.categories.saved[0])
	}

	if len(h.relocalizer.calls) != 1 {
		t.Fatalf("relocalizer calls = %d, want 1", len(h.relocalizer.calls))
	}
	got := h.relocalizer.calls[0]
	if got.filePath != tempFile || got.category != "telecom" || got.subcategory != "orange" || got.dateStr != "2026-01-15" {
		t.Fatalf("relocalize call = %+v", got)
	}

	updated, err := h.store.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if updated.Category != "telecom" || updated.Subcategory != "orange" {
		t.Fatalf("category/subcategory = %q/%q", updated.Category, updated.Subcategory)
	}
	if updated.NewPath != mockedNewPath {
		t.Fatalf("new_path = %q, want %q", updated.NewPath, mockedNewPath)
	}
}

func TestUpdateDocumentMetadataNoMoveKeepsNewPath(t *testing.T) {
	h := newHarness(t)

	tempFile := filepath.Join(t.TempDir(), "mcp-relocalize-nomove-test.pdf")
	if err := os.WriteFile(tempFile, []byte("dummy pdf bytes"), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}

	doc := sampleDoc()
	doc.NewPath = tempFile
	id := h.insertDoc(t, doc)
	h.relocalizer.result = relocalize.RelocalizeResult{NewPath: tempFile, Moved: false}

	if result := h.call(t, "update_document_metadata", map[string]any{"docId": id, "category": "invoices", "subcategory": "sfr2"}); result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	updated, err := h.store.GetDocumentByID(id)
	if err != nil {
		t.Fatalf("GetDocumentByID: %v", err)
	}
	if updated.NewPath != tempFile {
		t.Fatalf("new_path = %q, want %q", updated.NewPath, tempFile)
	}
}

func TestUpdateDocumentMetadataRecordsManualDecision(t *testing.T) {
	h := newHarness(t)
	doc := sampleDoc()
	doc.NewPath = ""
	id := h.insertDoc(t, doc)

	if result := h.call(t, "update_document_metadata", map[string]any{"docId": id, "category": "telecom", "subcategory": "orange"}); result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	if len(h.decisions.records) != 1 {
		t.Fatalf("manual decisions = %d, want 1", len(h.decisions.records))
	}
	rec := h.decisions.records[0]
	if rec.DocumentID != id || rec.OldCategory != "invoices" || rec.NewCategory != "telecom" || rec.NewSubcategory != "orange" {
		t.Fatalf("record = %+v", rec)
	}
}

func TestTriggerTriageSuccess(t *testing.T) {
	h := newHarness(t)
	h.scanner.result = triagescan.Result{
		ScannedCount:   3,
		ProcessedCount: 2,
		SkippedCount:   1,
		Items:          []triagescan.ResultItem{{Filename: "a.pdf"}},
	}

	result := h.call(t, "trigger_triage", map[string]any{})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	parsed := mustUnmarshal[triagePayload](t, resultText(t, result))
	if parsed.ScannedCount != 3 || parsed.ProcessedCount != 2 || parsed.SkippedCount != 1 {
		t.Fatalf("payload = %+v", parsed)
	}
}

func TestTriggerTriageScanInProgress(t *testing.T) {
	h := newHarness(t)
	h.scanner.err = &scanlock.ScanInProgressError{HolderPID: 4242}

	result := h.call(t, "trigger_triage", map[string]any{})
	if !result.IsError {
		t.Fatal("expected isError")
	}
	text := resultText(t, result)
	if !strings.Contains(text, "already in progress") || !strings.Contains(text, "4242") {
		t.Fatalf("text = %q", text)
	}
}

func TestTriggerTriageUnrelatedError(t *testing.T) {
	h := newHarness(t)
	h.scanner.err = errors.New("disk exploded")

	result := h.call(t, "trigger_triage", map[string]any{})
	if !result.IsError {
		t.Fatal("expected isError")
	}
	if !strings.Contains(resultText(t, result), "disk exploded") {
		t.Fatalf("text = %q", resultText(t, result))
	}
}

func TestListCategories(t *testing.T) {
	h := newHarness(t)
	categories := []*documentschema.CategoryItem{
		{ID: "invoices", Name: "Invoices", Description: "", Aliases: []string{}, Subcategories: []*documentschema.SubcategoryItem{}},
	}
	h.categories.config = documentschema.CategoriesConfig{Categories: categories}

	parsed := mustUnmarshal[[]*documentschema.CategoryItem](t, resultText(t, h.call(t, "list_categories", map[string]any{})))
	if !reflect.DeepEqual(parsed, categories) {
		t.Fatalf("categories = %+v", parsed)
	}
}

func TestPackageDocumentsRequiresSelection(t *testing.T) {
	h := newHarness(t)
	result := h.call(t, "package_documents", map[string]any{})
	if !result.IsError {
		t.Fatal("expected isError")
	}
	if got := resultText(t, result); got != "Error: provide either docIds or dossierType." {
		t.Fatalf("text = %q", got)
	}
}

func TestPackageDocumentsAllMissing(t *testing.T) {
	h := newHarness(t)
	id := h.insertDoc(t, sampleDoc())

	result := h.call(t, "package_documents", map[string]any{"docIds": []any{id}})
	if !result.IsError {
		t.Fatal("expected isError")
	}
	if !strings.Contains(resultText(t, result), "Missing IDs: 1") {
		t.Fatalf("text = %q", resultText(t, result))
	}
}

func TestPackageDocumentsBuildsZip(t *testing.T) {
	h := newHarness(t)

	tempFile := filepath.Join(t.TempDir(), "mcp-package-test.pdf")
	if err := os.WriteFile(tempFile, []byte("dummy pdf bytes"), 0o644); err != nil {
		t.Fatalf("write temp: %v", err)
	}

	found := sampleDoc()
	found.Checksum = "pkg-found"
	found.Title = "Found Doc"
	foundID := h.insertDoc(t, found)

	missing := sampleDoc()
	missing.Checksum = "pkg-missing"
	missing.Title = "Missing Doc"
	missingID := h.insertDoc(t, missing)

	h.locator.fn = func(doc *database.DocumentRecord) string {
		if doc != nil && doc.ID == foundID {
			return tempFile
		}
		return ""
	}

	result := h.call(t, "package_documents", map[string]any{
		"docIds":  []any{foundID, missingID},
		"zipName": "unit_test_pack",
	})
	if result.IsError {
		t.Fatalf("unexpected error: %s", resultText(t, result))
	}
	parsed := mustUnmarshal[packagePayload](t, resultText(t, result))
	if parsed.FileCount != 1 {
		t.Fatalf("fileCount = %d, want 1", parsed.FileCount)
	}
	if !reflect.DeepEqual(parsed.IncludedDocIDs, []int64{foundID}) {
		t.Fatalf("included = %v", parsed.IncludedDocIDs)
	}
	if !reflect.DeepEqual(parsed.MissingDocIDs, []int64{missingID}) {
		t.Fatalf("missing = %v", parsed.MissingDocIDs)
	}
	wantZip := filepath.Join(h.baseDir, "__packages", "unit_test_pack.zip")
	if parsed.ZipPath != wantZip {
		t.Fatalf("zipPath = %q, want %q", parsed.ZipPath, wantZip)
	}
	raw, err := os.ReadFile(parsed.ZipPath)
	if err != nil {
		t.Fatalf("read zip: %v", err)
	}
	if len(raw) < 4 || string(raw[:4]) != "PK\x03\x04" {
		t.Fatalf("zip magic = % x", raw[:min(4, len(raw))])
	}
}

func TestUnknownTool(t *testing.T) {
	h := newHarness(t)
	result := h.call(t, "does_not_exist", map[string]any{})
	if !result.IsError {
		t.Fatal("expected isError")
	}
	if got := resultText(t, result); got != "Unknown tool name: does_not_exist" {
		t.Fatalf("text = %q", got)
	}
}

func categoryConfigHas(categories []*documentschema.CategoryItem, category, subcategory string) bool {
	for _, cat := range categories {
		if cat == nil || cat.ID != category {
			continue
		}
		for _, sub := range cat.Subcategories {
			if sub != nil && sub.ID == subcategory {
				return true
			}
		}
	}
	return false
}
