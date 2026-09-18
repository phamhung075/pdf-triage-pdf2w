package aichat

import (
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/chatquery"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// These cases are ported case-for-case from pdf-triage's
// src/application/ai-chat-assistant.test.ts (14 cases). The upstream TypeScript suite is GREEN at
// port time (`npx vitest run src/application/ai-chat-assistant.test.ts` -> 14 passed), so no
// upstream case is pinned red.
//
// The TS suite mocks db/database (getAllDocuments + searchDocumentsFts), ollama-client, and the
// chat-query-planner module. Here those become the Deps fields: DocumentStore, TextChat and the
// PlanQuery function value. The real database.Store satisfies DocumentStore, exercised by the
// temp-DB integration test at the bottom of this file.

// ---- fakes ----

type fakeStore struct {
	allDocs   func() ([]database.DocumentRecord, error)
	searchFts func(match string, filters database.FtsSearchFilters, limit int) ([]database.DocumentRecord, error)
	ftsCalls  []ftsCall
}

type ftsCall struct {
	match   string
	filters database.FtsSearchFilters
	limit   int
}

func (f *fakeStore) GetAllDocuments() ([]database.DocumentRecord, error) {
	if f.allDocs == nil {
		return []database.DocumentRecord{}, nil
	}
	return f.allDocs()
}

func (f *fakeStore) SearchDocumentsFts(match string, filters database.FtsSearchFilters, limit int) ([]database.DocumentRecord, error) {
	f.ftsCalls = append(f.ftsCalls, ftsCall{match, filters, limit})
	if f.searchFts == nil {
		return []database.DocumentRecord{}, nil
	}
	return f.searchFts(match, filters, limit)
}

type fakeChat struct {
	response ollama.TextCompletion
	err      error
	calls    int
}

func (f *fakeChat) RequestTextChatCompletion(system, user string) (ollama.TextCompletion, error) {
	f.calls++
	return f.response, f.err
}

func planFunc(plan chatquery.StructuredQuery) PlanQueryFunc {
	return func(userMessage string, now time.Time) (chatquery.StructuredQuery, error) {
		return plan, nil
	}
}

// ---- fixtures ----

func mockDocs() []database.DocumentRecord {
	return []database.DocumentRecord{
		{
			ID: 1, Checksum: "abc1", Title: "Facture EDF Electricite",
			Category: "housing", Subcategory: "edf", Date: "2024-01-15",
			Summary: "Invoice for electricity", TotalAmount: "85.50",
			OriginalFilename: "2024-01-15_EDF_Facture.pdf",
			NewPath:          `C:\archive\housing\edf\2024\2024-01-15_EDF_Facture.pdf`,
		},
		{
			ID: 2, Checksum: "abc2", Title: "Bulletin de Salaire Mai 2026",
			Category: "bulletin_salaire", Subcategory: "acme_corp", Date: "2026-05-31",
			Summary: "Monthly pay slip AcmeCorp", TotalAmount: "2450.00",
			OriginalFilename: "2026-05-31_AcmeCorp_Bulletin_de_Salaire_Mai.pdf",
			NewPath:          `C:\archive\bulletin_salaire\acme_corp\2026\2026-05-31_AcmeCorp_Bulletin_de_Salaire_Mai.pdf`,
		},
		{
			ID: 3, Checksum: "abc3", Title: "Bulletin de Salaire Juin 2026",
			Category: "bulletin_salaire", Subcategory: "acme_corp", Date: "2026-06-30",
			Summary: "June pay slip AcmeCorp", TotalAmount: "2450.00",
			OriginalFilename: "2026-06-30_AcmeCorp_Bulletin_de_Salaire_Juin.pdf",
			NewPath:          `C:\archive\bulletin_salaire\acme_corp\2026\2026-06-30_AcmeCorp_Bulletin_de_Salaire_Juin.pdf`,
		},
	}
}

func localDate(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.Local)
}

// ---- ai-chat-assistant describe block ----

func TestSearchRelevantDocumentsFiltersAndSortsPaySlips(t *testing.T) {
	// ported: 'searchRelevantDocuments filters and sorts pay slips by date descending'
	deps := Deps{Store: &fakeStore{allDocs: func() ([]database.DocumentRecord, error) { return mockDocs(), nil }}}
	results, err := SearchRelevantDocuments(deps, "j'ai besoin 2 derniers fiche de paie")
	if err != nil {
		t.Fatalf("SearchRelevantDocuments: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].ID != 3 {
		t.Fatalf("results[0].ID = %d, want 3", results[0].ID)
	}
	if results[1].ID != 2 {
		t.Fatalf("results[1].ID = %d, want 2", results[1].ID)
	}
}

func TestBuildPromptContextEmbedsMetadata(t *testing.T) {
	// ported: 'buildPromptContext embeds document metadata into prompt'
	system, userPrompt := BuildPromptContext("Show my pay slip", mockDocs(), localDate(2026, 8, 12))
	if !strings.Contains(system, "assistant archiviste IA local") {
		t.Fatalf("system prompt missing the assistant persona:\n%s", system)
	}
	if !strings.Contains(userPrompt, "Bulletin de Salaire Mai 2026") {
		t.Fatalf("user prompt missing document metadata:\n%s", userPrompt)
	}
}

func TestBuildPromptContextGroundsCurrentDate(t *testing.T) {
	// ported: 'buildPromptContext grounds the system prompt in the current date ...'
	system, _ := BuildPromptContext("3 derniers mois", mockDocs(), localDate(2026, 8, 12))
	if !strings.Contains(system, "Nous sommes le 2026-08-12") {
		t.Fatalf("system prompt not grounded in the current date:\n%s", system)
	}
}

func TestProcessChatQueryReturnsExactCitedDocuments(t *testing.T) {
	// ported: 'processChatQuery calls Ollama and returns answer with exact cited documents'
	store := &fakeStore{searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
		docs := mockDocs()
		return []database.DocumentRecord{docs[2], docs[1]}, nil
	}}
	chat := &fakeChat{response: ollama.TextCompletion{
		Response: "Voici votre bulletin : [Doc #3: Bulletin de Salaire Juin 2026]",
	}}
	deps := Deps{
		Store: store, Ollama: chat,
		PlanQuery: planFunc(chatquery.StructuredQuery{DocTypes: []string{"bulletin de salaire", "fiche de paie"}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{}}),
	}

	res, err := ProcessChatQuery(deps, "j'ai besoin fiche de paie", nil, localDate(2026, 8, 12))
	if err != nil {
		t.Fatalf("ProcessChatQuery: %v", err)
	}
	if !strings.Contains(res.Answer, "Bulletin de Salaire Juin 2026") {
		t.Fatalf("answer = %q, want the cited title", res.Answer)
	}
	if len(res.MatchedDocuments) != 1 {
		t.Fatalf("len(matchedDocuments) = %d, want 1", len(res.MatchedDocuments))
	}
	if res.MatchedDocuments[0].ID != 3 {
		t.Fatalf("matchedDocuments[0].ID = %d, want 3", res.MatchedDocuments[0].ID)
	}
}

func TestProcessChatQueryKeepsCorrectlyRetrievedDocsOnUnderCitation(t *testing.T) {
	// ported: 'does not silently drop correctly-retrieved documents when the AI under-cites an
	// explicit-count request (citation-pruning regression)'
	all := append(mockDocs(), database.DocumentRecord{
		ID: 4, Checksum: "abc4", Title: "Bulletin de Salaire Avril 2026",
		Category: "bulletin_salaire", Subcategory: "acme_corp", Date: "2026-04-30",
		Summary: "April pay slip AcmeCorp", TotalAmount: "2450.00",
		OriginalFilename: "2026-04-30_AcmeCorp_Bulletin.pdf",
		NewPath:          `C:\archive\bulletin_salaire\acme_corp\2026\2026-04-30_AcmeCorp_Bulletin.pdf`,
	})
	store := &fakeStore{allDocs: func() ([]database.DocumentRecord, error) { return all, nil }}
	chat := &fakeChat{response: ollama.TextCompletion{
		Response: "Voici : [Doc #3: Bulletin de Salaire Juin 2026] et [Doc #2: Bulletin de Salaire Mai 2026]",
	}}
	// The TS suite leaves planQuery as the reset mock (undefined), which makes retrieveDocuments
	// throw into its token-scorer fallback. A nil PlanQuery is the Go form of that failure.
	deps := Deps{Store: store, Ollama: chat}

	res, err := ProcessChatQuery(deps, "3 fiche de paie", nil, localDate(2026, 8, 12))
	if err != nil {
		t.Fatalf("ProcessChatQuery: %v", err)
	}
	if len(res.MatchedDocuments) != 3 {
		t.Fatalf("len(matchedDocuments) = %d, want 3", len(res.MatchedDocuments))
	}
	ids := []int64{res.MatchedDocuments[0].ID, res.MatchedDocuments[1].ID, res.MatchedDocuments[2].ID}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	if ids[0] != 2 || ids[1] != 3 || ids[2] != 4 {
		t.Fatalf("matched ids = %v, want [2 3 4]", ids)
	}
}

func TestDedupeByPeriodKeepsNewestScan(t *testing.T) {
	// ported: 'dedupeByPeriod de-duplicates re-scanned copies of the same pay period, keeping the
	// newest scan'
	docs := mockDocs()
	input := []database.DocumentRecord{
		docs[1],
		docs[2],
		{
			ID: 5, Checksum: "abc5-dup", Title: "BULLETIN DE SALAIRE JUIN 2026",
			Category: "bulletin_salaire", Subcategory: "acme_corp", Date: "2026-06-30",
			Summary: "duplicate scan", TotalAmount: "2450.00",
			OriginalFilename: "converted.pdf",
			NewPath:          `C:\archive\bulletin_salaire\acme_corp\2026\converted.pdf`,
		},
		{
			ID: 6, Checksum: "abc6", Title: "Bulletin de Salaire Avril 2026",
			Category: "bulletin_salaire", Subcategory: "acme_corp", Date: "2026-04-30",
			Summary: "April pay slip", TotalAmount: "2450.00",
			OriginalFilename: "2026-04-30_AcmeCorp_Bulletin.pdf",
			NewPath:          `C:\archive\bulletin_salaire\acme_corp\2026\2026-04-30_AcmeCorp_Bulletin.pdf`,
		},
	}

	results := DedupeByPeriod(input)
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	months := map[string]bool{}
	for _, d := range results {
		months[d.Date] = true
	}
	if len(months) != 3 {
		t.Fatalf("distinct months = %d, want 3 (%v)", len(months), months)
	}
	if !months["2026-04-30"] {
		t.Fatalf("results do not contain the distinct April month: %v", months)
	}
	for _, d := range results {
		if d.ID == 3 {
			t.Fatalf("old June scan id 3 survived; want the newest scan id 5")
		}
	}
	found5 := false
	for _, d := range results {
		if d.ID == 5 {
			found5 = true
		}
	}
	if !found5 {
		t.Fatalf("newest June scan id 5 is missing from %v", results)
	}
}

func TestDedupeByPeriodPreservesOrderAndLeavesNonPaySlips(t *testing.T) {
	// ported: 'dedupeByPeriod preserves input order and leaves non-pay-slip documents untouched,
	// even within the same month'
	march1 := database.DocumentRecord{ID: 10, Category: "housing", Date: "2024-03-05", Title: "Invoice A"}
	march2 := database.DocumentRecord{ID: 11, Category: "housing", Date: "2024-03-20", Title: "Invoice B"}
	juneOld := database.DocumentRecord{ID: 20, Category: "bulletin_salaire", Date: "2026-06-30", Title: "Bulletin Juin (old scan)"}
	juneRescan := database.DocumentRecord{ID: 21, Category: "bulletin_salaire", Date: "2026-06-15", Title: "Bulletin Juin (re-scan)"}
	may := database.DocumentRecord{ID: 22, Category: "bulletin_salaire", Date: "2026-05-31", Title: "Bulletin Mai"}

	results := DedupeByPeriod([]database.DocumentRecord{march1, march2, juneOld, juneRescan, may})
	got := make([]int64, len(results))
	for i, d := range results {
		got[i] = d.ID
	}
	want := []int64{10, 11, 21, 22}
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v (input order must be preserved)", got, want)
		}
	}
}

// ---- retrieveDocuments describe block ----

func TestRetrieveDocumentsRunsCompiledExpressionThroughFts(t *testing.T) {
	// ported: 'runs the compiled expression through FTS5 and returns its ranked rows'
	store := &fakeStore{searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
		return []database.DocumentRecord{{ID: 4280, Title: "RIB"}}, nil
	}}
	deps := Deps{
		Store: store,
		PlanQuery: planFunc(chatquery.StructuredQuery{
			DocTypes: []string{"rib"}, Entities: []string{"credit mutuel"}, Keywords: []string{}, NotTerms: []string{},
		}),
	}

	docs, err := RetrieveDocuments(deps, "RIB credit mutuel", localDate(2026, 8, 12))
	if err != nil {
		t.Fatalf("RetrieveDocuments: %v", err)
	}
	if len(store.ftsCalls) != 1 {
		t.Fatalf("fts calls = %d, want 1", len(store.ftsCalls))
	}
	if store.ftsCalls[0].match != `("rib") AND ("credit mutuel")` {
		t.Fatalf("match = %q, want %q", store.ftsCalls[0].match, `("rib") AND ("credit mutuel")`)
	}
	if len(docs) != 1 || docs[0].ID != 4280 {
		t.Fatalf("docs = %#v, want [4280]", docs)
	}
}

func TestRetrieveDocumentsPassesTaxonomyAndDateFilters(t *testing.T) {
	// ported: 'passes the taxonomy and date filters through to SQL'
	category := "bank"
	dateFrom := "2023-01-01"
	dateTo := "2023-12-31"
	store := &fakeStore{searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
		return []database.DocumentRecord{{ID: 1}}, nil
	}}
	deps := Deps{
		Store: store,
		PlanQuery: planFunc(chatquery.StructuredQuery{
			DocTypes: []string{"rib"}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{},
			Category: &category, DateFrom: &dateFrom, DateTo: &dateTo,
		}),
	}

	if _, err := RetrieveDocuments(deps, "rib 2023", localDate(2026, 8, 12)); err != nil {
		t.Fatalf("RetrieveDocuments: %v", err)
	}
	got := store.ftsCalls[0].filters
	if got.Category != "bank" || got.DateFrom != "2023-01-01" || got.DateTo != "2023-12-31" {
		t.Fatalf("filters = %#v, want category=bank dateFrom=2023-01-01 dateTo=2023-12-31", got)
	}
}

func TestRetrieveDocumentsClimbsRelaxationLadder(t *testing.T) {
	// ported: 'climbs the relaxation ladder when the first query returns nothing'
	call := 0
	store := &fakeStore{searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
		call++
		if call == 1 {
			return []database.DocumentRecord{}, nil
		}
		return []database.DocumentRecord{{ID: 7}}, nil
	}}
	deps := Deps{
		Store: store,
		PlanQuery: planFunc(chatquery.StructuredQuery{
			DocTypes: []string{"rib"}, Entities: []string{"ccm"}, Keywords: []string{"2023"}, NotTerms: []string{},
		}),
	}

	docs, err := RetrieveDocuments(deps, "rib ccm 2023", localDate(2026, 8, 12))
	if err != nil {
		t.Fatalf("RetrieveDocuments: %v", err)
	}
	if len(store.ftsCalls) != 2 {
		t.Fatalf("fts calls = %d, want 2", len(store.ftsCalls))
	}
	if strings.Contains(store.ftsCalls[1].match, "2023") {
		t.Fatalf("second match = %q, must have relaxed the 2023 keyword", store.ftsCalls[1].match)
	}
	if len(docs) != 1 || docs[0].ID != 7 {
		t.Fatalf("docs = %#v, want [7]", docs)
	}
}

func TestRetrieveDocumentsFallsBackToTokenScorerOnFtsError(t *testing.T) {
	// ported: 'falls back to the token scorer when FTS5 throws, never surfacing an error'
	getAllCalls := 0
	store := &fakeStore{
		allDocs: func() ([]database.DocumentRecord, error) {
			getAllCalls++
			return []database.DocumentRecord{{ID: 99, Title: "RIB Banque", Category: "bank", Subcategory: "x", Date: "2024-01-01", Summary: ""}}, nil
		},
		searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
			return nil, errors.New("no such module: fts5")
		},
	}
	deps := Deps{
		Store: store,
		PlanQuery: planFunc(chatquery.StructuredQuery{
			DocTypes: []string{"rib"}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{},
		}),
	}

	docs, err := RetrieveDocuments(deps, "rib", localDate(2026, 8, 12))
	if err != nil {
		t.Fatalf("RetrieveDocuments must not surface the FTS error: %v", err)
	}
	if docs == nil {
		t.Fatalf("docs must be a non-nil slice")
	}
	if getAllCalls != 1 {
		t.Fatalf("GetAllDocuments calls = %d, want 1", getAllCalls)
	}
}

func TestRetrieveDocumentsHonoursExplicitCountOverModelLimit(t *testing.T) {
	// ported: 'honours an explicit count in the user words over the model limit'
	modelLimit := 10
	dates := []string{"2025-07-31", "2025-08-31", "2025-09-30", "2025-10-31", "2025-11-30", "2025-12-31"}
	hits := make([]database.DocumentRecord, len(dates))
	for i, d := range dates {
		hits[i] = database.DocumentRecord{ID: int64(i + 1), Title: "Bulletin " + d, Category: "bulletin_salaire", Subcategory: "acme", Date: d, Summary: ""}
	}
	store := &fakeStore{searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
		return hits, nil
	}}
	deps := Deps{
		Store: store,
		PlanQuery: planFunc(chatquery.StructuredQuery{
			DocTypes: []string{"bulletin"}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{}, Limit: &modelLimit,
		}),
	}

	docs, err := RetrieveDocuments(deps, "les 3 derniers bulletins de salaire", localDate(2026, 8, 12))
	if err != nil {
		t.Fatalf("RetrieveDocuments: %v", err)
	}
	if len(docs) != 3 {
		t.Fatalf("len(docs) = %d, want 3 (the user's explicit count)", len(docs))
	}
	if store.ftsCalls[0].limit <= 3 {
		t.Fatalf("fts limit = %d, want the over-fetch headroom above 3", store.ftsCalls[0].limit)
	}
}

func TestRetrieveDocumentsIndefiniteArticleIsNotACount(t *testing.T) {
	// ported: 'does not read the French indefinite article as a request for exactly one document'
	store := &fakeStore{searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
		return []database.DocumentRecord{}, nil
	}}
	deps := Deps{
		Store: store,
		PlanQuery: planFunc(chatquery.StructuredQuery{
			DocTypes: []string{"rib"}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{},
		}),
	}

	if _, err := RetrieveDocuments(deps, "j'ai besoin d'un RIB", localDate(2026, 8, 12)); err != nil {
		t.Fatalf("RetrieveDocuments: %v", err)
	}
	if store.ftsCalls[0].limit <= 1 {
		t.Fatalf("fts limit = %d, want > 1 (no explicit count)", store.ftsCalls[0].limit)
	}
}

func TestRetrieveDocumentsReadsSingularRequestAsOne(t *testing.T) {
	// ported: 'still reads an explicit singular request as one document'
	dates := []string{"2025-06-30", "2025-07-31", "2025-08-31", "2025-09-30"}
	hits := make([]database.DocumentRecord, len(dates))
	for i, d := range dates {
		hits[i] = database.DocumentRecord{ID: int64(i + 1), Title: "Bulletin " + d, Category: "bulletin_salaire", Subcategory: "acme", Date: d, Summary: ""}
	}
	store := &fakeStore{searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
		return hits, nil
	}}
	deps := Deps{
		Store: store,
		PlanQuery: planFunc(chatquery.StructuredQuery{
			DocTypes: []string{"bulletin"}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{},
		}),
	}

	docs, err := RetrieveDocuments(deps, "mon dernier bulletin de salaire", localDate(2026, 8, 12))
	if err != nil {
		t.Fatalf("RetrieveDocuments: %v", err)
	}
	if len(docs) != 1 {
		t.Fatalf("len(docs) = %d, want 1", len(docs))
	}
	if store.ftsCalls[0].limit <= 1 {
		t.Fatalf("fts limit = %d, want > 1 (over-fetch headroom)", store.ftsCalls[0].limit)
	}
}

// ---- added: real store integration over a temp database ----

// TestRetrieveDocumentsWithRealStore seeds a t.TempDir() database through the store's own
// InsertDocumentRecord and runs the real FTS5 path end to end. Never touches the live
// pdf_triage.db or any operator file.
func TestRetrieveDocumentsWithRealStore(t *testing.T) {
	store, err := database.Open(filepath.Join(t.TempDir(), "chat.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	id, err := store.InsertDocumentRecord(database.NewDocument{
		Checksum:         "rib-checksum",
		Title:            "RIB Crédit Mutuel",
		Date:             "2024-01-15",
		Category:         "bank",
		Subcategory:      "credit_mutuel",
		Summary:          "Relevé d'identité bancaire",
		Tags:             []string{"rib"},
		OriginalFilename: "rib.pdf",
		OriginalPath:     "/raws/rib.pdf",
		NewPath:          "/archive/bank/credit_mutuel/2024/rib.pdf",
	})
	if err != nil {
		t.Fatalf("InsertDocumentRecord: %v", err)
	}

	deps := Deps{
		Store: store,
		PlanQuery: planFunc(chatquery.StructuredQuery{
			DocTypes: []string{"rib"}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{},
		}),
	}
	docs, err := RetrieveDocuments(deps, "RIB", localDate(2026, 8, 12))
	if err != nil {
		t.Fatalf("RetrieveDocuments: %v", err)
	}
	if len(docs) != 1 || docs[0].ID != id {
		t.Fatalf("docs = %#v, want the seeded document id %d", docs, id)
	}
}
