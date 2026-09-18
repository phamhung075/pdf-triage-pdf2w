package entitydictionary

// These cases are ported case-for-case from pdf-triage's
// src/infrastructure/entity-dictionary-store.test.ts (7 cases across the getEntityDictionary and
// getEntityDictionary caching describes). The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/entity-dictionary-store.test.ts` -> 7 passed), so no upstream
// case is pinned red.
//
// The TS suite mocks ./settings.js so CONFIG.ENTITY_DICTIONARY_FILE points at a temp dir, and spies
// on fs.readFileSync to prove the cache. This port takes the path explicitly and accepts an injected
// ReadFunc so the "does not re-read" case stays observable; tests use t.TempDir() only.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
)

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func newTestStore(t *testing.T, filePath string, readFile ReadFunc) *Store {
	t.Helper()
	if readFile == nil {
		readFile = os.ReadFile
	}
	return New(Options{FilePath: filePath, Stderr: io.Discard, ReadFile: readFile})
}

func emptyDictionary() documentschema.EntityDictionary {
	return documentschema.EntityDictionary{
		Banks:     []*documentschema.EntityItem{},
		Energy:    []*documentschema.EntityItem{},
		Telecom:   []*documentschema.EntityItem{},
		Insurance: []*documentschema.EntityItem{},
		Gov:       []*documentschema.EntityItem{},
		Health:    []*documentschema.EntityItem{},
	}
}

func TestGetEntityDictionary(t *testing.T) {
	t.Run("returns an empty (schema-defaulted) dictionary when entity_dictionary.json does not exist", func(t *testing.T) {
		store := newTestStore(t, filepath.Join(t.TempDir(), "entity_dictionary.json"), nil)

		got := store.GetEntityDictionary()
		assertEmptyDictionary(t, got)
	})

	t.Run("returns the parsed content when the file exists and is valid", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "entity_dictionary.json")
		writeJSON(t, file, map[string]any{
			"banks": []any{map[string]any{"slug": "credit_mutuel", "name": "Credit Mutuel", "aliases": []string{"cm"}}},
		})
		store := newTestStore(t, file, nil)

		got := store.GetEntityDictionary()
		if len(got.Banks) != 1 {
			t.Fatalf("expected 1 bank, got %d", len(got.Banks))
		}
		bank := got.Banks[0]
		if bank.Slug != "credit_mutuel" || bank.Name != "Credit Mutuel" {
			t.Fatalf("unexpected bank: %+v", bank)
		}
		if len(bank.Aliases) != 1 || bank.Aliases[0] != "cm" {
			t.Fatalf("unexpected aliases: %v", bank.Aliases)
		}
		if len(got.Energy) != 0 {
			t.Fatalf("expected empty energy default, got %d", len(got.Energy))
		}
	})

	t.Run("falls back to an empty dictionary when the file contains malformed JSON", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "entity_dictionary.json")
		if err := os.WriteFile(file, []byte("{not valid json"), 0o644); err != nil {
			t.Fatal(err)
		}
		store := newTestStore(t, file, nil)

		assertEmptyDictionary(t, store.GetEntityDictionary())
	})

	t.Run("falls back to an empty dictionary when an entity entry fails schema validation", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "entity_dictionary.json")
		writeJSON(t, file, map[string]any{"banks": []any{map[string]any{"name": "Missing slug field"}}})
		store := newTestStore(t, file, nil)

		assertEmptyDictionary(t, store.GetEntityDictionary())
	})
}

func TestGetEntityDictionaryCaching(t *testing.T) {
	t.Run("does not re-read the file on a second call when nothing changed", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "entity_dictionary.json")
		writeJSON(t, file, map[string]any{
			"banks": []any{map[string]any{"slug": "bnp", "name": "BNP", "aliases": []string{}}},
		})
		reads := 0
		store := newTestStore(t, file, func(path string) ([]byte, error) {
			reads++
			return os.ReadFile(path)
		})

		first := store.GetEntityDictionary()
		second := store.GetEntityDictionary()

		if reads != 1 {
			t.Fatalf("expected 1 read (second call served from cache), got %d", reads)
		}
		// Same object identity, not a re-parse: the returned struct copies the slice header, so the
		// parsed element pointers are shared.
		if first.Banks[0] != second.Banks[0] {
			t.Fatal("expected the cached parse to be reused, got a fresh object")
		}
	})

	t.Run("picks up an edit to the dictionary made while the process is running", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "entity_dictionary.json")
		writeJSON(t, file, map[string]any{
			"banks": []any{map[string]any{"slug": "bnp", "name": "BNP", "aliases": []string{}}},
		})
		store := newTestStore(t, file, nil)
		if got := bankSlugs(store.GetEntityDictionary()); len(got) != 1 || got[0] != "bnp" {
			t.Fatalf("expected [bnp], got %v", got)
		}

		// Rewrite with different content AND bump mtime, the way an editor save would.
		writeJSON(t, file, map[string]any{
			"banks": []any{
				map[string]any{"slug": "bnp", "name": "BNP", "aliases": []string{}},
				map[string]any{"slug": "sg", "name": "Societe Generale", "aliases": []string{}},
			},
		})
		future := time.Now().Add(5 * time.Second)
		if err := os.Chtimes(file, future, future); err != nil {
			t.Fatal(err)
		}

		if got := bankSlugs(store.GetEntityDictionary()); len(got) != 2 || got[0] != "bnp" || got[1] != "sg" {
			t.Fatalf("expected [bnp sg], got %v", got)
		}
	})

	t.Run("retries instead of caching the empty fallback when the file is malformed", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "entity_dictionary.json")
		if err := os.WriteFile(file, []byte("{half-written"), 0o644); err != nil {
			t.Fatal(err)
		}
		store := newTestStore(t, file, nil)
		if got := store.GetEntityDictionary().Banks; len(got) != 0 {
			t.Fatalf("expected empty banks on malformed file, got %d", len(got))
		}

		// The save completes; the very next call must see the good content.
		writeJSON(t, file, map[string]any{
			"banks": []any{map[string]any{"slug": "lcl", "name": "LCL", "aliases": []string{}}},
		})
		if got := bankSlugs(store.GetEntityDictionary()); len(got) != 1 || got[0] != "lcl" {
			t.Fatalf("expected [lcl], got %v", got)
		}
	})
}

func assertEmptyDictionary(t *testing.T, got documentschema.EntityDictionary) {
	t.Helper()
	if len(got.Banks) != 0 || len(got.Energy) != 0 || len(got.Telecom) != 0 ||
		len(got.Insurance) != 0 || len(got.Gov) != 0 || len(got.Health) != 0 ||
		got.Banks == nil || got.Energy == nil || got.Telecom == nil ||
		got.Insurance == nil || got.Gov == nil || got.Health == nil {
		t.Fatalf("expected an empty dictionary with non-nil domains, got %+v", got)
	}
}

func bankSlugs(dict documentschema.EntityDictionary) []string {
	out := make([]string, 0, len(dict.Banks))
	for _, b := range dict.Banks {
		out = append(out, b.Slug)
	}
	return out
}
