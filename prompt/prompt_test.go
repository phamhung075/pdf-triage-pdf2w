package prompt

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
)

// All cases in this file are ported verbatim from pdf-triage's src/domain/prompt.test.ts (15
// cases) and src/domain/prompt-injection.test.ts (4 cases). The TypeScript source is the
// behavioral source of truth. The template-content-dependent cases run against the REAL committed
// prompts/ templates, exactly as the TS suite does (it reads CONFIG.PROMPTS_DIR); the injected
// personalization is a fake so the result does not depend on the operator's gitignored overlay.
// src/domain/prompt-hygiene.test.ts is deliberately NOT ported — see the package comment.

const fixtureCategories = "- Category bank: bank documents\n- Category invoices: bills and receipts"
const fixtureFilename = "invoice_2024.pdf"
const fixtureRawText = "Invoice#INV2405030068\nTotalpayable€12.98\nIssuer: ACME CONSEIL"

// realPromptsDir is the committed prompts/ directory of the enclosing pdf-triage checkout. The
// test runs in services/pdf-triage-pdf2w/prompt, so it is three levels up. The package is also
// built as a standalone submodule, so a missing directory skips rather than fails.
func realPromptsDir() string { return filepath.Join("..", "..", "..", "prompts") }

func realPromptsFS(t *testing.T) fs.FS {
	t.Helper()
	dir := realPromptsDir()
	if _, err := os.Stat(dir); err != nil {
		t.Skipf("real prompts directory %s unavailable: %v", dir, err)
	}
	return os.DirFS(dir)
}

// emptyTemplates is the injected no-I/O filesystem with no template files, so every
// loadPromptPart falls back to its hardcoded TS default.
func emptyTemplates() fs.FS { return fstest.MapFS{} }

func staticPersonalization(p promptpersonalization.PromptPersonalization) func() promptpersonalization.PromptPersonalization {
	return func() promptpersonalization.PromptPersonalization { return p }
}

func mustPersonalization(t *testing.T, raw string) promptpersonalization.PromptPersonalization {
	t.Helper()
	p, err := promptpersonalization.Parse([]byte(raw))
	if err != nil {
		t.Fatalf("promptpersonalization.Parse(%s) error = %v, want nil", raw, err)
	}
	return p
}

func TestBuildClassificationPrompt(t *testing.T) {
	t.Run("embeds the categories description string into the system prompt", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildClassificationPrompt("- Category invoices: bills", "facture.pdf", "some text", ClassificationOptions{})
		if !strings.Contains(got.System, "- Category invoices: bills") {
			t.Fatalf("System does not contain the categories description:\n%s", got.System)
		}
	})

	t.Run("truncates document text over 4000 chars in the user prompt, with an ellipsis", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		longText := strings.Repeat("a", 5000)
		got := d.BuildClassificationPrompt("categories", "doc.pdf", longText, ClassificationOptions{})
		if !strings.Contains(got.User, strings.Repeat("a", 4000)+"...") {
			t.Fatal("User does not contain the 4000-char prefix followed by an ellipsis")
		}
		if strings.Contains(got.User, strings.Repeat("a", 4001)) {
			t.Fatal("User contains 4001 consecutive 'a' characters, want at most 4000")
		}
		if got.TextSnippetLength != 4003 {
			t.Fatalf("TextSnippetLength = %d, want 4003", got.TextSnippetLength)
		}
	})

	t.Run("does not truncate document text at or under 4000 chars", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		shortText := strings.Repeat("b", 4000)
		got := d.BuildClassificationPrompt("categories", "doc.pdf", shortText, ClassificationOptions{})
		if !strings.Contains(got.User, shortText) {
			t.Fatal("User does not contain the full 4000-char text")
		}
		if strings.Contains(got.User, "...") {
			t.Fatal("User contains an ellipsis, want no truncation at exactly 4000 chars")
		}
		if got.TextSnippetLength != 4000 {
			t.Fatalf("TextSnippetLength = %d, want 4000", got.TextSnippetLength)
		}
	})

	t.Run("includes the filename in the user prompt", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildClassificationPrompt("categories", "my_invoice.pdf", "text", ClassificationOptions{})
		if !strings.Contains(got.User, "Filename: my_invoice.pdf") {
			t.Fatal("User does not contain 'Filename: my_invoice.pdf'")
		}
	})

	t.Run("appends the previous-error feedback block only when previousError is provided", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		withoutError := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{})
		if strings.Contains(withoutError.User, "PREVIOUS ATTEMPT FEEDBACK") {
			t.Fatal("User contains PREVIOUS ATTEMPT FEEDBACK without previousError")
		}

		withError := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{PreviousError: "subcategory was ungrounded"})
		if !strings.Contains(withError.User, "PREVIOUS ATTEMPT FEEDBACK") {
			t.Fatal("User does not contain PREVIOUS ATTEMPT FEEDBACK with previousError")
		}
		if !strings.Contains(withError.User, "subcategory was ungrounded") {
			t.Fatal("User does not contain the previousError text")
		}
	})

	t.Run("appends an explicit, prioritized entity-hint block only when entityHint.entity is provided", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		withoutHint := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{})
		if strings.Contains(withoutHint.User, "PRE-EXTRACTED ENTITY HINT") {
			t.Fatal("User contains PRE-EXTRACTED ENTITY HINT without an entity hint")
		}

		withHint := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{
			EntityHint: &EntityHint{Entity: "Crédit Mutuel", DocType: "Bank Statement"},
		})
		for _, want := range []string{"PRE-EXTRACTED ENTITY HINT", "Crédit Mutuel", "Bank Statement", "GROUND TRUTH"} {
			if !strings.Contains(withHint.User, want) {
				t.Fatalf("User does not contain %q", want)
			}
		}
	})

	t.Run("omits the entity-hint block when entityHint.entity is an empty string", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{EntityHint: &EntityHint{Entity: ""}})
		if strings.Contains(got.User, "PRE-EXTRACTED ENTITY HINT") {
			t.Fatal("User contains PRE-EXTRACTED ENTITY HINT for an empty entity")
		}
	})

	t.Run("instructs the model not to confuse the document's own \"date\" with a validity/expiry date found elsewhere in the text", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{})
		if !regexp.MustCompile(`DATE vs EXPIRY_DATE`).MatchString(got.System) {
			t.Fatal("System does not match /DATE vs EXPIRY_DATE/")
		}
		if !regexp.MustCompile(`never let the expiry/validity date silently overwrite "date"`).MatchString(got.System) {
			t.Fatal(`System does not match /never let the expiry\/validity date silently overwrite "date"/`)
		}
	})

	t.Run("injects the current date into the formatting rules so Step D can guard against future-dated OCR misreads", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		now := time.Date(2026, time.August, 12, 0, 0, 0, 0, time.Local)
		got := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{Now: now})
		if !strings.Contains(got.System, "Today's date is 2026-08-12") {
			t.Fatal("System does not contain \"Today's date is 2026-08-12\"")
		}
		if strings.Contains(got.System, "{{CURRENT_DATE}}") {
			t.Fatal("System still contains {{CURRENT_DATE}}")
		}
	})

	t.Run("no longer requests a markdown_content JSON key from Step D (Step C already produces it) — the lang instruction only mentions titre/summary", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{})
		if regexp.MustCompile(`Generate the output.*markdown_content|Générez.*markdown_content`).MatchString(got.System) {
			t.Fatal("System lang instruction still mentions markdown_content")
		}

		schemaBytes, err := fs.ReadFile(realPromptsFS(t), "json_schema_response.json")
		if err != nil {
			t.Fatalf("reading json_schema_response.json: %v", err)
		}
		var schema map[string]any
		if err := json.Unmarshal(schemaBytes, &schema); err != nil {
			t.Fatalf("parsing json_schema_response.json: %v", err)
		}
		if _, ok := schema["markdown_content"]; ok {
			t.Fatal("json_schema_response.json still declares a markdown_content key")
		}
	})
}

func TestBuildMarkdownConversionPrompt(t *testing.T) {
	t.Run("instructs the model to preserve illegible fragments near-verbatim instead of fabricating recognized-template content (Problem A safety net)", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildMarkdownConversionPrompt("some garbled chunk", nil, "")
		if !strings.Contains(got.User, "NEVER FABRICATE FROM PATTERN-RECOGNITION") {
			t.Fatal("User does not contain NEVER FABRICATE FROM PATTERN-RECOGNITION")
		}
		if !strings.Contains(got.User, "does not resolve into coherent words in ANY language") {
			t.Fatal("User does not contain the ANY-language illegibility wording")
		}
		if !strings.Contains(got.User, IllegibleFragmentMarker) {
			t.Fatalf("User does not contain the illegible-fragment marker %q", IllegibleFragmentMarker)
		}
		if !regexp.MustCompile(`(?i)not license to fabricate`).MatchString(got.User) {
			t.Fatal("User does not match /not license to fabricate/i")
		}
	})

	t.Run("includes the raw chunk text in the user prompt", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildMarkdownConversionPrompt("BANG cAN oor xf roAN garbled OCR noise", nil, "")
		if !strings.Contains(got.User, "BANG cAN oor xf roAN garbled OCR noise") {
			t.Fatal("User does not contain the raw chunk text")
		}
	})

	t.Run("omits any continuation-context block when no continuation context is passed", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildMarkdownConversionPrompt("some chunk text", nil, "")
		if strings.Contains(got.User, "⚠️ CONTINUATION CONTEXT:") {
			t.Fatal("User contains the injected continuation-context block")
		}
		if strings.Contains(got.User, "ended mid-table") {
			t.Fatal("User contains 'ended mid-table'")
		}
	})

	t.Run("injects an explicit continuation-context block instructing the model to continue the open table without repeating the header (Problem B)", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildMarkdownConversionPrompt("100.00 | 200.00 | food", &MarkdownContinuationContext{
			Header:    "| Date | Amount | Label |",
			Separator: "| --- | --- | --- |",
		}, "")
		for _, want := range []string{"⚠️ CONTINUATION CONTEXT:", "| Date | Amount | Label |", "| --- | --- | --- |"} {
			if !strings.Contains(got.User, want) {
				t.Fatalf("User does not contain %q", want)
			}
		}
		if !regexp.MustCompile(`(?i)do NOT repeat the header/separator row`).MatchString(got.User) {
			t.Fatal("User does not match /do NOT repeat the header\\/separator row/i")
		}
		if !regexp.MustCompile(`(?i)do NOT start a new table`).MatchString(got.User) {
			t.Fatal("User does not match /do NOT start a new table/i")
		}
	})

	t.Run("does not inject continuation context when header is an empty string", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildMarkdownConversionPrompt("some chunk text", &MarkdownContinuationContext{Header: "", Separator: ""}, "")
		if strings.Contains(got.User, "⚠️ CONTINUATION CONTEXT:") {
			t.Fatal("User contains the continuation-context block for an empty header")
		}
	})
}

func TestPersonalPromptOverlayInjection(t *testing.T) {
	t.Run("injects the user priority rules as a STEP 0 ahead of the generic STEP 1 flow", func(t *testing.T) {
		overlay := mustPersonalization(t, `{"priority_rules":[{"keywords":["MYCODE_42"],"category":"bank","subcategory":"my_bank"}]}`)
		d := Deps{Templates: realPromptsFS(t), Personalization: staticPersonalization(overlay)}
		got := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{})

		if !strings.Contains(got.System, "MYCODE_42") {
			t.Fatal("System does not contain MYCODE_42")
		}
		step0 := strings.Index(got.System, "STEP 0: USER-SPECIFIC HIGH-PRIORITY OVERRIDES")
		if step0 <= -1 {
			t.Fatal("System does not contain the STEP 0 heading")
		}
		step1 := strings.Index(got.System, "STEP 1: BANK STATEMENTS")
		if step1 <= -1 {
			t.Fatal("System does not contain the STEP 1 heading")
		}
		if !(step0 < step1) {
			t.Fatalf("STEP 0 index %d is not before STEP 1 index %d", step0, step1)
		}
	})

	t.Run("injects the known entities into the Step A entity-extraction prompt", func(t *testing.T) {
		overlay := mustPersonalization(t, `{"known_entities":["ACME CONSEIL"]}`)
		d := Deps{Templates: realPromptsFS(t), Personalization: staticPersonalization(overlay)}
		got := d.BuildEntityExtractionPrompt("doc.pdf", "some document text")
		if !strings.Contains(got.User, "ACME CONSEIL") {
			t.Fatal("User does not contain ACME CONSEIL")
		}
	})

	t.Run("leaves no unresolved {{USER_*}} placeholder when there is no personalization at all", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{})
		entity := d.BuildEntityExtractionPrompt("doc.pdf", "text")

		re := regexp.MustCompile(`\{\{USER_[A-Z_]+\}\}`)
		for _, rendered := range []string{got.System, got.User, entity.System, entity.User} {
			if re.MatchString(rendered) {
				t.Fatalf("rendered prompt still contains an unresolved USER placeholder:\n%s", rendered)
			}
		}
	})

	t.Run("keeps the generic flow intact when there is no personalization — STEP 1 still leads", func(t *testing.T) {
		d := Deps{Templates: realPromptsFS(t)}
		got := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{})
		if strings.Contains(got.System, "STEP 0") {
			t.Fatal("System contains STEP 0 without personalization")
		}
		if !strings.Contains(got.System, "STEP 1: BANK STATEMENTS") {
			t.Fatal("System does not contain STEP 1: BANK STATEMENTS")
		}
	})
}

// TestRealTemplatesSubstituteEveryPlaceholder is the template-contract guard the Go port must
// honour: every {{PLACEHOLDER}} the committed prompts/ templates declare must be substituted in
// every built prompt, with no literal {{...}} left behind. Every placeholder is exercised by
// building all three prompt kinds with the optional branches populated (retry feedback, entity
// hint, continuation context, repair note) and a fake personalization overlay.
func TestRealTemplatesSubstituteEveryPlaceholder(t *testing.T) {
	templates := realPromptsFS(t)

	placeholderRe := regexp.MustCompile(`\{\{[^{}]+\}\}`)
	var declared []string
	if err := fs.WalkDir(templates, ".", func(_ string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		data, readErr := fs.ReadFile(templates, entry.Name())
		if readErr != nil {
			return readErr
		}
		declared = append(declared, placeholderRe.FindAllString(string(data), -1)...)
		return nil
	}); err != nil {
		t.Fatalf("walking real prompts: %v", err)
	}
	if len(declared) == 0 {
		t.Fatal("no {{PLACEHOLDER}} found in the committed templates; the guard would be vacuous")
	}

	overlay := mustPersonalization(t, `{
		"known_entities":["ACME CONSEIL"],
		"priority_rules":[{"keywords":["MYCODE_42"],"category":"bank","subcategory":"my_bank"}]
	}`)
	d := Deps{Templates: templates, Personalization: staticPersonalization(overlay)}
	now := time.Date(2026, time.August, 12, 0, 0, 0, 0, time.Local)

	classification := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{
		PreviousError:  "subcategory was ungrounded",
		SystemLanguage: "EN",
		EntityHint:     &EntityHint{Entity: "Crédit Mutuel", DocType: "Bank Statement"},
		Now:            now,
	})
	entity := d.BuildEntityExtractionPrompt("doc.pdf", "text")
	markdown := d.BuildMarkdownConversionPrompt("chunk", &MarkdownContinuationContext{Header: "h", Separator: "s"}, "repair")

	built := map[string]string{
		"classification.system": classification.System,
		"classification.user":   classification.User,
		"entity.system":         entity.System,
		"entity.user":           entity.User,
		"markdown.system":       markdown.System,
		"markdown.user":         markdown.User,
	}
	for name, rendered := range built {
		if leftover := placeholderRe.FindString(rendered); leftover != "" {
			t.Fatalf("%s still contains the unsubstituted placeholder %q", name, leftover)
		}
	}
}

// TestFallbackTemplates drives the hardcoded fallback path with an injected empty filesystem, so
// the fallback constants are exercised (the fixture test pins them byte-exactly against TS).
func TestFallbackTemplates(t *testing.T) {
	d := Deps{Templates: emptyTemplates()}
	entity := d.BuildEntityExtractionPrompt("doc.pdf", "text")
	if !strings.Contains(entity.User, "Respond ONLY with raw JSON") {
		t.Fatalf("fallback entity user prompt missing its inline default:\n%s", entity.User)
	}
	classification := d.BuildClassificationPrompt("categories", "doc.pdf", "text", ClassificationOptions{})
	if strings.Contains(classification.System, "{{") {
		t.Fatal("fallback classification system still contains a placeholder")
	}
}
