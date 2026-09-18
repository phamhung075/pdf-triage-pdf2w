// Package categories is a Go port of pdf-triage's src/infrastructure/categories-store.ts (114 lines):
// the built-in default taxonomy, readCategoriesFile / loadPublicCategories / loadPrivateCategories,
// mergeCategories, getCategoriesConfig (merge + strip forbidden subcategories), and
// saveCategoriesConfig (one-way diff into the private overlay plus the onCategoryCreated callback).
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/categories-store.test.ts` -> 15 passed), so no upstream case is
// pinned red. All 15 cases are ported, plus a concurrent-use case the TS suite never covered.
//
// Golden Rule 5: the committed categories.json is NEVER written. saveCategoriesConfig persists only
// the categories and subcategories that are absent from the CURRENT public file, into
// .categories.private.json. That one-way diff (not a two-way sync) is reproduced exactly.
//
// The TS module reads CONFIG.CATEGORIES_FILE / CONFIG.CATEGORIES_PRIVATE_FILE and holds a
// module-global callback. The settings port is a later phase and this package must not import it, so
// the two paths and the callback are explicit per-Store state. The exported behavior is otherwise
// identical.
//
// WHY-comments from the TS source are preserved verbatim in the functions below.
//
// Deviations, all resolved in favor of matching the TS acceptance bar:
//
//  1. Config is per-Store instead of module-global, so tests no longer need to mock ./settings.js.
//  2. A JS callback that `throw`s maps to a Go callback that `panic`s: fireOnCategoryCreated
//     recovers the panic so SaveCategoriesConfig still returns normally, mirroring the TS try/catch.
//  3. JSON.stringify(data, null, 2) does not escape HTML; Go's encoding/json escapes <, > and & by
//     default, so the writer disables HTML escaping and strips the encoder's trailing newline.
//  4. The private file is written with a plain os.WriteFile, exactly as the TS fs.writeFileSync did.
//     (Unlike infra/settings and infra/jsonregistry this store is not made atomic: the task pins
//     atomicity only for taxonomy hints, and adding it here would silently diverge from the source.)
//  5. SaveCategoriesConfig re-marshals the input and runs documentschema.ParseCategoriesConfig,
//     because Go's struct types cannot express Zod's `.min(1)` on id/name. Nil slices are normalized
//     to [] first, which is the Zod `.default([])` for an absent (undefined) value; an explicit nil
//     element is preserved and, as null, fails validation exactly as Zod would reject it.
//  6. mergeCategories shallow-copies each category and fresh-copies its subcategory slice, which is
//     what the TS object spread did; the subcategory elements themselves stay shared by pointer.
package categories

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// BUILT_IN_DEFAULT_CATEGORIES is the TS BUILT_IN_DEFAULT_CATEGORIES array, verbatim.
var builtInDefaultCategories = []*documentschema.CategoryItem{
	{ID: "invoices", Name: "Factures", Description: "Factures et reçus", Aliases: []string{"facture", "invoice"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "bulletin_salaire", Name: "Bulletins de Salaire", Description: "Fiches de paie par entreprise", Aliases: []string{"bulletin_salaire", "paie", "salaire"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "contracts", Name: "Contrats", Description: "Contrats et baux", Aliases: []string{"contrat", "contract"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "bank", Name: "Banque & Relevés", Description: "Relevés de compte bancaire et RIB", Aliases: []string{"bank", "banque", "releve", "rib"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "administrative", Name: "Administratif", Description: "Documents administratifs", Aliases: []string{"tax", "impot"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "health", Name: "Santé", Description: "Santé et mutuelle", Aliases: []string{"health", "sante"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "identity", Name: "Identité", Description: "Passeports et cartes d identite", Aliases: []string{"identity", "passport"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "housing", Name: "Logement", Description: "Justificatifs de domicile et loyers", Aliases: []string{"housing", "logement"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "insurance", Name: "Assurances", Description: "Contrats d assurance", Aliases: []string{"insurance", "assurance"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "education", Name: "Éducation", Description: "Formations et diplômes", Aliases: []string{"education", "formation"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "recruitment", Name: "Recrutement", Description: "Lettres et CV", Aliases: []string{"recrutement", "candidature"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "correspondence", Name: "Courriers", Description: "Emails et lettres", Aliases: []string{"courrier", "mail"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "technical", Name: "Technique", Description: "Manuels et guides", Aliases: []string{"tech", "manual"}, Subcategories: []*documentschema.SubcategoryItem{}},
	{ID: "reports", Name: "Rapports", Description: "Rapports de projets", Aliases: []string{"report"}, Subcategories: []*documentschema.SubcategoryItem{}},
}

// categoriesFile mirrors the `{ categories: [...] }` document the private overlay is written as.
type categoriesFile struct {
	Categories []*documentschema.CategoryItem `json:"categories"`
}

// Store owns the two taxonomy paths and the onCategoryCreated callback. Methods are safe for
// concurrent callers.
type Store struct {
	mu          sync.RWMutex
	publicFile  string
	privateFile string
	stderr      io.Writer

	onCategoryCreated func()
}

// New builds a Store. publicFile is the committed categories.json, privateFile the gitignored
// overlay. stderr receives the "Invalid ... schema, ignoring" messages (TS console.error); nil
// defaults to os.Stderr.
func New(publicFile, privateFile string, stderr io.Writer) *Store {
	if stderr == nil {
		stderr = os.Stderr
	}
	return &Store{publicFile: publicFile, privateFile: privateFile, stderr: stderr}
}

// readCategoriesFile is the TS readCategoriesFile: the parsed category list, or nil when the file is
// absent or invalid. A nil result is the Go equivalent of the TS `null` that callers coalesce.
func (s *Store) readCategoriesFile(filePath, label string) []*documentschema.CategoryItem {
	// fs.existsSync: any stat failure (including not-exist) means "no file", silently.
	if _, err := os.Stat(filePath); err != nil {
		return nil
	}
	raw, err := os.ReadFile(filePath)
	if err != nil {
		s.logInvalid(label, err)
		return nil
	}
	parsed, err := documentschema.ParseCategoriesConfig(raw)
	if err != nil {
		s.logInvalid(label, err)
		return nil
	}
	return parsed.Categories
}

func (s *Store) logInvalid(label string, err error) {
	fmt.Fprintf(s.stderr, "Invalid %s schema, ignoring %v\n", label, err)
}

// loadPublicCategories is `readCategoriesFile(CONFIG.CATEGORIES_FILE, 'categories.json') ??
// BUILT_IN_DEFAULT_CATEGORIES`. A nil (missing-or-invalid) result falls back to the defaults; a
// valid empty list stays empty, exactly as `??` only replaces null/undefined.
func (s *Store) loadPublicCategories() []*documentschema.CategoryItem {
	if cats := s.readCategoriesFile(s.publicFile, "categories.json"); cats != nil {
		return cats
	}
	return builtInDefaultCategories
}

// loadPrivateCategories is `readCategoriesFile(CONFIG.CATEGORIES_PRIVATE_FILE,
// '.categories.private.json') ?? []`.
func (s *Store) loadPrivateCategories() []*documentschema.CategoryItem {
	if cats := s.readCategoriesFile(s.privateFile, ".categories.private.json"); cats != nil {
		return cats
	}
	return []*documentschema.CategoryItem{}
}

// Layers the private (personal, gitignored, auto-created-from-your-documents) overlay onto the
// public (generic, committed) base — new categories from the overlay are appended, and new
// subcategories on a category that exists in both are merged in by id. This lets categories.json
// stay a clean, shareable starter taxonomy while .categories.private.json accumulates real
// entities (bank branches, employers, etc.) without either file needing manual reconciliation.
func mergeCategories(base, overlay []*documentschema.CategoryItem) []*documentschema.CategoryItem {
	merged := make([]*documentschema.CategoryItem, 0, len(base)+len(overlay))
	for _, cat := range base {
		clone := *cat
		clone.Subcategories = append([]*documentschema.SubcategoryItem{}, cat.Subcategories...)
		merged = append(merged, &clone)
	}

	for _, overlayCat := range overlay {
		var existing *documentschema.CategoryItem
		for _, cat := range merged {
			if cat.ID == overlayCat.ID {
				existing = cat
				break
			}
		}
		if existing == nil {
			clone := *overlayCat
			clone.Subcategories = append([]*documentschema.SubcategoryItem{}, overlayCat.Subcategories...)
			merged = append(merged, &clone)
			continue
		}
		existingSubIDs := make(map[string]struct{}, len(existing.Subcategories))
		for _, sub := range existing.Subcategories {
			existingSubIDs[sub.ID] = struct{}{}
		}
		for _, overlaySub := range overlayCat.Subcategories {
			if _, ok := existingSubIDs[overlaySub.ID]; !ok {
				existing.Subcategories = append(existing.Subcategories, overlaySub)
			}
		}
	}

	return merged
}

// GetCategoriesConfig is `getCategoriesConfig()`: the merged public+private taxonomy with forbidden
// subcategories stripped on read.
func (s *Store) GetCategoriesConfig() documentschema.CategoriesConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()

	merged := mergeCategories(s.loadPublicCategories(), s.loadPrivateCategories())

	// Strip forbidden/invalid subcategories (e.g. anyscanner, raw file extensions like verso_png, 102818_jpg)
	for _, cat := range merged {
		if cat.Subcategories != nil {
			filtered := make([]*documentschema.SubcategoryItem, 0, len(cat.Subcategories))
			for _, sub := range cat.Subcategories {
				if !taxonomy.IsForbiddenSubcategory(sub.ID) {
					filtered = append(filtered, sub)
				}
			}
			cat.Subcategories = filtered
		}
	}

	return documentschema.CategoriesConfig{Categories: merged}
}

// SetOnCategoryCreatedCallback registers the callback fired after a successful private-overlay
// write. A nil callback clears it.
func (s *Store) SetOnCategoryCreatedCallback(cb func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onCategoryCreated = cb
}

// SaveCategoriesConfig is `saveCategoriesConfig(categories)`.
//
// Persists ONLY what isn't already in the public categories.json — new categories wholesale,
// and new subcategories on categories that already exist publicly. This is a one-way diff
// against the CURRENT public file, not a general two-way sync: renaming or editing a category
// that came from the public base won't be captured here (that's an intentionally out-of-scope
// edge case — the common path this exists for is auto-created categories/subcategories from
// document classification, never edits to the shipped defaults).
//
// A validation failure is returned as an error, which is the Go form of Zod's thrown parse error.
func (s *Store) SaveCategoriesConfig(categories []*documentschema.CategoryItem) error {
	s.mu.Lock()
	err := s.saveLocked(categories)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	s.fireOnCategoryCreated()
	return nil
}

func (s *Store) saveLocked(categories []*documentschema.CategoryItem) error {
	validated, err := validateCategories(categories)
	if err != nil {
		return err
	}
	publicCategories := s.loadPublicCategories()

	privateDiff := make([]*documentschema.CategoryItem, 0)
	for _, cat := range validated.Categories {
		var publicCat *documentschema.CategoryItem
		for _, candidate := range publicCategories {
			if candidate.ID == cat.ID {
				publicCat = candidate
				break
			}
		}
		if publicCat == nil {
			privateDiff = append(privateDiff, cat)
			continue
		}
		publicSubIDs := make(map[string]struct{}, len(publicCat.Subcategories))
		for _, sub := range publicCat.Subcategories {
			publicSubIDs[sub.ID] = struct{}{}
		}
		extraSubs := make([]*documentschema.SubcategoryItem, 0)
		for _, sub := range cat.Subcategories {
			if _, ok := publicSubIDs[sub.ID]; !ok {
				extraSubs = append(extraSubs, sub)
			}
		}
		if len(extraSubs) > 0 {
			clone := *cat
			clone.Subcategories = extraSubs
			privateDiff = append(privateDiff, &clone)
		}
	}

	payload, err := marshalIndentNoHTMLEscape(categoriesFile{Categories: privateDiff})
	if err != nil {
		return err
	}
	return os.WriteFile(s.privateFile, payload, 0o644)
}

// fireOnCategoryCreated runs the registered callback, swallowing a panic the way the TS try/catch
// swallowed a thrown error. It takes the read lock itself, so a callback may safely call back into
// the store (e.g. a future SSE broadcaster reading GetCategoriesConfig).
func (s *Store) fireOnCategoryCreated() {
	s.mu.RLock()
	cb := s.onCategoryCreated
	s.mu.RUnlock()
	if cb == nil {
		return
	}
	defer func() { _ = recover() }()
	cb()
}

// validateCategories re-runs the Zod CategoriesConfigSchema.parse acceptance bar. Nil slices are
// normalized to [] so an omitted aliases/subcategories list takes the schema default instead of
// failing as JSON null.
func validateCategories(categories []*documentschema.CategoryItem) (documentschema.CategoriesConfig, error) {
	normalized := make([]*documentschema.CategoryItem, len(categories))
	for i, cat := range categories {
		if cat == nil {
			normalized[i] = nil
			continue
		}
		clone := *cat
		if clone.Aliases == nil {
			clone.Aliases = []string{}
		}
		clone.Subcategories = normalizeSubcategories(cat.Subcategories)
		normalized[i] = &clone
	}
	raw, err := json.Marshal(categoriesFile{Categories: normalized})
	if err != nil {
		return documentschema.CategoriesConfig{}, err
	}
	return documentschema.ParseCategoriesConfig(raw)
}

func normalizeSubcategories(in []*documentschema.SubcategoryItem) []*documentschema.SubcategoryItem {
	if in == nil {
		return []*documentschema.SubcategoryItem{}
	}
	out := make([]*documentschema.SubcategoryItem, len(in))
	for i, sub := range in {
		if sub == nil {
			out[i] = nil
			continue
		}
		clone := *sub
		if clone.Aliases == nil {
			clone.Aliases = []string{}
		}
		clone.Subcategories = normalizeSubcategories(sub.Subcategories)
		out[i] = &clone
	}
	return out
}

// marshalIndentNoHTMLEscape is `JSON.stringify(data, null, 2)`: two-space indent and no HTML
// escaping (Go escapes <, > and & by default). The encoder appends a newline that stringify does
// not, so it is trimmed. Duplicated from infra/settings (whose helper is unexported and may not be
// modified) rather than introducing an import for one function.
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
