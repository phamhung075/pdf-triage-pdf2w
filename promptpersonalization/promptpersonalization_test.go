package promptpersonalization

import (
	"regexp"
	"strings"
	"testing"
)

// All cases below are ported verbatim from pdf-triage's src/domain/prompt-personalization.test.ts.
// The TypeScript source is the behavioral source of truth.

func mustParse(t *testing.T, jsonStr string) PromptPersonalization {
	t.Helper()
	p, err := Parse([]byte(jsonStr))
	if err != nil {
		t.Fatalf("Parse(%s) error = %v, want nil", jsonStr, err)
	}
	return p
}

func TestPromptPersonalizationSchema(t *testing.T) {
	t.Run("parses an empty object into fully-defaulted empty collections", func(t *testing.T) {
		parsed := mustParse(t, `{}`)
		if len(parsed.KnownEntities) != 0 {
			t.Fatalf("KnownEntities = %v, want []", parsed.KnownEntities)
		}
		if len(parsed.PriorityRules) != 0 {
			t.Fatalf("PriorityRules = %v, want []", parsed.PriorityRules)
		}
		if parsed.ExtraRulesText != "" {
			t.Fatalf("ExtraRulesText = %q, want \"\"", parsed.ExtraRulesText)
		}
	})

	t.Run("rejects a priority rule with no keywords — a rule that matches nothing is a config mistake, not an empty rule", func(t *testing.T) {
		if _, err := Parse([]byte(`{"priority_rules":[{"keywords":[],"category":"bank"}]}`)); err == nil {
			t.Fatal("Parse() error = nil, want an error")
		}
	})

	t.Run("rejects a priority rule with no category", func(t *testing.T) {
		if _, err := Parse([]byte(`{"priority_rules":[{"keywords":["STMT_CHK_"]}]}`)); err == nil {
			t.Fatal("Parse() error = nil, want an error")
		}
	})

	t.Run("accepts a rule that pins only the category, leaving the subcategory to entity resolution", func(t *testing.T) {
		parsed := mustParse(t, `{"priority_rules":[{"keywords":["MY_CODE"],"category":"bank"}]}`)
		if parsed.PriorityRules[0].Subcategory != "" {
			t.Fatalf("Subcategory = %q, want \"\" (undefined)", parsed.PriorityRules[0].Subcategory)
		}
	})
}

func TestRenderPriorityRulesBlock(t *testing.T) {
	t.Run("renders nothing at all when there is no personalization — the placeholder must vanish cleanly", func(t *testing.T) {
		if got := RenderPriorityRulesBlock(EMPTY_PROMPT_PERSONALIZATION); got != "" {
			t.Fatalf("RenderPriorityRulesBlock(EMPTY) = %q, want \"\"", got)
		}
	})

	t.Run("renders a STEP 0 override block placed before the generic STEP 1 flow", func(t *testing.T) {
		block := RenderPriorityRulesBlock(mustParse(t, `{
			"priority_rules": [
				{"keywords": ["STMT_CHK_", "C/C MYPRODUCT"], "category": "bank", "subcategory": "my_bank"}
			]
		}`))
		for _, want := range []string{
			"STEP 0: USER-SPECIFIC HIGH-PRIORITY OVERRIDES (EVALUATE BEFORE STEP 1)",
			`"STMT_CHK_", "C/C MYPRODUCT"`,
			"Category = 'bank', Subcategory = 'my_bank'",
		} {
			if !strings.Contains(block, want) {
				t.Fatalf("block = %q, want it to contain %q", block, want)
			}
		}
	})

	t.Run("tells the model to resolve the subcategory itself when a rule pins only the category", func(t *testing.T) {
		block := RenderPriorityRulesBlock(mustParse(t, `{
			"priority_rules": [{"keywords": ["BCTC"], "category": "administrative"}]
		}`))
		if !strings.Contains(block, "Category = 'administrative' (resolve the Subcategory from the issuing entity as usual)") {
			t.Fatalf("block = %q, want the resolve-subcategory wording", block)
		}
		if strings.Contains(block, "Subcategory = ''") {
			t.Fatalf("block = %q, want it not to contain Subcategory = ''", block)
		}
	})

	t.Run("appends a per-rule note when one is given", func(t *testing.T) {
		block := RenderPriorityRulesBlock(mustParse(t, `{
			"priority_rules": [{"keywords": ["recXX"], "category": "identity", "subcategory": "recipisse_sejour", "note": "Scan filename prefix only."}]
		}`))
		if !strings.Contains(block, "Scan filename prefix only.") {
			t.Fatalf("block = %q, want it to contain the note", block)
		}
	})

	t.Run("injects extra_rules_text verbatim even when there are no structured rules", func(t *testing.T) {
		block := RenderPriorityRulesBlock(mustParse(t, `{
			"extra_rules_text": "- NEVER file my landlord statements under invoices."
		}`))
		if !strings.Contains(block, "- NEVER file my landlord statements under invoices.") {
			t.Fatalf("block = %q, want it to contain the extra rules text", block)
		}
		if !strings.Contains(block, "STEP 0") {
			t.Fatalf("block = %q, want it to contain STEP 0", block)
		}
	})

	t.Run("ignores rules whose keywords are all blank rather than emitting an unmatchable rule line", func(t *testing.T) {
		block := RenderPriorityRulesBlock(mustParse(t, `{
			"priority_rules": [{"keywords": ["   "], "category": "bank", "subcategory": "x"}]
		}`))
		if block != "" {
			t.Fatalf("block = %q, want \"\"", block)
		}
	})
}

func TestRenderKnownEntitiesBlock(t *testing.T) {
	t.Run("renders nothing when no entities are configured", func(t *testing.T) {
		if got := RenderKnownEntitiesBlock(EMPTY_PROMPT_PERSONALIZATION); got != "" {
			t.Fatalf("RenderKnownEntitiesBlock(EMPTY) = %q, want \"\"", got)
		}
	})

	t.Run("lists the entities and forbids forcing a match the text does not support", func(t *testing.T) {
		block := RenderKnownEntitiesBlock(mustParse(t, `{
			"known_entities": ["ACME CONSEIL", "Lakeside Dental"]
		}`))
		if !strings.Contains(block, `"ACME CONSEIL", "Lakeside Dental"`) {
			t.Fatalf("block = %q, want it to contain the quoted entities", block)
		}
		if !regexp.MustCompile(`(?i)never force one that the text does not actually contain`).MatchString(block) {
			t.Fatalf("block = %q, want the no-forcing warning", block)
		}
	})

	t.Run("drops blank entries instead of emitting empty quoted slots", func(t *testing.T) {
		block := RenderKnownEntitiesBlock(mustParse(t, `{
			"known_entities": ["ACME CONSEIL", "  ", ""]
		}`))
		if !strings.Contains(block, `"ACME CONSEIL"`) {
			t.Fatalf("block = %q, want it to contain ACME CONSEIL", block)
		}
		if strings.Contains(block, `""`) {
			t.Fatalf("block = %q, want no empty quoted slot", block)
		}
	})
}

func TestMatchPriorityRules(t *testing.T) {
	overlay := mustParse(t, `{
		"priority_rules": [
			{"keywords": ["STMT_CHK_", "C/C MYPRODUCT"], "category": "bank", "subcategory": "my_bank"},
			{"keywords": ["GAN"], "category": "health", "subcategory": "my_mutuelle"},
			{"keywords": ["Northwind Academy"], "category": "education", "subcategory": "northwind"},
			{"keywords": ["DEFERRED"], "category": "invoices"}
		]
	}`)

	t.Run("returns null when there is no personalization", func(t *testing.T) {
		if got := MatchPriorityRules("anything at all", EMPTY_PROMPT_PERSONALIZATION); got != nil {
			t.Fatalf("MatchPriorityRules() = %+v, want nil", got)
		}
	})

	t.Run("matches a prefix-style statement code immediately followed by the rest of the filename", func(t *testing.T) {
		// The trailing boundary is deliberately dropped for keywords ending in a separator —
		// "stmt_chk_" is always glued to the account number that follows it.
		hit := MatchPriorityRules("stmt_chk_101_00047_20240607.pdf releve de cheques", overlay)
		if hit == nil {
			t.Fatal("MatchPriorityRules() = nil, want a hit")
		}
		if hit.Categorie != "bank" || hit.Subcategorie != "my_bank" || hit.Keyword != "stmt_chk_" {
			t.Fatalf("hit = %+v, want {bank my_bank stmt_chk_}", hit)
		}
	})

	t.Run("matches a keyword containing regex metacharacters literally", func(t *testing.T) {
		hit := MatchPriorityRules("releve de compte c/c myproduct solde crediteur", overlay)
		if hit == nil || hit.Subcategorie != "my_bank" {
			t.Fatalf("hit = %+v, want subcategorie my_bank", hit)
		}
	})

	t.Run("does not match a short keyword occurring inside a longer word", func(t *testing.T) {
		// "gan" must not fire on "organization" — the classic substring-matching bug.
		if got := MatchPriorityRules("this government organization issued it", overlay); got != nil {
			t.Fatalf("MatchPriorityRules() = %+v, want nil", got)
		}
	})

	t.Run("matches a short keyword standing on its own", func(t *testing.T) {
		hit := MatchPriorityRules("mutuelle gan remboursement soins", overlay)
		if hit == nil || hit.Subcategorie != "my_mutuelle" {
			t.Fatalf("hit = %+v, want subcategorie my_mutuelle", hit)
		}
	})

	t.Run("matches a keyword glued to a trailing date or account number", func(t *testing.T) {
		// Scan prefixes and statement codes are written this way in practice
		// ("recXX20240424", "STMT_CHK_101"), so the boundary must exclude letters, not digits.
		glued := mustParse(t, `{
			"priority_rules": [{"keywords": ["recXX"], "category": "identity", "subcategory": "recipisse"}]
		}`)
		hit := MatchPriorityRules("recxx20240424.pdf", glued)
		if hit == nil || hit.Subcategorie != "recipisse" {
			t.Fatalf("hit = %+v, want subcategorie recipisse", hit)
		}
	})

	t.Run("still refuses to match a keyword glued to trailing LETTERS", func(t *testing.T) {
		glued := mustParse(t, `{
			"priority_rules": [{"keywords": ["NORTHWIND"], "category": "education", "subcategory": "northwind"}]
		}`)
		if got := MatchPriorityRules("a northwinds gale", glued); got != nil {
			t.Fatalf("MatchPriorityRules() = %+v, want nil", got)
		}
	})

	t.Run("matches a multi-word entity name", func(t *testing.T) {
		hit := MatchPriorityRules("northwind academy certificat de scolarite", overlay)
		if hit == nil || hit.Subcategorie != "northwind" {
			t.Fatalf("hit = %+v, want subcategorie northwind", hit)
		}
	})

	t.Run("skips a rule that defers subcategory resolution — a regex classifier has nothing to act on", func(t *testing.T) {
		if got := MatchPriorityRules("this mentions DEFERRED explicitly", overlay); got != nil {
			t.Fatalf("MatchPriorityRules() = %+v, want nil", got)
		}
	})

	t.Run("returns the first matching rule in file order, so ordering is the tie-break", func(t *testing.T) {
		both := mustParse(t, `{
			"priority_rules": [
				{"keywords": ["shared"], "category": "bank", "subcategory": "first"},
				{"keywords": ["shared"], "category": "health", "subcategory": "second"}
			]
		}`)
		hit := MatchPriorityRules("a shared token", both)
		if hit == nil || hit.Subcategorie != "first" {
			t.Fatalf("hit = %+v, want subcategorie first", hit)
		}
	})
}

func TestMatchPriorityRulesFilenameScope(t *testing.T) {
	scoped := mustParse(t, `{
		"priority_rules": [
			{"keywords": ["paiement"], "category": "invoices", "subcategory": "cdiscount", "scope": "filename"}
		]
	}`)
	unscoped := mustParse(t, `{
		"priority_rules": [
			{"keywords": ["paiement"], "category": "invoices", "subcategory": "cdiscount"}
		]
	}`)

	t.Run("never fires a filename-scoped rule on body text alone", func(t *testing.T) {
		// 'paiement' appears in the body but NOT in the filename — the learned rule must stay silent.
		if got := MatchPriorityRules("avis d impôt — mode de paiement", scoped, "Avis_d_impot_2026.pdf"); got != nil {
			t.Fatalf("MatchPriorityRules() = %+v, want nil", got)
		}
	})

	t.Run("fires a filename-scoped rule when the keyword is in the filename", func(t *testing.T) {
		hit := MatchPriorityRules("some body text", scoped, "calendrier de paiement.PDF")
		if hit == nil || hit.Subcategorie != "cdiscount" {
			t.Fatalf("hit = %+v, want subcategorie cdiscount", hit)
		}
	})

	t.Run("keeps the legacy default scope of matching text or filename for hand-curated rules", func(t *testing.T) {
		hit := MatchPriorityRules("mode de paiement", unscoped, "Avis_d_impot_2026.pdf")
		if hit == nil || hit.Subcategorie != "cdiscount" {
			t.Fatalf("hit = %+v, want subcategorie cdiscount", hit)
		}
	})
}

func TestRenderPriorityRulesBlockFilenameScopeWording(t *testing.T) {
	t.Run("tells the model a filename-scoped rule matches the FILENAME only", func(t *testing.T) {
		block := RenderPriorityRulesBlock(mustParse(t, `{
			"priority_rules": [
				{"keywords": ["paiement"], "category": "invoices", "subcategory": "cdiscount", "scope": "filename"},
				{"keywords": ["Foncia"], "category": "housing", "subcategory": "foncia"}
			]
		}`))
		if !regexp.MustCompile(`(?i)FILENAME contains`).MatchString(block) {
			t.Fatalf("block = %q, want FILENAME wording", block)
		}
		if regexp.MustCompile(`(?i)text or filename contains "paiement"`).MatchString(block) {
			t.Fatalf("block = %q, want no text-or-filename wording for paiement", block)
		}
		if !regexp.MustCompile(`(?i)text or filename contains "Foncia"`).MatchString(block) {
			t.Fatalf("block = %q, want text-or-filename wording for Foncia", block)
		}
		if !regexp.MustCompile(`(?i)STEPS 1-3 STILL WIN`).MatchString(block) {
			t.Fatalf("block = %q, want the STEPS 1-3 exception wording", block)
		}
	})
}
