// Package entitydictionary is a Go port of pdf-triage's src/infrastructure/entity-dictionary-store.ts
// (69 lines): the mtime/size-cached, schema-validated read of entity_dictionary.json.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/entity-dictionary-store.test.ts` -> 7 passed), so no upstream
// case is pinned red. All 7 cases are ported.
//
// The TS module reads CONFIG.ENTITY_DICTIONARY_FILE. The settings port is a later phase and this
// package must not import it, so the path is an explicit Options field; ReadFile is an injectable
// seam so the "does not re-read on a second call" case stays observable without spying on a global
// fs module.
//
// WHY-comments from the TS source are preserved verbatim in the code below.
//
// Deviations, all resolved in favor of matching the TS acceptance bar:
//
//  1. The path is per-Store instead of CONFIG, and the read is injectable, so no test mocks fs.
//  2. Go has no object identity for a struct return. The cache still stores the parsed value and
//     GetEntityDictionary copies only the struct header, so the parsed element pointers (and the
//     backing arrays) are shared between calls — the observable equivalent of the TS `toBe` case.
//  3. The TS cache keys on fs.Stats.mtimeMs (a JS float of milliseconds). This port keys on
//     FileInfo.ModTime().UnixNano(), which is finer-grained but identical in observable behavior
//     (an edit that changes mtime invalidates the cache).
//  4. TS probes existence with fs.existsSync and then fs.statSync separately, so a mocked or exotic
//     filesystem where stat is unavailable re-reads. Go's os.Stat serves as both probes; a stat
//     failure is treated as "not exists" (fs.existsSync's own behavior for any error).
//  5. A malformed file reports the TS console.error text to Stderr and is never cached, so the next
//     call retries; the empty fallback is documentschema.ParseEntityDictionary("{}") and has non-nil
//     empty domain slices, exactly as the Zod defaults do.
package entitydictionary

import (
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
)

// ReadFunc reads a whole file. Defaults to os.ReadFile; injectable for tests.
type ReadFunc func(path string) ([]byte, error)

// Options configures New. FilePath is required. Stderr receives the invalid-schema error (TS
// console.error); nil defaults to os.Stderr. ReadFile defaults to os.ReadFile.
type Options struct {
	FilePath string
	Stderr   io.Writer
	ReadFile ReadFunc
}

// Store owns the parsed dictionary cache. Methods are safe for concurrent callers.
type Store struct {
	mu       sync.Mutex
	filePath string
	stderr   io.Writer
	readFile ReadFunc

	cache *cacheEntry
}

// entity_dictionary.json is ~145KB of curated entities and nothing in the app ever writes it, yet
// getEntityDictionary() used to re-read, JSON.parse and Zod-validate the whole file on EVERY call.
// That is not a once-per-run cost: classification-resolution.ts calls it per ungrounded subcategory
// and repair-registry.ts calls it inside its per-document loop, so a 258-document repair paid the
// full parse 258 times (~6-7ms each, measured; ~0.02ms once cached).
//
// The existence probe stays fs.existsSync on purpose. Several suites auto-mock the whole fs module
// and only stub existsSync/readFileSync; probing with statSync instead made those mocks return
// undefined and silently yielded an empty dictionary, which flipped a real classification assertion
// (France Travail pay slip -> 'employeur' instead of 'france_travail'). stat is therefore used only
// to validate the cache, and when it is unavailable the code simply re-reads — the exact behavior
// this function had before the cache existed.
type cacheEntry struct {
	path       string
	mtimeNanos int64
	size       int64
	value      documentschema.EntityDictionary
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
	return &Store{filePath: opts.FilePath, stderr: stderr, readFile: readFile}
}

// ClearCache is `clearEntityDictionaryCache()`: exposed for tests and for any future hot-reload
// endpoint that needs to force a re-read.
func (s *Store) ClearCache() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cache = nil
}

// GetEntityDictionary is `getEntityDictionary()`.
func (s *Store) GetEntityDictionary() documentschema.EntityDictionary {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked()
}

func (s *Store) getLocked() documentschema.EntityDictionary {
	filePath := s.filePath

	if info, err := os.Stat(filePath); err == nil {
		// Keyed on path + mtime + size so a dictionary edited while the server runs is picked up on the
		// next call. No stat (mocked fs, exotic filesystem) simply means "cannot prove the cache is
		// still valid", so fall through and re-read rather than serve something possibly stale.
		mtimeNanos := info.ModTime().UnixNano()
		size := info.Size()
		if s.cache != nil && s.cache.path == filePath && s.cache.mtimeNanos == mtimeNanos && s.cache.size == size {
			return s.cache.value
		}

		raw, readErr := s.readFile(filePath)
		if readErr == nil {
			value, parseErr := documentschema.ParseEntityDictionary(raw)
			if parseErr == nil {
				// A malformed file is deliberately never cached: it is usually a half-written save, and the
				// next call should retry rather than serve an empty dictionary for the process's lifetime.
				s.cache = &cacheEntry{path: filePath, mtimeNanos: mtimeNanos, size: size, value: value}
				return value
			}
			readErr = parseErr
		}
		fmt.Fprintf(s.stderr, "Invalid entity_dictionary.json schema, using empty dictionary %v\n", readErr)
		s.cache = nil
	} else {
		s.cache = nil
	}

	empty, _ := documentschema.ParseEntityDictionary([]byte("{}"))
	return empty
}
