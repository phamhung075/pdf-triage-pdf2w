package chatplanner

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
)

// These cases are ported case-for-case from pdf-triage's
// src/application/chat-query-planner.test.ts (9 cases). The upstream TypeScript suite is GREEN at
// port time (`npx vitest run src/application/chat-query-planner.test.ts` -> 9 passed), so no
// upstream case is pinned red.
//
// The TS suite's two module mocks (the Ollama client and the categories store) become the
// injected Deps here, exactly the seam the TS suite replaced with vi.mock.

func localDate(year int, month time.Month, day int) time.Time {
	return time.Date(year, month, day, 0, 0, 0, 0, time.Local)
}

type fakeChat struct {
	response   ollama.TextCompletion
	err        error
	calls      int
	lastSystem string
	lastUser   string
}

func (f *fakeChat) RequestTextChatCompletion(system, user string) (ollama.TextCompletion, error) {
	f.calls++
	f.lastSystem, f.lastUser = system, user
	return f.response, f.err
}

// standardCategories mirrors the TS suite's getCategoriesConfig mock.
func standardCategories() documentschema.CategoriesConfig {
	return documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{
		{ID: "bank"},
		{ID: "identity"},
	}}
}

func depsFor(chat TextChat) Deps {
	return Deps{Ollama: chat, Categories: standardCategories}
}

func TestBuildPlannerPromptListsLiveTaxonomy(t *testing.T) {
	// ported: 'lists the live taxonomy so the model cannot invent a category'
	system, _ := BuildPlannerPrompt("rib", []string{"bank", "identity"}, localDate(2026, 8, 26))
	if !strings.Contains(system, "bank") {
		t.Fatalf("system prompt does not contain 'bank':\n%s", system)
	}
	if !strings.Contains(system, "identity") {
		t.Fatalf("system prompt does not contain 'identity':\n%s", system)
	}
}

func TestBuildPlannerPromptGroundsCurrentDate(t *testing.T) {
	// ported: 'grounds the prompt in the current date for relative expressions'
	system, _ := BuildPlannerPrompt("les 3 derniers mois", []string{"bank"}, localDate(2026, 8, 26))
	if !strings.Contains(system, "2026") {
		t.Fatalf("system prompt does not contain the current year:\n%s", system)
	}
	if !strings.Contains(system, "2026-08-26") {
		t.Fatalf("system prompt does not contain the grounded date:\n%s", system)
	}
}

func TestBuildPlannerPromptHasNoPersonalEntity(t *testing.T) {
	// ported: 'contains no personal entity — the taxonomy is the only source of names'
	system, userPrompt := BuildPlannerPrompt("rib", []string{"bank"}, localDate(2026, 8, 26))
	haystack := strings.ToLower(system + "\n" + userPrompt)
	if regexp.MustCompile(`paribas|mutuel|foncia`).MatchString(haystack) {
		t.Fatalf("planner prompt leaked a personal entity:\n%s", haystack)
	}
}

func TestPlanQueryParsesWellFormedPlan(t *testing.T) {
	// ported: 'parses a well-formed plan from the model'
	chat := &fakeChat{response: ollama.TextCompletion{Response: `{"docTypes":["rib"],"entities":["credit mutuel"],"keywords":[],"notTerms":["relevé de compte"]}`}}
	plan := PlanQuery(depsFor(chat), "RIB credit mutuel", localDate(2026, 8, 26))

	if !equalStrings(plan.DocTypes, []string{"rib"}) {
		t.Fatalf("docTypes = %#v, want [rib]", plan.DocTypes)
	}
	if !equalStrings(plan.NotTerms, []string{"relevé de compte"}) {
		t.Fatalf("notTerms = %#v, want [relevé de compte]", plan.NotTerms)
	}
	// The live taxonomy must be injected into the request, never hardcoded.
	if !strings.Contains(chat.lastSystem, "bank") || !strings.Contains(chat.lastSystem, "identity") {
		t.Fatalf("requested system prompt missing injected categories:\n%s", chat.lastSystem)
	}
	if !strings.Contains(chat.lastUser, "RIB credit mutuel") {
		t.Fatalf("requested user prompt missing the query: %q", chat.lastUser)
	}
}

func TestPlanQueryUnwrapsFencedPlan(t *testing.T) {
	// ported: 'unwraps a plan the model fenced in a markdown code block'
	chat := &fakeChat{response: ollama.TextCompletion{Response: "```json\n{\"docTypes\":[\"rib\"]}\n```"}}
	plan := PlanQuery(depsFor(chat), "rib", localDate(2026, 8, 26))
	if !equalStrings(plan.DocTypes, []string{"rib"}) {
		t.Fatalf("docTypes = %#v, want [rib]", plan.DocTypes)
	}
}

func TestPlanQueryFallsBackWhenOllamaUnreachable(t *testing.T) {
	// ported: 'falls back to the heuristic planner when Ollama is unreachable'
	chat := &fakeChat{err: errors.New("ECONNREFUSED")}
	plan := PlanQuery(depsFor(chat), "RIB de credit mutuel j'ai besoin", localDate(2026, 8, 26))

	if !containsFold(plan.Keywords, "rib") {
		t.Fatalf("keywords = %#v, want to contain 'rib'", plan.Keywords)
	}
	if containsFold(plan.Keywords, "besoin") {
		t.Fatalf("keywords = %#v, must not contain the stopword 'besoin'", plan.Keywords)
	}
}

func TestPlanQueryFallsBackWhenResponseUnparseable(t *testing.T) {
	// ported: 'falls back to the heuristic planner when the model returns unparseable text'
	chat := &fakeChat{response: ollama.TextCompletion{Response: "Bien sûr ! Voici votre RIB."}}
	plan := PlanQuery(depsFor(chat), "rib credit mutuel", localDate(2026, 8, 26))
	if len(plan.Keywords) == 0 {
		t.Fatalf("keywords = %#v, want a non-empty heuristic plan", plan.Keywords)
	}
}

func TestPlanQueryFallsBackWhenValidJSONHasNoFacet(t *testing.T) {
	// ported: 'falls back when the model returns valid JSON that yields no searchable facet'
	chat := &fakeChat{response: ollama.TextCompletion{Response: `{"docTypes":[],"entities":[],"keywords":[]}`}}
	plan := PlanQuery(depsFor(chat), "rib credit mutuel", localDate(2026, 8, 26))
	if len(plan.Keywords) == 0 {
		t.Fatalf("keywords = %#v, want a non-empty heuristic plan", plan.Keywords)
	}
}

func TestPlanQueryNeverThrows(t *testing.T) {
	// ported: 'never throws, whatever the model does'
	chat := &fakeChat{response: ollama.TextCompletion{Response: ""}}
	plan := PlanQuery(depsFor(chat), "rib", localDate(2026, 8, 26))
	if plan.DocTypes == nil || plan.Entities == nil || plan.Keywords == nil || plan.NotTerms == nil {
		t.Fatalf("plan term arrays must be non-nil, got %#v", plan)
	}
}

// Added (not in the TS suite): a nil Ollama client and a nil categories provider must degrade to
// the heuristic planner rather than panicking — the "never throws" contract has to hold for the
// unwired composition root too.
func TestPlanQueryFallsBackWithNilCollaborators(t *testing.T) {
	plan := PlanQuery(Deps{}, "RIB credit mutuel", localDate(2026, 8, 26))
	if !containsFold(plan.Keywords, "rib") {
		t.Fatalf("keywords = %#v, want to contain 'rib'", plan.Keywords)
	}

	chat := &fakeChat{response: ollama.TextCompletion{Response: `{"docTypes":["rib"]}`}}
	plan = PlanQuery(Deps{Ollama: chat}, "rib", localDate(2026, 8, 26))
	if !equalStrings(plan.DocTypes, []string{"rib"}) {
		t.Fatalf("docTypes = %#v, want [rib] with an empty category list", plan.DocTypes)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
