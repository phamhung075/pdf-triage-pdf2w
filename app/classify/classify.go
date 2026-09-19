// Package classify is a Go port of pdf-triage's src/application/classify-document.ts (615 lines):
// the classification pipeline Step A (entity extraction) + Step C (zero-loss Markdown conversion) +
// Step D (classification), the taxonomy resolution, the duplicate guard and the
// convertRawTextToZeroLossMarkdown helper. The entry point is Deps.ClassifyPDFText
// (classify-document.ts:383).
//
// Upstream status. The two suites this job ports were run first and are GREEN at port time:
//
//	npx vitest run src/application/classify-document.test.ts src/application/micro-prompt-pipeline.test.ts
//	-> Test Files 2 passed (2); Tests 49 passed (49)
//
// No upstream case is pinned red. All 39 classify-document.test.ts cases are ported. Of the 10
// micro-prompt-pipeline.test.ts cases, the 7 that belong to this module are ported (the chunkText
// case and the six detectOpenTableTail cases); the other three exercise buildEntityExtractionPrompt
// / buildMarkdownConversionPrompt (already covered by the prompt package suite) and
// sanitizeDocumentNoise (already covered by infra/pdfextractor), so they are deliberately not
// duplicated here.
//
// Collaborators are injected, never globals. TypeScript reached into infrastructure stores and the
// ollama module directly; the port takes a Deps struct whose fields are small interfaces, so tests
// substitute fakes and the Go store save path is the store's own private-overlay writer:
//
//   - Ollama            (*infra/ollama.Client): EnsureOllamaModel, RequestClassificationCompletion,
//     RequestTextChatCompletion.
//   - Categories        (*store/categories.Store): GetCategoriesConfig, SaveCategoriesConfig. The
//     Golden Rule 5 pre-move auto-creation calls SaveCategoriesConfig, which writes
//     .categories.private.json; the committed categories.json is never written.
//   - EntityDictionary  (*store/entitydictionary.Store): GetEntityDictionary.
//   - PromptPersonalization (*store/promptpersonalization.Store): GetPromptPersonalization.
//   - TaxonomyHints     (*store/taxonomyhints.Store): RecordTaxonomyHint.
//   - Prompts           (prompt.Deps): the three Build*Prompt methods plus the template FS and the
//     per-build personalization callback.
//   - Log               (*infra/logger.Logger): Debug/Info/Warn. Nil is silent.
//
// The TS source is the behavioral source of truth. TS-vs-Go gaps, each resolved in favor of
// matching TS exactly:
//
//  1. JavaScript whitespace. Every `.trim()` / `.trimEnd()` / `.trimStart()` uses JavaScript's
//     WhiteSpace+LineTerminator set (NBSP U+00A0, the Unicode space separators, U+2028/U+2029, the
//     BOM U+FEFF, …), which differs from Go's unicode.White_Space (it contains U+0085 NEL and omits
//     U+FEFF). jsTrim / jsTrimEnd / jsTrimStart reproduce the JS set. The package cannot import the
//     unexported copies in classification / markdowntables / prompt, so it carries its own.
//  2. UTF-16 length and substring. `str.length` counts UTF-16 code units and `str.slice` /
//     `str.substring` cut on a code unit. chunkText and splitOverlongLine index by UTF-16 code
//     units (utf16Len / utf16Slice), which is the documented load-bearing behavior of the over-long
//     line fix. For astral text a straddling rune is dropped, whereas JS would keep a lone
//     surrogate — unrepresentable in a Go UTF-8 string.
//  3. Regex. RE2 has no lookaround; none of the patterns here uses it. The two GFM patterns keep
//     JavaScript's `.` (jsDotClass excludes \r, U+2028, U+2029 as well as \n) and JavaScript's `\s`
//     (jsWhitespaceClass), because Go's `\s` and `.` would diverge on NBSP / U+2028 text.
//  4. Math.round. The table-integrity percentage and the recall percentages use jsRound
//     (floor(x+0.5), i.e. half-up) rather than Go's math.Round (half away from zero); the values are
//     non-negative so the two agree, but jsRound states the JS rule.
//  5. `parsed.issuing_entity || parsed.entity_name || ”`. Go's decoded JSON is map[string]any, so
//     jsonStringField tries the keys in order and takes the first non-empty string. A missing key
//     yields "" where JS yields undefined; the downstream truthiness checks behave identically.
//  6. `DocumentMetadataSchema.parse(parsedD)`. The port re-marshals the decoded value and calls
//     documentschema.ParseDocumentMetadata, which is the port of the same Zod schema (including the
//     required `titre`/`categorie` and the tags default). The rule-based fallback object is built
//     directly, as it is already a fully-populated Go struct.
//  7. JS `||` fallbacks on optional diagnostics (`filename || 'document'`, `extractedEntity ||
//     undefined`). filenameOrDocument and the nil-empty helpers reproduce them.
//
// Non-negotiables pinned by explicit tests: Golden Rule 4 (an ungrounded subcategory is surfaced as
// the literal "general" so the downstream guard can BLOCK, never smoothed over), Golden Rule 5 (the
// pre-move auto-creation goes through the private-overlay Save), Golden Rules 6/7 (the bank-statement
// trap: a SFR/PayPal row inside a Crédit Mutuel statement is a transaction, not the document type,
// and a specific non-fallback Step D category wins over the entity hint), Golden Rule 14 (only the
// configured model is ever asked), Golden Rule 20 (temperature 0.1 through the ollama package),
// loud JSON repair, chunk-overlap / open-table-tail joining across chunk boundaries, and the single
// re-conversion of a malformed chunk with a corrective note.
package classify

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/classification"
	"github.com/phamhung075/pdf-triage-pdf2w/classificationresolution"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/aiprovider"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/prompt"
	"github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/store/categories"
	"github.com/phamhung075/pdf-triage-pdf2w/store/entitydictionary"
	promptpersonalizationstore "github.com/phamhung075/pdf-triage-pdf2w/store/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/store/taxonomyhints"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomyconflicts"
)

// The TS logger module names used by classify-document.ts.
const (
	moduleOllamaAI      = "OLLAMA_AI"
	moduleDecisionLogic = "CLASSIFIER_DECISION"
	moduleTaxonomyGuard = "TAXONOMY_GUARD"
)

// Ollama is the slice of *infra/ollama.Client the pipeline needs.
type Ollama interface {
	EnsureOllamaModel(modelName string) bool
	RequestClassificationCompletion(system, user string) (ollama.Completion, error)
	RequestTextChatCompletion(system, user string) (ollama.TextCompletion, error)
}

// Categories is the slice of *store/categories.Store the pipeline needs. SaveCategoriesConfig is
// the private-overlay writer required by Golden Rule 5.
type Categories interface {
	GetCategoriesConfig() documentschema.CategoriesConfig
	SaveCategoriesConfig(categories []*documentschema.CategoryItem) error
}

// EntityDictionary is the slice of *store/entitydictionary.Store the pipeline needs.
type EntityDictionary interface {
	GetEntityDictionary() documentschema.EntityDictionary
}

// PromptPersonalization is the slice of *store/promptpersonalization.Store the pipeline needs.
type PromptPersonalization interface {
	GetPromptPersonalization() promptpersonalization.PromptPersonalization
}

// TaxonomyHints is the slice of *store/taxonomyhints.Store the pipeline needs.
type TaxonomyHints interface {
	RecordTaxonomyHint(entry *taxonomyconflicts.TaxonomyHintEntry)
}

// Logger is the slice of *infra/logger.Logger the pipeline needs. Nil is silent.
type Logger interface {
	Debug(moduleName, message string, meta any, filename ...string)
	Info(moduleName, message string, meta any, filename ...string)
	Warn(moduleName, message string, meta any, filename ...string)
}

// Config carries the CONFIG values classify-document.ts read directly. The settings port is a later
// phase, so they are explicit here.
type Config struct {
	OllamaModel          string
	OllamaHost           string
	Language             string
	PersonalNameDenylist []string
	AIProvider           string
	CloudProvider        string
	CloudModel           string
	GetConfig            func() Config
}

// Deps is the explicit injected surface. See the package comment.
type Deps struct {
	Config                Config
	Ollama                Ollama
	Categories            Categories
	EntityDictionary      EntityDictionary
	PromptPersonalization PromptPersonalization
	TaxonomyHints         TaxonomyHints
	Prompts               prompt.Deps
	Log                   Logger
}

// Compile-time proof that the real collaborators satisfy the injected seams the later wiring passes
// in.
var (
	_ Ollama                = (*ollama.Client)(nil)
	_ Categories            = (*categories.Store)(nil)
	_ EntityDictionary      = (*entitydictionary.Store)(nil)
	_ PromptPersonalization = (*promptpersonalizationstore.Store)(nil)
	_ TaxonomyHints         = (*taxonomyhints.Store)(nil)
	_ Logger                = (*logger.Logger)(nil)
)

func (d Deps) effectiveConfig() Config {
	if d.Config.GetConfig != nil {
		return d.Config.GetConfig()
	}
	return d.Config
}

func (d Deps) aiInfo() (moduleTag, providerName, modelName string) {
	cfg := d.effectiveConfig()
	if strings.EqualFold(cfg.AIProvider, "cloud") {
		canonical, ok := aiprovider.NormalizeCloud(cfg.CloudProvider)
		if !ok {
			if strings.TrimSpace(cfg.CloudProvider) != "" {
				// Unknown non-empty provider: label it generically rather than claiming Gemini.
				return "CLOUD_AI", "Cloud AI", cfg.CloudModel
			}
			canonical = "google"
		}
		model := cfg.CloudModel
		if model == "" {
			switch canonical {
			case "deepseek":
				model = "deepseek-chat"
			case "claude":
				model = "claude-3-7-sonnet-20250219"
			case "openai":
				model = "gpt-4o-mini"
			default:
				model = "gemini-2.5-flash"
			}
		}
		switch canonical {
		case "deepseek":
			return "DEEPSEEK_AI", "DeepSeek", model
		case "google":
			return "GEMINI_AI", "Google Gemini", model
		case "claude":
			return "CLAUDE_AI", "Anthropic Claude", model
		case "openai":
			return "OPENAI_AI", "OpenAI", model
		}
		return "CLOUD_AI", "Cloud AI", model
	}
	model := cfg.OllamaModel
	if model == "" {
		model = "qwen3.5:9b"
	}
	return moduleOllamaAI, "Ollama", model
}

func (d Deps) logModule() string {
	tag, _, _ := d.aiInfo()
	return tag
}

func (d Deps) debug(message string, meta map[string]any) {
	if d.Log != nil {
		d.Log.Debug(d.logModule(), message, meta)
	}
}

func (d Deps) info(module, message string, meta map[string]any) {
	if d.Log != nil {
		if module == moduleOllamaAI {
			module = d.logModule()
		}
		d.Log.Info(module, message, meta)
	}
}

func (d Deps) warn(module, message string, meta map[string]any) {
	if d.Log != nil {
		if module == moduleOllamaAI {
			module = d.logModule()
		}
		d.Log.Warn(module, message, meta)
	}
}

// ClassifyPDFText ports classifyPDFText (classify-document.ts:383). doclingMarkdown is the optional
// premade Markdown supplied by the extractor; when non-blank Step C is skipped. previousError is the
// TS optional retry feedback. A zero `now` is the TS default `new Date()`.
func (d Deps) ClassifyPDFText(rawText, filename, previousError string, now time.Time, doclingMarkdown string) (documentschema.DocumentMetadata, error) {
	_, aiProviderName, aiModelName := d.aiInfo()
	modelHealthy := d.Ollama.EnsureOllamaModel(aiModelName)

	categoriesConfig := d.Categories.GetCategoriesConfig()
	dictionary := d.EntityDictionary.GetEntityDictionary()
	// rawText, not the Step C markdown: the filter only needs to know which entities this document
	// mentions, and rawText is available before Step C runs.
	categoriesDescriptionStr := classification.BuildCategoriesDescriptionStr(categoriesConfig, dictionary, rawText)

	personalization := d.PromptPersonalization.GetPromptPersonalization()
	if now.IsZero() {
		now = time.Now()
	}

	var validated documentschema.DocumentMetadata
	decisionMethod := fmt.Sprintf("Modular %s AI Pipeline — Step A (entity) + Step C (markdown) + Step D (classification), %s", aiProviderName, aiModelName)
	decisionReason := fmt.Sprintf("Analyzed via %s Step A entity extraction + Step C markdown conversion + Step D classification", aiProviderName)
	extractedEntity := ""
	extractedDocType := ""

	pipelineErr := func() error {
		if !modelHealthy {
			// Ollama is down: NEVER fall through to the rule-based classifier here. That silent
			// fallback is what misfiled a SEPA mandate and two tax notices on 2026-08-31 — the
			// fallback exists for a healthy model's unparseable JSON, not for an unreachable one.
			// Propagate so the caller blocks the file in __raws and reminds the user to start Ollama.
			return &ollama.OllamaUnavailableError{Message: fmt.Sprintf(
				"%s is down — model '%s' failed the capability check (%s). Start %s, then re-scan. No documents were triaged.",
				aiProviderName, aiModelName, d.effectiveConfig().OllamaHost, aiProviderName,
			)}
		}

		// Step A: Dedicated Primary Entity & Document Type Extractor. Failures here are non-fatal —
		// Step D still runs (without the hint) rather than falling all the way back to the
		// rule-based classifier just because this narrow, best-effort pass didn't parse.
		promptA := d.Prompts.BuildEntityExtractionPrompt(filename, rawText)
		resA, errA := d.Ollama.RequestClassificationCompletion(promptA.System, promptA.User)
		if errA != nil {
			d.debug(fmt.Sprintf("[STEP A] Entity extraction skipped for %s: %s", filename, errA.Error()), map[string]any{"filename": filename})
		} else if parsedA, parseErr := classification.CleanAndParseJSON(jsTrim(resA.Response)); parseErr != nil {
			d.debug(fmt.Sprintf("[STEP A] Entity extraction skipped for %s: %s", filename, parseErr.Error()), map[string]any{"filename": filename})
		} else {
			extractedEntity = jsonStringField(parsedA, "issuing_entity", "entity_name")
			extractedDocType = jsonStringField(parsedA, "document_type", "doc_type")
			d.info(moduleOllamaAI, fmt.Sprintf(`[STEP A] Detected Entity: "%s", DocType: "%s" for %s`, extractedEntity, extractedDocType, filename), map[string]any{
				"filename": filename, "extractedEntity": extractedEntity, "extractedDocType": extractedDocType, "thinking": truncateForLog(resA.Thinking),
			})
		}

		// Step C: Chunk-by-Chunk Zero-Loss Markdown Conversion — OR the Docling structured Markdown
		// that was already adopted at extraction. When the Docling output passed its quality gate,
		// fullMarkdownContent is that deterministic Markdown (no LLM round-trip, no truncation risk);
		// the pre-registration quality gate in triage-scan still audits it against raw_text before
		// anything is written. Otherwise Step C runs as before.
		fullMarkdownContent := rawText
		premade := jsTrim(doclingMarkdown)
		if utf16Len(premade) > 0 {
			fullMarkdownContent = premade
			d.info(moduleOllamaAI, fmt.Sprintf("[STEP C] Using Docling structured Markdown (%d chars) for %s — skipping the chunk-by-chunk LLM conversion", utf16Len(premade), filename), map[string]any{
				"filename": filename, "markdownChars": utf16Len(premade),
			})
		} else if utf16Len(jsTrim(rawText)) > 0 {
			d.info(moduleOllamaAI, fmt.Sprintf("[STEP C] Converting raw text (%d chars) to Markdown chunk-by-chunk for %s...", utf16Len(rawText), filename), map[string]any{"filename": filename})
			converted, convErr := d.ConvertRawTextToZeroLossMarkdown(rawText, filename)
			if convErr != nil {
				return convErr
			}
			fullMarkdownContent = converted
		}

		// Step D: Classification + Executive Summary + structured metadata, using the clean Step C
		// markdown as input, with Step A's entity/doc-type passed as an explicit, prioritized hint
		// (see buildClassificationPrompt's entityHint block) rather than an easily-ignored bracket note.
		var entityHint *prompt.EntityHint
		if extractedEntity != "" {
			entityHint = &prompt.EntityHint{Entity: extractedEntity, DocType: extractedDocType}
		}
		promptD := d.Prompts.BuildClassificationPrompt(categoriesDescriptionStr, filename, fullMarkdownContent, prompt.ClassificationOptions{
			PreviousError:  previousError,
			SystemLanguage: d.Config.Language,
			EntityHint:     entityHint,
			Now:            now,
		})

		resD, errD := d.Ollama.RequestClassificationCompletion(promptD.System, promptD.User)
		if errD != nil {
			return errD
		}
		parsedD, parseErr := classification.CleanAndParseJSON(jsTrim(resD.Response))
		if parseErr != nil {
			return parseErr
		}
		rawMetadata, marshalErr := json.Marshal(parsedD)
		if marshalErr != nil {
			return marshalErr
		}
		parsedMetadata, schemaErr := documentschema.ParseDocumentMetadata(rawMetadata)
		if schemaErr != nil {
			return schemaErr
		}
		validated = parsedMetadata

		// Defense-in-depth: Step D can still mis-derive a future date from an OCR-garbled two-digit
		// year even with the CURRENT_DATE guard in the prompt (see formatting_rules.md) — catch it
		// here against the titre's own stated year before it corrupts date-based sorting downstream.
		dateReconciliation := classification.ReconcileDocumentDate(validated.Date, validated.Titre, now)
		if dateReconciliation.Corrected {
			d.warn(moduleOllamaAI, fmt.Sprintf(`[STEP D] Corrected future-dated "date" field for %s: %s`, filename, dateReconciliation.Reason), map[string]any{
				"filename": filename, "originalDate": validated.Date, "correctedDate": dateReconciliation.Date,
			})
			validated.Date = dateReconciliation.Date
		}

		// markdown_content is no longer requested from Step D (see prompt.ts/json_schema_response.json) —
		// Step C's chunk-by-chunk conversion is always the source of truth, so it's applied unconditionally
		// instead of the former "keep Step D's copy unless it's under 50% of Step C's length" comparison,
		// which spent Step D's num_predict budget on markdown that was thrown away most of the time anyway.
		validated.MarkdownContent = fullMarkdownContent

		rawCategorie := jsonStringField(parsedD, "categorie")
		rawSubcategorie := jsonStringField(parsedD, "subcategorie")
		d.info(moduleOllamaAI, fmt.Sprintf(`[STEP D] Raw classification from %s for %s: categorie="%s", subcategorie="%s"`, aiProviderName, filename, rawCategorie, rawSubcategorie), map[string]any{
			"filename": filename, "rawCategorie": rawCategorie, "rawSubcategorie": rawSubcategorie,
			"thinking": truncateForLog(firstNonEmpty(resD.Thinking, jsonStringField(parsedD, "thinking"))),
		})

		// Entity-priority override (finding A): when Step A's entity is grounded in the curated
		// entity_dictionary.json and Step D fell back to a generic category, trust the grounded
		// entity over Step D's freeform guess — see classification-resolution.ts for why this is
		// scoped to "weak fallback" categories only.
		entityPriorityApplied := false
		if extractedEntity != "" {
			override := classificationresolution.ApplyEntityPriorityOverride(validated, extractedEntity, dictionary)
			if override.Overridden {
				entityPriorityApplied = true
				d.info(moduleOllamaAI, "[STEP A PRIORITY] "+override.Reason, map[string]any{
					"filename": filename, "from": validated.Categorie + "/" + validated.Subcategorie, "to": override.Categorie + "/" + override.Subcategorie,
				})
				validated.Categorie = override.Categorie
				validated.Subcategorie = override.Subcategorie
				decisionReason = fmt.Sprintf("Step D classified as %s/%s, but overridden by Step A entity priority: %s", rawCategorie, rawSubcategorie, override.Reason)
			}
		}

		if !entityPriorityApplied {
			decisionReason = fmt.Sprintf("Step D classified document as %s/%s (Step A entity: %s)", validated.Categorie, validated.Subcategorie, orNA(extractedEntity))
		}
		return nil
	}()

	if pipelineErr != nil {
		// Ollama down mid-pipeline (connection dropped between the health check and Step D) is the
		// same situation as the failed capability check: block, do not rule-fall-back.
		var unavailable *ollama.OllamaUnavailableError
		if errors.As(pipelineErr, &unavailable) {
			return documentschema.DocumentMetadata{}, pipelineErr
		}
		decisionMethod = "Rule-Based Pattern Classifier"
		rb := classification.RuleBasedClassify(rawText, filename, dictionary, d.Config.PersonalNameDenylist, personalization)
		decisionReason = "Rule-Based fallback: " + rb.Reason
		d.warn(moduleOllamaAI, fmt.Sprintf("%s AI request failed for %s: %s. %s", aiProviderName, filename, pipelineErr.Error(), decisionReason), nil)
		validated = documentschema.DocumentMetadata{
			Titre:           rb.Title,
			Registre:        "",
			Date:            rb.Date,
			Categorie:       rb.Categorie,
			Subcategorie:    rb.Subcategorie,
			Summary:         "Document: " + rb.Title + ". " + decisionReason,
			Tags:            truthyStrings(rb.Categorie, rb.Subcategorie),
			MarkdownContent: "# " + rb.Title + "\n\n" + rawText,
		}
	}

	resolutionDeps := classificationresolution.Deps{EntityDictionary: dictionary, PromptPersonalization: personalization}
	validated = classificationresolution.RefineClassification(validated, rawText, filename, dictionary, d.Config.PersonalNameDenylist, resolutionDeps)

	// The raw values the model proposed — kept BEFORE resolution so the duplicate guard can record
	// exactly what was blocked and what it was mapped onto (the hint that teaches future runs).
	proposedCategory := validated.Categorie
	proposedSubcategory := validated.Subcategorie

	categoryResult := classificationresolution.ResolveCategory(&categoriesConfig, proposedCategory, proposedSubcategory)
	if categoryResult.Conflict != nil {
		// BLOCK: never auto-create a near-duplicate top-level category or an entity-as-category
		// (e.g. 'administratif' -> 'administrative', 'france_travail' as a category). Remap to the
		// existing entry and record a hint so future runs stop proposing it.
		d.warn(moduleTaxonomyGuard, "[BLOCKED] "+categoryResult.Conflict.Hint, map[string]any{
			"filename": filename, "proposedCategory": proposedCategory, "proposedSubcategory": proposedSubcategory,
		})
		d.TaxonomyHints.RecordTaxonomyHint(&taxonomyconflicts.TaxonomyHintEntry{
			ProposedCategory:    proposedCategory,
			ProposedSubcategory: proposedSubcategory,
			MappedCategory:      categoryResult.Conflict.MappedCategoryID,
			MappedSubcategory:   categoryResult.Conflict.MappedSubcategoryID,
			Hint:                categoryResult.Conflict.Hint,
		})
		decisionReason += " | Taxonomy duplicate guard: " + categoryResult.Conflict.Hint
	}
	if categoryResult.IsNew {
		d.info(moduleOllamaAI, fmt.Sprintf("Auto-created new category '%s' for %s BEFORE move", categoryResult.Category.ID, filename), nil)
		if saveErr := d.Categories.SaveCategoriesConfig(categoriesConfig.Categories); saveErr != nil {
			return documentschema.DocumentMetadata{}, saveErr
		}
	}
	validated.Categorie = categoryResult.Category.ID

	subcategoryResult := classificationresolution.ResolveSubcategory(categoryResult.Category, proposedSubcategory, rawText, filename, d.Config.PersonalNameDenylist, resolutionDeps, &categoriesConfig)
	if subcategoryResult.Conflict != nil {
		// BLOCK: the slug already exists elsewhere in the taxonomy (exact, alias or near-duplicate
		// spelling). Never create a second instance — reuse the existing slug, and when it lives
		// under another category, re-file the document there (one-instance-per-subcategory).
		d.warn(moduleTaxonomyGuard, "[BLOCKED] "+subcategoryResult.Conflict.Hint, map[string]any{
			"filename": filename, "proposedCategory": proposedCategory, "proposedSubcategory": proposedSubcategory,
		})
		d.TaxonomyHints.RecordTaxonomyHint(&taxonomyconflicts.TaxonomyHintEntry{
			ProposedCategory:    proposedCategory,
			ProposedSubcategory: proposedSubcategory,
			MappedCategory:      subcategoryResult.Conflict.MappedCategoryID,
			MappedSubcategory:   subcategoryResult.Conflict.MappedSubcategoryID,
			Hint:                subcategoryResult.Conflict.Hint,
		})
		decisionReason += " | Taxonomy duplicate guard: " + subcategoryResult.Conflict.Hint
		if subcategoryResult.Conflict.MappedCategoryID != categoryResult.Category.ID {
			validated.Categorie = subcategoryResult.Conflict.MappedCategoryID
		}
	}
	if subcategoryResult.IsNew {
		d.info(moduleOllamaAI, fmt.Sprintf("Auto-created new subcategory '%s' under '%s' BEFORE move", subcategoryResult.SubcategoryID, categoryResult.Category.ID), map[string]any{"filename": filename})
		if saveErr := d.Categories.SaveCategoriesConfig(categoriesConfig.Categories); saveErr != nil {
			return documentschema.DocumentMetadata{}, saveErr
		}
		validated.Subcategorie = subcategoryResult.SubcategoryID
	} else if subcategoryResult.SubcategoryID == "general" && validated.Subcategorie != "general" {
		d.warn(moduleOllamaAI, fmt.Sprintf("Rejected ungrounded subcategory slug '%s' for %s (not found in document content) — trying rule-based fallback", subcategoryResult.RawSubSlug, filename), nil)
		rbFallback := classification.RuleBasedClassify(rawText, filename, dictionary, d.Config.PersonalNameDenylist, personalization)
		if rbFallback.Subcategorie != "" && rbFallback.Subcategorie != "general" {
			validated.Categorie = rbFallback.Categorie
			validated.Subcategorie = rbFallback.Subcategorie
			decisionReason += " | Rule fallback override: " + rbFallback.Reason
		} else if validated.Categorie == "bulletin_salaire" {
			validated.Subcategorie = "bulletin_salaire"
		} else {
			validated.Subcategorie = "general"
		}
	} else {
		validated.Subcategorie = subcategoryResult.SubcategoryID
	}

	d.info(moduleDecisionLogic, fmt.Sprintf("[DECISION LOGIC] '%s' ➔ '%s/%s'", filename, validated.Categorie, validated.Subcategorie), map[string]any{
		"filename":         filename,
		"method":           decisionMethod,
		"reason":           decisionReason,
		"title":            validated.Titre,
		"category":         validated.Categorie,
		"subcategory":      validated.Subcategorie,
		"date":             validated.Date,
		"extractedEntity":  nilifyEmpty(extractedEntity),
		"extractedDocType": nilifyEmpty(extractedDocType),
		// The model's own self-reported reasoning for this classification, when it filled the
		// "thinking" field in its JSON response (see prompts/json_schema_response.json) — distinct
		// from Ollama's out-of-band response.thinking, which is intentionally disabled via
		// think:false (see ollama-client.ts) so the model's whole answer doesn't route there instead
		// of response.response.
		"thinking": truncateForLog(validated.Thinking),
	})

	return validated, nil
}

// ---- helpers ----

// truncateForLog condenses a model's chain-of-thought / reasoning text for structured logging —
// long enough to be useful when tracing a bad decision later, short enough not to flood
// logs/triage_debug.log. It returns nil where TS returned undefined (the omitted map field).
func truncateForLog(text string, maxLenOpt ...int) any {
	maxLen := 400
	if len(maxLenOpt) > 0 {
		maxLen = maxLenOpt[0]
	}
	if text == "" {
		return nil
	}
	trimmed := jsTrim(text)
	if trimmed == "" {
		return nil
	}
	if utf16Len(trimmed) > maxLen {
		return utf16Slice(trimmed, maxLen) + "…"
	}
	return trimmed
}

func orNA(value string) string {
	if value == "" {
		return "N/A"
	}
	return value
}

func filenameOrDocument(filename string) string {
	if filename == "" {
		return "document"
	}
	return filename
}

func nilifyEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// truthyStrings is `[categorie, subcategorie].filter(Boolean)`: only non-empty strings survive.
func truthyStrings(values ...string) []string {
	out := []string{}
	for _, v := range values {
		if v != "" {
			out = append(out, v)
		}
	}
	return out
}

// jsonStringField reproduces `parsed.a || parsed.b || ”` for the string keys the pipeline reads.
// Non-string JSON scalars are rendered with %v so a numeric field still yields a non-empty value
// (JS would assign the number; the downstream template then stringifies it the same way).
func jsonStringField(parsed any, keys ...string) string {
	m, ok := parsed.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range keys {
		value, ok := m[key]
		if !ok || value == nil {
			continue
		}
		s, ok := value.(string)
		if !ok {
			s = fmt.Sprintf("%v", value)
		}
		if s != "" {
			return s
		}
	}
	return ""
}

// ---- JavaScript string semantics ----

// jsWhitespaceClass is the exact character set matched by JavaScript's `\s`: WhiteSpace plus
// LineTerminator. It is inlined into the regexes below; Go's `\s` would silently drop NBSP,
// U+2028/29, the Unicode spaces and U+FEFF.
const jsWhitespaceClass = `\t\n\v\f\r \x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

// jsDotClass is JavaScript's `.`: any character except a line terminator. Go's `.` excludes only
// '\n', so it would also match a lone '\r' or U+2028/U+2029 left inside a line.
const jsDotClass = `[^\n\r\x{2028}\x{2029}]`

var lineSplitRe = regexp.MustCompile(`\r?\n`)

// jsTrim is `String.prototype.trim()`.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSWhitespace)
}

// jsTrimEnd is `String.prototype.trimEnd()`.
func jsTrimEnd(s string) string {
	return strings.TrimRightFunc(s, isJSWhitespace)
}

// jsTrimStart is `String.prototype.trimStart()`.
func jsTrimStart(s string) string {
	return strings.TrimLeftFunc(s, isJSWhitespace)
}

// isJSWhitespace reports whether r is in JavaScript's WhiteSpace+LineTerminator set.
func isJSWhitespace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ',
		0x00A0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}

// utf16Len is JavaScript's `str.length`: the number of UTF-16 code units, not runes.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// utf16Slice reproduces `str.substring(0, n)`: the first n UTF-16 code units. A rune whose code
// units would straddle the cut is dropped; JS would keep a lone surrogate there, which a Go UTF-8
// string cannot represent.
func utf16Slice(s string, n int) string {
	if n <= 0 {
		return ""
	}
	units := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if units+w > n {
			return s[:i]
		}
		units += w
	}
	return s
}

// utf16SliceFrom reproduces `str.slice(start)`: everything from UTF-16 code unit `start` onward. A
// rune straddling the cut is dropped (see the package comment).
func utf16SliceFrom(s string, start int) string {
	if start <= 0 {
		return s
	}
	units := 0
	for i, r := range s {
		if units >= start {
			return s[i:]
		}
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		units += w
	}
	return ""
}

// jsRound is Math.round: floor(x + 0.5), i.e. half-up (toward +Infinity), not Go's math.Round
// (half away from zero). The values it is used on are non-negative.
func jsRound(x float64) int {
	return int(x + 0.5)
}

// jsToFixed0 is `(x).toFixed(0)` with the spec's "pick the larger n" tie-break for positives, the
// same rule jsRound implements.
func jsToFixed0(x float64) string {
	return fmt.Sprintf("%d", jsRound(x))
}
