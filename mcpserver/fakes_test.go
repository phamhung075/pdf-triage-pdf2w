package mcpserver

import (
	"context"

	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/app/triagescan"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/osopen"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

// The fakes below stand in for the I/O-heavy collaborators mcp-server.test.ts mocks with vi.fn().
// The document store is deliberately NOT faked: handler tests run against a real temp SQLite store,
// exactly as the TypeScript suite does.

type fakeCategories struct {
	config  documentschema.CategoriesConfig
	saved   [][]*documentschema.CategoryItem
	saveErr error
}

func (f *fakeCategories) GetCategoriesConfig() documentschema.CategoriesConfig { return f.config }

func (f *fakeCategories) SaveCategoriesConfig(categories []*documentschema.CategoryItem) error {
	f.saved = append(f.saved, categories)
	return f.saveErr
}

type relocalizeCall struct {
	filePath    string
	category    string
	subcategory string
	dateStr     string
	title       *string
}

type fakeRelocalizer struct {
	result relocalize.RelocalizeResult
	err    error
	calls  []relocalizeCall
}

func (f *fakeRelocalizer) RelocalizeFileIfNeeded(filePath, category string, subcategory, dateStr, title *string) (relocalize.RelocalizeResult, error) {
	call := relocalizeCall{filePath: filePath, category: category, title: title}
	if subcategory != nil {
		call.subcategory = *subcategory
	}
	if dateStr != nil {
		call.dateStr = *dateStr
	}
	f.calls = append(f.calls, call)
	return f.result, f.err
}

type fakeFileLocator struct {
	byID map[int64]string
	fn   func(*database.DocumentRecord) string
}

func (f *fakeFileLocator) Find(doc *database.DocumentRecord) string {
	if f.fn != nil {
		return f.fn(doc)
	}
	if doc == nil {
		return ""
	}
	return f.byID[doc.ID]
}

type fakeRegistry struct {
	calls int
	err   error
}

func (f *fakeRegistry) SyncJSONRegistry() error {
	f.calls++
	return f.err
}

type scanResponse struct {
	result triagescan.Result
	err    error
}

type fakeScanner struct {
	result   triagescan.Result
	err      error
	sequence []scanResponse
	calls    int
}

func (f *fakeScanner) RunTriageScan(ctx context.Context, onProgress func(triagescan.Event), shouldAbort func() bool) (triagescan.Result, error) {
	f.calls++
	if len(f.sequence) > 0 {
		next := f.sequence[0]
		f.sequence = f.sequence[1:]
		return next.result, next.err
	}
	return f.result, f.err
}

type fakeChatSearch struct {
	docs      []database.DocumentRecord
	err       error
	lastQuery string
}

func (f *fakeChatSearch) SearchRelevantDocuments(userMessage string) ([]database.DocumentRecord, error) {
	f.lastQuery = userMessage
	return f.docs, f.err
}

type fakeOpener struct {
	filePath string
	plan     osopen.Plan
}

func (f *fakeOpener) RevealInFileManager(filePath string) osopen.Plan {
	f.filePath = filePath
	return f.plan
}

type fakeRunner struct {
	plans []osopen.Plan
	err   error
}

func (f *fakeRunner) Run(plan osopen.Plan) error {
	f.plans = append(f.plans, plan)
	return f.err
}

type fakeDecisions struct {
	records []manualdecisions.Record
}

func (f *fakeDecisions) RecordManualDecision(record manualdecisions.Record) {
	f.records = append(f.records, record)
}

// testCategoriesConfig is the tiny categories payload the list_categories tests use.
func testCategoriesConfig() documentschema.CategoriesConfig {
	return documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
		{
			ID:            "invoices",
			Name:          "Invoices",
			Description:   "",
			Aliases:       []string{},
			Subcategories: []*documentschema.SubcategoryItem{},
		},
	}}
}
