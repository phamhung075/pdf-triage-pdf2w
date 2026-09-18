// Category routes #17 and #23, ported from web-server.ts:488-531 and :634-647.
//
// GET  /api/categories merges the configured taxonomy with per-category/subcategory document counts
// from SQLite, and injects DB-only subcategories (skipping "general" and bare years) so a document
// is never invisible in the UI just because no one renamed its subcategory into categories.json.
// PUT  /api/categories validates CategoriesConfigSchema and saves through the categories store,
// which diffs new entries into the private .categories.private.json overlay (Golden Rule 5), then
// broadcasts CATEGORIES_UPDATED.
package httpapi

import (
	"net/http"
	"sort"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

func (s *server) registerCategories(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/categories", s.getCategoriesHandler)
	mux.HandleFunc("PUT /api/categories", s.putCategoriesHandler)
}

func (s *server) getCategoriesHandler(w http.ResponseWriter, r *http.Request) {
	config := s.deps.Categories.GetCategoriesConfig()
	stats, err := s.deps.DB.GetCategorySubcategoryStats()
	if err != nil {
		writeError(w, 500, err.Error())
		return
	}

	categories := make([]any, 0, len(config.Categories))
	for _, cat := range config.Categories {
		if cat == nil {
			continue
		}
		catIDLower := strings.ToLower(cat.ID)
		catCount := stats.CategoryCounts[catIDLower]
		subMap := stats.SubcategoryCounts[catIDLower]

		subcategories := make([]any, 0, len(cat.Subcategories))
		seen := map[string]bool{}
		for _, sub := range cat.Subcategories {
			if sub == nil {
				continue
			}
			subIDLower := strings.ToLower(sub.ID)
			seen[subIDLower] = true
			view := subcategoryView(sub)
			view["count"] = subMap[subIDLower]
			subcategories = append(subcategories, view)
		}

		// Dynamically include subcategories present in DB that are not yet in categories.json.
		// Sorted for deterministic output; TS iterated the DB object's own key order.
		injected := make([]string, 0, len(subMap))
		for subID := range subMap {
			if subID == "general" || taxonomy.IsYearString(subID) || seen[subID] {
				continue
			}
			injected = append(injected, subID)
		}
		sort.Strings(injected)
		for _, subID := range injected {
			subcategories = append(subcategories, map[string]any{
				"id":      subID,
				"name":    prettifySubID(subID),
				"aliases": []string{subID},
				"count":   subMap[subID],
			})
		}

		view := categoryView(cat)
		view["count"] = catCount
		view["subcategories"] = subcategories
		categories = append(categories, view)
	}

	writeJSON(w, 200, map[string]any{
		"totalDocuments": stats.Total,
		"categories":     categories,
	})
}

func (s *server) putCategoriesHandler(w http.ResponseWriter, r *http.Request) {
	parsed, err := documentschema.ParseCategoriesConfig(bodyBytes(r))
	if err != nil {
		writeError(w, 400, err.Error())
		return
	}
	normalizeCategories(parsed.Categories)

	if err := s.deps.Categories.SaveCategoriesConfig(parsed.Categories); err != nil {
		writeError(w, 400, err.Error())
		return
	}
	s.hub.Broadcast(map[string]any{"type": "CATEGORIES_UPDATED"})
	writeJSON(w, 200, map[string]any{
		"message":    "Categories updated successfully",
		"categories": parsed.Categories,
	})
}

// subcategoryView mirrors the TS `{ ...sub, count }`: only present optional fields are emitted.
func subcategoryView(sub *documentschema.SubcategoryItem) map[string]any {
	view := map[string]any{"id": sub.ID, "name": sub.Name}
	if sub.NameFR != nil {
		view["name_fr"] = *sub.NameFR
	}
	if sub.NameEN != nil {
		view["name_en"] = *sub.NameEN
	}
	view["aliases"] = nonNilStrings(sub.Aliases)
	nested := make([]any, 0, len(sub.Subcategories))
	for _, child := range sub.Subcategories {
		if child != nil {
			nested = append(nested, subcategoryView(child))
		}
	}
	view["subcategories"] = nested
	return view
}

// categoryView mirrors the TS `{ ...cat, count, subcategories }`; the subcategories key is replaced
// by the caller, so it is initialized empty here to keep the key present and ordered last.
func categoryView(cat *documentschema.CategoryItem) map[string]any {
	view := map[string]any{"id": cat.ID, "name": cat.Name}
	if cat.NameFR != nil {
		view["name_fr"] = *cat.NameFR
	}
	if cat.NameEN != nil {
		view["name_en"] = *cat.NameEN
	}
	view["description"] = cat.Description
	if cat.DescriptionFR != nil {
		view["description_fr"] = *cat.DescriptionFR
	}
	if cat.DescriptionEN != nil {
		view["description_en"] = *cat.DescriptionEN
	}
	view["aliases"] = nonNilStrings(cat.Aliases)
	view["subcategories"] = []any{}
	return view
}

// prettifySubID is `subId.split('_').map(w => w.charAt(0).toUpperCase() + w.slice(1)).join(' ')`.
func prettifySubID(slug string) string {
	words := strings.Split(slug, "_")
	for i, word := range words {
		if word == "" {
			continue
		}
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

// normalizeCategories replaces nil slices with the empty slices Zod's `.default([])` produces, so
// the response never contains a JSON null where TS emits [].
func normalizeCategories(categories []*documentschema.CategoryItem) {
	for _, cat := range categories {
		if cat == nil {
			continue
		}
		if cat.Aliases == nil {
			cat.Aliases = []string{}
		}
		if cat.Subcategories == nil {
			cat.Subcategories = []*documentschema.SubcategoryItem{}
		}
		normalizeSubcategories(cat.Subcategories)
	}
}

func normalizeSubcategories(subs []*documentschema.SubcategoryItem) {
	for _, sub := range subs {
		if sub == nil {
			continue
		}
		if sub.Aliases == nil {
			sub.Aliases = []string{}
		}
		if sub.Subcategories == nil {
			sub.Subcategories = []*documentschema.SubcategoryItem{}
		}
		normalizeSubcategories(sub.Subcategories)
	}
}

func nonNilStrings(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
