// Tests for the Go port of src/infrastructure/prompt-personalization-store.ts. Case-for-case port
// of src/infrastructure/prompt-personalization-store.test.ts (6 cases, all green upstream), plus
// cases for the taxonomy-hint block, the unreadable-file branch, JS trim semantics and the two
// injected surfaces.
//
// Golden Rule 15: every fixture below is a synthetic .prompts.private.json / manual_decisions.json
// / taxonomy_hints.json written inside t.TempDir(). The operator's real files are never opened.
package promptpersonalization

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/classificationresolution"
	"github.com/phamhung075/pdf-triage-pdf2w/prompt"
	promptdomain "github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
	"github.com/phamhung075/pdf-triage-pdf2w/store/taxonomyhints"
)

type testEnv struct {
	store         *Store
	promptsFile   string
	decisionsFile string
	hintsFile     string
	stderr        *bytes.Buffer
}

// newTestEnv builds a Store over private files inside t.TempDir(). Both read surfaces are real
// stores pointed at (possibly absent) temp files; io.Discard is replaced by a capture buffer.
func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	dir := t.TempDir()
	env := &testEnv{
		promptsFile:   filepath.Join(dir, ".prompts.private.json"),
		decisionsFile: filepath.Join(dir, "manual_decisions.json"),
		hintsFile:     filepath.Join(dir, "taxonomy_hints.json"),
		stderr:        &bytes.Buffer{},
	}
	env.store = New(Options{
		PromptsPrivateFile: env.promptsFile,
		ManualDecisions:    manualdecisions.New(nil, manualdecisions.Options{DecisionsFile: env.decisionsFile}),
		TaxonomyHints:      taxonomyhints.New(taxonomyhints.Options{FilePath: env.hintsFile}),
		Stderr:             env.stderr,
	})
	return env
}

func (e *testEnv) write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// Case 1: returns an empty personalization when neither the overlay nor any decision exists.
func TestEmptyWhenNothingExists(t *testing.T) {
	env := newTestEnv(t)
	p := env.store.GetPromptPersonalization()
	if len(p.PriorityRules) != 0 {
		t.Fatalf("priority_rules = %#v, want empty", p.PriorityRules)
	}
	if p.KnownEntities == nil || len(p.KnownEntities) != 0 {
		t.Fatalf("known_entities = %#v, want non-nil empty", p.KnownEntities)
	}
	if p.ExtraRulesText != "" {
		t.Fatalf("extra_rules_text = %q, want empty", p.ExtraRulesText)
	}
}

// Case 2: reads the hand-curated overlay unchanged when there are no decisions.
func TestCuratedOverlayWithNoDecisions(t *testing.T) {
	env := newTestEnv(t)
	env.write(t, env.promptsFile, `{
		"known_entities": ["ACME CORP"],
		"priority_rules": [{"keywords": ["STMT_CHK_"], "category": "bank", "subcategory": "my_bank"}]
	}`)

	p := env.store.GetPromptPersonalization()
	if len(p.KnownEntities) != 1 || p.KnownEntities[0] != "ACME CORP" {
		t.Fatalf("known_entities = %#v", p.KnownEntities)
	}
	if len(p.PriorityRules) != 1 {
		t.Fatalf("priority_rules = %#v, want 1 rule", p.PriorityRules)
	}
	rule := p.PriorityRules[0]
	if len(rule.Keywords) != 1 || rule.Keywords[0] != "STMT_CHK_" || rule.Category != "bank" || rule.Subcategory != "my_bank" {
		t.Fatalf("rule = %#v", rule)
	}
	// Zod `.default('all')` fills scope, and it survives the Go Parse unchanged.
	if rule.Scope != "all" {
		t.Fatalf("scope = %q, want %q", rule.Scope, "all")
	}
}

// Case 3: appends enabled human decisions as STEP 0 rules AFTER the hand-curated ones.
func TestAppendsEnabledDecisionsAfterCurated(t *testing.T) {
	env := newTestEnv(t)
	env.write(t, env.promptsFile, `{
		"priority_rules": [{"keywords": ["HAND_MADE"], "category": "housing", "subcategory": "my_landlord"}]
	}`)
	env.write(t, env.decisionsFile, `[{
		"id": 3,
		"document_id": 1,
		"checksum": "c",
		"original_filename": "STMT_CHK_101.pdf",
		"title": "Relevé de chèques BNP",
		"old_category": "housing",
		"old_subcategory": "northwind_realty",
		"new_category": "bank",
		"new_subcategory": "bnp_paribas",
		"user_feedback_reason": "This is a BNP check statement",
		"rule_keywords": ["STMT_CHK_"],
		"enabled": 1,
		"created_at": "2026-01-01T00:00:00.000Z"
	}]`)

	p := env.store.GetPromptPersonalization()
	if len(p.PriorityRules) != 2 {
		t.Fatalf("priority_rules = %#v, want 2 rules", p.PriorityRules)
	}
	if p.PriorityRules[0].Keywords[0] != "HAND_MADE" {
		t.Fatalf("first rule = %#v, want the curated HAND_MADE rule first", p.PriorityRules[0])
	}
	learned := p.PriorityRules[1]
	if len(learned.Keywords) != 1 || learned.Keywords[0] != "STMT_CHK_" || learned.Category != "bank" || learned.Subcategory != "bnp_paribas" {
		t.Fatalf("learned rule = %#v", learned)
	}
	if !strings.Contains(learned.Note, "decision #3") {
		t.Fatalf("learned note = %q, want it to mention decision #3", learned.Note)
	}
}

// Case 4: does NOT inject disabled decisions.
func TestSkipsDisabledDecisions(t *testing.T) {
	env := newTestEnv(t)
	env.write(t, env.decisionsFile, `[{
		"id": 3,
		"document_id": 1,
		"checksum": "c",
		"original_filename": "STMT_CHK_101.pdf",
		"title": "Relevé",
		"old_category": "housing",
		"old_subcategory": "northwind_realty",
		"new_category": "bank",
		"new_subcategory": "bnp_paribas",
		"rule_keywords": ["STMT_CHK_"],
		"enabled": 0
	}]`)

	if got := env.store.GetPromptPersonalization().PriorityRules; len(got) != 0 {
		t.Fatalf("priority_rules = %#v, want empty", got)
	}
}

// Case 5: derives keywords for legacy decision records that have none stored.
func TestDerivesKeywordsForLegacyDecisions(t *testing.T) {
	env := newTestEnv(t)
	env.write(t, env.decisionsFile, `[{
		"id": 4,
		"document_id": 1,
		"checksum": "c",
		"original_filename": "NORTHWIND_2024.pdf",
		"title": "Northwind Academy — certificat de scolarité",
		"old_category": "correspondence",
		"old_subcategory": "general",
		"new_category": "education",
		"new_subcategory": "northwind",
		"enabled": 1
	}]`)

	p := env.store.GetPromptPersonalization()
	if len(p.PriorityRules) != 1 {
		t.Fatalf("priority_rules = %#v, want 1 rule", p.PriorityRules)
	}
	if !slices.Contains(p.PriorityRules[0].Keywords, "northwind") {
		t.Fatalf("keywords = %#v, want to contain northwind", p.PriorityRules[0].Keywords)
	}
	if p.PriorityRules[0].Category != "education" {
		t.Fatalf("category = %q, want education", p.PriorityRules[0].Category)
	}
}

// Case 6: ignores an invalid overlay file but still merges decisions (never throws).
func TestInvalidOverlayStillMergesDecisions(t *testing.T) {
	env := newTestEnv(t)
	env.write(t, env.promptsFile, "{ not json !!!")
	env.write(t, env.decisionsFile, `[{
		"id": 5,
		"document_id": 1,
		"checksum": "c",
		"original_filename": "BNP.pdf",
		"title": "BNP",
		"new_category": "bank",
		"new_subcategory": "bnp_paribas",
		"rule_keywords": ["bnp"],
		"enabled": 1
	}]`)

	p := env.store.GetPromptPersonalization()
	if len(p.PriorityRules) != 1 || len(p.PriorityRules[0].Keywords) != 1 || p.PriorityRules[0].Keywords[0] != "bnp" {
		t.Fatalf("priority_rules = %#v, want the single bnp rule", p.PriorityRules)
	}
	if !strings.Contains(env.stderr.String(), "Invalid .prompts.private.json") {
		t.Fatalf("stderr = %q, want the invalid-overlay message", env.stderr.String())
	}
}

// Case 7: appends the taxonomy duplicate-guard block to extra_rules_text, after any hand-curated
// text, trimmed and terminated by one newline.
func TestAppendsTaxonomyHintBlock(t *testing.T) {
	env := newTestEnv(t)
	env.write(t, env.promptsFile, `{
		"extra_rules_text": "KEEP ME"
	}`)
	env.write(t, env.hintsFile, `[{
		"proposed_category": "bank",
		"proposed_subcategory": "bnp",
		"mapped_category": "bank",
		"mapped_subcategory": "bnp_paribas",
		"hint": "Duplicate subcategory BLOCKED: reuse the existing slug.",
		"created_at": "2026-01-01T00:00:00.000Z"
	}]`)

	p := env.store.GetPromptPersonalization()
	if !strings.HasPrefix(p.ExtraRulesText, "KEEP ME\n\nSTEP 0: TAXONOMY DUPLICATE GUARD") {
		t.Fatalf("extra_rules_text = %q, want curated text then the guard block", p.ExtraRulesText)
	}
	if !strings.Contains(p.ExtraRulesText, `"bank/bnp" → ALWAYS use "bank/bnp_paribas".`) {
		t.Fatalf("extra_rules_text = %q, want the rendered mapping", p.ExtraRulesText)
	}
	if !strings.HasSuffix(p.ExtraRulesText, "\n") || strings.HasSuffix(p.ExtraRulesText, "\n\n") {
		t.Fatalf("extra_rules_text = %q, want exactly one trailing newline", p.ExtraRulesText)
	}
	if strings.HasPrefix(p.ExtraRulesText, "\n") {
		t.Fatalf("extra_rules_text = %q, want the block's leading newline trimmed", p.ExtraRulesText)
	}
}

// Case 8: an empty hint list leaves extra_rules_text byte-for-byte untouched (the guard `if` never
// runs), including surrounding whitespace.
func TestEmptyHintsLeaveExtraRulesTextUntouched(t *testing.T) {
	env := newTestEnv(t)
	env.write(t, env.promptsFile, `{"extra_rules_text": "  keep  "}`)

	if got := env.store.GetPromptPersonalization().ExtraRulesText; got != "  keep  " {
		t.Fatalf("extra_rules_text = %q, want %q", got, "  keep  ")
	}
}

// Case 9: an overlay that exists but cannot be read is logged and treated as empty, matching the
// TS try/catch, while decisions still merge.
func TestUnreadableOverlayLogsAndMergesDecisions(t *testing.T) {
	env := newTestEnv(t)
	env.write(t, env.promptsFile, `{}`)
	env.write(t, env.decisionsFile, `[{
		"id": 6, "document_id": 1, "checksum": "c",
		"original_filename": "BNP.pdf", "title": "BNP",
		"new_category": "bank", "new_subcategory": "bnp_paribas",
		"rule_keywords": ["bnp"], "enabled": 1
	}]`)

	stderr := &bytes.Buffer{}
	s := New(Options{
		PromptsPrivateFile: env.promptsFile,
		ManualDecisions:    manualdecisions.New(nil, manualdecisions.Options{DecisionsFile: env.decisionsFile}),
		Stderr:             stderr,
		ReadFile:           func(string) ([]byte, error) { return nil, errors.New("boom") },
	})
	p := s.GetPromptPersonalization()
	if len(p.PriorityRules) != 1 || p.PriorityRules[0].Keywords[0] != "bnp" {
		t.Fatalf("priority_rules = %#v, want the bnp rule", p.PriorityRules)
	}
	if !strings.Contains(stderr.String(), "Invalid .prompts.private.json") {
		t.Fatalf("stderr = %q, want the invalid-overlay message", stderr.String())
	}
}

// Case 10: a directory at the overlay path stats successfully and then fails to read, so it is
// logged exactly like a malformed file.
func TestDirectoryOverlayLogs(t *testing.T) {
	env := newTestEnv(t)
	if err := os.Mkdir(env.promptsFile, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if p := env.store.GetPromptPersonalization(); len(p.PriorityRules) != 0 {
		t.Fatalf("priority_rules = %#v, want empty", p.PriorityRules)
	}
	if !strings.Contains(env.stderr.String(), "Invalid .prompts.private.json") {
		t.Fatalf("stderr = %q, want the invalid-overlay message", env.stderr.String())
	}
}

// Case 11: jsTrim reproduces String.prototype.trim(), not strings.TrimSpace: NBSP and BOM are
// stripped (JS), while U+0085 NEL is kept (Go would strip it).
func TestJSTrimSemantics(t *testing.T) {
	if got := jsTrim("\u00a0x\u00a0"); got != "x" {
		t.Fatalf("jsTrim(NBSP x NBSP) = %q, want x", got)
	}
	if got := jsTrim("\ufeffx\ufeff"); got != "x" {
		t.Fatalf("jsTrim(BOM x BOM) = %q, want x", got)
	}
	if got := jsTrim("\u0085x\u0085"); got != "\u0085x\u0085" {
		t.Fatalf("jsTrim(NEL x NEL) = %q, want it preserved", got)
	}
	if got := joinRenderedBlocks("  ", "  "); got != "" {
		t.Fatalf("joinRenderedBlocks(blank, blank) = %q, want empty", got)
	}
}

// Case 12: the store satisfies the two injected surfaces the ported domain packages expect.
func TestSatisfiesInjectedSurfaces(t *testing.T) {
	env := newTestEnv(t)

	promptDeps := prompt.Deps{Personalization: env.store.GetPromptPersonalization}
	if got := promptDeps.Personalization(); got.KnownEntities == nil {
		t.Fatal("prompt.Deps.Personalization returned nil KnownEntities")
	}

	resolutionDeps := classificationresolution.Deps{PromptPersonalization: env.store.GetPromptPersonalization()}
	if resolutionDeps.PromptPersonalization.PriorityRules == nil {
		t.Fatal("classificationresolution.Deps.PromptPersonalization has nil PriorityRules")
	}
}

// Compile-time proof of the exact function type prompt.Deps.Personalization requires.
var _ func() promptdomain.PromptPersonalization = New(Options{}).GetPromptPersonalization
