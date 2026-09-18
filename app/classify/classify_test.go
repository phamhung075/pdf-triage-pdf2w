package classify

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
)

// classifyScript / textScript shortcuts for the ported mock sequences.
func classifyResponses(responses ...string) []completionScript {
	out := make([]completionScript, 0, len(responses))
	for _, r := range responses {
		out = append(out, completionScript{res: ollama.Completion{Response: r}})
	}
	return out
}

func text(responses ...string) []textScript {
	out := make([]textScript, 0, len(responses))
	for _, r := range responses {
		out = append(out, textScript{res: ollama.TextCompletion{Response: r, DoneReason: "stop"}})
	}
	return out
}

// TS: parses a valid JSON response into DocumentMetadata (happy path).
func TestClassifyPDFTextParsesValidJSONResponse(t *testing.T) {
	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"SFR","document_type":"Invoice"}`,
		`{"titre":"Facture SFR","registre":"REF-1","date":"2024-05-12","categorie":"invoices","subcategorie":"sfr","summary":"A vendor invoice","tags":["sfr"]}`,
	)
	e.ollama.textScripts = text("# SFR\n\nFacture Total TTC 45.99")

	result, err := e.deps().ClassifyPDFText("SFR Facture Total TTC 45.99", "facture.pdf", "", time.Now(), "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Categorie != "invoices" {
		t.Errorf("categorie = %q, want invoices", result.Categorie)
	}
	if result.Subcategorie != "sfr" {
		t.Errorf("subcategorie = %q, want sfr", result.Subcategorie)
	}
	if result.Titre != "Facture SFR" {
		t.Errorf("titre = %q, want Facture SFR", result.Titre)
	}
}

// TS: falls back to ruleBasedClassify when Step D returns an empty response.response (the pre-fix
// failure shape).
func TestClassifyPDFTextFallsBackWhenStepDEmpty(t *testing.T) {
	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"","document_type":""}`,
		"", // Step D: unparseable — classifyPDFText never reads response.thinking
	)
	e.ollama.textScripts = text("")

	result, err := e.deps().ClassifyPDFText("SFR Facture Total TTC 45.99", "facture.pdf", "", time.Now(), "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Categorie != "invoices" {
		t.Errorf("categorie = %q, want invoices", result.Categorie)
	}
	if result.Subcategorie != "sfr" {
		t.Errorf("subcategorie = %q, want sfr", result.Subcategorie)
	}
}

// TS: does not special-case a classification-shaped Step A response — Step D always runs
// independently.
func TestClassifyPDFTextDoesNotSpecialCaseStepAShape(t *testing.T) {
	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"titre":"Wrong Shape","categorie":"invoices","subcategorie":"sfr"}`,
		`{"titre":"Bulletin de Salaire","registre":"","date":"2024-05-01","categorie":"bulletin_salaire","subcategorie":"acme_corp","summary":"s","tags":[]}`,
	)
	e.ollama.textScripts = text("# Bulletin de salaire AcmeCorp")

	result, err := e.deps().ClassifyPDFText("Bulletin de salaire AcmeCorp", "bulletin.pdf", "", time.Now(), "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Categorie != "bulletin_salaire" || result.Subcategorie != "acme_corp" {
		t.Errorf("got %s/%s, want bulletin_salaire/acme_corp", result.Categorie, result.Subcategorie)
	}
	if result.Titre != "Bulletin de Salaire" {
		t.Errorf("titre = %q, want Bulletin de Salaire", result.Titre)
	}
	// health + Step A + Step C + Step D — Step D was NOT skipped.
	if got := e.ollama.totalCalls(); got != 4 {
		t.Errorf("total ollama calls = %d, want 4", got)
	}
}

// TS: corrects a future-dated "date" field from Step D when it conflicts with the titre's stated
// year (doc #2472 regression).
func TestClassifyPDFTextCorrectsFutureDate(t *testing.T) {
	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"Lakeside Dental","document_type":"Pay Slip"}`,
		`{"titre":"Bulletin de salaire - Novembre 2025","registre":"","date":"30/11/2026","categorie":"bulletin_salaire","subcategorie":"lakeside_dental","summary":"s","tags":[]}`,
	)
	e.ollama.textScripts = text("# Bulletin de salaire Novembre 2025")

	now := time.Date(2026, 8, 12, 0, 0, 0, 0, time.Local)
	result, err := e.deps().ClassifyPDFText("Poriodo: Novonbro 2025 Paiemont lo 30/11/26", "bulletin.pdf", "", now, "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Date != "2025-11-30" {
		t.Errorf("date = %q, want 2025-11-30", result.Date)
	}
}

// TS: prioritizes Step A's grounded entity over Step D's wrong fallback category (Crédit Mutuel
// bank-statement regression). Golden Rules 6 and 7.
func TestClassifyPDFTextEntityPriorityOverridesFallbackCategory(t *testing.T) {
	e := newEnv()
	e.dict.dict = bankDictionary(entity("credit_mutuel", "Crédit Mutuel", "credit mutuel", "ccm"))
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"CAISSE DE CREDIT MUTUEL SPRINGFIELD CENTRE","document_type":"Bank Statement"}`,
		`{"titre":"Relevé bancaire","registre":"","date":"2024-05-01","categorie":"reports","subcategorie":"credit_mutuel_springfield_centre","summary":"s","tags":[]}`,
	)
	e.ollama.textScripts = text("# Relevé")

	result, err := e.deps().ClassifyPDFText(
		"RELEVE DE COMPTE Caisse de Crédit Mutuel Springfield Centre IBAN FR76 0000 0000 0000",
		"releve.pdf", "", time.Now(), "",
	)
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Categorie != "bank" {
		t.Errorf("categorie = %q, want bank (Golden Rule 6/7 bank-statement trap)", result.Categorie)
	}
	if result.Subcategorie != "credit_mutuel" {
		t.Errorf("subcategorie = %q, want credit_mutuel", result.Subcategorie)
	}
}

// TS: does NOT override a specific (non-fallback) Step D category even when the entity is
// dictionary-grounded elsewhere (France Travail issuing a pay slip).
func TestClassifyPDFTextDoesNotOverrideSpecificCategory(t *testing.T) {
	e := newEnv()
	e.dict.dict = govDictionary(entity("france_travail", "France Travail", "france travail", "pole emploi"))
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"France Travail","document_type":"Pay Slip"}`,
		`{"titre":"Bulletin ARE","registre":"","date":"2024-05-01","categorie":"bulletin_salaire","subcategorie":"france_travail","summary":"s","tags":[]}`,
	)
	e.ollama.textScripts = text("# Bulletin ARE")

	result, err := e.deps().ClassifyPDFText("Bulletin de salaire France Travail Net à payer", "are.pdf", "", time.Now(), "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Categorie != "bulletin_salaire" || result.Subcategorie != "france_travail" {
		t.Errorf("got %s/%s, want bulletin_salaire/france_travail", result.Categorie, result.Subcategorie)
	}
}

// TS: surfaces subcategorie="general" verbatim for a short document with an ungrounded AI
// subcategory guess, so the downstream Golden Rule #4 BLOCK guard has something to catch.
func TestClassifyPDFTextSurfacesGeneralForUngroundedSubcategory(t *testing.T) {
	const shortText = "Illegible scan content, no identifiable entity here at all in this short snippet."
	if len(shortText) >= 800 {
		t.Fatalf("test setup: shortText must be < 800 chars")
	}
	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"","document_type":""}`,
		`{"titre":"Scan","registre":"","date":"","categorie":"correspondence","subcategorie":"randomgibberish123","summary":"","tags":[]}`,
	)
	e.ollama.textScripts = text("# Scan\n\nIllegible scan content, no identifiable entity here at all in this short snippet.")

	result, err := e.deps().ClassifyPDFText(shortText, "IMG_0001.pdf", "", time.Now(), "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Subcategorie != "general" {
		t.Errorf("subcategorie = %q, want general (Golden Rule 4 must be catchable downstream)", result.Subcategorie)
	}
}

// TS: runs Step C (markdown conversion) even for short documents instead of skipping it below 800
// chars.
func TestClassifyPDFTextRunsStepCForShortDocuments(t *testing.T) {
	const shortText = "SFR Facture Total TTC 45.99"
	if len(shortText) >= 800 {
		t.Fatalf("test setup: shortText must be < 800 chars")
	}
	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"SFR","document_type":"Invoice"}`,
		`{"titre":"Facture SFR","registre":"","date":"2024-05-12","categorie":"invoices","subcategorie":"sfr","summary":"s","tags":[]}`,
	)
	e.ollama.textScripts = text("# SFR\n\n**Total TTC:** 45.99€")

	result, err := e.deps().ClassifyPDFText(shortText, "facture.pdf", "", time.Now(), "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if got := e.ollama.totalCalls(); got != 4 {
		t.Errorf("total ollama calls = %d, want 4 (health + Step A + Step C + Step D)", got)
	}
	if result.MarkdownContent != "# SFR\n\n**Total TTC:** 45.99€" {
		t.Errorf("markdown_content = %q", result.MarkdownContent)
	}
}

// TS: throws OllamaUnavailableError when the model is down — it must NEVER silently fall back to
// the rule-based classifier (2026-08-31 regression).
func TestClassifyPDFTextThrowsWhenOllamaDown(t *testing.T) {
	e := newEnv()
	e.ollama.healthy = false

	_, err := e.deps().ClassifyPDFText("SFR Facture Total TTC 45.99", "facture.pdf", "", time.Now(), "")
	if err == nil {
		t.Fatal("expected an error when Ollama is down")
	}
	var unavailable *ollama.OllamaUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %T %v, want *ollama.OllamaUnavailableError", err, err)
	}
}

// TS: uses Docling markdown as markdown_content and SKIPS the Step C chunk-by-chunk LLM conversion
// when doclingMarkdown is provided. Also the pre-made-markdown skip from the port brief.
func TestClassifyPDFTextUsesPremadeMarkdownAndSkipsStepC(t *testing.T) {
	doclingMd := strings.Join([]string{
		"# Relevé de compte Crédit Mutuel",
		"",
		"| Date | Opération | Débit |",
		"| --- | --- | --- |",
		"| 03/10/2023 | PRLV SEPA PAYPAL | -2,00 EUR |",
		"| 28/09/2023 | VIR DE MME DUPONT MARIE | +1 000,00 EUR |",
	}, "\n")

	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"Crédit Mutuel","document_type":"Bank Statement"}`,
		`{"titre":"Relevé Crédit Mutuel","registre":"","date":"2023-10-03","categorie":"bank","subcategorie":"credit_mutuel","summary":"s","tags":[]}`,
	)

	result, err := e.deps().ClassifyPDFText("Relevé Crédit Mutuel PAYPAL DUPONT", "releve.pdf", "", time.Now(), doclingMd)
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	// health + Step A + Step D only — Step C never ran.
	if got := e.ollama.totalCalls(); got != 3 {
		t.Errorf("total ollama calls = %d, want 3 (no Step C)", got)
	}
	if len(e.ollama.textCalls) != 0 {
		t.Errorf("text chat calls = %d, want 0", len(e.ollama.textCalls))
	}
	if result.MarkdownContent != doclingMd {
		t.Errorf("markdown_content = %q, want the premade docling markdown", result.MarkdownContent)
	}
	if result.Categorie != "bank" {
		t.Errorf("categorie = %q, want bank", result.Categorie)
	}
}

// TS: still runs the normal Step C path when doclingMarkdown is empty/whitespace.
func TestClassifyPDFTextRunsStepCForBlankPremadeMarkdown(t *testing.T) {
	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"SFR","document_type":"Invoice"}`,
		`{"titre":"Facture SFR","registre":"","date":"2024-05-12","categorie":"invoices","subcategorie":"sfr","summary":"s","tags":[]}`,
	)
	e.ollama.textScripts = text("# SFR\n\n**Total TTC:** 45.99€")

	result, err := e.deps().ClassifyPDFText("SFR Facture Total TTC 45.99", "facture.pdf", "", time.Now(), "   ")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if got := e.ollama.totalCalls(); got != 4 {
		t.Errorf("total ollama calls = %d, want 4", got)
	}
	if result.MarkdownContent != "# SFR\n\n**Total TTC:** 45.99€" {
		t.Errorf("markdown_content = %q", result.MarkdownContent)
	}
}

// Added: a forbidden subcategory slug (Golden Rule 4's narrow set) is surfaced verbatim, never
// silently rewritten, so the downstream strict fail guard can BLOCK the file.
func TestClassifyPDFTextPassesForbiddenSubcategoryThrough(t *testing.T) {
	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"","document_type":""}`,
		`{"titre":"Scan","registre":"","date":"","categorie":"invoices","subcategorie":"jpg","summary":"","tags":[]}`,
	)
	e.ollama.textScripts = text("# Scan\n\nSFR Facture Total TTC 45.99")

	result, err := e.deps().ClassifyPDFText("SFR Facture Total TTC 45.99", "scan.pdf", "", time.Now(), "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Subcategorie != "jpg" {
		t.Errorf("subcategorie = %q, want jpg passed through for the downstream BLOCK", result.Subcategorie)
	}
}

// Added: the loud JSON repair runs on a truncated Step D answer and the repaired fields win (not
// the rule-based fallback).
func TestClassifyPDFTextRepairsTruncatedStepDJSON(t *testing.T) {
	e := newEnv()
	e.ollama.classScripts = classifyResponses(
		`{"issuing_entity":"SFR","document_type":"Invoice"}`,
		`{"titre":"Facture SFR","registre":"","date":"2024-05-12","categorie":"invoices","subcategorie":"sfr","summary":"repaired-summary","tags":[]`,
	)
	e.ollama.textScripts = text("# SFR\n\nFacture Total TTC 45.99")

	result, err := e.deps().ClassifyPDFText("SFR Facture Total TTC 45.99", "facture.pdf", "", time.Now(), "")
	if err != nil {
		t.Fatalf("ClassifyPDFText: %v", err)
	}
	if result.Summary != "repaired-summary" {
		t.Fatalf("summary = %q, want repaired-summary (truncated JSON must be repaired, not rule-fallback'd)", result.Summary)
	}
	if result.Categorie != "invoices" || result.Subcategorie != "sfr" {
		t.Errorf("got %s/%s, want invoices/sfr", result.Categorie, result.Subcategorie)
	}
}
