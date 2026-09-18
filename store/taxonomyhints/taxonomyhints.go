// Package taxonomyhints is a Go port of pdf-triage's src/infrastructure/taxonomy-hints-store.ts
// (69 lines): MAX_TAXONOMY_HINTS (50), the null-safe absolute-path guard, recordTaxonomyHint
// (newest-first append, dedup, temp+rename atomic write, never throws) and readTaxonomyHintsSync
// (mtime-cached synchronous read).
//
// taxonomy-hints-store.ts has no upstream Vitest file, so there is no case-for-case port to
// reproduce. The ported behavior is pinned from the TS source: :14 (cap 50), :24-29 (newest-first,
// dedup on the four-field conflict key, never throws), :30-45 (temp+rename atomic write, cache
// invalidated) and :55-63 (mtime-cached sync read, filter on truthy mapped_category).
//
// The TaxonomyHintEntry type is imported from the already-ported top-level taxonomyconflicts package.
//
// The TS module reads CONFIG.TAXONOMY_HINTS_FILE and logs through ./logger. The settings port is a
// later phase and this package must not import it, so the path is an explicit Options field and the
// logger is a small ErrorLogFunc. ReadFile is injectable so the mtime-cache behavior stays
// observable; renameFile is a package seam for the EPERM/EBUSY fallback.
//
// WHY-comments from the TS source are preserved verbatim in the code below.
//
// Deviations, all resolved in favor of matching the TS behavior:
//
//  1. Config and logger are per-Store instead of module-global CONFIG/logger.
//  2. The atomic-write EPERM/EBUSY helper is duplicated from infra/jsonregistry (its isPermOrBusy is
//     unexported and that package may not be modified) with the same build-tagged unix/windows forms.
//  3. Cache keying uses FileInfo.ModTime().UnixNano() rather than fs.Stats.mtimeMs (a JS-float
//     millisecond value). Finer-grained but observably identical: any mtime change invalidates.
//  4. The read filter accepts only a non-empty string mapped_category, the JSON value TS's
//     `h.mapped_category` keeps for the realistic on-disk shape. A non-string truthy value (e.g. a
//     number), which TS would keep, is dropped here because TaxonomyHintEntry.MappedCategory is a Go
//     string.
//  5. A non-array JSON document reads as [] without logging, matching `Array.isArray(parsed) ? parsed
//     : []`; only a JSON.parse failure is logged.
//  6. recordTaxonomyHint never returns an error: a failed read, marshaling or write is logged and the
//     call still returns, because the classification hot path must not fail on a hint-file problem.
package taxonomyhints

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/taxonomyconflicts"
)

// Persists the HINT half of the "block then return a hint" loop (see
// domain/taxonomy-conflicts.ts): every time a duplicate category/subcategory creation is blocked,
// the conflict is recorded here (gitignored taxonomy_hints.json, alongside manual_decisions.json)
// and re-injected into the model's {{USER_PRIORITY_RULES}} STEP 0 block on every future run, so
// Qwen stops proposing the blocked slugs. Capped to the newest MAX_TAXONOMY_HINTS entries so a
// long-running archive cannot bloat the prompt.

// MaxTaxonomyHints is the TS `MAX_TAXONOMY_HINTS`.
const MaxTaxonomyHints = 50

// ErrorLogFunc reports a failure the way the TS `logger.error('TAXONOMY_GUARD', message, err)` did.
type ErrorLogFunc func(category, message string, err error)

// ReadFunc reads a whole file. Defaults to os.ReadFile; injectable for tests.
type ReadFunc func(path string) ([]byte, error)

// Options configures New. FilePath is required (and must be absolute when used). Logger defaults to
// a no-op; ReadFile defaults to os.ReadFile.
type Options struct {
	FilePath string
	Logger   ErrorLogFunc
	ReadFile ReadFunc
}

// Store owns the hint-file cache. Methods are safe for concurrent callers.
type Store struct {
	mu       sync.Mutex
	filePath string
	logger   ErrorLogFunc
	readFile ReadFunc

	cache *cacheEntry
}

type cacheEntry struct {
	mtimeNanos int64
	hints      []*taxonomyconflicts.TaxonomyHintEntry
}

// New builds a Store.
func New(opts Options) *Store {
	logger := opts.Logger
	if logger == nil {
		logger = func(string, string, error) {}
	}
	readFile := opts.ReadFile
	if readFile == nil {
		readFile = os.ReadFile
	}
	return &Store{filePath: opts.FilePath, logger: logger, readFile: readFile}
}

// hintsFilePath is the TS hintsFilePath(): the configured path, or an error when it is empty or not
// absolute. The TS message is preserved.
func (s *Store) hintsFilePath() (string, error) {
	configured := s.filePath
	if configured == "" || !filepath.IsAbs(configured) {
		return "", fmt.Errorf("CONFIG.TAXONOMY_HINTS_FILE must be an absolute path, got %q", configured)
	}
	return configured, nil
}

func (s *Store) logError(message string, err error) {
	s.logger("TAXONOMY_GUARD", message, err)
}

// RecordTaxonomyHint is `recordTaxonomyHint(entry)`.
//
// Appends one conflict hint (newest first) and trims the list to the cap. Never throws — the
// classification hot path must not fail because the hint file could not be written; a failed
// write degrades to a logged error and the block still happened. An identical conflict already
// in the list is not appended again — repeated blocks of the same slug must not fill the cap.
func (s *Store) RecordTaxonomyHint(entry *taxonomyconflicts.TaxonomyHintEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(entry)
}

func (s *Store) recordLocked(entry *taxonomyconflicts.TaxonomyHintEntry) {
	if entry == nil {
		// TS dereferenced a null entry and threw; its try/catch logged that and returned.
		s.logError("Failed to record taxonomy conflict hint:", fmt.Errorf("nil taxonomy hint entry"))
		return
	}
	targetFilePath, err := s.hintsFilePath()
	if err != nil {
		s.logError("Failed to record taxonomy conflict hint:", err)
		return
	}
	key := hintKey(entry)
	existing := s.readLocked()
	for _, e := range existing {
		if hintKey(e) == key {
			return
		}
	}
	created := *entry
	if created.CreatedAt == "" {
		created.CreatedAt = time.Now().UTC().Format("2006-01-02T15:04:05.000Z")
	}
	next := make([]*taxonomyconflicts.TaxonomyHintEntry, 0, len(existing)+1)
	next = append(next, &created)
	next = append(next, existing...)
	if len(next) > MaxTaxonomyHints {
		next = next[:MaxTaxonomyHints]
	}

	payload, err := marshalIndentNoHTMLEscape(next)
	if err != nil {
		s.logError("Failed to record taxonomy conflict hint:", err)
		return
	}
	if err := writeFileAtomic(targetFilePath, payload, 0o644); err != nil {
		s.logError("Failed to record taxonomy conflict hint:", err)
		return
	}
	s.cache = nil
}

// hintKey is the TS dedup key:
//
//	JSON.stringify([proposed_category || '', proposed_subcategory || '',
//		mapped_category, mapped_subcategory || '']).
func hintKey(entry *taxonomyconflicts.TaxonomyHintEntry) string {
	parts, err := json.Marshal([]string{
		entry.ProposedCategory,
		entry.ProposedSubcategory,
		entry.MappedCategory,
		entry.MappedSubcategory,
	})
	if err != nil {
		return ""
	}
	return string(parts)
}

// ReadTaxonomyHintsSync is `readTaxonomyHintsSync()`: the synchronous read of the hint list (newest
// first) — the prompt build path is synchronous. A read or parse failure is logged and yields [].
func (s *Store) ReadTaxonomyHintsSync() []*taxonomyconflicts.TaxonomyHintEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked()
}

func (s *Store) readLocked() []*taxonomyconflicts.TaxonomyHintEntry {
	targetFilePath, err := s.hintsFilePath()
	if err != nil {
		s.logError("Failed to read taxonomy conflict hints:", err)
		return []*taxonomyconflicts.TaxonomyHintEntry{}
	}
	info, statErr := os.Stat(targetFilePath)
	if statErr != nil {
		return []*taxonomyconflicts.TaxonomyHintEntry{}
	}
	mtimeNanos := info.ModTime().UnixNano()
	if s.cache != nil && s.cache.mtimeNanos == mtimeNanos {
		return s.cache.hints
	}
	raw, readErr := s.readFile(targetFilePath)
	if readErr != nil {
		s.logError("Failed to read taxonomy conflict hints:", readErr)
		return []*taxonomyconflicts.TaxonomyHintEntry{}
	}
	hints, parseErr := parseHints(raw)
	if parseErr != nil {
		s.logError("Failed to read taxonomy conflict hints:", parseErr)
		return []*taxonomyconflicts.TaxonomyHintEntry{}
	}
	s.cache = &cacheEntry{mtimeNanos: mtimeNanos, hints: hints}
	return hints
}

// parseHints is `(Array.isArray(parsed) ? parsed : []).filter(h => h && h.mapped_category)`.
func parseHints(raw []byte) ([]*taxonomyconflicts.TaxonomyHintEntry, error) {
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	arr, ok := parsed.([]any)
	if !ok {
		return []*taxonomyconflicts.TaxonomyHintEntry{}, nil
	}
	hints := make([]*taxonomyconflicts.TaxonomyHintEntry, 0, len(arr))
	for _, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if mapped, ok := obj["mapped_category"].(string); !ok || mapped == "" {
			continue
		}
		rawItem, err := json.Marshal(item)
		if err != nil {
			continue
		}
		var entry taxonomyconflicts.TaxonomyHintEntry
		if err := json.Unmarshal(rawItem, &entry); err != nil {
			continue
		}
		hints = append(hints, &entry)
	}
	return hints, nil
}
