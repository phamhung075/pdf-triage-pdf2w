package chatquery

import (
	"strings"
	"testing"
)

// All cases below are ported verbatim from pdf-triage's src/domain/chat-query.test.ts.
// The TypeScript source is the behavioral source of truth.

// q mirrors the TS test helper `q(overrides: Partial<StructuredQuery> = {})`.
func q(mutate func(*StructuredQuery)) StructuredQuery {
	s := StructuredQuery{DocTypes: []string{}, Entities: []string{}, Keywords: []string{}, NotTerms: []string{}}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

func mustExpr(t *testing.T, s StructuredQuery) string {
	t.Helper()
	e := BuildFtsMatchExpression(s)
	if e == nil {
		t.Fatalf("BuildFtsMatchExpression(%+v) = nil, want non-nil", s)
	}
	return *e
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func lowerStrings(values []string) []string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = strings.ToLower(v)
	}
	return out
}

func TestStructuredQuerySchema(t *testing.T) {
	t.Run("fills the four term arrays with empty defaults when absent", func(t *testing.T) {
		parsed, err := Parse([]byte(`{}`))
		if err != nil {
			t.Fatalf("Parse() error = %v, want nil", err)
		}
		if len(parsed.DocTypes) != 0 || len(parsed.Entities) != 0 || len(parsed.Keywords) != 0 || len(parsed.NotTerms) != 0 {
			t.Fatalf("parsed = %+v, want all four term arrays empty", parsed)
		}
		if parsed.Category != nil || parsed.Subcategory != nil || parsed.DateFrom != nil || parsed.DateTo != nil || parsed.Limit != nil {
			t.Fatalf("parsed = %+v, want the optional fields undefined (nil)", parsed)
		}
	})

	t.Run("accepts explicit null on optional string fields, as Qwen frequently emits", func(t *testing.T) {
		parsed, err := Parse([]byte(`{"category":null,"dateFrom":null}`))
		if err != nil {
			t.Fatalf("Parse() error = %v, want nil", err)
		}
		if parsed.Category != nil {
			t.Fatalf("Category = %q, want nil (undefined)", *parsed.Category)
		}
		if parsed.DateFrom != nil {
			t.Fatalf("DateFrom = %q, want nil (undefined)", *parsed.DateFrom)
		}
	})

	t.Run("accepts an explicit null term array, which Qwen emits as often as it omits the key", func(t *testing.T) {
		parsed, err := Parse([]byte(`{"docTypes":null,"keywords":["rib"]}`))
		if err != nil {
			t.Fatalf("Parse() error = %v, want nil", err)
		}
		if len(parsed.DocTypes) != 0 {
			t.Fatalf("DocTypes = %v, want []", parsed.DocTypes)
		}
		if len(parsed.Keywords) != 1 || parsed.Keywords[0] != "rib" {
			t.Fatalf("Keywords = %v, want [rib]", parsed.Keywords)
		}
	})

	t.Run("coerces a stringified limit and rejects an absurd one", func(t *testing.T) {
		parsed, err := Parse([]byte(`{"limit":"3"}`))
		if err != nil {
			t.Fatalf("Parse() error = %v, want nil", err)
		}
		if parsed.Limit == nil || *parsed.Limit != 3 {
			t.Fatalf("Limit = %v, want 3", parsed.Limit)
		}
		parsed, err = Parse([]byte(`{"limit":9999}`))
		if err != nil {
			t.Fatalf("Parse() error = %v, want nil", err)
		}
		if parsed.Limit != nil {
			t.Fatalf("Limit = %d, want nil (undefined)", *parsed.Limit)
		}
	})

	t.Run("drops non-string entries inside a term array instead of throwing", func(t *testing.T) {
		parsed, err := Parse([]byte(`{"keywords":["rib",42,null]}`))
		if err != nil {
			t.Fatalf("Parse() error = %v, want nil", err)
		}
		if len(parsed.Keywords) != 1 || parsed.Keywords[0] != "rib" {
			t.Fatalf("Keywords = %v, want [rib]", parsed.Keywords)
		}
	})
}

func TestBuildFtsMatchExpression(t *testing.T) {
	t.Run("ORs terms inside a facet and ANDs the facets", func(t *testing.T) {
		got := mustExpr(t, q(func(s *StructuredQuery) {
			s.DocTypes = []string{"rib"}
			s.Entities = []string{"credit mutuel"}
		}))
		if want := `("rib") AND ("credit mutuel")`; got != want {
			t.Fatalf("expr = %q, want %q", got, want)
		}
	})

	t.Run("reproduces the expression proven against the real index", func(t *testing.T) {
		got := mustExpr(t, q(func(s *StructuredQuery) {
			s.DocTypes = []string{"rib", "identité bancaire"}
			s.Entities = []string{"mutuel", "credit mutuel"}
		}))
		if want := `("rib" OR "identité bancaire") AND ("mutuel" OR "credit mutuel")`; got != want {
			t.Fatalf("expr = %q, want %q", got, want)
		}
	})

	t.Run("parenthesises the positive part before NOT, because FTS5 binds NOT tighter than AND", func(t *testing.T) {
		// Without the outer parens, `A AND B NOT C` parses as `A AND (B NOT C)` and the
		// exclusion would only apply to the second facet.
		got := mustExpr(t, q(func(s *StructuredQuery) {
			s.DocTypes = []string{"rib"}
			s.Entities = []string{"mutuel"}
			s.NotTerms = []string{"relevé de compte"}
		}))
		if want := `(("rib") AND ("mutuel")) NOT ("relevé de compte")`; got != want {
			t.Fatalf("expr = %q, want %q", got, want)
		}
	})

	t.Run("returns null when every facet is empty, so the caller knows no FTS query is possible", func(t *testing.T) {
		if got := BuildFtsMatchExpression(q(nil)); got != nil {
			t.Fatalf("expr = %q, want nil", *got)
		}
		if got := BuildFtsMatchExpression(q(func(s *StructuredQuery) { s.NotTerms = []string{"x"} })); got != nil {
			t.Fatalf("expr = %q, want nil", *got)
		}
	})

	t.Run("escapes embedded double quotes by doubling them", func(t *testing.T) {
		got := mustExpr(t, q(func(s *StructuredQuery) { s.Keywords = []string{`le "vrai" doc`} }))
		if want := `("le ""vrai"" doc")`; got != want {
			t.Fatalf("expr = %q, want %q", got, want)
		}
	})

	for _, term := range []string{
		"a NEAR/2 b", "col:value", "wild*card", "(paren)", "dash-term",
		"AND", "OR", "NOT", "^caret", "a AND b OR c",
	} {
		t.Run("neutralises FTS5 syntax in "+term+" by quoting it as a phrase", func(t *testing.T) {
			got := mustExpr(t, q(func(s *StructuredQuery) { s.Keywords = []string{term} }))
			if want := `("` + term + `")`; got != want {
				t.Fatalf("expr = %q, want %q", got, want)
			}
		})
	}

	t.Run("drops terms with no alphanumeric content, which tokenise to nothing and error the MATCH", func(t *testing.T) {
		got := mustExpr(t, q(func(s *StructuredQuery) { s.Keywords = []string{"???", "  ", "-", "rib"} }))
		if want := `("rib")`; got != want {
			t.Fatalf("expr = %q, want %q", got, want)
		}
	})

	t.Run("drops a facet that becomes empty after filtering rather than emitting ()", func(t *testing.T) {
		got := mustExpr(t, q(func(s *StructuredQuery) {
			s.DocTypes = []string{"???"}
			s.Entities = []string{"mutuel"}
		}))
		if want := `("mutuel")`; got != want {
			t.Fatalf("expr = %q, want %q", got, want)
		}
	})

	t.Run("trims surrounding whitespace and de-duplicates case-insensitively within a facet", func(t *testing.T) {
		got := mustExpr(t, q(func(s *StructuredQuery) { s.Keywords = []string{"  RIB  ", "rib", "Rib"} }))
		if want := `("RIB")`; got != want {
			t.Fatalf("expr = %q, want %q", got, want)
		}
	})
}

func TestPlanQueryHeuristic(t *testing.T) {
	t.Run("drops French stopwords and the filler that polluted the old scorer", func(t *testing.T) {
		plan := PlanQueryHeuristic("RIB de credit mutuel j'ai besoin")
		if containsString(plan.Keywords, "besoin") {
			t.Fatalf("Keywords = %v, want it not to contain besoin", plan.Keywords)
		}
		if containsString(plan.Keywords, "de") {
			t.Fatalf("Keywords = %v, want it not to contain de", plan.Keywords)
		}
		lowered := lowerStrings(plan.Keywords)
		for _, want := range []string{"rib", "credit", "mutuel"} {
			if !containsString(lowered, want) {
				t.Fatalf("Keywords = %v, want it to contain %q", plan.Keywords, want)
			}
		}
	})

	t.Run("promotes a token matching a known tag into the entities facet", func(t *testing.T) {
		plan := PlanQueryHeuristic("relevé credit_mutuel 2023", []string{"credit_mutuel", "rib"}...)
		if !containsString(plan.Entities, "credit_mutuel") {
			t.Fatalf("Entities = %v, want it to contain credit_mutuel", plan.Entities)
		}
		if containsString(plan.Keywords, "credit_mutuel") {
			t.Fatalf("Keywords = %v, want it not to contain credit_mutuel", plan.Keywords)
		}
	})

	t.Run("extracts a bare year into an ISO date range and out of the keywords", func(t *testing.T) {
		plan := PlanQueryHeuristic("relevé de compte 2023")
		if plan.DateFrom == nil || *plan.DateFrom != "2023-01-01" {
			t.Fatalf("DateFrom = %v, want 2023-01-01", plan.DateFrom)
		}
		if plan.DateTo == nil || *plan.DateTo != "2023-12-31" {
			t.Fatalf("DateTo = %v, want 2023-12-31", plan.DateTo)
		}
		if containsString(plan.Keywords, "2023") {
			t.Fatalf("Keywords = %v, want it not to contain 2023", plan.Keywords)
		}
	})

	t.Run("ignores a 4-digit number that is not a plausible document year", func(t *testing.T) {
		plan := PlanQueryHeuristic("facture 9999")
		if plan.DateFrom != nil {
			t.Fatalf("DateFrom = %q, want nil (undefined)", *plan.DateFrom)
		}
	})

	t.Run("produces a compilable expression for a realistic query", func(t *testing.T) {
		if BuildFtsMatchExpression(PlanQueryHeuristic("bulletin de salaire")) == nil {
			t.Fatal("BuildFtsMatchExpression() = nil, want non-nil")
		}
	})

	t.Run("returns an all-empty plan for a message of pure stopwords, so the caller falls back", func(t *testing.T) {
		plan := PlanQueryHeuristic("j'ai besoin de le la les")
		if BuildFtsMatchExpression(plan) != nil {
			t.Fatal("BuildFtsMatchExpression() != nil, want nil")
		}
	})
}

func TestRelaxQuery(t *testing.T) {
	full := StructuredQuery{
		DocTypes: []string{"rib"}, Entities: []string{"credit mutuel"},
		Keywords: []string{"2023"}, NotTerms: []string{"relevé"},
	}

	t.Run("drops keywords first — the weakest facet", func(t *testing.T) {
		r := RelaxQuery(full)
		if r == nil {
			t.Fatal("RelaxQuery() = nil, want non-nil")
		}
		if len(r.Keywords) != 0 {
			t.Fatalf("Keywords = %v, want []", r.Keywords)
		}
		if len(r.NotTerms) != 1 || r.NotTerms[0] != "relevé" {
			t.Fatalf("NotTerms = %v, want [relevé]", r.NotTerms)
		}
		if len(r.Entities) != 1 || r.Entities[0] != "credit mutuel" {
			t.Fatalf("Entities = %v, want [credit mutuel]", r.Entities)
		}
	})

	t.Run("drops notTerms second", func(t *testing.T) {
		first := RelaxQuery(full)
		r := RelaxQuery(*first)
		if r == nil {
			t.Fatal("RelaxQuery() = nil, want non-nil")
		}
		if len(r.NotTerms) != 0 {
			t.Fatalf("NotTerms = %v, want []", r.NotTerms)
		}
		if len(r.Entities) != 1 || r.Entities[0] != "credit mutuel" {
			t.Fatalf("Entities = %v, want [credit mutuel]", r.Entities)
		}
	})

	t.Run("drops entities third, keeping the document type longest", func(t *testing.T) {
		first := RelaxQuery(full)
		second := RelaxQuery(*first)
		r := RelaxQuery(*second)
		if r == nil {
			t.Fatal("RelaxQuery() = nil, want non-nil")
		}
		if len(r.Entities) != 0 {
			t.Fatalf("Entities = %v, want []", r.Entities)
		}
		if len(r.DocTypes) != 1 || r.DocTypes[0] != "rib" {
			t.Fatalf("DocTypes = %v, want [rib]", r.DocTypes)
		}
	})

	t.Run("returns null once only docTypes remain, so the ladder terminates", func(t *testing.T) {
		cur := &full
		for i := 0; i < 3; i++ {
			cur = RelaxQuery(*cur)
		}
		if got := RelaxQuery(*cur); got != nil {
			t.Fatalf("RelaxQuery() = %+v, want nil", got)
		}
	})

	t.Run("preserves the taxonomy and date filters at every rung", func(t *testing.T) {
		withFilters := full
		cat := "bank"
		dateFrom := "2023-01-01"
		withFilters.Category = &cat
		withFilters.DateFrom = &dateFrom
		r := RelaxQuery(withFilters)
		if r == nil {
			t.Fatal("RelaxQuery() = nil, want non-nil")
		}
		if r.Category == nil || *r.Category != "bank" {
			t.Fatalf("Category = %v, want bank", r.Category)
		}
		if r.DateFrom == nil || *r.DateFrom != "2023-01-01" {
			t.Fatalf("DateFrom = %v, want 2023-01-01", r.DateFrom)
		}
	})
}
