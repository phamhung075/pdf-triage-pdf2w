// Package chatplanner is a Go port of pdf-triage's src/application/chat-query-planner.ts (79
// lines): buildPlannerPrompt and planQuery. It turns a free-text request into a
// chatquery.StructuredQuery using the Qwen model, and falls back to the deterministic heuristic
// planner on any failure.
//
// Upstream status. `npx vitest run src/application/chat-query-planner.test.ts` -> 9 passed at port
// time, so no upstream case is pinned red. All 9 cases are ported, plus cases for nil collaborators
// and for the injected category list reaching the model request.
//
// Collaborators are injected, never globals, exactly where TS used module imports:
//
//   - TextChat is the Ollama text-chat call (chat-query-planner.ts:67). It is satisfied by
//     *infra/ollama.Client's RequestTextChatCompletion method.
//   - Categories is the live taxonomy read (chat-query-planner.ts:65). It is satisfied by
//     *store/categories.Store.GetCategoriesConfig.
//   - Logger is the warn sink (chat-query-planner.ts:71, :76). A nil logger is silent.
//
// The TS source is the behavioral source of truth. Deviations, all resolved in favor of matching
// TS:
//
//  1. `response ?? ”` (chat-query-planner.ts:69). Go's TextCompletion.Response is a plain string,
//     so the null case is the empty string, which cleanAndParseJSON rejects exactly as TS did.
//  2. cleanAndParseJSON returns the decoded `any` (classification.ts:289), while Zod's
//     StructuredQuerySchema.parse (chat-query-planner.ts:69) validates the object. The ported
//     chatquery.Parse takes raw JSON bytes, so the cleaned value is re-marshalled to JSON before
//     Parse. The schema reads only strings, string arrays, numbers and null, so the round-trip is
//     lossless for every field the schema can accept.
//  3. `now = new Date()` is an explicit time.Time; the prompt grounds on its LOCAL calendar fields
//     through classification.FormatLocalDate (classification.ts:659), so a caller passes the same
//     clock value it will later use for relative expressions.
//  4. Any parse/validation failure degrades to PlanQueryHeuristic, the same single fallback branch
//     TS uses for every error (chat-query-planner.ts:61-78). PlanQuery therefore never returns an
//     error and never returns an unusable plan.
package chatplanner

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/chatquery"
	"github.com/phamhung075/pdf-triage-pdf2w/classification"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/store/categories"
)

// TextChat is the slice of the Ollama client the planner needs. *infra/ollama.Client satisfies it.
type TextChat interface {
	RequestTextChatCompletion(system, user string) (ollama.TextCompletion, error)
}

// Logger is the slice of *infra/logger.Logger the planner needs. A nil Logger is silent.
type Logger interface {
	Warn(moduleName, message string, meta any, filename ...string)
}

// Deps carries the injected collaborators. Zero values degrade to the heuristic planner rather
// than panicking: a missing Ollama client or missing taxonomy is a planning failure, and a planning
// failure is always the heuristic fallback.
type Deps struct {
	Ollama     TextChat
	Categories func() documentschema.CategoriesConfig
	Log        Logger
}

// Compile-time proof that the real collaborators satisfy the injected seams the later wiring will
// pass in.
var (
	_ TextChat                               = (*ollama.Client)(nil)
	_ Logger                                 = (*logger.Logger)(nil)
	_ func() documentschema.CategoriesConfig = (*categories.Store)(nil).GetCategoriesConfig
)

// BuildPlannerPrompt ports buildPlannerPrompt.
//
// Builds the planner prompt. The category list is a parameter, read from the live taxonomy by the
// caller — never a literal here. Committed prompts stay free of personal entities (Golden Rule);
// the model learns real names from the taxonomy at runtime, not from this file.
func BuildPlannerPrompt(userMessage string, categoryIDs []string, now time.Time) (system, userPrompt string) {
	system = "Tu convertis une demande de document en requête de recherche structurée JSON.\n" +
		"Tu ne réponds JAMAIS à la demande — tu produis UNIQUEMENT l'objet JSON.\n" +
		"\n" +
		"Nous sommes le " + classification.FormatLocalDate(now) + ". Résous toute expression temporelle relative\n" +
		"(\"les 3 derniers mois\", \"cette année\", \"l'an dernier\") en dates ISO à partir de cette date.\n" +
		"\n" +
		"Catégories disponibles: " + strings.Join(categoryIDs, ", ") + ".\n" +
		"\n" +
		"Renvoie exactement cet objet:\n" +
		"{\n" +
		"  \"docTypes\": [],   // le TYPE de document demandé, avec ses synonymes usuels et son sigle.\n" +
		"                    // Ex: pour un RIB -> [\"rib\", \"relevé d'identité bancaire\", \"iban\", \"bic\"]\n" +
		"  \"entities\":  [],  // l'organisme / l'émetteur cité, avec ses variantes et abréviations\n" +
		"  \"keywords\":  [],  // les autres termes porteurs de sens (jamais de mots vides)\n" +
		"  \"notTerms\":  [],  // les types de documents à EXCLURE quand ils se confondent avec la demande.\n" +
		"                    // Ex: pour un RIB -> [\"relevé de compte\", \"mouvement\"]\n" +
		"  \"category\":  null,        // une des catégories ci-dessus, ou null\n" +
		"  \"subcategory\": null,\n" +
		"  \"dateFrom\":  null,        // \"YYYY-MM-DD\" ou null\n" +
		"  \"dateTo\":    null,\n" +
		"  \"limit\":     null         // nombre de documents demandé, ou null\n" +
		"}\n" +
		"\n" +
		"Règles:\n" +
		"- Mets des SYNONYMES dans docTypes: c'est ce qui rattrape un document mal titré.\n" +
		"- notTerms est ce qui sépare deux documents du même organisme. Utilise-le.\n" +
		"- N'invente pas de catégorie absente de la liste. Dans le doute, null.\n" +
		"- Aucun texte hors du JSON."

	return system, `Demande: "` + userMessage + `"`
}

// PlanQuery ports planQuery.
//
// Turns a free-text request into a StructuredQuery.
//
// Never returns an unusable plan: any failure — Ollama down, prose instead of JSON, valid JSON
// with nothing searchable in it — degrades to the deterministic heuristic planner, so the chat
// keeps working without a model.
func PlanQuery(deps Deps, userMessage string, now time.Time) chatquery.StructuredQuery {
	fallback := func() chatquery.StructuredQuery { return chatquery.PlanQueryHeuristic(userMessage) }

	categoryIDs := []string{}
	if deps.Categories != nil {
		for _, c := range deps.Categories().Categories {
			if c != nil {
				categoryIDs = append(categoryIDs, c.ID)
			}
		}
	}
	system, userPrompt := BuildPlannerPrompt(userMessage, categoryIDs, now)

	if deps.Ollama == nil {
		deps.warn("Planner failed (ollama client not configured); using heuristic planner.")
		return fallback()
	}
	result, err := deps.Ollama.RequestTextChatCompletion(system, userPrompt)
	if err != nil {
		deps.warn(fmt.Sprintf("Planner failed (%s); using heuristic planner.", err.Error()))
		return fallback()
	}

	plan, err := parseStructuredQuery(result.Response)
	if err != nil {
		deps.warn(fmt.Sprintf("Planner failed (%s); using heuristic planner.", err.Error()))
		return fallback()
	}
	if chatquery.BuildFtsMatchExpression(plan) == nil {
		deps.warn("Model plan had no searchable facet; using heuristic planner.")
		return fallback()
	}
	return plan
}

// parseStructuredQuery is `StructuredQuerySchema.parse(cleanAndParseJSON(response ?? ”))`
// (chat-query-planner.ts:69). CleanAndParseJSON yields a decoded value, so it is re-marshalled to
// JSON for chatquery.Parse (package comment deviation 2).
func parseStructuredQuery(response string) (chatquery.StructuredQuery, error) {
	parsed, err := classification.CleanAndParseJSON(response)
	if err != nil {
		return chatquery.StructuredQuery{}, err
	}
	raw, err := json.Marshal(parsed)
	if err != nil {
		return chatquery.StructuredQuery{}, err
	}
	return chatquery.Parse(raw)
}

func (d Deps) warn(message string) {
	if d.Log != nil {
		d.Log.Warn("CHAT_PLANNER", message, nil)
	}
}
