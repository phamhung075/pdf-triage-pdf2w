package categories

// These cases are ported case-for-case from pdf-triage's
// src/infrastructure/categories-store.test.ts (15 cases across the getCategoriesConfig and
// saveCategoriesConfig describes). The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/categories-store.test.ts` -> 15 passed), so no upstream case
// is pinned red.
//
// The TS suite mocks ./settings.js so CONFIG.CATEGORIES_FILE / CATEGORIES_PRIVATE_FILE point at a
// temp dir; this port passes both paths explicitly to New and therefore never touches the real
// project's categories.json / .categories.private.json.

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"

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

func category(id, name string, subs ...*documentschema.SubcategoryItem) *documentschema.CategoryItem {
	if subs == nil {
		subs = []*documentschema.SubcategoryItem{}
	}
	return &documentschema.CategoryItem{
		ID:            id,
		Name:          name,
		Aliases:       []string{},
		Subcategories: subs,
	}
}

func subcategory(id, name string) *documentschema.SubcategoryItem {
	return &documentschema.SubcategoryItem{
		ID:            id,
		Name:          name,
		Aliases:       []string{},
		Subcategories: []*documentschema.SubcategoryItem{},
	}
}

func ids(cats []*documentschema.CategoryItem) []string {
	out := make([]string, 0, len(cats))
	for _, c := range cats {
		out = append(out, c.ID)
	}
	return out
}

func subIDs(subs []*documentschema.SubcategoryItem) []string {
	out := make([]string, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.ID)
	}
	return out
}

func categoriesConfigJSON(cats ...*documentschema.CategoryItem) map[string]any {
	return map[string]any{"categories": cats}
}

func readCategoriesFile(t *testing.T, path string) struct {
	Categories []*documentschema.CategoryItem `json:"categories"`
} {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	var got struct {
		Categories []*documentschema.CategoryItem `json:"categories"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal(%s): %v", path, err)
	}
	return got
}

func TestGetCategoriesConfig(t *testing.T) {
	t.Run("returns the built-in default categories when neither file exists", func(t *testing.T) {
		dir := t.TempDir()
		store := New(filepath.Join(dir, "categories.json"), filepath.Join(dir, ".categories.private.json"), io.Discard)

		config := store.GetCategoriesConfig()
		if len(config.Categories) == 0 {
			t.Fatal("expected at least one default category")
		}
		if !containsID(config.Categories, "invoices") {
			t.Fatalf("expected the built-in defaults to contain 'invoices', got %v", ids(config.Categories))
		}
	})

	t.Run("returns the parsed content when categories.json exists and is valid, with no private file", func(t *testing.T) {
		dir := t.TempDir()
		publicFile := filepath.Join(dir, "categories.json")
		writeJSON(t, publicFile, categoriesConfigJSON(category("custom", "Custom")))
		store := New(publicFile, filepath.Join(dir, ".categories.private.json"), io.Discard)

		config := store.GetCategoriesConfig()
		if len(config.Categories) != 1 {
			t.Fatalf("expected exactly one category, got %v", ids(config.Categories))
		}
		got := config.Categories[0]
		if got.ID != "custom" || got.Name != "Custom" || got.Description != "" {
			t.Fatalf("unexpected category: %+v", got)
		}
		if len(got.Aliases) != 0 {
			t.Fatalf("expected empty aliases, got %v", got.Aliases)
		}
		if len(got.Subcategories) != 0 {
			t.Fatalf("expected empty subcategories, got %v", subIDs(got.Subcategories))
		}
	})

	t.Run("falls back to defaults when categories.json contains malformed JSON", func(t *testing.T) {
		dir := t.TempDir()
		publicFile := filepath.Join(dir, "categories.json")
		if err := os.WriteFile(publicFile, []byte("{not valid json"), 0o644); err != nil {
			t.Fatal(err)
		}
		store := New(publicFile, filepath.Join(dir, ".categories.private.json"), io.Discard)

		config := store.GetCategoriesConfig()
		if !containsID(config.Categories, "invoices") {
			t.Fatalf("expected defaults, got %v", ids(config.Categories))
		}
	})

	t.Run("falls back to defaults when categories.json fails schema validation", func(t *testing.T) {
		dir := t.TempDir()
		publicFile := filepath.Join(dir, "categories.json")
		writeJSON(t, publicFile, map[string]any{"categories": []any{map[string]any{"name": "Missing id field"}}})
		store := New(publicFile, filepath.Join(dir, ".categories.private.json"), io.Discard)

		config := store.GetCategoriesConfig()
		if !containsID(config.Categories, "invoices") {
			t.Fatalf("expected defaults, got %v", ids(config.Categories))
		}
	})

	t.Run("adds a category that exists only in the private overlay", func(t *testing.T) {
		dir := t.TempDir()
		publicFile := filepath.Join(dir, "categories.json")
		privateFile := filepath.Join(dir, ".categories.private.json")
		writeJSON(t, publicFile, categoriesConfigJSON(category("invoices", "Factures")))
		writeJSON(t, privateFile, categoriesConfigJSON(category("bank", "Banque", subcategory("credit_mutuel", "Credit Mutuel"))))
		store := New(publicFile, privateFile, io.Discard)

		config := store.GetCategoriesConfig()
		got := ids(config.Categories)
		sort.Strings(got)
		if len(got) != 2 || got[0] != "bank" || got[1] != "invoices" {
			t.Fatalf("expected [bank invoices], got %v", got)
		}
		var bank *documentschema.CategoryItem
		for _, c := range config.Categories {
			if c.ID == "bank" {
				bank = c
			}
		}
		if bank == nil {
			t.Fatal("bank category missing")
		}
		if gotSubs := subIDs(bank.Subcategories); len(gotSubs) != 1 || gotSubs[0] != "credit_mutuel" {
			t.Fatalf("expected [credit_mutuel], got %v", gotSubs)
		}
	})

	t.Run("merges private subcategories into a category that already exists in the public file, without duplicating", func(t *testing.T) {
		dir := t.TempDir()
		publicFile := filepath.Join(dir, "categories.json")
		privateFile := filepath.Join(dir, ".categories.private.json")
		writeJSON(t, publicFile, categoriesConfigJSON(category("bank", "Banque", subcategory("generic_bank", "Generic Bank"))))
		writeJSON(t, privateFile, categoriesConfigJSON(category("bank", "Banque", subcategory("credit_mutuel", "Credit Mutuel"))))
		store := New(publicFile, privateFile, io.Discard)

		config := store.GetCategoriesConfig()
		if len(config.Categories) != 1 {
			t.Fatalf("expected 1 category, got %d", len(config.Categories))
		}
		got := subIDs(config.Categories[0].Subcategories)
		sort.Strings(got)
		if len(got) != 2 || got[0] != "credit_mutuel" || got[1] != "generic_bank" {
			t.Fatalf("expected [credit_mutuel generic_bank], got %v", got)
		}
	})

	t.Run("does not duplicate a subcategory that already exists in both files", func(t *testing.T) {
		dir := t.TempDir()
		publicFile := filepath.Join(dir, "categories.json")
		privateFile := filepath.Join(dir, ".categories.private.json")
		writeJSON(t, publicFile, categoriesConfigJSON(category("bank", "Banque", subcategory("credit_mutuel", "Credit Mutuel"))))
		writeJSON(t, privateFile, categoriesConfigJSON(category("bank", "Banque", subcategory("credit_mutuel", "Credit Mutuel"))))
		store := New(publicFile, privateFile, io.Discard)

		config := store.GetCategoriesConfig()
		if len(config.Categories[0].Subcategories) != 1 {
			t.Fatalf("expected 1 subcategory, got %v", subIDs(config.Categories[0].Subcategories))
		}
	})

	t.Run("ignores a private file with malformed JSON and still returns the public categories", func(t *testing.T) {
		dir := t.TempDir()
		publicFile := filepath.Join(dir, "categories.json")
		privateFile := filepath.Join(dir, ".categories.private.json")
		writeJSON(t, publicFile, categoriesConfigJSON(category("invoices", "Factures")))
		if err := os.WriteFile(privateFile, []byte("{not valid json"), 0o644); err != nil {
			t.Fatal(err)
		}
		store := New(publicFile, privateFile, io.Discard)

		config := store.GetCategoriesConfig()
		if got := ids(config.Categories); len(got) != 1 || got[0] != "invoices" {
			t.Fatalf("expected [invoices], got %v", got)
		}
	})
}

func TestSaveCategoriesConfig(t *testing.T) {
	t.Run("writes the full category list to .categories.private.json when categories.json does not exist (nothing public to diff against)", func(t *testing.T) {
		dir := t.TempDir()
		publicFile := filepath.Join(dir, "categories.json")
		privateFile := filepath.Join(dir, ".categories.private.json")
		store := New(publicFile, privateFile, io.Discard)

		if err := store.SaveCategoriesConfig([]*documentschema.CategoryItem{category("new_cat", "New")}); err != nil {
			t.Fatalf("SaveCategoriesConfig: %v", err)
		}

		if _, err := os.Stat(publicFile); !os.IsNotExist(err) {
			t.Fatalf("public file must never be written, stat err = %v", err)
		}
		written := readCategoriesFile(t, privateFile)
		if len(written.Categories) != 1 || written.Categories[0].ID != "new_cat" || written.Categories[0].Name != "New" {
			t.Fatalf("unexpected private file content: %+v", written.Categories)
		}
	})

	t.Run("writes only the categories/subcategories NOT already in the public categories.json", func(t *testing.T) {
		dir := t.TempDir()
		publicFile := filepath.Join(dir, "categories.json")
		privateFile := filepath.Join(dir, ".categories.private.json")
		writeJSON(t, publicFile, categoriesConfigJSON(category("bank", "Banque", subcategory("generic_bank", "Generic Bank"))))
		store := New(publicFile, privateFile, io.Discard)

		// Full merged list, as callers throughout the codebase pass it (public generic_bank + a
		// newly auto-created private subcategory) — only the new one should end up in the diff.
		err := store.SaveCategoriesConfig([]*documentschema.CategoryItem{
			category("bank", "Banque",
				subcategory("generic_bank", "Generic Bank"),
				subcategory("credit_mutuel", "Credit Mutuel"),
			),
		})
		if err != nil {
			t.Fatalf("SaveCategoriesConfig: %v", err)
		}

		written := readCategoriesFile(t, privateFile)
		if len(written.Categories) != 1 {
			t.Fatalf("expected 1 category, got %d", len(written.Categories))
		}
		if written.Categories[0].ID != "bank" {
			t.Fatalf("expected bank, got %q", written.Categories[0].ID)
		}
		if got := subIDs(written.Categories[0].Subcategories); len(got) != 1 || got[0] != "credit_mutuel" {
			t.Fatalf("expected [credit_mutuel], got %v", got)
		}
	})

	t.Run("never writes to categories.json (the public file is read-only from this module's perspective)", func(t *testing.T) {
		dir := t.TempDir()
		publicFile := filepath.Join(dir, "categories.json")
		privateFile := filepath.Join(dir, ".categories.private.json")
		writeJSON(t, publicFile, categoriesConfigJSON(category("invoices", "Factures")))
		original, err := os.ReadFile(publicFile)
		if err != nil {
			t.Fatal(err)
		}
		store := New(publicFile, privateFile, io.Discard)

		err = store.SaveCategoriesConfig([]*documentschema.CategoryItem{
			category("invoices", "Factures", subcategory("sfr", "SFR")),
		})
		if err != nil {
			t.Fatalf("SaveCategoriesConfig: %v", err)
		}

		after, err := os.ReadFile(publicFile)
		if err != nil {
			t.Fatal(err)
		}
		if string(after) != string(original) {
			t.Fatalf("public file changed:\n before=%s\n after=%s", original, after)
		}
	})

	t.Run("invokes the registered onCategoryCreatedCallback", func(t *testing.T) {
		dir := t.TempDir()
		store := New(filepath.Join(dir, "categories.json"), filepath.Join(dir, ".categories.private.json"), io.Discard)
		calls := 0
		store.SetOnCategoryCreatedCallback(func() { calls++ })

		if err := store.SaveCategoriesConfig([]*documentschema.CategoryItem{category("c", "C")}); err != nil {
			t.Fatalf("SaveCategoriesConfig: %v", err)
		}
		if calls != 1 {
			t.Fatalf("expected callback once, got %d", calls)
		}
	})

	t.Run("swallows an error thrown by the callback instead of letting it propagate", func(t *testing.T) {
		dir := t.TempDir()
		store := New(filepath.Join(dir, "categories.json"), filepath.Join(dir, ".categories.private.json"), io.Discard)
		store.SetOnCategoryCreatedCallback(func() { panic("callback exploded") })

		// A thrown JS error is a Go panic; SaveCategoriesConfig must recover and return normally.
		if err := store.SaveCategoriesConfig([]*documentschema.CategoryItem{category("c", "C")}); err != nil {
			t.Fatalf("SaveCategoriesConfig: %v", err)
		}
	})

	t.Run("does not invoke a callback when none has been registered", func(t *testing.T) {
		dir := t.TempDir()
		store := New(filepath.Join(dir, "categories.json"), filepath.Join(dir, ".categories.private.json"), io.Discard)
		if err := store.SaveCategoriesConfig([]*documentschema.CategoryItem{category("c", "C")}); err != nil {
			t.Fatalf("SaveCategoriesConfig: %v", err)
		}
	})
}

func TestCategoriesConcurrentUse(t *testing.T) {
	dir := t.TempDir()
	publicFile := filepath.Join(dir, "categories.json")
	privateFile := filepath.Join(dir, ".categories.private.json")
	writeJSON(t, publicFile, categoriesConfigJSON(category("bank", "Banque", subcategory("generic_bank", "Generic Bank"))))
	store := New(publicFile, privateFile, io.Discard)

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_ = store.GetCategoriesConfig()
			if i%2 == 0 {
				_ = store.SaveCategoriesConfig([]*documentschema.CategoryItem{
					category("bank", "Banque", subcategory("generic_bank", "Generic Bank")),
				})
			}
		}(i)
	}
	wg.Wait()
}

func containsID(cats []*documentschema.CategoryItem, id string) bool {
	for _, c := range cats {
		if c.ID == id {
			return true
		}
	}
	return false
}
