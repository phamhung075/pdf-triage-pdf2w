// Package promptpersonalization is a Go port of pdf-triage's
// src/infrastructure/prompt-personalization-store.ts (64 lines): getPromptPersonalization
// (:28) reads the gitignored .prompts.private.json overlay (:30-37), appends the priority rules
// learned from the user's enabled manual move decisions (:41-47), and appends the persisted
// taxonomy duplicate-guard hint block (:53-62).
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/prompt-personalization-store.test.ts` -> 6 passed), so no
// upstream case is pinned red. All 6 cases are ported (promptpersonalization_test.go), plus cases
// for the taxonomy-hint block and the JS-trim edge the TS suite never covered.
//
// Golden Rule 15: the private file is the operator's real data. This package reads it only when
// the caller points PromptsPrivateFile at it; the tests always write their own synthetic
// .prompts.private.json inside t.TempDir().
//
// The TS module reads CONFIG.PROMPTS_PRIVATE_FILE plus the manual-decisions and taxonomy-hints
// stores. The settings port is a later phase and this package must not import it, so the private
// file path and the two narrow synchronous read surfaces are explicit Options fields.
//
// WHY-comments from the TS source are preserved verbatim in the code below.
//
// Deviations, all resolved in favor of matching the TS acceptance bar:
//
//  1. Config is per-Store instead of the module-global CONFIG.
//  2. readManualDecisionsSync / readTaxonomyHintsSync are reached through the narrow DecisionReader
//     and HintsReader interfaces, so the hot-path synchronous reads are injected (and fakeable) and
//     the store does not hard-depend on the concrete stores. *manualdecisions.Store and
//     *taxonomyhints.Store satisfy them. A nil reader is the no-decisions / no-hints feed.
//  3. JS String.prototype.trim() semantics. extra_rules_text is trimmed with jsTrim, which strips
//     exactly JavaScript's WhiteSpace+LineTerminator set. Go's strings.TrimSpace would additionally
//     strip U+0085 NEL (JS does not) and would leave U+FEFF (JS strips it).
//  4. The invalid-overlay message is written to an injected io.Writer (TS console.error); nil
//     defaults to os.Stderr, the same shape as store/entitydictionary.
//  5. ReadFile is an injectable seam so the "file exists but is unreadable" branch is testable
//     without chmod; it defaults to os.ReadFile.
//  6. TS `decisionsToPriorityRules` receives the store records structurally; this port converts the
//     concrete []manualdecisions.Record to []decisionrule.HumanDecisionLike. Record.ID (int64)
//     narrows to HumanDecisionLike.ID (int), which only the note text uses.
package promptpersonalization

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/decisionrule"
	promptdomain "github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomyconflicts"
)

// ReadFunc reads a whole file. Defaults to os.ReadFile; injectable for tests.
type ReadFunc func(path string) ([]byte, error)

// DecisionReader is the narrow synchronous read surface this store needs from
// store/manualdecisions; *manualdecisions.Store satisfies it. A nil reader yields no learned rules.
type DecisionReader interface {
	ReadManualDecisionsSync() []manualdecisions.Record
}

// HintsReader is the narrow synchronous read surface this store needs from store/taxonomyhints;
// *taxonomyhints.Store satisfies it. A nil reader yields no guard block.
type HintsReader interface {
	ReadTaxonomyHintsSync() []*taxonomyconflicts.TaxonomyHintEntry
}

// Options configure a Store. PromptsPrivateFile is TS CONFIG.PROMPTS_PRIVATE_FILE (the gitignored
// BASE_DIR/.prompts.private.json); an absent file is the normal fresh-clone state.
type Options struct {
	PromptsPrivateFile string
	ManualDecisions    DecisionReader
	TaxonomyHints      HintsReader
	// Stderr receives the invalid-overlay message (TS console.error); nil defaults to os.Stderr.
	Stderr io.Writer
	// ReadFile reads PromptsPrivateFile. Defaults to os.ReadFile.
	ReadFile ReadFunc
}

// Store owns the private-file path and the two injected read surfaces. It holds no cache: the
// upstream store re-reads the private file on every prompt build, and the manual-decisions /
// taxonomy-hints stores apply their own mtime caches. Methods are safe for concurrent callers.
type Store struct {
	promptsPrivateFile string
	manualDecisions    DecisionReader
	taxonomyHints      HintsReader
	stderr             io.Writer
	readFile           ReadFunc
}

// New builds a Store.
func New(opts Options) *Store {
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	readFile := opts.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	return &Store{
		promptsPrivateFile: opts.PromptsPrivateFile,
		manualDecisions:    opts.ManualDecisions,
		taxonomyHints:      opts.TaxonomyHints,
		stderr:             stderr,
		readFile:           readFile,
	}
}

// GetPromptPersonalization is `getPromptPersonalization()`
// (prompt-personalization-store.ts:28). Its method value is exactly the
// `func() promptpersonalization.PromptPersonalization` that prompt.Deps.Personalization and the
// PromptPersonalization value classificationresolution.Deps expects are built from.
func (s *Store) GetPromptPersonalization() promptdomain.PromptPersonalization {
	// Reads the gitignored `.prompts.private.json` overlay that supplies the personal signals the
	// committed prompts/ templates deliberately don't carry (see domain/prompt-personalization.ts
	// for the why and prompts.private.json.example for the shape).
	//
	// An absent file is the normal state for a fresh clone, not an error: the prompts are written
	// to read correctly with both injected blocks empty. An INVALID file is logged and treated as
	// empty rather than thrown, matching entity-dictionary-store.ts / categories-store.ts — a typo
	// in a personalization file must never take the whole triage pipeline down.
	personalization := promptdomain.EMPTY_PROMPT_PERSONALIZATION
	// os.Stat mirrors fs.existsSync: any stat error means "absent" and is silently skipped, while a
	// path that stats successfully (including a directory) falls into the same try/read/parse path
	// as TS, where a read error is logged and treated as empty.
	if _, err := os.Stat(s.promptsPrivateFile); err == nil {
		raw, readErr := s.readFile(s.promptsPrivateFile)
		if readErr != nil {
			s.logInvalid(readErr)
		} else if parsed, parseErr := promptdomain.Parse(raw); parseErr != nil {
			s.logInvalid(parseErr)
		} else {
			personalization = parsed
		}
	}

	// On top of the hand-curated file, every ENABLED human move decision (manual_decisions store)
	// is appended as a STEP 0 priority rule — the feedback-teaches-AI loop (Golden Rule #18). The
	// decisions are injected through the SAME {{USER_PRIORITY_RULES}} block, so the Qwen prompt and
	// the deterministic ruleBasedClassify fallback (matchPriorityRules) both see them and stay
	// logically aligned. The hand-curated rules come first: deliberate manual curation outranks an
	// auto-derived rule when matchPriorityRules resolves ties (first match wins).
	//
	// readManualDecisionsSync returns newest first; decisionsToPriorityRules caps the injection
	// so a growing feedback log cannot bloat the prompt.
	learned := s.learnedRules()
	if len(learned) > 0 {
		// A fresh slice reproduces the TS object spread `[...personalization.priority_rules, ...learned]`
		// instead of appending into whatever backing array Parse happened to return.
		merged := make([]promptdomain.PriorityRule, 0, len(personalization.PriorityRules)+len(learned))
		merged = append(merged, personalization.PriorityRules...)
		merged = append(merged, learned...)
		personalization.PriorityRules = merged
	}

	// Taxonomy duplicate-guard hints (blocked duplicate creations) are appended to the STEP 0
	// block's extra_rules_text: they are taxonomy FACTS ("slug X must map to Y under Z"), not
	// keyword match rules, so they render as instructions the model reads before classifying —
	// the HINT half of the block-then-hint loop (see domain/taxonomy-conflicts.ts).
	var hints []*taxonomyconflicts.TaxonomyHintEntry
	if s.taxonomyHints != nil {
		hints = s.taxonomyHints.ReadTaxonomyHintsSync()
	}
	guardBlock := taxonomyconflicts.RenderTaxonomyConflictHintsBlock(hints)
	if guardBlock != "" {
		personalization.ExtraRulesText = joinRenderedBlocks(personalization.ExtraRulesText, guardBlock) + "\n"
	}
	return personalization
}

// learnedRules ports `decisionsToPriorityRules(readManualDecisionsSync())`: newest-first records
// mapped onto the structural HumanDecisionLike shape decisionrule consumes.
func (s *Store) learnedRules() []promptdomain.PriorityRule {
	if s.manualDecisions == nil {
		return nil
	}
	records := s.manualDecisions.ReadManualDecisionsSync()
	if len(records) == 0 {
		return nil
	}
	decisions := make([]decisionrule.HumanDecisionLike, 0, len(records))
	for _, r := range records {
		decisions = append(decisions, decisionrule.HumanDecisionLike{
			ID:                 int(r.ID),
			OriginalFilename:   r.OriginalFilename,
			Title:              r.Title,
			NewCategory:        r.NewCategory,
			NewSubcategory:     r.NewSubcategory,
			UserFeedbackReason: r.UserFeedbackReason,
			RuleKeywords:       r.RuleKeywords,
			Enabled:            r.Enabled,
			CreatedAt:          r.CreatedAt,
		})
	}
	return decisionrule.DecisionsToPriorityRules(decisions)
}

// joinRenderedBlocks is the TS expression `[ (extra_rules_text || "").trim(),
// guardBlock.trim() ].filter(Boolean).join("\n\n")`: an absent or blank block is dropped, then the
// survivors are separated by a blank line.
func joinRenderedBlocks(existing, guardBlock string) string {
	parts := make([]string, 0, 2)
	if t := jsTrim(existing); t != "" {
		parts = append(parts, t)
	}
	if t := jsTrim(guardBlock); t != "" {
		parts = append(parts, t)
	}
	return strings.Join(parts, "\n\n")
}

func (s *Store) logInvalid(err error) {
	fmt.Fprintf(s.stderr, "Invalid .prompts.private.json, ignoring prompt personalization %v\n", err)
}

// jsTrim is String.prototype.trim(): it strips exactly the JavaScript whitespace set below. Go's
// strings.TrimSpace would additionally strip U+0085 NEL and would leave U+FEFF.
func jsTrim(s string) string {
	return strings.TrimFunc(s, isJSWhitespace)
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
