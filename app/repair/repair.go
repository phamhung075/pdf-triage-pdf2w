// Package repair is a Go port of pdf-triage's src/application/repair-registry.ts (269 lines) and
// its suite src/application/repair-registry.test.ts (9 cases): the Repair Registry &
// Relocalization use case. Entry RepairRegistry (repair-registry.ts:19).
//
// Responsibility, in the TypeScript order (Golden Rule 16): purge ghost DB rows whose physical
// file is gone, re-extract every archived file (infra/pdfextractor -> pdf2w), re-classify generic
// subcategories (app/classify plus classification.RuleBasedClassify), relocalize to canonical
// (app/relocalize), move back to __raws when nothing specific can be recovered, backfill contact
// metadata, insert unindexed archive files with an embedding (infra/ollama), then sync the JSON
// registry. No code path deletes a document file: an unrecoverable file is moved back to __raws
// (Golden Rules 15/16).
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/application/repair-registry.test.ts` -> 9 passed), so no upstream case is
// pinned red. All nine cases are ported, plus cases for the event stream, the scan lock, the
// canonical-category correction and the REPAIR_COMPLETED payload.
//
// # Scan lock (app/scanlock)
//
// repairRegistry acquired the cross-process scan lock itself (repair-registry.ts:26) and released
// it in a finally, so a scan, repair or clear can never run concurrently against the same
// __raws/__archive pair. This port preserves that: when Deps.Lock is set, RepairRegistry calls
// Lock.Acquire() for the whole run and releases it with a defer, propagating a
// *scanlock.ScanInProgressError unchanged. When Deps.Lock is nil the CALLER already holds the
// guard (the route-level acquire mode) and this package must not acquire it again — a second
// acquisition inside the same process is reported as in-progress. Production wiring passes the
// same *scanlock.Guard the scan/clear routes use; tests pass a fake or a real temp-dir guard.
//
// # Collaborator seams
//
// The TypeScript imported infrastructure singletons directly. The port takes a Deps struct whose
// fields are small interfaces (or stateless function fields), exactly like app/relocalize, so
// tests substitute fakes and the composition root wires the real stores:
//
//   - Settings (settings.Store): ReloadFromDisk + EnsureDirectoriesExist + Config, the port of
//     reloadConfigFromDisk()/ensureDirectoriesExist()/CONFIG (:3, :28-29). Nil falls back to the
//     static Deps.Config, for a caller that already refreshed and created its directories.
//   - DB (store/database.Store): the named document methods. No raw SQL is issued here.
//   - Categories (store/categories.Store): the read side of findCanonicalCategoryForSubcategory.
//   - Extractor (relocalize.PDFExtractor over infra/pdfextractor): re-extraction of each file.
//   - Relocalizer (relocalize.Deps): RelocalizeFileIfNeeded + MoveBackToRaws. FindActualFileOnDisk
//     is the shared guard and lives in app/guards; this package imports it rather than
//     reimplementing it (migration design decision 6).
//   - Classifier (app/classify.Deps): classifyPDFText for an unindexed file.
//   - RuleBasedClassify / ExtractRuleBasedContact / GenerateEmbedding: function seams over
//     classification and infra/ollama; nil falls back to the real classification functions (or a
//     no-op embedding) so production wiring stays short.
//   - EntityDictionary / PromptPersonalization: the two stores ruleBasedClassify reads.
//   - Registry (relocalize.JSONRegistrySync over infra/jsonregistry): the JSON mirror.
//   - Log (*infra/logger.Logger): REPAIR module lines. Nil is silent.
//   - Lock (app/scanlock.Guard): see above.
//
// # TS-vs-Go gaps, each resolved to MATCH the TypeScript surface
//
//  1. JS String.prototype.trim() / .length. The content guards use the JavaScript
//     WhiteSpace+LineTerminator set and UTF-16 code units; strings.go reproduces them.
//  2. `undefined` vs "". TS's optional onProgress is a nil ProgressFunc; this function has no
//     optional string arguments.
//  3. Exceptions vs error returns. TS throws out of repairRegistry for the lock, config, the
//     top-level DB read, the JSON sync and the per-file insert; it swallows every other per-file
//     error with console.warn, except an OllamaUnavailableError, which becomes FILE_FAILED and a
//     skip. Go returns the fatal ones and funnels each per-file error through handleFileError.
//  4. console.log / console.warn become Log.Info / Log.Warn on the REPAIR module, so the same
//     narration reaches the log stream instead of stdout.
//  5. The extraction type carries Pdf2wMarkdown (the pdf2w-extraction swap); it is passed to
//     classifyPDFText for an unindexed file exactly as repair-registry.ts:83/:172-174 did.
//  6. REPAIR_COMPLETED. TS emits that broadcast in the HTTP layer (web-server.ts:272), after
//     repairRegistry returned the result and after finishTask — repairRegistry itself emits only
//     REPAIR_STARTED, FILE_PROGRESS and FILE_FAILED. This port preserves that split so the SSE
//     ordering matches TS exactly: RepairRegistry emits the three in-band events, and the exported
//     CompletedEvent(result) builds the byte-identical `{ type, ...result }` payload for the HTTP
//     adapter to broadcast at the same point.
//  7. The TS unindexed branch computed `ruleContact = extractRuleBasedContact(raw_text)` and then
//     never used it (the inserted row takes metadata.contact_*). The call is preserved for parity,
//     with the dead assignment made explicit.
package repair

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/classify"
	"github.com/phamhung075/pdf-triage-pdf2w/app/guards"
	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/app/scanlock"
	"github.com/phamhung075/pdf-triage-pdf2w/classification"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfextractor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfscanner"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/store/categories"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/entitydictionary"
	storepromptpersonalization "github.com/phamhung075/pdf-triage-pdf2w/store/promptpersonalization"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// Result mirrors the TS return object at repair-registry.ts:259-265. The JSON tags are the frozen
// REST/SSE field names: the route spreads `...result` into both its response body and the
// REPAIR_COMPLETED event.
type Result struct {
	ScannedCount     int `json:"scannedCount"`
	RepairedCount    int `json:"repairedCount"`
	UpdatedCount     int `json:"updatedCount"`
	RelocalizedCount int `json:"relocalizedCount"`
	MovedToRawsCount int `json:"movedToRawsCount"`
}

// runCounts carries the mutable per-run tallies. TS used four local `let`s; a pointer keeps the
// per-file helpers from returning a four-value tuple.
type runCounts struct {
	repaired    int
	updated     int
	relocalized int
	movedToRaws int
}

// Settings is the composition-root config surface RepairRegistry refreshes before it runs. It is
// the port of reloadConfigFromDisk() (:28), ensureDirectoriesExist() (:29) and CONFIG. When
// Deps.Settings is nil the static Deps.Config is used unchanged.
type Settings interface {
	ReloadFromDisk()
	EnsureDirectoriesExist() error
	Config() settings.Config
}

// DocumentStore is the narrow store/database surface this package needs. *store/database.Store
// satisfies it; every write goes through a named method, so this package issues no raw SQL.
type DocumentStore interface {
	GetAllDocuments() ([]database.DocumentRecord, error)
	GetDocumentByChecksum(checksum string) (*database.DocumentRecord, error)
	InsertDocumentRecord(doc database.NewDocument) (int64, error)
	UpdateDocumentRecord(id any, updates database.DocumentUpdates) (bool, error)
	DeleteDocument(id int64) error
}

// CategoriesStore is the read side of the taxonomy the canonical-category correction needs. The
// pre-move auto-creation writer (guards.EnsureCategoryAndSubcategoryExist) is owned by the
// relocalizer, which was handed the same store by the composition root.
type CategoriesStore interface {
	GetCategoriesConfig() documentschema.CategoriesConfig
}

// Extractor is the injected pdf2w extraction seam. relocalize.PDFExtractor is the production
// adapter over infra/pdfextractor.
type Extractor interface {
	ExtractPDFContent(filePath string) (pdfextractor.ExtractedPDF, error)
}

// Relocalizer is the injected relocation seam. relocalize.Deps is the production value and
// already satisfies it: RelocalizeFileIfNeeded is repair-registry.ts:138/:190 and MoveBackToRaws is
// repair-registry.ts:88/:185/:119. MoveBackToRaws also un-registers the row when a checksum is
// supplied, exactly as the TS helper did.
type Relocalizer interface {
	RelocalizeFileIfNeeded(filePath, category string, subcategory, dateStr, title *string) (relocalize.RelocalizeResult, error)
	MoveBackToRaws(filePath string, checksum *string) (string, error)
}

// Classifier is the injected AI classification seam. Its single method matches
// app/classify.Deps.ClassifyPDFText and relocalize.Classifier. A zero `now` means time.Now().
type Classifier interface {
	ClassifyPDFText(rawText, filename, previousError string, now time.Time, doclingMarkdown string) (documentschema.DocumentMetadata, error)
}

// EntityDictionary is the injected entity-dictionary reader used by ruleBasedClassify.
type EntityDictionary interface {
	GetEntityDictionary() documentschema.EntityDictionary
}

// PromptPersonalization is the injected private-overlay reader used by ruleBasedClassify.
type PromptPersonalization interface {
	GetPromptPersonalization() promptpersonalization.PromptPersonalization
}

// RegistrySyncer is the injected JSON-mirror writer. relocalize.JSONRegistrySync is the production
// adapter; tests use a counting fake.
type RegistrySyncer interface {
	SyncJSONRegistry() error
}

// Logger is the slice of *infra/logger.Logger this package logs through (module "REPAIR"). Nil is
// silent, matching the other application ports.
type Logger interface {
	Info(moduleName, message string, meta any, filename ...string)
	Warn(moduleName, message string, meta any, filename ...string)
}

// Locker is the app/scanlock seam. *scanlock.Guard satisfies it. See the package comment for the
// two acquisition modes.
type Locker interface {
	Acquire() (release func(), err error)
}

// RuleBasedClassifyFunc is the injected rule-based classifier seam; classification.RuleBasedClassify
// is the production value.
type RuleBasedClassifyFunc func(rawText, filename string, dictionary documentschema.EntityDictionary, personalNameDenylist []string, personalization ...promptpersonalization.PromptPersonalization) classification.RuleBasedClassifyResult

// Compile-time proof that the real collaborators satisfy the injected seams the composition root
// passes in.
var (
	_ Settings              = (*settings.Store)(nil)
	_ DocumentStore         = (*database.Store)(nil)
	_ CategoriesStore       = (*categories.Store)(nil)
	_ EntityDictionary      = (*entitydictionary.Store)(nil)
	_ PromptPersonalization = (*storepromptpersonalization.Store)(nil)
	_ Extractor             = relocalize.PDFExtractor{}
	_ Relocalizer           = relocalize.Deps{}
	_ Classifier            = classify.Deps{}
	_ RegistrySyncer        = relocalize.JSONRegistrySync{}
	_ Logger                = (*logger.Logger)(nil)
	_ Locker                = (*scanlock.Guard)(nil)
)

// Deps is the explicit injected surface. Construct it in the composition root; tests build it with
// fakes and a temp SQLite store. See the package comment for each field.
type Deps struct {
	Config   settings.Config
	Settings Settings

	DB          DocumentStore
	Categories  CategoriesStore
	Extractor   Extractor
	Relocalizer Relocalizer
	Classifier  Classifier

	RuleBasedClassify       RuleBasedClassifyFunc
	ExtractRuleBasedContact func(rawText string) classification.RuleBasedContact
	GenerateEmbedding       func(text string) []float64

	EntityDictionary      EntityDictionary
	PromptPersonalization PromptPersonalization

	Registry RegistrySyncer
	Log      Logger
	Lock     Locker
}

// info is a nil-safe Logger.Info.
func (d Deps) info(module, message string, meta any) {
	if d.Log != nil {
		d.Log.Info(module, message, meta)
	}
}

// warn is a nil-safe Logger.Warn.
func (d Deps) warn(module, message string, meta any) {
	if d.Log != nil {
		d.Log.Warn(module, message, meta)
	}
}

// progress invokes the caller's onProgress when one was supplied.
func (d Deps) progress(onProgress ProgressFunc, event any) {
	if onProgress != nil {
		onProgress(event)
	}
}

// validate rejects a Deps missing a mandatory seam before any file is touched.
func (d Deps) validate() error {
	switch {
	case d.DB == nil:
		return errors.New("repair: Deps.DB (document store) is required")
	case d.Extractor == nil:
		return errors.New("repair: Deps.Extractor is required")
	case d.Classifier == nil:
		return errors.New("repair: Deps.Classifier is required")
	case d.Relocalizer == nil:
		return errors.New("repair: Deps.Relocalizer is required")
	}
	return nil
}

// effectiveConfig is the port of reloadConfigFromDisk() + ensureDirectoriesExist() + reading
// CONFIG (:28-31). A nil Settings uses Deps.Config as-is.
func (d Deps) effectiveConfig() (settings.Config, error) {
	if d.Settings == nil {
		return d.Config, nil
	}
	d.Settings.ReloadFromDisk()
	if err := d.Settings.EnsureDirectoriesExist(); err != nil {
		return settings.Config{}, err
	}
	return d.Settings.Config(), nil
}

// categoriesConfig is the nil-safe getCategoriesConfig() read.
func (d Deps) categoriesConfig() documentschema.CategoriesConfig {
	if d.Categories == nil {
		return documentschema.CategoriesConfig{Categories: []*documentschema.CategoryItem{}}
	}
	return d.Categories.GetCategoriesConfig()
}

// ruleBasedClassify is the nil-safe ruleBasedClassify call (repair-registry.ts:107). A nil seam
// falls back to the real classification.RuleBasedClassify.
func (d Deps) ruleBasedClassify(cfg settings.Config, rawText, filename string) classification.RuleBasedClassifyResult {
	fn := d.RuleBasedClassify
	if fn == nil {
		fn = classification.RuleBasedClassify
	}
	dictionary := documentschema.EntityDictionary{}
	if d.EntityDictionary != nil {
		dictionary = d.EntityDictionary.GetEntityDictionary()
	}
	personalization := promptpersonalization.EMPTY_PROMPT_PERSONALIZATION
	if d.PromptPersonalization != nil {
		personalization = d.PromptPersonalization.GetPromptPersonalization()
	}
	return fn(rawText, filename, dictionary, cfg.PersonalNameDenylist, personalization)
}

// extractContact is the nil-safe extractRuleBasedContact call.
func (d Deps) extractContact(rawText string) classification.RuleBasedContact {
	fn := d.ExtractRuleBasedContact
	if fn == nil {
		fn = classification.ExtractRuleBasedContact
	}
	return fn(rawText)
}

// generateEmbedding is the nil-safe generateEmbedding call; a nil seam yields no embedding.
func (d Deps) generateEmbedding(rawText string) []float64 {
	if d.GenerateEmbedding == nil {
		return nil
	}
	return d.GenerateEmbedding(rawText)
}

// syncRegistry runs the JSON mirror when one is injected. Nil is a no-op so a Deps without a mirror
// stays usable; production wiring always sets it.
func (d Deps) syncRegistry() error {
	if d.Registry == nil {
		return nil
	}
	return d.Registry.SyncJSONRegistry()
}

// moveBackToRaws is the Golden Rule 15/16 move-back step plus the movedToRaws tally. It never
// deletes a file.
func (d Deps) moveBackToRaws(filePath, checksum string, counts *runCounts) error {
	if _, err := d.Relocalizer.MoveBackToRaws(filePath, &checksum); err != nil {
		return err
	}
	counts.movedToRaws++
	return nil
}

// RepairRegistry is the port of repairRegistry (repair-registry.ts:19). See the package comment for
// the scan-lock contract and the event stream.
func (d Deps) RepairRegistry(onProgress ProgressFunc) (Result, error) {
	// TS acquires the scan lock first and releases it in a finally. A nil Lock means the caller
	// already holds the guard (see the package comment).
	if d.Lock != nil {
		release, err := d.Lock.Acquire()
		if err != nil {
			return Result{}, err
		}
		defer release()
	}

	if err := d.validate(); err != nil {
		return Result{}, err
	}

	cfg, err := d.effectiveConfig()
	if err != nil {
		return Result{}, err
	}

	d.info("REPAIR", fmt.Sprintf("Starting Repair Registry & Relocalization on: %s", cfg.OutputRootDir), nil)

	existingDocs, err := d.DB.GetAllDocuments()
	if err != nil {
		return Result{}, err
	}

	// Ghost purge + year-string normalization (repair-registry.ts:33-49). Order matters: the
	// subcategory is normalized before the file-on-disk check, and the check itself is the shared
	// guards.FindActualFileOnDisk.
	for _, doc := range existingDocs {
		if taxonomy.IsYearString(doc.Subcategory) {
			general := "general"
			if _, err := d.DB.UpdateDocumentRecord(doc.ID, database.DocumentUpdates{Subcategory: &general}); err != nil {
				return Result{}, err
			}
		}
		actual := guards.FindActualFileOnDisk(
			guards.DocumentLocation{
				NewPath:          doc.NewPath,
				OriginalPath:     doc.OriginalPath,
				OriginalFilename: doc.OriginalFilename,
			},
			cfg.InputDir,
			cfg.OutputRootDir,
		)
		if actual == "" || !pathExists(actual) {
			d.info("REPAIR", fmt.Sprintf("Purging ghost database record ID %d (%s) - missing on disk", doc.ID, doc.Title), nil)
			if err := d.DB.DeleteDocument(doc.ID); err != nil {
				return Result{}, err
			}
		}
	}

	archivedFiles := pdfscanner.GetAllFilesRecursively(cfg.OutputRootDir)

	d.progress(onProgress, RepairStartedEvent{
		Type:       EventRepairStarted,
		TotalFiles: len(archivedFiles),
		Message:    fmt.Sprintf("Starting repair & relocalization of %d archived PDF file(s)...", len(archivedFiles)),
	})

	counts := &runCounts{}
	processedIndex := 0
	for _, filePath := range archivedFiles {
		processedIndex++
		if err := d.repairArchiveFile(cfg, onProgress, filePath, processedIndex, len(archivedFiles), counts); err != nil {
			d.handleFileError(cfg, onProgress, filePath, err)
		}
	}

	if err := d.syncRegistry(); err != nil {
		return Result{}, err
	}

	result := Result{
		ScannedCount:     len(archivedFiles),
		RepairedCount:    counts.repaired,
		UpdatedCount:     counts.updated,
		RelocalizedCount: counts.relocalized,
		MovedToRawsCount: counts.movedToRaws,
	}
	// REPAIR_COMPLETED is NOT emitted here: TS broadcasts it from the HTTP layer after this result
	// is returned and finishTask ran (web-server.ts:272). The route emits CompletedEvent(result).
	return result, nil
}

// handleFileError is the port of repair-registry.ts:239-254: Ollama-down becomes a FILE_FAILED
// event and a skip; every other per-file error is a warning and a skip.
func (d Deps) handleFileError(cfg settings.Config, onProgress ProgressFunc, filePath string, err error) {
	var unavailable *ollama.OllamaUnavailableError
	if errors.As(err, &unavailable) {
		message := fmt.Sprintf(
			"⛔ Ollama is down — %s unreachable. Start Ollama, then re-run Repair. Skipped: %s",
			cfg.OllamaModel,
			filepath.Base(filePath),
		)
		d.warn("REPAIR", message, map[string]any{"filePath": filePath})
		d.progress(onProgress, FileFailedEvent{
			Type:     EventFileFailed,
			Filename: filepath.Base(filePath),
			Stage:    stageFailed,
			Message:  message,
		})
		return
	}
	d.warn("REPAIR", fmt.Sprintf("Error repairing file %s: %s", filePath, err.Error()), nil)
}

// repairArchiveFile runs the per-file body of the loop (repair-registry.ts:65-238). A returned
// error is the TS throw that escaped the file's try/catch; handleFileError decides how to report
// it. The "handled" early exits (missing content, generic subcategory) return nil after moving the
// file back.
func (d Deps) repairArchiveFile(cfg settings.Config, onProgress ProgressFunc, filePath string, processedIndex, total int, counts *runCounts) error {
	if !pathExists(filePath) {
		return nil
	}

	file := filepath.Base(filePath)
	d.progress(onProgress, FileProgressEvent{
		Type:           EventFileProgress,
		Filename:       file,
		ScannedCount:   total,
		ProcessedCount: processedIndex,
		Stage:          stageRepairing,
		Message:        fmt.Sprintf("Analyzing & repairing file %d/%d: %s", processedIndex, total, file),
	})

	extracted, err := d.Extractor.ExtractPDFContent(filePath)
	if err != nil {
		return err
	}
	checksum := extracted.Checksum
	rawText := extracted.RawText
	doclingMarkdown := extracted.Pdf2wMarkdown

	isMissingContent := rawText == "" || jsTrim(rawText) == "" || strings.Contains(rawText, "[No raw text extracted]")
	if isMissingContent {
		return d.moveBackToRaws(filePath, checksum, counts)
	}

	existing, err := d.DB.GetDocumentByChecksum(checksum)
	if err != nil {
		return err
	}
	if existing != nil {
		return d.repairExisting(cfg, filePath, file, rawText, checksum, existing, counts)
	}
	return d.repairUnindexed(cfg, filePath, file, rawText, doclingMarkdown, checksum, counts)
}

// repairExisting is repair-registry.ts:94-163: refresh stale text, fix a generic subcategory via
// ruleBasedClassify or a non-generic one via findCanonicalCategoryForSubcategory, relocalize,
// backfill contacts and persist the winning path.
func (d Deps) repairExisting(cfg settings.Config, filePath, file, rawText, checksum string, existing *database.DocumentRecord, counts *runCounts) error {
	currentText := jsTrim(existing.RawText)
	// Text-refresh rule, preserved verbatim from repair-registry.ts:96.
	if utf16Len(currentText) < 15 || strings.Contains(currentText, "[No raw text extracted]") || (utf16Len(rawText) > 20 && currentText != rawText) {
		d.info("REPAIR", fmt.Sprintf("Updating raw text for doc ID %d (%s): %d chars", existing.ID, file, utf16Len(rawText)), nil)
		if _, err := d.DB.UpdateDocumentRecord(existing.ID, database.DocumentUpdates{RawText: &rawText}); err != nil {
			return err
		}
		counts.updated++
	}

	currentCat := existing.Category
	currentSub := existing.Subcategory
	isGeneric := currentSub == "" || currentSub == "general" || currentSub == "other" || currentSub == "divers" || currentCat == "personal"

	if isGeneric {
		rb := d.ruleBasedClassify(cfg, firstNonEmpty(rawText, currentText), file)
		if rb.Subcategorie != "general" && rb.Subcategorie != "other" && rb.Subcategorie != "divers" {
			currentCat = rb.Categorie
			currentSub = rb.Subcategorie
			d.info("REPAIR", fmt.Sprintf(
				"Re-classified document ID %d (%s): %s/%s -> %s/%s",
				existing.ID, file, existing.Category, existing.Subcategory, currentCat, currentSub,
			), nil)
			if _, err := d.DB.UpdateDocumentRecord(existing.ID, database.DocumentUpdates{Category: &currentCat, Subcategory: &currentSub}); err != nil {
				return err
			}
			counts.updated++
		} else {
			d.warn("REPAIR", fmt.Sprintf("Document ID %d (%s) has no specific subcategory. Moving back to __raws!", existing.ID, file), nil)
			return d.moveBackToRaws(filePath, checksum, counts)
		}
	} else {
		// currentCat is passed so an ambiguous slug (one living under several categories) keeps the
		// placement classification already chose, instead of being relocated by array order.
		canonicalCat := taxonomy.FindCanonicalCategoryForSubcategory(currentSub, toTaxonomyConfig(d.categoriesConfig()), currentCat)
		if canonicalCat != nil && *canonicalCat != currentCat {
			d.info("REPAIR", fmt.Sprintf(
				"Canonical category changed for doc ID %d (%s) subcategory '%s': %s -> %s",
				existing.ID, file, currentSub, currentCat, *canonicalCat,
			), nil)
			currentCat = *canonicalCat
			if _, err := d.DB.UpdateDocumentRecord(existing.ID, database.DocumentUpdates{Category: &currentCat}); err != nil {
				return err
			}
			counts.updated++
		}
	}

	relocalized, err := d.Relocalizer.RelocalizeFileIfNeeded(filePath, currentCat, &currentSub, &existing.Date, &existing.Title)
	if err != nil {
		return err
	}
	if relocalized.Moved {
		counts.relocalized++
	}

	// Backfill missing contact metadata if not present (only authentic extracted contact info).
	if existing.ContactName == "" && existing.ContactEmail == "" {
		ruleContact := d.extractContact(firstNonEmpty(rawText, currentText))
		if ruleContact.ContactName != "" || ruleContact.ContactEmail != "" || ruleContact.ContactPhone != "" {
			d.info("REPAIR", fmt.Sprintf(
				"Extracted contact details for doc ID %d (%s): %s",
				existing.ID, file, firstNonEmpty(ruleContact.ContactName, ruleContact.ContactEmail),
			), nil)
			if _, err := d.DB.UpdateDocumentRecord(existing.ID, database.DocumentUpdates{
				ContactName:    &ruleContact.ContactName,
				ContactEmail:   &ruleContact.ContactEmail,
				ContactPhone:   &ruleContact.ContactPhone,
				ContactAddress: &ruleContact.ContactAddress,
				ContactWebsite: &ruleContact.ContactWebsite,
			}); err != nil {
				return err
			}
		}
	}

	if existing.NewPath != relocalized.NewPath || existing.Status != "MOVED" {
		status := "MOVED"
		if _, err := d.DB.UpdateDocumentRecord(existing.ID, database.DocumentUpdates{NewPath: &relocalized.NewPath, Status: &status}); err != nil {
			return err
		}
		counts.updated++
	}
	return nil
}

// repairUnindexed is repair-registry.ts:164-238: classify a file that has no matching DB row,
// move it back to __raws when even the AI resolves a generic subcategory, otherwise relocalize and
// insert the new record (with the UNIQUE-checksum collision handled through the shared guard).
func (d Deps) repairUnindexed(cfg settings.Config, filePath, file, rawText, doclingMarkdown, checksum string, counts *runCounts) error {
	// Path hint, used only for the narration line, exactly as in TS.
	parts := relativeParts(cfg.OutputRootDir, filePath)
	pathCat := "other"
	if len(parts) > 0 && parts[0] != "" {
		pathCat = parts[0]
	}
	pathSub := "general"
	if len(parts) >= 3 {
		pathSub = parts[1]
	}
	d.info("REPAIR", fmt.Sprintf("Repairing & analyzing unindexed file '%s' (Path hint: %s/%s)...", file, pathCat, pathSub), nil)

	var metadata documentschema.DocumentMetadata
	var err error
	if jsTrim(doclingMarkdown) != "" {
		metadata, err = d.Classifier.ClassifyPDFText(rawText, file, "", time.Time{}, doclingMarkdown)
	} else {
		metadata, err = d.Classifier.ClassifyPDFText(rawText, file, "", time.Time{}, "")
	}
	if err != nil {
		return err
	}

	embedding := d.generateEmbedding(rawText)
	// TS computed `ruleContact` here and never read it: the inserted row's contact fields come from
	// metadata. The call is preserved for parity; see the package comment gap 7.
	_ = d.extractContact(rawText)

	targetCat := metadata.Categorie
	targetSub := metadata.Subcategorie
	targetDate := metadata.Date

	isGenericTarget := targetSub == "" || targetSub == "general" || targetSub == "other" || targetSub == "divers"
	if isGenericTarget {
		d.warn("REPAIR", fmt.Sprintf("Unindexed file '%s' has no specific subcategory. Moving back to __raws!", file), nil)
		return d.moveBackToRaws(filePath, checksum, counts)
	}

	relocalized, err := d.Relocalizer.RelocalizeFileIfNeeded(filePath, targetCat, &targetSub, &targetDate, &metadata.Titre)
	if err != nil {
		return err
	}
	if relocalized.Moved {
		counts.relocalized++
	}

	newDoc := database.NewDocument{
		Checksum:         checksum,
		Title:            firstNonEmpty(metadata.Titre, stripPDFExt(file)),
		Registre:         metadata.Registre,
		Date:             targetDate,
		Category:         targetCat,
		Subcategory:      targetSub,
		Summary:          metadata.Summary,
		Tags:             metadata.Tags,
		RawText:          rawText,
		MarkdownContent:  metadata.MarkdownContent,
		TotalAmount:      metadata.TotalAmount,
		VatAmount:        metadata.VatAmount,
		Siren:            metadata.Siren,
		Iban:             metadata.Iban,
		ExpiryDate:       metadata.ExpiryDate,
		ContactName:      metadata.ContactName,
		ContactEmail:     metadata.ContactEmail,
		ContactPhone:     metadata.ContactPhone,
		ContactAddress:   metadata.ContactAddress,
		ContactWebsite:   metadata.ContactWebsite,
		OriginalFilename: file,
		OriginalPath:     filePath,
		NewPath:          relocalized.NewPath,
		Embedding:        embedding,
		Status:           "MOVED",
	}
	if _, err := d.DB.InsertDocumentRecord(newDoc); err != nil {
		if guards.IsChecksumUniqueViolation(err.Error()) {
			existingDoc, lookupErr := d.DB.GetDocumentByChecksum(checksum)
			if lookupErr != nil {
				return lookupErr
			}
			if existingDoc != nil {
				status := "MOVED"
				if _, updateErr := d.DB.UpdateDocumentRecord(existingDoc.ID, database.DocumentUpdates{
					Category:    &targetCat,
					Subcategory: &targetSub,
					NewPath:     &relocalized.NewPath,
					Status:      &status,
				}); updateErr != nil {
					return updateErr
				}
				counts.updated++
			}
		} else {
			d.warn("REPAIR", fmt.Sprintf("Error inserting record for %s: %s", file, err.Error()), nil)
		}
	} else {
		counts.repaired++
	}
	return nil
}

// relativeParts is `path.relative(root, target).split(path.sep)`, used only for the path hint.
func relativeParts(root, target string) []string {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return nil
	}
	return strings.Split(rel, string(filepath.Separator))
}

// toTaxonomyConfig projects the merged taxonomy store value onto the small taxonomy.TaxonomyConfig
// shape FindCanonicalCategoryForSubcategory reads.
func toTaxonomyConfig(cfg documentschema.CategoriesConfig) *taxonomy.TaxonomyConfig {
	out := &taxonomy.TaxonomyConfig{Categories: make([]*taxonomy.Category, 0, len(cfg.Categories))}
	for _, c := range cfg.Categories {
		if c == nil {
			continue
		}
		tc := &taxonomy.Category{ID: c.ID}
		for _, s := range c.Subcategories {
			if s == nil {
				continue
			}
			tc.Subcategories = append(tc.Subcategories, &taxonomy.Subcategory{ID: s.ID, Name: s.Name, Aliases: s.Aliases})
		}
		out.Categories = append(out.Categories, tc)
	}
	return out
}
