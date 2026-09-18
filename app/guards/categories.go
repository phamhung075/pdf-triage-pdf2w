package guards

import (
	"errors"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
)

// CategoriesStore is the minimal collaborator EnsureCategoryAndSubcategoryExist needs. It is
// satisfied by *store/categories.Store; the interface lives here (rather than importing the store)
// so tests use a fake and so this package has no dependency on store internals. See the package
// comment on dependency injection.
type CategoriesStore interface {
	GetCategoriesConfig() documentschema.CategoriesConfig
	SaveCategoriesConfig(categories []*documentschema.CategoryItem) error
}

// EnsureCategoryAndSubcategoryExist is the ONE implementation of Golden Rule 5's pre-move
// auto-creation step. It registers a missing category and/or subcategory before any physical file
// move and is idempotent: an already-present category/subcategory is left untouched (though the
// store is still asked to save, exactly as the TS source does, so the onCategoryCreated callback
// still fires).
//
// The TypeScript WHY comment, preserved verbatim (relocalize-document.ts:163-165):
//
// Golden Rule #5: the category/subcategory must exist in categories.json BEFORE any
// physical file move — every caller that lets an explicit category/subcategory be set
// (not just the AI classification path) must run this first.
//
// It writes through store/categories.SaveCategoriesConfig, which diffs against the committed
// categories.json and persists ONLY the new entries into the private .categories.private.json
// overlay — the committed file is never written.
//
// Callers (web-server.ts:1277, mcp-server.ts:264, relocalize-document.ts:298) pass the target
// category/subcategory, which may be empty; the TS source still calls in that case (a category
// named "" would be created), so this port does not silently skip.
func EnsureCategoryAndSubcategoryExist(store CategoriesStore, category, subcategory string) error {
	if store == nil {
		return errors.New("guards: EnsureCategoryAndSubcategoryExist requires a categories store")
	}

	config := store.GetCategoriesConfig()

	var catObj *documentschema.CategoryItem
	for _, c := range config.Categories {
		if c != nil && c.ID == category {
			catObj = c
			break
		}
	}
	if catObj == nil {
		catObj = &documentschema.CategoryItem{
			ID:            category,
			Name:          capitalizeFirst(category),
			Description:   "Category auto-created for " + category,
			Aliases:       []string{category},
			Subcategories: []*documentschema.SubcategoryItem{},
		}
		config.Categories = append(config.Categories, catObj)
	}

	if catObj.Subcategories == nil {
		catObj.Subcategories = []*documentschema.SubcategoryItem{}
	}
	exists := false
	for _, s := range catObj.Subcategories {
		if s != nil && s.ID == subcategory {
			exists = true
			break
		}
	}
	if !exists {
		catObj.Subcategories = append(catObj.Subcategories, &documentschema.SubcategoryItem{
			ID:      subcategory,
			Name:    prettifySubcategory(subcategory),
			Aliases: []string{subcategory},
		})
	}

	return store.SaveCategoriesConfig(config.Categories)
}
