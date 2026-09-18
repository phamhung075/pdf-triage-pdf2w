package aichat

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/chatquery"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

// Added (not in the TS suite): the catch path (ai-chat-assistant.ts:300-320) returns a fallback
// answer over the already-retrieved documents and deliberately omits file_type from every
// formatted document. This pins that response shape.
func TestProcessChatQueryCatchPathOmitsFileType(t *testing.T) {
	docs := mockDocs()
	store := &fakeStore{searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
		return []database.DocumentRecord{docs[2]}, nil
	}}
	chat := &fakeChat{err: errors.New("Ollama is down")}
	deps := Deps{
		Store: store, Ollama: chat,
		PlanQuery: planFunc(chatquery.StructuredQuery{DocTypes: []string{"bulletin"}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{}}),
	}

	res, err := ProcessChatQuery(deps, "bulletin", nil, time.Date(2026, 8, 12, 0, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatalf("ProcessChatQuery must not surface the Ollama failure: %v", err)
	}
	if !strings.Contains(res.Answer, "trouvé(s) dans vos archives") {
		t.Fatalf("answer = %q, want the archive-fallback wording", res.Answer)
	}
	if len(res.MatchedDocuments) != 1 || res.MatchedDocuments[0].ID != 3 {
		t.Fatalf("matchedDocuments = %#v, want the single retrieved doc", res.MatchedDocuments)
	}
	if res.MatchedDocuments[0].FileType != "" {
		t.Fatalf("catch path FileType = %q, want empty", res.MatchedDocuments[0].FileType)
	}
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "file_type") {
		t.Fatalf("catch path JSON must omit file_type: %s", raw)
	}
}

// Added (not in the TS suite): the success path always resolves a file_type (detectFileType) and
// the empty-answer case falls back to the selected-documents wording.
func TestProcessChatQuerySuccessPathIncludesFileType(t *testing.T) {
	docs := mockDocs()
	store := &fakeStore{searchFts: func(string, database.FtsSearchFilters, int) ([]database.DocumentRecord, error) {
		return []database.DocumentRecord{docs[2]}, nil
	}}
	chat := &fakeChat{response: ollama.TextCompletion{Response: ""}}
	deps := Deps{
		Store: store, Ollama: chat,
		PlanQuery: planFunc(chatquery.StructuredQuery{DocTypes: []string{"bulletin"}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{}}),
	}

	res, err := ProcessChatQuery(deps, "bulletin", nil, time.Date(2026, 8, 12, 0, 0, 0, 0, time.Local))
	if err != nil {
		t.Fatalf("ProcessChatQuery: %v", err)
	}
	if !strings.Contains(res.Answer, "sélectionné(s)") {
		t.Fatalf("answer = %q, want the selected-documents wording for an empty model answer", res.Answer)
	}
	if len(res.MatchedDocuments) != 1 {
		t.Fatalf("matchedDocuments = %#v, want one document", res.MatchedDocuments)
	}
	if res.MatchedDocuments[0].FileType != "PDF" {
		t.Fatalf("success path FileType = %q, want PDF (detectFileType fallback)", res.MatchedDocuments[0].FileType)
	}
	if res.MatchedDocuments[0].Subcategory != "acme_corp" {
		t.Fatalf("subcategory = %q, want acme_corp", res.MatchedDocuments[0].Subcategory)
	}
}
