// Package relocalize is a Go port of pdf-triage's src/application/relocalize-document.ts (390
// lines): the canonical-path relocation primitive, the move-back-to-__raws repair step, the
// re-extract / re-classify / relocalize use case, and the delete-to-trash use case.
//
// Exported surface (the TS names in parentheses):
//
//   - Deps.RelocalizeFileIfNeeded        (relocalize-document.ts:47)
//   - Deps.MoveBackToRaws                (relocalize-document.ts:108)
//   - Deps.ReclassifyAndRelocalizeDocument (relocalize-document.ts:191)
//   - Deps.DeleteDocumentAndMoveToTrash  (relocalize-document.ts:345)
//
// findActualFileOnDisk (:142) and ensureCategoryAndSubcategoryExist (:163) are NOT reimplemented
// here: they are the shared Golden-Rule guards and live in app/guards (guards.FindActualFileOnDisk,
// guards.EnsureCategoryAndSubcategoryExist). This package imports them, exactly as the migration
// design decision 6 requires ("no call site may reimplement a guard"). The upstream cases for those
// two functions are ported in app/guards/files_test.go and app/guards/categories_test.go and are not
// duplicated here.
//
// # Golden Rules pinned
//
//   - Golden Rule 4 (strict no-subcategory fail guard): an EXPLICIT forbidden subcategory is
//     rejected before anything is read or written, with the canonical guards message
//     (relocalize-document.ts:208-210).
//   - Golden Rule 5 (pre-move dynamic auto-create): the target category/subcategory is registered
//     through guards.EnsureCategoryAndSubcategoryExist (which writes only the private overlay)
//     BEFORE the physical move (relocalize-document.ts:297-299).
//   - Golden Rule 15 / 16 (never delete a file): Deletion moves the physical file to
//     __raws/.delete_files and only then un-registers the row; moveBackToRaws moves a file back to
//     __raws. No path in this package ever calls os.Remove on a document file.
//
// # The "a re-analysis must never make the record WORSE" logic
//
// Preserved verbatim from relocalize-document.ts:236-262, including the WHY comment and the
// degraded-OCR guard: a re-extraction that fell back to the degraded engine must not replace
// longer stored text. See ReclassifyAndRelocalizeDocument.
//
// # Injected collaborators (no globals)
//
// The TypeScript imported infrastructure singletons directly; the port takes a Deps struct whose
// fields are small interfaces, so tests substitute fakes and the production composition root wires
// the real stores:
//
//   - DB         (*store/database.Store): the named document methods. No raw SQL is issued here.
//   - Categories (guards.CategoriesStore, *store/categories.Store): the Golden Rule 5 writer.
//   - Extractor  (PDFExtractor over infra/pdfextractor): re-extraction of the file on disk.
//   - Classifier (see the Classifier interface): app/classify, injected so this package does not
//     depend on the concurrently-ported package.
//   - Registry   (JSONRegistrySync over infra/jsonregistry): the JSON mirror.
//   - Decisions  (*store/manualdecisions.Store): recordManualDecision.
//   - Log        (*infra/logger.Logger). Nil is silent.
//
// # TS-vs-Go gaps, each resolved to MATCH the TypeScript surface
//
//  1. `path.extname` / `path.basename(p, ext)` vs Go filepath.Ext/Base: identical for the
//     documents this code moves (".pdf", ".jpg", ...). A leading-dot filename (`.gitignore`) is the
//     one divergence — Node extname returns "" there, Go returns ".gitignore" — and cannot occur for
//     a triaged PDF/photo.
//  2. JS `String.prototype.trim()` and `.length`. The fresh-text liveness test and the degraded
//     guard use JavaScript's WhiteSpace+LineTerminator set and UTF-16 code units. guards' copies are
//     unexported, so strings.go reproduces jsTrim/utf16Len exactly as app/classify and guards do.
//  3. `undefined` vs `""` for the optional inputs. TS skips the Golden Rule 4 guard when
//     explicitSubcategory is `undefined` but treats `""` as forbidden; Go strings conflate them, so
//     the optional parameters are *string and the guard runs only when the pointer is non-nil
//     (guards package-comment deviation 5). The re-classification call shape "3 args vs 5 args" maps
//     to the Classifier method's trailing previousError/doclingMarkdown strings, with `""` standing
//     in for `undefined`.
//  4. The HTTP hop to the canonical-path service is gone: the target path is computed in-process by
//     canonicalpath.ComputeCanonicalPath, the port of the same computeCanonicalPath the service
//     wraps (migration design decision 5).
//  5. `syncJSONRegistry()` maps the store's named GetAllDocuments read into infra/jsonregistry's
//     DocumentRecord in JSONRegistrySync; no registry internals are reimplemented.
//  6. The TS raw-SQL `DELETE FROM documents / documents_fts` pairs become the single named
//     store/database.DeleteDocument; the FTS delete is best-effort there exactly as the TS try/catch
//     makes it.
//  7. `renameAtomicNoOverwrite`'s EXDEV branch is detected by the build-tagged isCrossDeviceError
//     (syscall.EXDEV on !windows; plus Win32 ERROR_NOT_SAME_DEVICE 17 on windows, which the syscall
//     package does not name).
package relocalize

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/guards"
	"github.com/phamhung075/pdf-triage-pdf2w/canonicalpath"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/jsonregistry"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfextractor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/categories"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/store/manualdecisions"
)

// maxRenameAttempts is the TS default `maxAttempts = 20` (relocalize-document.ts:21).
const maxRenameAttempts = 20

// DocumentStore is the narrow store/database surface this package needs. *store/database.Store
// satisfies it. Every write goes through a named method, so this package issues no raw SQL.
type DocumentStore interface {
	GetDocumentByID(id int64) (*database.DocumentRecord, error)
	GetDocumentByChecksum(checksum string) (*database.DocumentRecord, error)
	UpdateDocumentRecord(id any, updates database.DocumentUpdates) (bool, error)
	DeleteDocument(id int64) error
}

// Classifier is the injected re-classification seam. Its single method matches
// app/classify.Deps.ClassifyPDFText, which is the port of src/application/classify-document.ts:383:
//
//	classifyPDFText(rawText, filename, previousError?, now = new Date(), doclingMarkdown?)
//	    -> Promise<DocumentMetadata>
//
// The TS optional arguments collapse to Go strings: previousError and doclingMarkdown are "" when
// the caller passed `undefined`, and a zero `now` means "use time.Now()" (the TS default). At
// cutover the composition root passes classify.Deps, which satisfies this interface directly; the
// port keeps the interface local so app/relocalize does not depend on the concurrently-ported
// app/classify package (reported interface gap: classify.Deps.ClassifyPDFText already has exactly
// this signature, no adapter needed).
type Classifier interface {
	ClassifyPDFText(rawText, filename, previousError string, now time.Time, doclingMarkdown string) (documentschema.DocumentMetadata, error)
}

// Extractor is the injected pdf2w extraction seam. *PDFExtractor (below) is the production adapter
// over infra/pdfextractor; *infra/pdfextractor exposes a package function, not a method, so the
// adapter is needed to satisfy the interface.
type Extractor interface {
	ExtractPDFContent(filePath string) (pdfextractor.ExtractedPDF, error)
}

// PDFExtractor adapts infra/pdfextractor.ExtractPDFContent to the Extractor interface.
type PDFExtractor struct {
	Config pdfextractor.Config
}

// ExtractPDFContent forwards to the ported extractor with the configured pdf2w service.
func (p PDFExtractor) ExtractPDFContent(filePath string) (pdfextractor.ExtractedPDF, error) {
	return pdfextractor.ExtractPDFContent(filePath, p.Config)
}

// NewPDFExtractor builds the production extractor from CONFIG. The logger may be nil.
func NewPDFExtractor(cfg settings.Config, log Logger) PDFExtractor {
	return PDFExtractor{Config: pdfextractor.Config{
		BaseURL: cfg.PDF2WServiceURL,
		Timeout: time.Duration(cfg.PDF2WServiceTimeoutMS) * time.Millisecond,
		Logger:  log,
	}}
}

// RegistrySource is the read-only store surface JSONRegistrySync needs. *store/database.Store
// satisfies it through its named GetAllDocuments method.
type RegistrySource interface {
	GetAllDocuments() ([]database.DocumentRecord, error)
}

// RegistrySyncer is the injected JSON-mirror writer. *JSONRegistrySync is the production adapter;
// tests use a counting fake to assert the mirror runs exactly when the TS would call it.
type RegistrySyncer interface {
	SyncJSONRegistry() error
}

// JSONRegistrySync adapts infra/jsonregistry.SyncJSONRegistry — which takes an injected
// DocumentSource — to the zero-argument syncJSONRegistry() the TS calls.
type JSONRegistrySync struct {
	DB   RegistrySource
	Path string
}

// SyncJSONRegistry reads the store's document rows and writes the JSON mirror atomically.
func (s JSONRegistrySync) SyncJSONRegistry() error {
	if s.DB == nil {
		return errors.New("relocalize: JSONRegistrySync requires a document source")
	}
	return jsonregistry.SyncJSONRegistry(s.Path, func() ([]jsonregistry.DocumentRecord, error) {
		docs, err := s.DB.GetAllDocuments()
		if err != nil {
			return nil, err
		}
		out := make([]jsonregistry.DocumentRecord, 0, len(docs))
		for _, d := range docs {
			out = append(out, jsonregistry.DocumentRecord{
				ID:               d.ID,
				Checksum:         d.Checksum,
				Title:            d.Title,
				Registre:         d.Registre,
				Date:             d.Date,
				Category:         d.Category,
				Subcategory:      d.Subcategory,
				Summary:          d.Summary,
				Tags:             d.Tags,
				RawText:          d.RawText,
				MarkdownContent:  d.MarkdownContent,
				TotalAmount:      d.TotalAmount,
				VatAmount:        d.VatAmount,
				Siren:            d.Siren,
				Iban:             d.Iban,
				ExpiryDate:       d.ExpiryDate,
				ContactName:      d.ContactName,
				ContactEmail:     d.ContactEmail,
				ContactPhone:     d.ContactPhone,
				ContactAddress:   d.ContactAddress,
				ContactWebsite:   d.ContactWebsite,
				OriginalFilename: d.OriginalFilename,
				OriginalPath:     d.OriginalPath,
				NewPath:          d.NewPath,
				FileType:         d.FileType,
				SourceImagePath:  d.SourceImagePath,
				Embedding:        d.Embedding,
				Status:           d.Status,
				CreatedAt:        d.CreatedAt,
				UpdatedAt:        d.UpdatedAt,
			})
		}
		return out, nil
	})
}

// ManualDecisions is the injected manual-decision writer. *store/manualdecisions.Store satisfies it.
type ManualDecisions interface {
	RecordManualDecision(record manualdecisions.Record)
}

// Logger is the slice of *infra/logger.Logger this package logs through (TS module names
// RELOCALIZE / REPAIR / DELETE). Nil is silent, matching the classify port's convention.
type Logger interface {
	Info(moduleName, message string, meta any, filename ...string)
	Warn(moduleName, message string, meta any, filename ...string)
}

// Compile-time proof that the real collaborators satisfy the injected seams.
var (
	_ DocumentStore          = (*database.Store)(nil)
	_ guards.CategoriesStore = (*categories.Store)(nil)
	_ Extractor              = PDFExtractor{}
	_ RegistrySyncer         = JSONRegistrySync{}
	_ ManualDecisions        = (*manualdecisions.Store)(nil)
	_ Logger                 = (*logger.Logger)(nil)
)

// Deps is the explicit injected surface. Construct it in the composition root; tests build it with
// fakes and a temp SQLite store.
type Deps struct {
	Config     settings.Config
	DB         DocumentStore
	Categories guards.CategoriesStore
	Extractor  Extractor
	Classifier Classifier
	Registry   RegistrySyncer
	Decisions  ManualDecisions
	Log        Logger
}

// RelocalizeResult mirrors the TS return `{ newPath, moved }`.
type RelocalizeResult struct {
	NewPath string
	Moved   bool
}

// ReclassifyResult mirrors the TS return object of reclassifyAndRelocalizeDocument. The TS optional
// fields are the zero value when absent; Error is set on a handled failure and is not a Go error.
type ReclassifyResult struct {
	Success      bool
	StaleCleaned bool
	Error        string
	Message      string
	Document     *database.DocumentRecord
}

// DeleteResult mirrors the TS return object of deleteDocumentAndMoveToTrash.
type DeleteResult struct {
	Success bool
	Error   string
	Message string
}

func (d Deps) info(module, message string, meta map[string]any) {
	if d.Log != nil {
		d.Log.Info(module, message, meta)
	}
}

func (d Deps) warn(module, message string, meta map[string]any) {
	if d.Log != nil {
		d.Log.Warn(module, message, meta)
	}
}

// syncRegistry runs the JSON mirror when one is injected. Nil is a no-op so a Deps without a mirror
// stays usable; production wiring always sets it.
func (d Deps) syncRegistry() error {
	if d.Registry == nil {
		return nil
	}
	return d.Registry.SyncJSONRegistry()
}

// location projects the document fields guards.FindActualFileOnDisk reads.
func (d Deps) location(doc *database.DocumentRecord) guards.DocumentLocation {
	return guards.DocumentLocation{
		NewPath:          doc.NewPath,
		OriginalPath:     doc.OriginalPath,
		OriginalFilename: doc.OriginalFilename,
	}
}

// RelocalizeFileIfNeeded ports relocalizeFileIfNeeded (relocalize-document.ts:47). It computes the
// canonical target path IN-PROCESS with canonicalpath.ComputeCanonicalPath (at cutover the HTTP hop
// to the pdf-triage-pdf2w canonical-path service disappears), and moves the file there without ever
// overwriting an existing target.
func (d Deps) RelocalizeFileIfNeeded(filePath, category string, subcategory, dateStr, title *string) (RelocalizeResult, error) {
	originalFilename := filepath.Base(filePath)
	targetPath := canonicalpath.ComputeCanonicalPath(filePath, category, d.Config.OutputRootDir, subcategory, dateStr, title)
	targetFilename := filepath.Base(targetPath)

	normTarget := strings.ToLower(filepath.Clean(targetPath))
	normCurrent := strings.ToLower(filepath.Clean(filePath))

	if normTarget == normCurrent {
		return RelocalizeResult{NewPath: filePath, Moved: false}, nil
	}

	isRenamed := strings.ToLower(originalFilename) != strings.ToLower(targetFilename)
	isRelocatedFolder := filepath.Dir(normCurrent) != filepath.Dir(normTarget)

	// The TS `subcategory || 'general'` for the decision log.
	subForLog := "general"
	if subcategory != nil && *subcategory != "" {
		subForLog = *subcategory
	}

	switch {
	case isRenamed && isRelocatedFolder:
		d.info("RELOCALIZE", fmt.Sprintf("Decision: Moving folder & renaming file '%s' ➔ '%s'", originalFilename, targetFilename), map[string]any{
			"from": filePath, "to": targetPath, "category": category, "subcategory": subForLog,
		})
	case isRenamed:
		d.info("RELOCALIZE", fmt.Sprintf("Decision: Renaming file '%s' ➔ '%s'", originalFilename, targetFilename), map[string]any{
			"from": filePath, "to": targetPath,
		})
	case isRelocatedFolder:
		d.info("RELOCALIZE", fmt.Sprintf("Decision: Moving file to folder '__archive/%s/%s'", category, subForLog), map[string]any{
			"from": filePath, "to": targetPath,
		})
	}

	targetDir := filepath.Dir(targetPath)
	if !pathExists(targetDir) {
		if err := os.MkdirAll(targetDir, 0o755); err != nil {
			return RelocalizeResult{}, err
		}
	}

	finalTarget, err := renameAtomicNoOverwrite(filePath, targetPath, maxRenameAttempts)
	if err != nil {
		return RelocalizeResult{}, err
	}
	removeEmptyDirAndParent(filePath)

	return RelocalizeResult{NewPath: finalTarget, Moved: true}, nil
}

// MoveBackToRaws ports moveBackToRaws (relocalize-document.ts:108). Golden Rules 15/16: a file is
// moved back to __raws, never deleted. When a checksum is supplied the matching document row (and
// its FTS entry) is removed through the store's named DeleteDocument.
func (d Deps) MoveBackToRaws(filePath string, checksum *string) (string, error) {
	filename := filepath.Base(filePath)
	desiredTargetPath := filepath.Join(d.Config.InputDir, filename)

	d.warn("REPAIR", fmt.Sprintf("Moving file '%s' back to __raws", filename), map[string]any{"targetPath": desiredTargetPath})

	targetPath := filePath
	if strings.ToLower(filepath.Clean(desiredTargetPath)) != strings.ToLower(filepath.Clean(filePath)) {
		moved, err := renameAtomicNoOverwrite(filePath, desiredTargetPath, maxRenameAttempts)
		if err != nil {
			return "", err
		}
		targetPath = moved
	}

	if checksum != nil && *checksum != "" {
		existing, err := d.DB.GetDocumentByChecksum(*checksum)
		if err != nil {
			return "", err
		}
		if existing != nil {
			// TS deletes documents then documents_fts; store/database.DeleteDocument does both, with
			// the FTS delete best-effort.
			if err := d.DB.DeleteDocument(existing.ID); err != nil {
				return "", err
			}
		}
	}

	removeEmptyDirAndParent(filePath)

	return targetPath, nil
}

// ReclassifyAndRelocalizeDocument ports reclassifyAndRelocalizeDocument
// (relocalize-document.ts:191): re-extract the file, optionally re-classify it, auto-create the
// taxonomy branch, relocalize, persist the winning record, log the manual decision and sync the
// JSON mirror.
//
// explicitCategory / explicitSubcategory / userFeedbackReason are pointers so the TS `undefined`
// (absent) is distinguishable from `""`; see the package comment gap 3.
func (d Deps) ReclassifyAndRelocalizeDocument(id int64, explicitCategory, explicitSubcategory, userFeedbackReason *string) (ReclassifyResult, error) {
	doc, err := d.DB.GetDocumentByID(id)
	if err != nil {
		return ReclassifyResult{}, err
	}
	if doc == nil {
		return ReclassifyResult{Success: false, Error: "Document not found"}, nil
	}

	if explicitSubcategory != nil && guards.IsForbiddenSubcategory(*explicitSubcategory) {
		v := guards.ForbiddenSubcategoryViolation(*explicitSubcategory)
		return ReclassifyResult{Success: false, Error: v.Message}, nil
	}

	actualPath := guards.FindActualFileOnDisk(d.location(doc), d.Config.InputDir, d.Config.OutputRootDir)
	if actualPath == "" || !pathExists(actualPath) {
		d.info("RELOCALIZE", fmt.Sprintf("Purging stale ghost database record ID %d (%s) - missing on disk", id, doc.Title), nil)
		if err := d.DB.DeleteDocument(id); err != nil {
			return ReclassifyResult{}, err
		}
		if err := d.syncRegistry(); err != nil {
			return ReclassifyResult{}, err
		}
		return ReclassifyResult{
			Success:      false,
			StaleCleaned: true,
			Error:        fmt.Sprintf("Physical file '%s' was missing on disk. Cleaned up stale record.", firstNonEmpty(doc.OriginalFilename, doc.Title)),
		}, nil
	}

	extracted, err := d.Extractor.ExtractPDFContent(actualPath)
	if err != nil {
		return ReclassifyResult{}, err
	}
	freshText := extracted.RawText
	storedText := doc.RawText
	// pdf2w's structured Markdown, when the fresh extraction adopted it — only usable when the fresh
	// text is the input actually re-analyzed (the stored text predates pdf2w, so its markdown must
	// not replace a markdown built from stored text).
	freshDoclingMarkdown := extracted.Pdf2wMarkdown

	// A re-analysis must never make the record WORSE than it already was.
	//
	// The file's bytes are unchanged, so the stored text — produced by a healthy extraction — is
	// strictly the better input whenever the fresh one came out of the Tesseract availability
	// fallback instead of PaddleOCR. The two engines are not comparable on a photographed document:
	// on a photographed ID card PaddleOCR returned the clean numbered form fields where Tesseract
	// returned line noise ('3 > U NI NV me').
	//
	// The old test here was `raw_text.trim().length > 10` — a LIVENESS check, not a quality one. It
	// could not tell the two apart, so 346 chars of OCR noise replaced 433 chars of clean text and
	// the title, date, summary and markdown were all rebuilt from the noise. The document was then
	// physically moved into the wrong year folder, with nothing in the record to say why.
	freshUsable := utf16Len(jsTrim(freshText)) > 10
	rejectDegraded := extracted.OCRDegraded && utf16Len(jsTrim(storedText)) > 10
	if rejectDegraded {
		d.warn("RELOCALIZE", fmt.Sprintf(
			"Re-extraction of '%s' fell back to the degraded OCR engine (%d chars) — keeping the %d chars of stored text from the original extraction rather than re-analyzing from worse input.",
			filepath.Base(actualPath), utf16Len(jsTrim(freshText)), utf16Len(jsTrim(storedText)),
		), map[string]any{"documentId": id})
	}
	// Degraded text still beats NO text: the guard prevents a downgrade, it does not make
	// re-analysis impossible for a document that never had usable text to begin with.
	useFreshText := freshUsable && !rejectDegraded
	textToAnalyze := storedText
	if useFreshText {
		textToAnalyze = freshText
	}

	newCategory := doc.Category
	newSubcategory := doc.Subcategory
	newTitle := doc.Title
	newDate := doc.Date
	newSummary := doc.Summary
	newMarkdown := doc.MarkdownContent

	explicitCat := deref(explicitCategory)
	explicitSub := deref(explicitSubcategory)
	feedback := deref(userFeedbackReason)

	if explicitCat != "" && explicitSub != "" {
		// User explicitly chose Category & Subcategory from Modal
		newCategory = strings.ToLower(jsTrim(explicitCat))
		newSubcategory = strings.ToLower(jsTrim(explicitSub))
	} else {
		// Re-run Qwen 3.5 AI with optional user feedback note. When the fresh Docling extraction was
		// adopted AND is the text actually being analyzed, its deterministic Markdown rides along so
		// Step C's LLM conversion is skipped; otherwise call exactly as before (absent docling) so the
		// historical call shape — and tests asserting it — never change just because Docling is
		// configured elsewhere.
		d.info("RELOCALIZE", fmt.Sprintf("Re-analyzing document content with AI for ID %d (%s)...", id, doc.Title), map[string]any{"userFeedbackReason": feedback})
		doclingMd := ""
		if useFreshText {
			doclingMd = freshDoclingMarkdown
		}
		filename := firstNonEmpty(doc.OriginalFilename, filepath.Base(actualPath))

		var meta documentschema.DocumentMetadata
		var classifyErr error
		// TS `doclingMd && doclingMd.trim().length > 0`.
		if jsTrim(doclingMd) != "" {
			meta, classifyErr = d.Classifier.ClassifyPDFText(textToAnalyze, filename, feedback, time.Time{}, doclingMd)
		} else {
			meta, classifyErr = d.Classifier.ClassifyPDFText(textToAnalyze, filename, feedback, time.Time{}, "")
		}
		if classifyErr != nil {
			return ReclassifyResult{}, classifyErr
		}

		newCategory = meta.Categorie
		newSubcategory = meta.Subcategorie
		newTitle = firstNonEmpty(meta.Titre, doc.Title)
		newDate = firstNonEmpty(meta.Date, doc.Date)
		newSummary = firstNonEmpty(meta.Summary, doc.Summary)
		newMarkdown = firstNonEmpty(meta.MarkdownContent, doc.MarkdownContent)
	}

	newCategory = strings.ToLower(jsTrim(newCategory))
	newSubcategory = strings.ToLower(jsTrim(newSubcategory))
	if newCategory != "" && newSubcategory != "" {
		if err := guards.EnsureCategoryAndSubcategoryExist(d.Categories, newCategory, newSubcategory); err != nil {
			return ReclassifyResult{}, err
		}
	}

	relocalized, err := d.RelocalizeFileIfNeeded(actualPath, newCategory, &newSubcategory, &newDate, &newTitle)
	if err != nil {
		return ReclassifyResult{}, err
	}
	newPath := relocalized.NewPath
	moved := relocalized.Moved

	updates := database.DocumentUpdates{
		Title:           &newTitle,
		Category:        &newCategory,
		Subcategory:     &newSubcategory,
		Date:            &newDate,
		Summary:         &newSummary,
		MarkdownContent: &newMarkdown,
		NewPath:         &newPath,
		Status:          ptrString("MOVED"),
	}
	// Persist the text the rest of this update was actually derived from. Leaving it behind is what
	// let the record contradict itself — a conclusion rebuilt from new text sitting next to the old
	// evidence, with no way for the user (or the next reader) to see the mismatch.
	if useFreshText {
		updates.RawText = &freshText
	}
	if _, err := d.DB.UpdateDocumentRecord(id, updates); err != nil {
		return ReclassifyResult{}, err
	}

	if doc.Category != newCategory || doc.Subcategory != newSubcategory || feedback != "" || explicitCat != "" {
		reason := feedback
		if reason == "" {
			if explicitCat != "" {
				reason = "Manual user selection"
			} else {
				reason = "AI re-analysis"
			}
		}
		if d.Decisions != nil {
			d.Decisions.RecordManualDecision(manualdecisions.Record{
				DocumentID:         id,
				Checksum:           doc.Checksum,
				OriginalFilename:   firstNonEmpty(doc.OriginalFilename, filepath.Base(actualPath)),
				Title:              newTitle,
				OldCategory:        doc.Category,
				OldSubcategory:     doc.Subcategory,
				NewCategory:        newCategory,
				NewSubcategory:     newSubcategory,
				UserFeedbackReason: reason,
				RawTextSnippet:     textToAnalyze,
			})
		}
	}

	if err := d.syncRegistry(); err != nil {
		return ReclassifyResult{}, err
	}
	updatedDoc, err := d.DB.GetDocumentByID(id)
	if err != nil {
		return ReclassifyResult{}, err
	}

	message := fmt.Sprintf("📍 Document re-analyzed & confirmed in canonical location: %s / %s", strings.ToUpper(newCategory), strings.ToUpper(newSubcategory))
	if moved {
		message = fmt.Sprintf("📍 Re-analyzed & relocated document to: %s / %s", strings.ToUpper(newCategory), strings.ToUpper(newSubcategory))
	}

	return ReclassifyResult{Success: true, Message: message, Document: updatedDoc}, nil
}

// DeleteDocumentAndMoveToTrash ports deleteDocumentAndMoveToTrash (relocalize-document.ts:345).
// Golden Rule 15: the physical file is MOVED to __raws/.delete_files and preserved; only the DB row
// is removed. A missing file never blocks un-registering the stale row.
func (d Deps) DeleteDocumentAndMoveToTrash(id int64) (DeleteResult, error) {
	doc, err := d.DB.GetDocumentByID(id)
	if err != nil {
		return DeleteResult{}, err
	}
	if doc == nil {
		return DeleteResult{Success: false, Error: "Document not found"}, nil
	}

	trashDir := filepath.Join(d.Config.InputDir, ".delete_files")
	if !pathExists(trashDir) {
		if err := os.MkdirAll(trashDir, 0o755); err != nil {
			return DeleteResult{}, err
		}
	}

	actualPath := guards.FindActualFileOnDisk(d.location(doc), d.Config.InputDir, d.Config.OutputRootDir)
	trashPath := ""

	if actualPath != "" && pathExists(actualPath) {
		desiredPath := filepath.Join(trashDir, filepath.Base(actualPath))
		moved, err := renameAtomicNoOverwrite(actualPath, desiredPath, maxRenameAttempts)
		if err != nil {
			return DeleteResult{}, err
		}
		trashPath = moved
		removeEmptyDirAndParent(actualPath)
	}

	if err := d.DB.DeleteDocument(id); err != nil {
		return DeleteResult{}, err
	}
	if err := d.syncRegistry(); err != nil {
		return DeleteResult{}, err
	}

	d.info("DELETE", fmt.Sprintf("Deleted document ID %d (%s) and moved file to __raws/.delete_files", id, doc.Title), map[string]any{"trashPath": trashPath})

	return DeleteResult{Success: true, Message: fmt.Sprintf("🗑️ Document '%s' un-registered and moved to __raws/.delete_files", doc.Title)}, nil
}
