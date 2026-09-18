package taxonomyhints

// taxonomy-hints-store.ts has no upstream Vitest file (there is no
// src/infrastructure/taxonomy-hints-store.test.ts), so these cases are not a port: they pin the
// behaviors the TS implementation documents at src/infrastructure/taxonomy-hints-store.ts:14
// (MAX_TAXONOMY_HINTS = 50), :24-29 (newest-first, dedup, never throws), :30-45 (temp+rename atomic
// write) and :55-63 (mtime-cached sync read). Tests use t.TempDir() only and never the real
// taxonomy_hints.json.

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/taxonomyconflicts"
)

func validFile(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "taxonomy_hints.json")
}

func writeHintsFile(t *testing.T, path string, raw string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func hint(proposedCat, proposedSub, mappedCat, mappedSub string) *taxonomyconflicts.TaxonomyHintEntry {
	return &taxonomyconflicts.TaxonomyHintEntry{
		ProposedCategory:    proposedCat,
		ProposedSubcategory: proposedSub,
		MappedCategory:      mappedCat,
		MappedSubcategory:   mappedSub,
		Hint:                "conflict",
	}
}

type logCapture struct {
	mu      sync.Mutex
	entries []string
}

func (c *logCapture) log(category, message string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, fmt.Sprintf("%s|%s|%v", category, message, err))
}

func (c *logCapture) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

func TestRecordTaxonomyHint(t *testing.T) {
	t.Run("appends the newest entry first and fills created_at", func(t *testing.T) {
		file := validFile(t)
		store := New(Options{FilePath: file})

		store.RecordTaxonomyHint(hint("cat_a", "sub_a", "bank", "generic_bank"))
		store.RecordTaxonomyHint(hint("cat_b", "sub_b", "bank", "credit_mutuel"))

		got := store.ReadTaxonomyHintsSync()
		if len(got) != 2 {
			t.Fatalf("expected 2 hints, got %d", len(got))
		}
		if got[0].ProposedCategory != "cat_b" || got[1].ProposedCategory != "cat_a" {
			t.Fatalf("expected newest first, got %q then %q", got[0].ProposedCategory, got[1].ProposedCategory)
		}
		if got[0].CreatedAt == "" {
			t.Fatal("expected created_at to be filled")
		}
	})

	t.Run("does not append an identical conflict again", func(t *testing.T) {
		file := validFile(t)
		store := New(Options{FilePath: file})

		store.RecordTaxonomyHint(hint("cat_a", "sub_a", "bank", "generic_bank"))
		store.RecordTaxonomyHint(hint("cat_a", "sub_a", "bank", "generic_bank"))

		if got := store.ReadTaxonomyHintsSync(); len(got) != 1 {
			t.Fatalf("expected the duplicate to be dropped, got %d", len(got))
		}
	})

	t.Run("dedups on the four conflict fields, ignoring the hint text", func(t *testing.T) {
		file := validFile(t)
		store := New(Options{FilePath: file})

		first := hint("cat_a", "sub_a", "bank", "generic_bank")
		second := hint("cat_a", "sub_a", "bank", "generic_bank")
		second.Hint = "another explanation"
		store.RecordTaxonomyHint(first)
		store.RecordTaxonomyHint(second)

		// The TS key is [proposed_category, proposed_subcategory, mapped_category, mapped_subcategory],
		// so a differing hint text is the same conflict.
		if got := store.ReadTaxonomyHintsSync(); len(got) != 1 {
			t.Fatalf("expected the same conflict once, got %d", len(got))
		}
	})

	t.Run("keeps entries that differ in the mapped fields", func(t *testing.T) {
		file := validFile(t)
		store := New(Options{FilePath: file})

		store.RecordTaxonomyHint(hint("cat_a", "sub_a", "bank", "generic_bank"))
		store.RecordTaxonomyHint(hint("cat_a", "sub_a", "bank", "credit_mutuel"))

		if got := store.ReadTaxonomyHintsSync(); len(got) != 2 {
			t.Fatalf("expected 2 distinct hints, got %d", len(got))
		}
	})

	t.Run("caps the list at the newest 50 entries", func(t *testing.T) {
		file := validFile(t)
		store := New(Options{FilePath: file})

		for i := 0; i < MaxTaxonomyHints+5; i++ {
			store.RecordTaxonomyHint(hint(fmt.Sprintf("cat_%02d", i), "", "bank", "generic_bank"))
		}

		got := store.ReadTaxonomyHintsSync()
		if len(got) != MaxTaxonomyHints {
			t.Fatalf("expected cap %d, got %d", MaxTaxonomyHints, len(got))
		}
		if got[0].ProposedCategory != "cat_54" {
			t.Fatalf("expected newest first (cat_54), got %q", got[0].ProposedCategory)
		}
		if got[len(got)-1].ProposedCategory != "cat_05" {
			t.Fatalf("expected oldest kept cat_05, got %q", got[len(got)-1].ProposedCategory)
		}
	})

	t.Run("writes no .tmp file behind on success", func(t *testing.T) {
		file := validFile(t)
		store := New(Options{FilePath: file})
		store.RecordTaxonomyHint(hint("cat_a", "", "bank", "generic_bank"))

		if _, err := os.Stat(file + ".tmp"); !os.IsNotExist(err) {
			t.Fatalf("expected no %s.tmp, stat err = %v", file, err)
		}
	})
}

func TestReadTaxonomyHintsSync(t *testing.T) {
	t.Run("returns an empty list when the file does not exist", func(t *testing.T) {
		store := New(Options{FilePath: validFile(t)})
		if got := store.ReadTaxonomyHintsSync(); len(got) != 0 {
			t.Fatalf("expected empty, got %d", len(got))
		}
	})

	t.Run("returns an empty list and logs when the file is malformed", func(t *testing.T) {
		file := validFile(t)
		writeHintsFile(t, file, "{not valid json")
		capture := &logCapture{}
		store := New(Options{FilePath: file, Logger: capture.log})

		if got := store.ReadTaxonomyHintsSync(); len(got) != 0 {
			t.Fatalf("expected empty, got %d", len(got))
		}
		if capture.count() != 1 {
			t.Fatalf("expected one logged error, got %d", capture.count())
		}
	})

	t.Run("treats a non-array document as empty", func(t *testing.T) {
		file := validFile(t)
		writeHintsFile(t, file, `{"mapped_category":"bank"}`)
		store := New(Options{FilePath: file})
		if got := store.ReadTaxonomyHintsSync(); len(got) != 0 {
			t.Fatalf("expected empty, got %d", len(got))
		}
	})

	t.Run("keeps only entries with a truthy mapped_category", func(t *testing.T) {
		file := validFile(t)
		writeHintsFile(t, file, `[
			{"mapped_category":"bank","hint":"keep"},
			{"hint":"no mapped category"},
			{"proposed_category":"only proposed"},
			null,
			42,
			{"mapped_category":""},
			{"mapped_category":"invoices","hint":"also keep"}
		]`)
		store := New(Options{FilePath: file})

		got := store.ReadTaxonomyHintsSync()
		if len(got) != 2 {
			t.Fatalf("expected 2 hints, got %d: %+v", len(got), got)
		}
		if got[0].MappedCategory != "bank" || got[1].MappedCategory != "invoices" {
			t.Fatalf("unexpected hints: %+v", got)
		}
	})

	t.Run("caches by mtime and does not re-read an unchanged file", func(t *testing.T) {
		file := validFile(t)
		writeHintsFile(t, file, `[{"mapped_category":"bank","hint":"x"}]`)
		reads := 0
		store := New(Options{FilePath: file, ReadFile: func(path string) ([]byte, error) {
			reads++
			return os.ReadFile(path)
		}})

		_ = store.ReadTaxonomyHintsSync()
		_ = store.ReadTaxonomyHintsSync()
		if reads != 1 {
			t.Fatalf("expected 1 read, got %d", reads)
		}
	})

	t.Run("sees a hint recorded after a read", func(t *testing.T) {
		file := validFile(t)
		writeHintsFile(t, file, `[{"mapped_category":"bank","hint":"old"}]`)
		store := New(Options{FilePath: file})

		if got := store.ReadTaxonomyHintsSync(); len(got) != 1 {
			t.Fatalf("expected 1 initial hint, got %d", len(got))
		}
		store.RecordTaxonomyHint(hint("cat_new", "", "bank", "generic_bank"))
		if got := store.ReadTaxonomyHintsSync(); len(got) != 2 {
			t.Fatalf("expected the cache to be invalidated by the write, got %d", len(got))
		}
	})
}

func TestTaxonomyHintsAtomicFallback(t *testing.T) {
	file := validFile(t)
	store := New(Options{FilePath: file})
	store.RecordTaxonomyHint(hint("cat_a", "", "bank", "generic_bank"))

	original := renameFile
	renameFile = func(oldpath, newpath string) error { return fs.ErrPermission }
	defer func() { renameFile = original }()

	// A new, distinct conflict triggers a second write; the rename fails with an EPERM-family error
	// and the copy fallback must still land the file.
	store.RecordTaxonomyHint(hint("cat_b", "", "bank", "credit_mutuel"))

	got := store.ReadTaxonomyHintsSync()
	if len(got) != 2 || got[0].MappedSubcategory != "credit_mutuel" {
		t.Fatalf("expected the fallback write to persist, got %+v", got)
	}
	if _, err := os.Stat(file + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("expected the .tmp file to be cleaned up, stat err = %v", err)
	}
}

func TestTaxonomyHintsRejectsRelativePathWithoutThrowing(t *testing.T) {
	capture := &logCapture{}
	store := New(Options{FilePath: "relative_taxonomy_hints.json", Logger: capture.log})

	// Record must never throw: the classification hot path must not fail because the hint file
	// could not be written.
	store.RecordTaxonomyHint(hint("cat_a", "", "bank", "generic_bank"))
	if got := store.ReadTaxonomyHintsSync(); len(got) != 0 {
		t.Fatalf("expected empty on a non-absolute path, got %d", len(got))
	}
	if capture.count() != 2 {
		t.Fatalf("expected both operations to log, got %d", capture.count())
	}
}

func TestTaxonomyHintsConcurrentUse(t *testing.T) {
	file := validFile(t)
	store := New(Options{FilePath: file})

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			store.RecordTaxonomyHint(hint(fmt.Sprintf("cat_%d", i), "", "bank", "generic_bank"))
			_ = store.ReadTaxonomyHintsSync()
		}(i)
	}
	wg.Wait()

	got := store.ReadTaxonomyHintsSync()
	if len(got) == 0 || len(got) > MaxTaxonomyHints {
		t.Fatalf("unexpected hint count %d", len(got))
	}
}

func TestTaxonomyHintsJSONShape(t *testing.T) {
	file := validFile(t)
	store := New(Options{FilePath: file})
	entry := hint("cat_a", "sub_a", "bank", "generic_bank")
	entry.CreatedAt = "2026-01-01T00:00:00.000Z"
	store.RecordTaxonomyHint(entry)

	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var parsed []map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("on-disk hints are not a JSON array: %v", err)
	}
	if len(parsed) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(parsed))
	}
	if parsed[0]["mapped_category"] != "bank" || parsed[0]["created_at"] != "2026-01-01T00:00:00.000Z" {
		t.Fatalf("unexpected entry: %+v", parsed[0])
	}
}
