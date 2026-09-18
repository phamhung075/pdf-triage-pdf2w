package decisionrule

import (
	"strings"
	"testing"
)

// All cases below are ported verbatim from pdf-triage's src/domain/decision-rule.test.ts.
// The TypeScript source is the behavioral source of truth.

func intPtr(v int) *int { return &v }

func baseDecision() HumanDecisionLike {
	return HumanDecisionLike{
		ID:                 7,
		OriginalFilename:   "STMT_CHK_101.pdf",
		Title:              "Relevé de chèques BNP",
		NewCategory:        "bank",
		NewSubcategory:     "bnp_paribas",
		UserFeedbackReason: "This is a BNP check statement, not rent",
		Enabled:            intPtr(1),
	}
}

func containsString(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func TestDeriveRuleKeywords(t *testing.T) {
	t.Run("keeps distinctive filename codes (bank product codes, scanner prefixes) and drops years/account numbers", func(t *testing.T) {
		keywords := DeriveRuleKeywords("STMT_CHK_101_00047_20240607.pdf", "Relevé de chèques BNP")
		// 'stmt', 'chk' come from the filename; 'bnp' from the title (issuer name).
		for _, want := range []string{"stmt", "chk", "bnp"} {
			if !containsString(keywords, want) {
				t.Fatalf("keywords = %v, want it to contain %q", keywords, want)
			}
		}
		for _, k := range keywords {
			if isAllDigits(k) {
				t.Fatalf("keywords = %v, want no pure-digit token", keywords)
			}
		}
	})

	t.Run("treats a token found in BOTH filename and title as the strongest signal and ranks it first", func(t *testing.T) {
		keywords := DeriveRuleKeywords("NORTHWIND_2024.pdf", "Northwind Academy — certificat de scolarité")
		if len(keywords) == 0 || keywords[0] != "northwind" {
			t.Fatalf("keywords = %v, want first element northwind", keywords)
		}
	})

	t.Run("filters generic document-type words regardless of accents", func(t *testing.T) {
		keywords := DeriveRuleKeywords("Releve_2024.pdf", "Relevé de compte")
		for _, bad := range []string{"releve", "relevé", "compte"} {
			if containsString(keywords, bad) {
				t.Fatalf("keywords = %v, want it not to contain %q", keywords, bad)
			}
		}
	})

	t.Run("keeps a scanner prefix glued to a date as ONE token", func(t *testing.T) {
		keywords := DeriveRuleKeywords("recXX20240424.pdf", "Récépissé de demande de titre de séjour")
		if !containsString(keywords, "recxx20240424") {
			t.Fatalf("keywords = %v, want it to contain recxx20240424", keywords)
		}
		for _, bad := range []string{"recepisse", "sejour", "demande"} {
			if containsString(keywords, bad) {
				t.Fatalf("keywords = %v, want it not to contain %q", keywords, bad)
			}
		}
	})

	t.Run("returns an empty array when nothing distinctive can be found", func(t *testing.T) {
		if got := DeriveRuleKeywords("document.pdf", "Document"); len(got) != 0 {
			t.Fatalf("keywords = %v, want empty", got)
		}
	})

	t.Run("caps the number of derived keywords", func(t *testing.T) {
		keywords := DeriveRuleKeywords("alpha_beta_gamma_delta_epsilon.pdf", "")
		if len(keywords) > 3 {
			t.Fatalf("len(keywords) = %d, want <= 3", len(keywords))
		}
	})
}

func TestDecisionsToPriorityRules(t *testing.T) {
	t.Run("turns an enabled decision into a STEP 0 priority rule pinning category AND subcategory", func(t *testing.T) {
		rules := DecisionsToPriorityRules([]HumanDecisionLike{baseDecision()})
		if len(rules) != 1 {
			t.Fatalf("len(rules) = %d, want 1", len(rules))
		}
		if rules[0].Category != "bank" {
			t.Fatalf("category = %q, want bank", rules[0].Category)
		}
		if rules[0].Subcategory != "bnp_paribas" {
			t.Fatalf("subcategory = %q, want bnp_paribas", rules[0].Subcategory)
		}
		if len(rules[0].Keywords) == 0 {
			t.Fatal("keywords = empty, want non-empty")
		}
	})

	t.Run("skips disabled decisions", func(t *testing.T) {
		d := baseDecision()
		d.Enabled = intPtr(0)
		if rules := DecisionsToPriorityRules([]HumanDecisionLike{d}); len(rules) != 0 {
			t.Fatalf("len(rules) = %d, want 0", len(rules))
		}
	})

	t.Run("skips decisions whose target subcategory is forbidden (Golden Rule #4)", func(t *testing.T) {
		for _, bad := range []string{"general", "divers", "2024"} {
			d := baseDecision()
			d.NewSubcategory = bad
			if rules := DecisionsToPriorityRules([]HumanDecisionLike{d}); len(rules) != 0 {
				t.Fatalf("subcategory %q: len(rules) = %d, want 0", bad, len(rules))
			}
		}
	})

	t.Run("skips decisions with no target category at all", func(t *testing.T) {
		d := baseDecision()
		d.NewCategory = ""
		if rules := DecisionsToPriorityRules([]HumanDecisionLike{d}); len(rules) != 0 {
			t.Fatalf("len(rules) = %d, want 0", len(rules))
		}
	})

	t.Run("derives keywords lazily for legacy records that have none stored", func(t *testing.T) {
		legacy := baseDecision()
		legacy.RuleKeywords = []string{}
		rules := DecisionsToPriorityRules([]HumanDecisionLike{legacy})
		if len(rules) != 1 {
			t.Fatalf("len(rules) = %d, want 1", len(rules))
		}
		if len(rules[0].Keywords) == 0 {
			t.Fatal("keywords = empty, want non-empty")
		}
		if !containsString(rules[0].Keywords, "stmt") {
			t.Fatalf("keywords = %v, want it to contain stmt", rules[0].Keywords)
		}
	})

	t.Run("uses stored keywords verbatim when present (never re-derives)", func(t *testing.T) {
		d := baseDecision()
		d.RuleKeywords = []string{"MY_CODE", "acme"}
		rules := DecisionsToPriorityRules([]HumanDecisionLike{d})
		if len(rules) != 1 {
			t.Fatalf("len(rules) = %d, want 1", len(rules))
		}
		if len(rules[0].Keywords) != 2 || rules[0].Keywords[0] != "MY_CODE" || rules[0].Keywords[1] != "acme" {
			t.Fatalf("keywords = %v, want [MY_CODE acme]", rules[0].Keywords)
		}
	})

	t.Run("skips a decision that ends up with no usable keyword", func(t *testing.T) {
		d := baseDecision()
		d.OriginalFilename = "doc.pdf"
		d.Title = "Document"
		d.RuleKeywords = []string{}
		if rules := DecisionsToPriorityRules([]HumanDecisionLike{d}); len(rules) != 0 {
			t.Fatalf("len(rules) = %d, want 0", len(rules))
		}
	})

	t.Run("carries the human reason into the rule note (minus boilerplate), tagged with the decision id", func(t *testing.T) {
		rules := DecisionsToPriorityRules([]HumanDecisionLike{baseDecision()})
		if len(rules) != 1 {
			t.Fatalf("len(rules) = %d, want 1", len(rules))
		}
		for _, want := range []string{"Auto-learned from a human move", "(decision #7)", "This is a BNP check statement, not rent"} {
			if !strings.Contains(rules[0].Note, want) {
				t.Fatalf("note = %q, want it to contain %q", rules[0].Note, want)
			}
		}
	})

	t.Run("does not echo boilerplate reasons into the note", func(t *testing.T) {
		d := baseDecision()
		d.UserFeedbackReason = "Manual user selection"
		rules := DecisionsToPriorityRules([]HumanDecisionLike{d})
		if len(rules) != 1 {
			t.Fatalf("len(rules) = %d, want 1", len(rules))
		}
		if strings.Contains(rules[0].Note, "Manual user selection") {
			t.Fatalf("note = %q, want it not to contain the boilerplate reason", rules[0].Note)
		}
	})

	t.Run("caps the number of injected rules, keeping the NEWEST decisions", func(t *testing.T) {
		decisions := make([]HumanDecisionLike, 0, 40)
		for i := 1; i <= 40; i++ {
			d := baseDecision()
			d.ID = i
			decisions = append(decisions, d)
		}
		rules := DecisionsToPriorityRules(decisions, 10)
		if len(rules) != 10 {
			t.Fatalf("len(rules) = %d, want 10", len(rules))
		}
		if !strings.Contains(rules[0].Note, "(decision #1)") {
			t.Fatalf("note = %q, want it to contain (decision #1)", rules[0].Note)
		}
	})

	t.Run("keeps rules that pin only the category when the subcategory is empty", func(t *testing.T) {
		d := baseDecision()
		d.NewSubcategory = ""
		rules := DecisionsToPriorityRules([]HumanDecisionLike{d})
		if len(rules) != 1 {
			t.Fatalf("len(rules) = %d, want 1", len(rules))
		}
		if rules[0].Subcategory != "" {
			t.Fatalf("subcategory = %q, want empty", rules[0].Subcategory)
		}
	})
}

func TestDeriveRuleKeywordsFinanceGuard(t *testing.T) {
	t.Run("rejects the single-word keywords that misfiled three documents (\"paiement\", \"échéance\", \"calendrier\")", func(t *testing.T) {
		// These derived keywords fired on the BODY TEXT of unrelated documents: a SEPA mandate and
		// an income-tax notice matched 'paiement' -> invoices/cdiscount, a property-tax notice
		// matched 'échéance' -> invoices/foncia. They describe WHAT a document does, never WHO
		// issued it, so they must never become match keywords.
		cases := [][2]string{
			{"calendrier de paiement.PDF", "calendrier de paiement"},
			{"QuittanceDeLoyer-20190101.pdf", "Relevé de compte Crédit - Quittance et Avis d'échéance"},
			{"Echeancier 2024.pdf", "Échéancier"},
		}
		for _, c := range cases {
			if got := DeriveRuleKeywords(c[0], c[1]); len(got) != 0 {
				t.Fatalf("DeriveRuleKeywords(%q, %q) = %v, want empty", c[0], c[1], got)
			}
		}
	})

	t.Run("still keeps genuinely distinctive tokens from the same documents", func(t *testing.T) {
		// The vendor name survives; only the generic money-movement words are filtered.
		if got := DeriveRuleKeywords("calendrier de paiement cdiscount energie.PDF", "Calendrier de paiement Cdiscount Energie"); !containsString(got, "cdiscount") {
			t.Fatalf("keywords = %v, want it to contain cdiscount", got)
		}
		if got := DeriveRuleKeywords("QuittanceDeLoyer-Foncia-20190101.pdf", "Quittance de loyer Foncia"); !containsString(got, "foncia") {
			t.Fatalf("keywords = %v, want it to contain foncia", got)
		}
	})
}

func TestDecisionsToPriorityRulesFilenameScope(t *testing.T) {
	t.Run("tags every learned rule as filename-scoped so body-text mentions can never re-fire it", func(t *testing.T) {
		rules := DecisionsToPriorityRules([]HumanDecisionLike{{
			ID:               7,
			OriginalFilename: "STMT_CHK_101.pdf",
			Title:            "Relevé de chèques BNP",
			NewCategory:      "bank",
			NewSubcategory:   "bnp_paribas",
			Enabled:          intPtr(1),
		}})
		if len(rules) != 1 {
			t.Fatalf("len(rules) = %d, want 1", len(rules))
		}
		if rules[0].Scope != "filename" {
			t.Fatalf("scope = %q, want filename", rules[0].Scope)
		}
	})
}
