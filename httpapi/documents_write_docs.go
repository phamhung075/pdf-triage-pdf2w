// Document mutation routes #24 (subcategory rename), #40 (delete), #41 (edit) and #42 (relocalize),
// ported from web-server.ts:649-700, :1235-1343.
//
// All four are thin adapters over the ported application/domain packages; the Golden-Rule write
// guards are the shared app/guards implementation, never reimplemented here:
//
//   - #41 rejects an explicit forbidden subcategory BEFORE any write (Golden Rule 4) and calls
//     guards.EnsureCategoryAndSubcategoryExist BEFORE the physical move (Golden Rule 5).
//   - #41 records a manual decision whenever the edit actually re-classifies (Golden Rule 18).
//   - #42 forwards `reason` as previousError into ReclassifyAndRelocalizeDocument (Golden Rule 18).
//   - #24 merges the taxonomy entry through taxonomy.MergeSubcategoryInTaxonomy (the one shared
//     rename/merge implementation) and relocalizes each matching document through app/relocalize.
package httpapi

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/app/guards"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// subcategorySlugSanitizer is TS `.replace(/[^a-z0-9_-]+/g, '_')` (applied after lowercase+trim).
var subcategorySlugSanitizer = regexp.MustCompile(`[^a-z0-9_-]+`)

// --- #24 POST /api/subcategories/rename ----------------------------------------------------------

func (s *server) renameSubcategoryHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	body := struct {
		Category       string `json:"category"`
		OldSubcategory string `json:"oldSubcategory"`
		NewSubcategory string `json:"newSubcategory"`
	}{}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Category == "" || body.OldSubcategory == "" || body.NewSubcategory == "" {
		writeError(w, http.StatusBadRequest, "Missing required parameters: category, oldSubcategory, newSubcategory")
		return
	}
	if d.Categories == nil || d.DB == nil {
		writeError(w, http.StatusInternalServerError, "subcategory rename is not configured")
		return
	}
	if d.Relocalizer == nil {
		writeError(w, http.StatusInternalServerError, "relocalizer is not configured")
		return
	}

	cleanOld := strings.ToLower(strings.TrimSpace(body.OldSubcategory))
	cleanNew := subcategorySlugSanitizer.ReplaceAllString(strings.ToLower(strings.TrimSpace(body.NewSubcategory)), "_")

	if cleanOld == cleanNew {
		writeJSON(w, http.StatusOK, map[string]any{"message": "Subcategory name unchanged", "count": 0})
		return
	}

	cfg := d.Categories.GetCategoriesConfig()
	merged := mergeSubcategoryInDocumentschemaConfig(cfg, body.Category, cleanOld, cleanNew)
	if err := d.Categories.SaveCategoriesConfig(merged.Categories); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	allDocs, err := d.DB.GetAllDocuments()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	settingsCfg := d.config()
	categoryLower := strings.ToLower(strings.TrimSpace(body.Category))
	relocalizedCount := 0
	for _, doc := range allDocs {
		if strings.ToLower(doc.Category) != categoryLower || strings.ToLower(doc.Subcategory) != cleanOld {
			continue
		}
		actualPath := guards.FindActualFileOnDisk(guards.DocumentLocation{
			NewPath:          doc.NewPath,
			OriginalPath:     doc.OriginalPath,
			OriginalFilename: doc.OriginalFilename,
		}, settingsCfg.InputDir, settingsCfg.OutputRootDir)

		if actualPath != "" && fileExistsOnDisk(actualPath) {
			result, err := d.Relocalizer.RelocalizeFileIfNeeded(actualPath, doc.Category, &cleanNew, &doc.Date, nil)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			status := "MOVED"
			if _, err := d.DB.UpdateDocumentRecord(doc.ID, database.DocumentUpdates{
				Subcategory: &cleanNew,
				NewPath:     &result.NewPath,
				Status:      &status,
			}); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
			relocalizedCount++
		} else {
			if _, err := d.DB.UpdateDocumentRecord(doc.ID, database.DocumentUpdates{Subcategory: &cleanNew}); err != nil {
				writeError(w, http.StatusInternalServerError, err.Error())
				return
			}
		}
	}

	if err := d.syncRegistry(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// TS order (web-server.ts:690-691): REGISTRY_UPDATED then CATEGORIES_UPDATED.
	s.hub.Broadcast(map[string]any{"type": "REGISTRY_UPDATED"})
	s.hub.Broadcast(map[string]any{"type": "CATEGORIES_UPDATED"})

	writeJSON(w, http.StatusOK, map[string]any{
		"message": fmt.Sprintf("Successfully renamed subcategory '%s' ➔ '%s' and relocalized %d physical file(s).", cleanOld, cleanNew, relocalizedCount),
		"count":   relocalizedCount,
	})
}

// mergeSubcategoryInDocumentschemaConfig delegates to taxonomy.MergeSubcategoryInTaxonomy on a
// projected view, then writes the result back by pointer identity. See the package comment gap 4:
// the domain function models only id/name/aliases, while the store holds description, name_fr and
// nested subcategories that must survive the round trip.
func mergeSubcategoryInDocumentschemaConfig(cfg documentschema.CategoriesConfig, categoryID, oldSub, newSub string) documentschema.CategoriesConfig {
	if cfg.Categories == nil {
		return cfg
	}

	projected := make([]*taxonomy.Category, 0, len(cfg.Categories))
	catOwner := map[*taxonomy.Category]*documentschema.CategoryItem{}
	subOwner := map[*taxonomy.Subcategory]*documentschema.SubcategoryItem{}
	for _, cat := range cfg.Categories {
		if cat == nil {
			continue
		}
		tc := &taxonomy.Category{ID: cat.ID}
		catOwner[tc] = cat
		for _, sub := range cat.Subcategories {
			if sub == nil {
				continue
			}
			ts := &taxonomy.Subcategory{ID: sub.ID, Name: sub.Name, Aliases: append([]string(nil), sub.Aliases...)}
			subOwner[ts] = sub
			tc.Subcategories = append(tc.Subcategories, ts)
		}
		projected = append(projected, tc)
	}

	result := taxonomy.MergeSubcategoryInTaxonomy(projected, categoryID, oldSub, newSub)
	wanted := strings.ToLower(strings.TrimSpace(categoryID))
	for _, tc := range result {
		if tc == nil || tc.ID != wanted {
			continue
		}
		cat := catOwner[tc]
		if cat == nil {
			continue
		}
		rebuilt := make([]*documentschema.SubcategoryItem, 0, len(tc.Subcategories))
		for _, ts := range tc.Subcategories {
			if ts == nil {
				continue
			}
			if original, ok := subOwner[ts]; ok {
				original.ID = ts.ID
				if ts.Name != "" {
					original.Name = ts.Name
				}
				original.Aliases = append([]string(nil), ts.Aliases...)
				rebuilt = append(rebuilt, original)
				continue
			}
			rebuilt = append(rebuilt, &documentschema.SubcategoryItem{
				ID:            ts.ID,
				Name:          ts.Name,
				Aliases:       append([]string(nil), ts.Aliases...),
				Subcategories: []*documentschema.SubcategoryItem{},
			})
		}
		cat.Subcategories = rebuilt
	}
	return cfg
}

// --- #40 DELETE /api/documents/{id} --------------------------------------------------------------

func (s *server) deleteDocumentHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	if d.Relocalizer == nil {
		writeError(w, http.StatusInternalServerError, "document delete is not configured")
		return
	}
	id, _ := parseJSInt(r.PathValue("id"))
	result, err := d.Relocalizer.DeleteDocumentAndMoveToTrash(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !result.Success {
		writeError(w, http.StatusNotFound, result.Error)
		return
	}
	s.hub.Broadcast(map[string]any{"type": "DOCUMENTS_UPDATED"})
	writeJSON(w, http.StatusOK, deleteResultMap(result))
}

// --- #41 PUT /api/documents/{id} -----------------------------------------------------------------

func (s *server) putDocumentHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	if d.DB == nil {
		writeError(w, http.StatusBadRequest, "document update is not configured")
		return
	}
	id, _ := parseJSInt(r.PathValue("id"))

	docBefore, err := d.DB.GetDocumentByID(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	parsed, err := documentschema.ParseUpdateDocument(bodyBytes(r))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	// Golden Rule 4. TS uses `validatedUpdates.subcategory ?? validatedUpdates.subcategorie`, so a
	// present-but-empty `subcategory` is still checked (and is forbidden), while an absent pair is
	// not. An existing forbidden subcategory on the record is deliberately not re-checked.
	explicitSubcategory := parsed.Subcategory
	if explicitSubcategory == nil {
		explicitSubcategory = parsed.Subcategorie
	}
	if explicitSubcategory != nil {
		if violation := guards.ForbiddenSubcategoryViolation(*explicitSubcategory); violation != nil {
			writeError(w, http.StatusBadRequest, violation.Message)
			return
		}
	}

	success, err := d.DB.UpdateDocumentRecord(id, updateInputToStore(parsed))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if !success || docBefore == nil {
		writeError(w, http.StatusNotFound, "Document not found or update failed")
		return
	}

	// Relocalize on disk only when the record HAS a file and that file exists.
	if docBefore.NewPath != "" && fileExistsOnDisk(docBefore.NewPath) {
		targetCategory := firstNonEmptyString(derefOr(parsed.Category), derefOr(parsed.Categorie), docBefore.Category)
		targetSubcategory := firstNonEmptyString(derefOr(parsed.Subcategory), derefOr(parsed.Subcategorie), docBefore.Subcategory)
		// Golden Rule 5: register the branch in .categories.private.json BEFORE the move. The
		// route previously skipped this step entirely (web-server.ts:1275-1277).
		if d.Categories == nil {
			writeError(w, http.StatusBadRequest, "categories store is not configured")
			return
		}
		if err := guards.EnsureCategoryAndSubcategoryExist(d.Categories, targetCategory, targetSubcategory); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if d.Relocalizer == nil {
			writeError(w, http.StatusBadRequest, "relocalizer is not configured")
			return
		}
		targetDate := firstNonEmptyString(derefOr(parsed.Date), docBefore.Date)
		result, err := d.Relocalizer.RelocalizeFileIfNeeded(docBefore.NewPath, targetCategory, &targetSubcategory, &targetDate, nil)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if result.NewPath != docBefore.NewPath {
			newPath := result.NewPath
			if _, err := d.DB.UpdateDocumentRecord(id, database.DocumentUpdates{NewPath: &newPath}); err != nil {
				writeError(w, http.StatusBadRequest, err.Error())
				return
			}
		}
	}

	finalCategory := firstNonEmptyString(derefOr(parsed.Category), derefOr(parsed.Categorie), docBefore.Category)
	finalSubcategory := firstNonEmptyString(derefOr(parsed.Subcategory), derefOr(parsed.Subcategorie), docBefore.Subcategory)
	categoryChanged := strings.ToLower(finalCategory) != strings.ToLower(docBefore.Category)
	subcategoryChanged := strings.ToLower(finalSubcategory) != strings.ToLower(docBefore.Subcategory)
	if (categoryChanged || subcategoryChanged) && d.ManualDecisions != nil {
		// Feedback teaches the AI (Golden Rule 18): the Edit modal is a human correction too, not
		// just the Relocalize modal. recordManualDecision derives keywords and never throws.
		d.ManualDecisions.RecordManualDecision(manualdecisions.Record{
			DocumentID:         id,
			Checksum:           docBefore.Checksum,
			OriginalFilename:   docBefore.OriginalFilename,
			Title:              firstNonEmptyString(derefOr(parsed.Title), docBefore.Title),
			OldCategory:        docBefore.Category,
			OldSubcategory:     docBefore.Subcategory,
			NewCategory:        finalCategory,
			NewSubcategory:     finalSubcategory,
			UserFeedbackReason: "Manual user selection (Edit modal)",
			RawTextSnippet:     docBefore.RawText,
		})
	}

	if err := d.syncRegistry(); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.hub.Broadcast(map[string]any{"type": "REGISTRY_UPDATED", "action": "EDIT", "docId": id})

	updatedDoc, err := d.DB.GetDocumentByID(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"message":  "Document updated successfully and relocalized if needed",
		"document": documentWithTags(updatedDoc),
	})
}

// updateInputToStore maps the validated UpdateDocumentInput onto the named store update type.
func updateInputToStore(input documentschema.UpdateDocumentInput) database.DocumentUpdates {
	return database.DocumentUpdates{
		Title:           input.Title,
		Titre:           input.Titre,
		Registre:        input.Registre,
		Date:            input.Date,
		Category:        input.Category,
		Categorie:       input.Categorie,
		Subcategory:     input.Subcategory,
		Subcategorie:    input.Subcategorie,
		Summary:         input.Summary,
		Tags:            input.Tags,
		MarkdownContent: input.MarkdownContent,
		TotalAmount:     input.TotalAmount,
		VatAmount:       input.VatAmount,
		Siren:           input.Siren,
		Iban:            input.Iban,
		ExpiryDate:      input.ExpiryDate,
		ContactName:     input.ContactName,
		ContactEmail:    input.ContactEmail,
		ContactPhone:    input.ContactPhone,
		ContactAddress:  input.ContactAddress,
		ContactWebsite:  input.ContactWebsite,
	}
}

// --- #42 POST /api/documents/{id}/relocalize -----------------------------------------------------

func (s *server) relocalizeDocumentHandler(d DocumentWriteDeps, w http.ResponseWriter, r *http.Request) {
	if d.Relocalizer == nil {
		writeError(w, http.StatusInternalServerError, "relocalize is not configured")
		return
	}
	body := struct {
		Category    *string `json:"category"`
		Subcategory *string `json:"subcategory"`
		Reason      *string `json:"reason"`
	}{}
	if err := decodeBody(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	id, _ := parseJSInt(r.PathValue("id"))

	// Golden Rule 18 (feedback teaches the AI): `reason` is handed to the classifier as
	// previousError, so a human's correction steers the re-analysis.
	result, err := d.Relocalizer.ReclassifyAndRelocalizeDocument(id, body.Category, body.Subcategory, body.Reason)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !result.Success {
		status := http.StatusBadRequest
		if result.StaleCleaned {
			status = http.StatusNotFound
		}
		writeJSON(w, status, reclassifyResultMap(result))
		return
	}
	s.hub.Broadcast(map[string]any{"type": "REGISTRY_UPDATED", "action": "RELOCALIZE", "docId": id})
	s.hub.Broadcast(map[string]any{"type": "CATEGORIES_UPDATED"})
	writeJSON(w, http.StatusOK, reclassifyResultMap(result))
}

// fileExistsOnDisk is `fs.existsSync`.
func fileExistsOnDisk(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
