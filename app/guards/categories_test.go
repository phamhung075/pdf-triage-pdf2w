package guards

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/store/categories"
)

// The real store satisfies the guard's minimal interface — this is the interface-gap check.
var _ CategoriesStore = (*categories.Store)(nil)

type fakeCategoriesStore struct {
	config  documentschema.CategoriesConfig
	saves   [][]*documentschema.CategoryItem
	saveErr error
}

func (f *fakeCategoriesStore) GetCategoriesConfig() documentschema.CategoriesConfig {
	return f.config
}

func (f *fakeCategoriesStore) SaveCategoriesConfig(cats []*documentschema.CategoryItem) error {
	f.saves = append(f.saves, cats)
	if f.saveErr == nil {
		// Persist like the real store so a second call observes the first call's writes.
		f.config = documentschema.CategoriesConfig{Categories: cats}
	}
	return f.saveErr
}

func catConfig(cats ...*documentschema.CategoryItem) documentschema.CategoriesConfig {
	return documentschema.CategoriesConfig{Categories: cats}
}

// Ported from relocalize-document.test.ts:268-306 (ensureCategoryAndSubcategoryExist).
func TestEnsureCategoryAndSubcategoryExist(t *testing.T) {
	t.Run("creates a brand-new category and subcategory when neither exists (Golden Rule #5)", func(t *testing.T) {
		store := &fakeCategoriesStore{config: catConfig()}
		if err := EnsureCategoryAndSubcategoryExist(store, "telecom", "orange"); err != nil {
			t.Fatal(err)
		}
		if len(store.saves) != 1 {
			t.Fatalf("save calls = %d, want 1", len(store.saves))
		}
		saved := store.saves[0]
		if len(saved) != 1 || saved[0].ID != "telecom" {
			t.Fatalf("saved = %#v, want one telecom category", saved)
		}
		if saved[0].Name != "Telecom" {
			t.Fatalf("name = %q, want Telecom", saved[0].Name)
		}
		if saved[0].Description != "Category auto-created for telecom" {
			t.Fatalf("description = %q", saved[0].Description)
		}
		if len(saved[0].Subcategories) != 1 || saved[0].Subcategories[0].ID != "orange" {
			t.Fatalf("subcategories = %#v, want [orange]", saved[0].Subcategories)
		}
		if saved[0].Subcategories[0].Name != "Orange" {
			t.Fatalf("sub name = %q, want Orange", saved[0].Subcategories[0].Name)
		}
	})

	t.Run("appends a new subcategory to an existing category without duplicating the category", func(t *testing.T) {
		store := &fakeCategoriesStore{config: catConfig(&documentschema.CategoryItem{
			ID: "invoices", Name: "Invoices",
			Subcategories: []*documentschema.SubcategoryItem{{ID: "sfr", Name: "SFR", Aliases: []string{"sfr"}}},
		})}
		if err := EnsureCategoryAndSubcategoryExist(store, "invoices", "edf"); err != nil {
			t.Fatal(err)
		}
		saved := store.saves[0]
		if len(saved) != 1 {
			t.Fatalf("categories = %d, want 1", len(saved))
		}
		got := []string{}
		for _, s := range saved[0].Subcategories {
			got = append(got, s.ID)
		}
		if strings.Join(got, ",") != "sfr,edf" {
			t.Fatalf("subcategories = %v, want [sfr edf]", got)
		}
	})

	t.Run("is a no-op on content when the category/subcategory already exist (still saves)", func(t *testing.T) {
		store := &fakeCategoriesStore{config: catConfig(&documentschema.CategoryItem{
			ID: "invoices", Name: "Invoices",
			Subcategories: []*documentschema.SubcategoryItem{{ID: "sfr", Name: "SFR", Aliases: []string{"sfr"}}},
		})}
		if err := EnsureCategoryAndSubcategoryExist(store, "invoices", "sfr"); err != nil {
			t.Fatal(err)
		}
		if len(store.saves) != 1 {
			t.Fatalf("save calls = %d, want 1 (TS still saves)", len(store.saves))
		}
		if len(store.saves[0][0].Subcategories) != 1 {
			t.Fatalf("subcategories = %#v, want exactly one", store.saves[0][0].Subcategories)
		}
	})

	t.Run("is idempotent across repeated calls", func(t *testing.T) {
		store := &fakeCategoriesStore{config: catConfig()}
		if err := EnsureCategoryAndSubcategoryExist(store, "telecom", "orange"); err != nil {
			t.Fatal(err)
		}
		// The store would normally now expose the created entries; the fake keeps the same config,
		// but the second call must still not grow the tree in a real store. Emulate the store's
		// persisted state so the second call sees what it wrote.
		if err := EnsureCategoryAndSubcategoryExist(store, "telecom", "orange"); err != nil {
			t.Fatal(err)
		}
		for _, saved := range store.saves {
			if len(saved) != 1 || len(saved[0].Subcategories) != 1 {
				t.Fatalf("repeated call duplicated entries: %#v", saved)
			}
		}
	})

	t.Run("propagates a save error (Go store returns one where TS threw)", func(t *testing.T) {
		sentinel := errors.New("disk full")
		store := &fakeCategoriesStore{config: catConfig(), saveErr: sentinel}
		if err := EnsureCategoryAndSubcategoryExist(store, "telecom", "orange"); !errors.Is(err, sentinel) {
			t.Fatalf("err = %v, want %v", err, sentinel)
		}
	})

	t.Run("nil store is rejected instead of panicking", func(t *testing.T) {
		if err := EnsureCategoryAndSubcategoryExist(nil, "telecom", "orange"); err == nil {
			t.Fatal("want an error for a nil store")
		}
	})
}

// Golden Rule 5's other half: the auto-creation must write ONLY the private
// .categories.private.json overlay, never the committed categories.json. This exercises the real
// store/categories writer end-to-end on t.TempDir() files.
func TestEnsureCategoryAndSubcategoryExist_WritesOnlyPrivateOverlay(t *testing.T) {
	dir := t.TempDir()
	publicFile := filepath.Join(dir, "categories.json")
	privateFile := filepath.Join(dir, ".categories.private.json")
	publicBody := `{"categories":[{"id":"invoices","name":"Factures","description":"d","aliases":["facture"],"subcategories":[]}]}`
	if err := os.WriteFile(publicFile, []byte(publicBody), 0o644); err != nil {
		t.Fatal(err)
	}

	store := categories.New(publicFile, privateFile, nil)
	if err := EnsureCategoryAndSubcategoryExist(store, "telecom", "orange"); err != nil {
		t.Fatal(err)
	}

	gotPublic, err := os.ReadFile(publicFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotPublic) != publicBody {
		t.Fatalf("public categories.json changed:\n got %s\nwant %s", gotPublic, publicBody)
	}

	gotPrivate, err := os.ReadFile(privateFile)
	if err != nil {
		t.Fatalf("private overlay not written: %v", err)
	}
	if !strings.Contains(string(gotPrivate), `"telecom"`) || !strings.Contains(string(gotPrivate), `"orange"`) {
		t.Fatalf("private overlay missing the new entries: %s", gotPrivate)
	}

	// Idempotence against the real store: a second call against the now-persisted overlay must not
	// duplicate anything.
	if err := EnsureCategoryAndSubcategoryExist(store, "telecom", "orange"); err != nil {
		t.Fatal(err)
	}
	cfg := store.GetCategoriesConfig()
	var telecom *documentschema.CategoryItem
	for _, c := range cfg.Categories {
		if c.ID == "telecom" {
			telecom = c
		}
	}
	if telecom == nil || len(telecom.Subcategories) != 1 || telecom.Subcategories[0].ID != "orange" {
		t.Fatalf("telecom subcategories after two calls = %#v", telecom)
	}
}
