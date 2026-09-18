// Package manualdecisions is a Go port of pdf-triage's src/infrastructure/manual-decisions-store.ts
// (315 lines): the feedback log that records a user's relocalize/edit choices, inserts them into
// the SQLite manual_decisions table AND mirrors them to manual_decisions.json, so the prompt
// builder can read the newest-first decision list synchronously on the hot path and inject the
// learned STEP 0 rules (decisionrule.DecisionsToPriorityRules, the port of domain/decision-rule.ts).
//
// Upstream status. `npx vitest run src/infrastructure/manual-decisions-store.test.ts` -> 9 passed
// at port time; no upstream case is red, so none is pinned. All 9 cases are ported
// (manualdecisions_test.go) plus 10 added cases for behaviour the TS suite left unasserted: the
// absolute-path guard on relative/empty paths, legacy rule_keywords/enabled normalization, the
// trim/lowercase/dedup patch rules, the 500-code-unit snippet cut, the update-creates-mirror path,
// the DB-unavailable mirror fallback, a DB-insert failure still mirroring, the mtime cache, and
// clear with a bad mirror path.
//
// The functions ported are the absolute-path guard (manual-decisions-store.ts:16), the JSON mirror
// helpers (:75, :92), recordManualDecision (:101), readManualDecisionsSync (:169),
// getManualDecisions (:194), updateManualDecision (:225), deleteManualDecision (:283) and
// clearManualDecisions (:310). deriveRuleKeywords (:6) was already ported as
// decisionrule.DeriveRuleKeywords and is reused rather than duplicated.
//
// Database dependency (GAP for the orchestrator). store/database owns the schema and the
// manual_decisions table (store/database/database.go:192-209) but exposes NO manual-decision CRUD
// and no accessor for its *sql.DB, so this package cannot call it directly. It therefore declares
// the narrow DecisionDB interface below — exactly the database surface it needs — and runs its own
// parameterised SQL against that interface. *sql.DB satisfies it, so the tests create the schema
// with store/database.Open and then open their own handle on the same temp file. For the
// orchestrator: either add Exec/Query/QueryRow passthroughs (or a DB() *sql.DB accessor) to
// store/database.Store so it satisfies DecisionDB, or move the six SQL statements into semantic
// methods (InsertManualDecision, GetManualDecision, ListManualDecisions, UpdateManualDecisionRow,
// DeleteManualDecisionRow, ClearManualDecisions) and re-point this package at them. Either way no
// caller should need a second handle.
//
// Semantic gaps, all resolved to MATCH TS:
//
//  1. Optional `enabled`. TS `enabled?: number` distinguishes undefined from an explicit 0
//     (`record.enabled === 0 ? 0 : 1`). Go's zero value cannot, so Record.Enabled is *int: nil means
//     unspecified (active), a pointer to 0 means disabled. updateManualDecision reproduces the JS
//     truthiness test `patch.enabled ? 1 : 0` via `*patch.Enabled != 0`.
//  2. Optional `id`. TS `id?: number`; Go int64 uses 0 for absent, safe because AUTOINCREMENT ids
//     start at 1. `decisionId ?? record.id` becomes "use the inserted id unless it is 0".
//  3. Nullable columns. TS objects expose SQL NULL as null; Go scans through sql.NullString /
//     sql.NullInt64 and normalises NULL to "". A NULL document_id becomes 0 where TS kept null.
//  4. `substring(0, 500)`. JS offsets are UTF-16 code units, not bytes or runes. jsSubstring
//     encodes to UTF-16, slices, and decodes, so ASCII and BMP text match Node exactly; a cut
//     through a surrogate pair yields U+FFFD because Go's UTF-8 strings cannot hold a lone
//     surrogate (JS keeps one).
//  5. `JSON.stringify(data, null, 2)` does not escape HTML. Go's encoding/json escapes <, > and &
//     by default, so the writer disables HTML escaping and strips the Encoder's trailing newline
//     (same approach as infra/jsonregistry).
//  6. `new Date().toISOString()` always renders exactly three fractional digits; Go's
//     time.RFC3339Nano trims trailing zeros, so created_at is formatted explicitly as
//     "2006-01-02T15:04:05.000Z" in UTC.
//  7. `SELECT *` column order. TS maps rows to a named object; Go scans positionally, so every
//     query names its columns explicitly in the CREATE TABLE order instead of using `*` (the same
//     reason recorded in store/database's package comment, semantic gap 1).
//  8. Unknown/legacy keys in a mirror record. TS's object spread preserved keys it did not know
//     about when rewriting the mirror; this port decodes into the typed Record, so unknown keys are
//     dropped on rewrite. No current writer produces them.
//  9. Raw `enabled` in update. TS writes back `existing.enabled ?? 1` un-normalised, so a legacy
//     row holding 2 stays 2 in SQLite while the returned record normalises to 1. This port
//     normalises on read, so it writes 1. Observable only for rows no current writer creates.
//  10. Module state. TS keeps `syncCache` in module scope; this port puts it on the Store behind a
//     mutex, because Go callers are concurrent and a package-level cache would race.
//  11. Logging. TS calls the logger singleton; this port injects the narrow Logger interface
//     (satisfied by *logger.Logger) and keeps the DECISION_REGISTRY module name and messages. A nil
//     Logger discards.
//
// Deviation from TS requested by the migration (not a semantic gap): the JSON mirror is written
// atomically — temp file plus rename, with the same EPERM/EBUSY copy fallback as infra/jsonregistry
// — instead of TS's plain writeFileSync, which can leave a truncated mirror after a crash.
package manualdecisions

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/phamhung075/pdf-triage-pdf2w/decisionrule"
)

// jsISOLayout is Date.toISOString(): always three fractional digits and a literal Z.
const jsISOLayout = "2006-01-02T15:04:05.000Z"

// maxRawTextSnippet is the `substring(0, 500)` cap recordManualDecision applies.
const maxRawTextSnippet = 500

// SQL. store/database owns the schema; these statements are the manual_decisions operations the
// migration inventory attributes to manual-decisions-store.ts. Every list is explicit for the same
// positional-scan reason as store/database.
const (
	insertDecisionSQL = `INSERT INTO manual_decisions (
        document_id, checksum, original_filename, title,
        old_category, old_subcategory, new_category, new_subcategory,
        user_feedback_reason, raw_text_snippet, rule_keywords, enabled, created_at
      ) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	decisionColumnNames = "id, document_id, checksum, original_filename, title, old_category, " +
		"old_subcategory, new_category, new_subcategory, user_feedback_reason, raw_text_snippet, " +
		"rule_keywords, enabled, created_at"

	selectDecisionsSQL  = "SELECT " + decisionColumnNames + " FROM manual_decisions ORDER BY id DESC"
	selectDecisionBySQL = "SELECT " + decisionColumnNames + " FROM manual_decisions WHERE id = ?"

	updateDecisionSQL = `UPDATE manual_decisions SET
        new_category = ?, new_subcategory = ?, user_feedback_reason = ?, rule_keywords = ?, enabled = ?
      WHERE id = ?`

	deleteDecisionSQL = "DELETE FROM manual_decisions WHERE id = ?"
	clearDecisionsSQL = "DELETE FROM manual_decisions"
)

// DecisionDB is the narrow database surface this package needs from store/database. See the GAP
// note in the package comment: *sql.DB satisfies it today; store/database.Store does not yet.
type DecisionDB interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// Logger is the narrow logging surface this package uses (TS's logger.error / logger.info with the
// DECISION_REGISTRY module name). *logger.Logger satisfies it; a nil Logger discards.
type Logger interface {
	Info(module, message string, meta any, filename ...string)
	Error(module, message string, meta any, filename ...string)
}

type noopLogger struct{}

func (noopLogger) Info(string, string, any, ...string)  {}
func (noopLogger) Error(string, string, any, ...string) {}

// Record mirrors the TS `ManualDecisionRecord` (manual-decisions-store.ts:26-48). Field order
// matches the TS interface so the mirror JSON keys come out in the same order. RuleKeywords is the
// decoded array; Enabled is a pointer so an omitted value stays distinguishable from a disabled 0
// (semantic gap 1).
type Record struct {
	ID                 int64  `json:"id,omitempty"`
	DocumentID         int64  `json:"document_id"`
	Checksum           string `json:"checksum"`
	OriginalFilename   string `json:"original_filename"`
	Title              string `json:"title"`
	OldCategory        string `json:"old_category"`
	OldSubcategory     string `json:"old_subcategory"`
	NewCategory        string `json:"new_category"`
	NewSubcategory     string `json:"new_subcategory"`
	UserFeedbackReason string `json:"user_feedback_reason,omitempty"`
	RawTextSnippet     string `json:"raw_text_snippet"`
	// Keywords this decision teaches the AI to match on FUTURE documents (injected into the
	// {{USER_PRIORITY_RULES}} STEP 0 block, see decisionrule). Auto-derived at record time from the
	// filename/title; empty for legacy or non-distinctive records; editable in the Settings → Human
	// Decisions tab. Empty string in the raw DB row means "not yet derived".
	RuleKeywords []string `json:"rule_keywords"`
	// 1 = active (injected into prompts), 0 = kept in the log but no longer teaches the AI.
	Enabled   *int   `json:"enabled,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}

// Patch is the updateManualDecision input (manual-decisions-store.ts:225-233). A nil field means
// "not provided", so it keeps the existing value; a non-nil pointer to "" clears it.
type Patch struct {
	NewCategory        *string
	NewSubcategory     *string
	UserFeedbackReason *string
	RuleKeywords       *[]string
	Enabled            *int
}

// Options configure a Store. DecisionsFile replaces CONFIG.MANUAL_DECISIONS_FILE; Now replaces
// Date.now / new Date (test seam); Logger replaces the logger singleton.
type Options struct {
	DecisionsFile string
	Logger        Logger
	Now           func() time.Time
}

// Store owns the database handle, the mirror path and the mtime cache. Build it with New.
type Store struct {
	db            DecisionDB
	decisionsFile string
	logger        Logger
	now           func() time.Time

	mu        sync.Mutex
	syncCache *syncCacheEntry
}

type syncCacheEntry struct {
	mtime     time.Time
	decisions []Record
}

// New constructs a Store over db with the given options.
func New(db DecisionDB, opts Options) *Store {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = noopLogger{}
	}
	return &Store{
		db:            db,
		decisionsFile: opts.DecisionsFile,
		logger:        opts.Logger,
		now:           opts.Now,
	}
}

// decisionsFilePath ports manualDecisionsFilePath (manual-decisions-store.ts:16).
//
// settings.ts always sets MANUAL_DECISIONS_FILE to an absolute path under DATA_DIR, so in normal
// operation this simply returns it. The point is the failure mode it removes: the previous
// `CONFIG.MANUAL_DECISIONS_FILE || 'manual_decisions.json'` fell back to a RELATIVE path, which
// resolves against process.cwd(). Any caller holding an incomplete CONFIG — a test mocking
// settings.js without this key — therefore wrote silently into the repo root, appending synthetic
// entries to the user's real feedback log and making two unrelated test suites corrupt each other.
// Both call sites already catch and log, so throwing here degrades to a logged error rather than a
// write landing somewhere nobody is looking.
func (s *Store) decisionsFilePath() (string, error) {
	configured := s.decisionsFile
	if configured == "" || !filepath.IsAbs(configured) {
		return "", fmt.Errorf(
			"CONFIG.MANUAL_DECISIONS_FILE must be an absolute path, got %s",
			jsJSONStringifyString(configured),
		)
	}
	return configured, nil
}

// recordJSON is the lenient decode shape for one mirror record: RuleKeywords and Enabled are left
// as `any` so a legacy JSON string (instead of an array/number) survives normalize.
type recordJSON struct {
	ID                 int64  `json:"id"`
	DocumentID         int64  `json:"document_id"`
	Checksum           string `json:"checksum"`
	OriginalFilename   string `json:"original_filename"`
	Title              string `json:"title"`
	OldCategory        string `json:"old_category"`
	OldSubcategory     string `json:"old_subcategory"`
	NewCategory        string `json:"new_category"`
	NewSubcategory     string `json:"new_subcategory"`
	UserFeedbackReason string `json:"user_feedback_reason"`
	RawTextSnippet     string `json:"raw_text_snippet"`
	RuleKeywords       any    `json:"rule_keywords"`
	Enabled            any    `json:"enabled"`
	CreatedAt          string `json:"created_at"`
}

func (r recordJSON) toRecord() Record {
	return Record{
		ID:                 r.ID,
		DocumentID:         r.DocumentID,
		Checksum:           r.Checksum,
		OriginalFilename:   r.OriginalFilename,
		Title:              r.Title,
		OldCategory:        r.OldCategory,
		OldSubcategory:     r.OldSubcategory,
		NewCategory:        r.NewCategory,
		NewSubcategory:     r.NewSubcategory,
		UserFeedbackReason: r.UserFeedbackReason,
		RawTextSnippet:     r.RawTextSnippet,
		RuleKeywords:       normalizeRuleKeywords(r.RuleKeywords),
		Enabled:            intPtr(normalizeEnabled(r.Enabled)),
		CreatedAt:          r.CreatedAt,
	}
}

// normalizeRuleKeywords ports the rule_keywords half of normalizeDecisionRecord
// (manual-decisions-store.ts:50-72): an array passes through, a non-blank string is parsed as JSON
// (an array wins; a parse error falls back to a comma-separated list), anything else is empty.
func normalizeRuleKeywords(value any) []string {
	switch v := value.(type) {
	case nil:
		return []string{}
	case []string:
		return append([]string{}, v...)
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(v) == "" {
			return []string{}
		}
		var parsed any
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			if arr, ok := parsed.([]any); ok {
				out := make([]string, 0, len(arr))
				for _, e := range arr {
					if s, ok := e.(string); ok {
						out = append(out, s)
					}
				}
				return out
			}
			// A parse success that is not an array leaves the list empty (TS only split on a parse
			// error, never on "parsed but not an array").
			return []string{}
		}
		parts := strings.Split(v, ",")
		out := []string{}
		for _, p := range parts {
			if t := strings.TrimSpace(p); t != "" {
				out = append(out, t)
			}
		}
		return out
	default:
		return []string{}
	}
}

// normalizeEnabled ports the enabled half of normalizeDecisionRecord: only an exact 0 (number) or
// "0" (string) disables; NULL/undefined and everything else is active.
func normalizeEnabled(value any) int {
	switch v := value.(type) {
	case nil:
		return 1
	case sql.NullInt64:
		if !v.Valid {
			return 1
		}
		if v.Int64 == 0 {
			return 0
		}
		return 1
	case int:
		if v == 0 {
			return 0
		}
		return 1
	case int64:
		if v == 0 {
			return 0
		}
		return 1
	case float64:
		if v == 0 {
			return 0
		}
		return 1
	case string:
		if v == "0" {
			return 0
		}
		return 1
	default:
		return 1
	}
}

// appendToJSONFile ports appendToJsonFile (manual-decisions-store.ts:74-89): best-effort, appending
// one record to the mirror and never returning an error.
func (s *Store) appendToJSONFile(record Record) {
	targetFilePath, err := s.decisionsFilePath()
	if err != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to save manual_decisions.json:", err)
		return
	}
	decisions := s.readMirrorLenient(targetFilePath)
	decisions = append(decisions, record)
	if err := writeMirrorAtomic(targetFilePath, decisions); err != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to save manual_decisions.json:", err)
	}
}

// writeJSONFile ports writeJsonFile (manual-decisions-store.ts:91-99): rewrites the whole mirror,
// best-effort, logging "Failed to save" on any error.
func (s *Store) writeJSONFile(decisions []Record) {
	targetFilePath, err := s.decisionsFilePath()
	if err != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to save manual_decisions.json:", err)
		return
	}
	if err := writeMirrorAtomic(targetFilePath, decisions); err != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to save manual_decisions.json:", err)
	}
}

// readMirrorLenient is appendToJsonFile's read: any error (missing, unreadable, unparseable)
// yields an empty list and the caller still writes.
func (s *Store) readMirrorLenient(path string) []Record {
	decisions, err := readMirrorStrict(path)
	if err != nil {
		return []Record{}
	}
	return decisions
}

// readMirrorStrict parses the mirror for the update/delete paths: a missing file yields an empty
// list (TS existsSync), but a present-but-unreadable/unparseable file returns the error so the
// caller's outer catch runs without writing.
func readMirrorStrict(path string) ([]Record, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Record{}, nil
	}
	if err != nil {
		return nil, err
	}
	var raw []recordJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.toRecord())
	}
	return out, nil
}

// RecordManualDecision ports recordManualDecision (manual-decisions-store.ts:101-160). It is
// deliberately non-throwing: the DB insert and the mirror write are both best-effort, matching the
// TS `Promise<void>` contract and the fact that both call sites already catch and log.
func (s *Store) RecordManualDecision(record Record) {
	createdAt := record.CreatedAt
	if createdAt == "" {
		createdAt = s.now().UTC().Format(jsISOLayout)
	}
	rawSnippet := jsSubstring(record.RawTextSnippet, maxRawTextSnippet)

	// Auto-derive the keywords this decision teaches the AI, unless the caller already pinned them
	// (the Settings tab edit path always pins them; every other path derives them from the file's
	// own identity). A legacy record with no distinctive token simply ends up with an empty list
	// and stays visible-but-inactive in the tab until the user fills it in.
	var ruleKeywords []string
	if hasNonBlank(record.RuleKeywords) {
		for _, k := range record.RuleKeywords {
			if t := strings.TrimSpace(k); t != "" {
				ruleKeywords = append(ruleKeywords, t)
			}
		}
	} else {
		ruleKeywords = decisionrule.DeriveRuleKeywords(record.OriginalFilename, record.Title)
	}
	if ruleKeywords == nil {
		ruleKeywords = []string{}
	}
	enabled := 1
	if record.Enabled != nil && *record.Enabled == 0 {
		enabled = 0
	}

	// 1. Insert into SQLite Database
	var decisionID int64
	res, err := s.db.Exec(
		insertDecisionSQL,
		record.DocumentID,
		record.Checksum,
		record.OriginalFilename,
		record.Title,
		record.OldCategory,
		record.OldSubcategory,
		record.NewCategory,
		record.NewSubcategory,
		record.UserFeedbackReason,
		rawSnippet,
		mustJSON(ruleKeywords),
		enabled,
		createdAt,
	)
	inserted := err == nil
	if err != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to insert manual decision into DB:", err)
	} else if id, idErr := res.LastInsertId(); idErr == nil {
		decisionID = id
	}
	if inserted {
		teaching := ""
		if len(ruleKeywords) > 0 {
			teaching = " — will teach future runs on: " + strings.Join(ruleKeywords, ", ")
		}
		s.logger.Info("DECISION_REGISTRY", fmt.Sprintf(
			"Recorded manual move decision for doc ID %d (%s): %s/%s ➔ %s/%s%s",
			record.DocumentID, record.OriginalFilename,
			record.OldCategory, record.OldSubcategory,
			record.NewCategory, record.NewSubcategory, teaching,
		), nil)
	}

	// 2. Persist into manual_decisions.json (mirror). The DB id is stored on the JSON record too,
	// so the Settings tab can update/delete individual records in BOTH stores.
	mirrorRecord := record
	if decisionID != 0 {
		mirrorRecord.ID = decisionID
	}
	mirrorRecord.RuleKeywords = ruleKeywords
	mirrorRecord.Enabled = intPtr(enabled)
	mirrorRecord.RawTextSnippet = rawSnippet
	mirrorRecord.CreatedAt = createdAt
	s.appendToJSONFile(mirrorRecord)

	// Invalidate the sync-read cache explicitly — a second write within the same millisecond could
	// otherwise slip past the mtime check in ReadManualDecisionsSync().
	s.mu.Lock()
	s.syncCache = nil
	s.mu.Unlock()
}

// ReadManualDecisionsSync ports readManualDecisionsSync (manual-decisions-store.ts:162-192).
//
// Synchronous read of the JSON mirror — the ONLY reader allowed in a hot path. The prompt
// personalization store calls this on every prompt build (classify-document → prompt.ts), and that
// path is synchronous, so this cannot go through the async SQLite layer. The mirror is maintained
// on every write above, and an mtime cache keeps repeated reads cheap.
func (s *Store) ReadManualDecisionsSync() []Record {
	filePath, err := s.decisionsFilePath()
	if err != nil {
		return []Record{}
	}
	info, err := os.Stat(filePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.logger.Error("DECISION_REGISTRY", "Failed to read manual_decisions.json:", err)
		}
		return []Record{}
	}

	s.mu.Lock()
	if s.syncCache != nil && s.syncCache.mtime.Equal(info.ModTime()) {
		cached := s.syncCache.decisions
		s.mu.Unlock()
		return cached
	}
	s.mu.Unlock()

	data, err := os.ReadFile(filePath)
	if err != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to read manual_decisions.json:", err)
		return []Record{}
	}
	var raw []recordJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to read manual_decisions.json:", err)
		return []Record{}
	}
	// The mirror is append-ordered (oldest first); the API/DB returns newest first. Keep the two
	// consistent so decisionsToPriorityRules caps the SAME (most recent) decisions either way.
	decisions := make([]Record, 0, len(raw))
	for i := len(raw) - 1; i >= 0; i-- {
		decisions = append(decisions, raw[i].toRecord())
	}

	s.mu.Lock()
	s.syncCache = &syncCacheEntry{mtime: info.ModTime(), decisions: decisions}
	s.mu.Unlock()
	return decisions
}

// GetManualDecisions ports getManualDecisions (manual-decisions-store.ts:194-203). It does not
// throw: a database failure falls back to the JSON mirror (already newest-first).
func (s *Store) GetManualDecisions() []Record {
	rows, err := s.db.Query(selectDecisionsSQL)
	if err != nil {
		return s.ReadManualDecisionsSync()
	}
	defer rows.Close()

	records, err := scanDecisionList(rows)
	if err != nil {
		return s.ReadManualDecisionsSync()
	}
	return records
}

// UpdateManualDecision ports updateManualDecision (manual-decisions-store.ts:218-277).
//
// Updates an existing decision (edit target category/subcategory, reason, keywords, enabled flag).
// Applies to BOTH the SQLite row and the JSON mirror. Throws on DB failure so the HTTP route can
// surface a 500 — unlike recordManualDecision, this is a user-initiated edit, and a silent no-op
// would look like a successful save. Returns the updated record, or nil if no decision with that id
// exists.
func (s *Store) UpdateManualDecision(id int64, patch Patch) (*Record, error) {
	existing, err := s.getDecision(id)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	newCategory := existing.NewCategory
	if patch.NewCategory != nil {
		newCategory = strings.ToLower(strings.TrimSpace(*patch.NewCategory))
	}
	newSubcategory := existing.NewSubcategory
	if patch.NewSubcategory != nil {
		newSubcategory = strings.ToLower(strings.TrimSpace(*patch.NewSubcategory))
	}
	reason := existing.UserFeedbackReason
	if patch.UserFeedbackReason != nil {
		reason = *patch.UserFeedbackReason
	}
	ruleKeywords := existing.RuleKeywords
	if patch.RuleKeywords != nil {
		ruleKeywords = dedupTrim(*patch.RuleKeywords)
	}
	if ruleKeywords == nil {
		ruleKeywords = []string{}
	}
	enabled := 1
	if existing.Enabled != nil {
		enabled = *existing.Enabled
	}
	if patch.Enabled != nil {
		if *patch.Enabled != 0 {
			enabled = 1
		} else {
			enabled = 0
		}
	}

	if _, err := s.db.Exec(
		updateDecisionSQL,
		newCategory, newSubcategory, reason, mustJSON(ruleKeywords), enabled, id,
	); err != nil {
		return nil, err
	}

	updated := *existing
	updated.NewCategory = newCategory
	updated.NewSubcategory = newSubcategory
	updated.UserFeedbackReason = reason
	updated.RuleKeywords = ruleKeywords
	updated.Enabled = intPtr(enabled)

	if targetFilePath, pathErr := s.decisionsFilePath(); pathErr != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to update manual_decisions.json mirror:", pathErr)
	} else if decisions, readErr := readMirrorStrict(targetFilePath); readErr != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to update manual_decisions.json mirror:", readErr)
	} else {
		if idx := findJSONIndex(decisions, id, *existing); idx != -1 {
			decisions[idx] = updated
		}
		s.writeJSONFile(decisions)
	}

	s.mu.Lock()
	s.syncCache = nil
	s.mu.Unlock()
	return &updated, nil
}

// DeleteManualDecision ports deleteManualDecision (manual-decisions-store.ts:279-307).
//
// Deletes one decision from BOTH stores. Throws on DB failure (user-initiated op, must not be
// silent). Returns false when no decision with that id exists.
func (s *Store) DeleteManualDecision(id int64) (bool, error) {
	existing, err := s.getDecision(id)
	if err != nil {
		return false, err
	}
	if existing == nil {
		return false, nil
	}

	if _, err := s.db.Exec(deleteDecisionSQL, id); err != nil {
		return false, err
	}

	if targetFilePath, pathErr := s.decisionsFilePath(); pathErr != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to remove decision from manual_decisions.json mirror:", pathErr)
	} else if decisions, readErr := readMirrorStrict(targetFilePath); readErr != nil {
		s.logger.Error("DECISION_REGISTRY", "Failed to remove decision from manual_decisions.json mirror:", readErr)
	} else if idx := findJSONIndex(decisions, id, *existing); idx != -1 {
		decisions = append(decisions[:idx], decisions[idx+1:]...)
		s.writeJSONFile(decisions)
	}

	s.mu.Lock()
	s.syncCache = nil
	s.mu.Unlock()

	s.logger.Info("DECISION_REGISTRY", fmt.Sprintf(
		"Deleted manual decision #%d (%s) — it no longer teaches the AI",
		id, existing.OriginalFilename,
	), nil)
	return true, nil
}

// ClearManualDecisions ports clearManualDecisions (manual-decisions-store.ts:309-315): deletes
// every decision from BOTH stores (Settings → Human Decisions → Delete All). The DB delete is
// authoritative and returns its error; the mirror rewrite is best-effort.
func (s *Store) ClearManualDecisions() error {
	if _, err := s.db.Exec(clearDecisionsSQL); err != nil {
		return err
	}
	s.writeJSONFile([]Record{})
	s.mu.Lock()
	s.syncCache = nil
	s.mu.Unlock()
	return nil
}

// getDecision is `db.get('SELECT * FROM manual_decisions WHERE id = ?')`. nil means no row.
func (s *Store) getDecision(id int64) (*Record, error) {
	row := s.db.QueryRow(selectDecisionBySQL, id)
	rec, err := scanDecision(row.Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

func scanDecisionList(rows *sql.Rows) ([]Record, error) {
	records := []Record{}
	for rows.Next() {
		rec, err := scanDecision(rows.Scan)
		if err != nil {
			return nil, err
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

// scanDecision reads one row in decisionColumnNames order and normalizes it. go vet accepts
// rows.Scan / row.Scan because both are func(dest ...any) error.
func scanDecision(scan func(dest ...any) error) (Record, error) {
	var (
		id                                                          int64
		documentID                                                  sql.NullInt64
		checksum, originalFilename, title                           sql.NullString
		oldCategory, oldSubcategory, newCategory, newSubcategory    sql.NullString
		userFeedbackReason, rawTextSnippet, ruleKeywords, createdAt sql.NullString
		enabled                                                     sql.NullInt64
	)
	if err := scan(
		&id, &documentID, &checksum, &originalFilename, &title,
		&oldCategory, &oldSubcategory, &newCategory, &newSubcategory,
		&userFeedbackReason, &rawTextSnippet, &ruleKeywords, &enabled, &createdAt,
	); err != nil {
		return Record{}, err
	}

	var ruleKeywordsValue any
	if ruleKeywords.Valid {
		ruleKeywordsValue = ruleKeywords.String
	}

	return Record{
		ID:                 id,
		DocumentID:         documentID.Int64,
		Checksum:           checksum.String,
		OriginalFilename:   originalFilename.String,
		Title:              title.String,
		OldCategory:        oldCategory.String,
		OldSubcategory:     oldSubcategory.String,
		NewCategory:        newCategory.String,
		NewSubcategory:     newSubcategory.String,
		UserFeedbackReason: userFeedbackReason.String,
		RawTextSnippet:     rawTextSnippet.String,
		RuleKeywords:       normalizeRuleKeywords(ruleKeywordsValue),
		Enabled:            intPtr(normalizeEnabled(enabled)),
		CreatedAt:          createdAt.String,
	}, nil
}

// findJSONIndex ports findJsonIndex (manual-decisions-store.ts:205-216): match by id when known,
// falling back to the content key (document_id, checksum, created_at) for legacy rows without one.
func findJSONIndex(decisions []Record, id int64, match Record) int {
	if id != 0 {
		for i, d := range decisions {
			if d.ID == id {
				return i
			}
		}
	}
	for i, d := range decisions {
		if d.DocumentID == match.DocumentID &&
			d.Checksum == match.Checksum &&
			d.CreatedAt == match.CreatedAt {
			return i
		}
	}
	return -1
}

// writeMirrorAtomic serializes decisions as `JSON.stringify(decisions, null, 2)` and writes them
// through a temp file plus rename, matching infra/jsonregistry. See the package comment's
// deviation note.
func writeMirrorAtomic(target string, decisions []Record) error {
	payload, err := marshalIndentNoHTMLEscape(decisions)
	if err != nil {
		return err
	}
	tmpPath := target + ".tmp"
	if err := os.WriteFile(tmpPath, payload, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		if isPermOrBusy(err) {
			if copyErr := copyFileContents(tmpPath, target); copyErr != nil {
				return copyErr
			}
			_ = os.Remove(tmpPath)
			return nil
		}
		return err
	}
	return nil
}

// marshalIndentNoHTMLEscape is `JSON.stringify(data, null, 2)`: two-space indent and no HTML
// escaping (Go escapes <, > and & by default). The Encoder appends a newline that stringify does
// not, so it is trimmed.
func marshalIndentNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// copyFileContents is the fallback replacement for fs.copyFileSync.
func copyFileContents(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// mustJSON is JSON.stringify for the keywords column; the value is always a string slice.
func mustJSON(v []string) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}

// hasNonBlank is `Array.isArray(list) && list.some(k => k.trim())`.
func hasNonBlank(values []string) bool {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// dedupTrim is `Array.from(new Set(list.map(k => k.trim()).filter(Boolean)))`: trim, drop empties,
// keep first-seen order.
func dedupTrim(values []string) []string {
	out := []string{}
	seen := map[string]struct{}{}
	for _, v := range values {
		t := strings.TrimSpace(v)
		if t == "" {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// jsSubstring mirrors String.prototype.substring(0, max). JS offsets count UTF-16 code units, so
// that is what this counts; see semantic gap 4.
func jsSubstring(s string, max int) string {
	if max <= 0 {
		return ""
	}
	units := utf16.Encode([]rune(s))
	if len(units) <= max {
		return s
	}
	return string(utf16.Decode(units[:max]))
}

// jsJSONStringifyString renders the configured path the way JSON.stringify would in the guard's
// error message. Go has no undefined, so an empty value (the missing-key case in TS) is rendered
// as undefined rather than "".
func jsJSONStringifyString(s string) string {
	if s == "" {
		return "undefined"
	}
	return strconv.Quote(s)
}

func intPtr(v int) *int { return &v }
