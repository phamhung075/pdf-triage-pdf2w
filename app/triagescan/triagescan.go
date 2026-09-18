// Package triagescan is a Go port of pdf-triage's src/application/triage-scan.ts (648 lines): the
// live scan pipeline, runTriageScan.
//
// Pipeline, in the TS order (triage-scan.ts:67-544):
//
//  1. Reload/ensure configuration, then the scan-level Ollama gate. When the model is unreachable
//     the scan STOPS before touching a single file — no bundling, no extraction, no OCR, no
//     classification, no move — and emits OLLAMA_DOWN. The rule-based fallback must NOT run: that
//     is exactly what misfiled three documents on 2026-08-31. The OLLAMA_DOWN reminder is
//     rate-limited to one broadcast per 60 seconds because the 10s auto-watcher keeps calling the
//     scan while Ollama is down.
//  2. Photo-only folder bundling (app/convertimage FindImageBundleFolders + ConvertImageFolderToPdf)
//     BEFORE the file walk, so the resulting PDF is picked up by this same scan. Bundling is an
//     enhancement, never a gate: a failure logs a warning and the walk triages the folder's photos
//     individually.
//  3. The recursive walk of INPUT_DIR (infra/pdfscanner GetPDFsRecursively, ignoring
//     OUTPUT_ROOT_DIR and the hidden/.duplicates/.blocked directories).
//  4. pruneBlockedFiles, then the SCAN_STARTED progress event.
//  5. Per file: abort poll, previously-blocked short-circuit, image->PDF conversion
//     (app/convertimage ConvertImageToPdf), extraction (infra/pdfextractor -> pdf2w), the
//     Golden Rule 3 no-text block, the checksum pre-check dedup, classification (app/classify),
//     the Golden Rule 4 forbidden-subcategory block, the pre-registration quality gate
//     (extractionqualitygate), embedding (infra/ollama), the insert (store/database named
//     methods), and the relocalize+move (app/relocalize).
//  6. cleanEmptyDirectories, the JSON mirror sync (infra/jsonregistry), SCAN_COMPLETED.
//
// # The insert-time UNIQUE-checksum collision (triage-scan.ts:373-418)
//
// Another checksum-owning row can appear between the pre-check and the insert: classifyPDFText's
// Step A/C/D round-trip takes tens of seconds, a wide window for a concurrent scan/repair/manual
// edit to insert the same content first. The TS code detects
// `/UNIQUE constraint failed.*checksum/i`, fetches the owner, moves the incoming file to
// __raws/.duplicates_files and reports SKIPPED_DUPLICATE. This package reproduces that through the
// shared guard app/guards.IsChecksumUniqueViolation, so the file is never left in __raws to be
// re-classified by every subsequent auto-watcher tick. It is not the fix for a real concurrent
// scan; the lock is (see below).
//
// # Who acquires the scan lock
//
// app/scanlock (scanlock.AcquireScanLock(dataDir)) is acquired by the CALLER, not by
// RunTriageScan. The HTTP layer / auto-watcher must claim its in-memory `isAutoScanning` guard and
// the cross-process scan lock synchronously before calling, and release after the call returns
// (web-server.ts:1426-1434 documents that ordering as the fix for the concurrent-scan checksum
// failure). The `npm run scan` CLI and the MCP server do the same. RunTriageScan deliberately does
// NOT take the lock itself because in the Go server the in-memory guard lives in httpapi and the
// two must be claimed in one ordering; re-acquiring here would deadlock a caller that already
// holds it (the in-process half of app/scanlock rejects re-entry).
//
// # Cancellation
//
// ctx and the injected shouldAbort func are both polled once per file, the only cancellation point
// runTriageScan has. shouldAbort is the HTTP layer's scanAbortRequested flag (POST
// /api/triage/unlock). When either trips, the loop breaks, the remaining files stay in __raws for
// the next scan, and the normal clean-up (cleanEmptyDirectories + JSON sync + SCAN_COMPLETED) still
// runs.
//
// # Injection
//
// The TS imported infrastructure singletons directly. The port takes a Deps struct whose fields are
// the narrow interfaces the pipeline needs, so tests substitute fakes and the composition root
// wires the real stores. *database.Store, *convertimage.Converter, classify.Deps, relocalize.Deps,
// *ollama.Client, *logger.Logger and JSONRegistrySync all satisfy those seams (compile-time
// assertions below).
package triagescan

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/app/classify"
	"github.com/phamhung075/pdf-triage-pdf2w/app/convertimage"
	"github.com/phamhung075/pdf-triage-pdf2w/app/guards"
	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/documentschema"
	"github.com/phamhung075/pdf-triage-pdf2w/extractionqualitygate"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/jsonregistry"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/ollama"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfextractor"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/pdfscanner"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
)

const (
	// moduleTriage is the TS `logger` module name.
	moduleTriage = "TRIAGE"
	// ollamaDownBroadcastCooldown is OLLAMA_DOWN_BROADCAST_COOLDOWN_MS (triage-scan.ts:55).
	ollamaDownBroadcastCooldown = 60 * time.Second
	// perFileDelay is the 50ms `await new Promise(resolve => setTimeout(resolve, 50))` the TS
	// awaits between files (triage-scan.ts:175, :238, :274, :316, :323, :416, :502, :521).
	perFileDelay = 50 * time.Millisecond
)

// --- logger seams -------------------------------------------------------------------------------

// DocumentLogger is the per-document logger the TS `logger.forDocument(file)` returns. Nil is
// silent.
type DocumentLogger interface {
	Info(moduleName, message string, meta any)
	Warn(moduleName, message string, meta any)
}

// Logger is the slice of *infra/logger.Logger this package logs through (module name TRIAGE). Nil
// is silent, matching the convention of the sibling ports.
type Logger interface {
	Info(moduleName, message string, meta any, filename ...string)
	Warn(moduleName, message string, meta any, filename ...string)
	Error(moduleName, message string, meta any, filename ...string)
	ForDocument(filename string) DocumentLogger
}

// loggerAdapter adapts *infra/logger.Logger to the Logger interface. The adapter exists because
// *logger.Logger.ForDocument returns the concrete *logger.DocumentLogger, which a fake cannot
// substitute through an interface-returning method.
type loggerAdapter struct{ inner *logger.Logger }

func (a loggerAdapter) Info(moduleName, message string, meta any, filename ...string) {
	a.inner.Info(moduleName, message, meta, filename...)
}

func (a loggerAdapter) Warn(moduleName, message string, meta any, filename ...string) {
	a.inner.Warn(moduleName, message, meta, filename...)
}

func (a loggerAdapter) Error(moduleName, message string, meta any, filename ...string) {
	a.inner.Error(moduleName, message, meta, filename...)
}

func (a loggerAdapter) ForDocument(filename string) DocumentLogger {
	return a.inner.ForDocument(filename)
}

// AdaptLogger wraps a *infra/logger.Logger for Deps.Log. A nil logger yields a nil Logger, which
// the pipeline treats as silent.
func AdaptLogger(l *logger.Logger) Logger {
	if l == nil {
		return nil
	}
	return loggerAdapter{inner: l}
}

// --- collaborator seams -------------------------------------------------------------------------

// DocumentStore is the store/database surface the scan needs. *store/database.Store satisfies it.
// Every write goes through a named method; this package issues no raw SQL.
type DocumentStore interface {
	GetBlockedFile(originalPath string) (*database.BlockedFileRecord, error)
	UpsertBlockedFile(entry database.BlockedFileEntry) error
	DeleteBlockedFile(originalPath string) error
	PruneBlockedFiles(existingPaths []string) error
	GetDocumentByChecksum(checksum string) (*database.DocumentRecord, error)
	InsertDocumentRecord(doc database.NewDocument) (int64, error)
	UpdateDocumentRecord(id any, updates database.DocumentUpdates) (bool, error)
}

// RegistrySource is the read-only store surface JSONRegistrySync needs.
type RegistrySource interface {
	GetAllDocuments() ([]database.DocumentRecord, error)
}

// RegistrySyncer is the injected JSON-mirror writer (TS `syncJSONRegistry`). Nil skips the sync.
type RegistrySyncer interface {
	SyncJSONRegistry() error
}

// JSONRegistrySync adapts infra/jsonregistry.SyncJSONRegistry — which takes an injected
// DocumentSource — to the zero-argument syncJSONRegistry() the TS calls. It is a local twin of
// relocalize.JSONRegistrySync so this package has no dependency on that package for the mirror.
type JSONRegistrySync struct {
	DB   RegistrySource
	Path string
}

// SyncJSONRegistry reads the store's document rows and writes the JSON mirror atomically.
func (s JSONRegistrySync) SyncJSONRegistry() error {
	if s.DB == nil {
		return errors.New("triagescan: JSONRegistrySync requires a document source")
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

// Extractor is the injected pdf2w extraction seam (TS extractPDFContent).
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

// ImageConverter is the photo -> archivable PDF seam (convert-image-document.ts).
type ImageConverter interface {
	ConvertImageToPdf(ctx context.Context, imagePath string) (convertimage.ConvertedImageDocument, error)
	ConvertImageFolderToPdf(ctx context.Context, folderPath string) (convertimage.ConvertedImageDocument, error)
}

// Classifier is the injected classification seam. Its single method matches
// classify.Deps.ClassifyPDFText, the port of src/application/classify-document.ts:383.
type Classifier interface {
	ClassifyPDFText(rawText, filename, previousError string, now time.Time, doclingMarkdown string) (documentschema.DocumentMetadata, error)
}

// OllamaClient is the model-ensure + embedding seam. *infra/ollama.Client satisfies it.
type OllamaClient interface {
	EnsureOllamaModel(modelName string) bool
	GenerateEmbedding(text string) []float64
}

// Relocalizer is the injected move seam. relocalize.Deps satisfies it.
type Relocalizer interface {
	RelocalizeFileIfNeeded(filePath, category string, subcategory, dateStr, title *string) (relocalize.RelocalizeResult, error)
}

// Compile-time proof that the real collaborators satisfy the injected seams.
var (
	_ DocumentStore  = (*database.Store)(nil)
	_ RegistrySource = (*database.Store)(nil)
	_ Extractor      = PDFExtractor{}
	_ ImageConverter = (*convertimage.Converter)(nil)
	_ Classifier     = classify.Deps{}
	_ OllamaClient   = (*ollama.Client)(nil)
	_ Relocalizer    = relocalize.Deps{}
	_ RegistrySyncer = JSONRegistrySync{}
	_ Logger         = loggerAdapter{}
)

// Deps is the explicit injected surface. DB, Extractor, Classifier, Ollama and Relocalizer are
// required for a real run; Converter may be nil (images are then extracted as-is, exactly as the
// TS conversion-failure fallback treats them). Log, Registry, the optional reload/ensure hooks,
// FindBundleFolders and Now may be nil.
type Deps struct {
	// Config is the effective settings snapshot (TS CONFIG).
	Config settings.Config

	// ReloadConfig, when set, refreshes Config from disk at the start of a run (TS
	// reloadConfigFromDisk). The returned config is used for that run.
	ReloadConfig func() settings.Config

	// EnsureDirectories, when set, creates the input/output directories (TS
	// ensureDirectoriesExist). When nil, settings.EnsureDirectoriesExist(Config) is used.
	EnsureDirectories func() error

	DB          DocumentStore
	Extractor   Extractor
	Converter   ImageConverter
	Classifier  Classifier
	Ollama      OllamaClient
	Relocalizer Relocalizer
	Registry    RegistrySyncer
	Log         Logger

	// FindBundleFolders defaults to convertimage.FindImageBundleFolders.
	FindBundleFolders func(rootDir string) []string

	// Now defaults to time.Now; it drives the OLLAMA_DOWN cooldown.
	Now func() time.Time
}

// Scanner runs the scan pipeline with one injected Deps set. The 60s OLLAMA_DOWN cooldown lives on
// the Scanner, so one Scanner per data directory reproduces the TS module-level state; a fresh
// Scanner (as each test builds) starts with an expired cooldown.
type Scanner struct {
	deps Deps

	mu               sync.Mutex
	lastOllamaDownAt time.Time
}

// New returns a Scanner. No work happens until RunTriageScan.
func New(deps Deps) *Scanner {
	return &Scanner{deps: deps}
}

// OllamaDownReminder is ollamaDownReminder (triage-scan.ts:57-59), byte-for-byte.
func OllamaDownReminder(model string) string {
	return fmt.Sprintf("⛔ Ollama is down — %s unreachable. Start Ollama, then re-scan. No files were processed.", model)
}

// RunTriageScan is runTriageScan. It does NOT acquire app/scanlock; see the package comment for who
// does. onProgress and shouldAbort may be nil.
func (s *Scanner) RunTriageScan(ctx context.Context, onProgress func(Event), shouldAbort func() bool) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cfg := s.deps.Config
	if s.deps.ReloadConfig != nil {
		cfg = s.deps.ReloadConfig()
	}
	ensure := s.deps.EnsureDirectories
	if ensure == nil {
		ensure = func() error { return settings.EnsureDirectoriesExist(cfg) }
	}
	if err := ensure(); err != nil {
		return Result{}, err
	}

	if s.deps.Ollama == nil {
		return Result{}, errors.New("triagescan: Deps.Ollama is required")
	}
	if s.deps.DB == nil {
		return Result{}, errors.New("triagescan: Deps.DB is required")
	}

	// --- Ollama-down gate, BEFORE touching a single file --------------------------------------
	modelUp := s.deps.Ollama.EnsureOllamaModel(cfg.OllamaModel)
	if !modelUp {
		message := OllamaDownReminder(cfg.OllamaModel)
		logWarn(s.deps.Log, moduleTriage, message, nil)
		now := s.nowTime()
		s.mu.Lock()
		shouldBroadcast := now.Sub(s.lastOllamaDownAt) >= ollamaDownBroadcastCooldown
		if shouldBroadcast {
			s.lastOllamaDownAt = now
		}
		s.mu.Unlock()
		if shouldBroadcast {
			emitEvent(onProgress, Event{
				Type:           EventOllamaDown,
				Message:        message,
				ScannedCount:   intPtr(0),
				ProcessedCount: intPtr(0),
				SkippedCount:   intPtr(0),
				TotalFiles:     intPtr(0),
			})
		}
		return Result{
			ScannedCount:   0,
			ProcessedCount: 0,
			SkippedCount:   0,
			Items:          []ResultItem{},
			OllamaDown:     true,
			Message:        message,
		}, nil
	}

	// --- photo-only folder bundling, before the walk ------------------------------------------
	findFolders := s.deps.FindBundleFolders
	if findFolders == nil {
		findFolders = convertimage.FindImageBundleFolders
	}
	if s.deps.Converter != nil {
		for _, bundleDir := range findFolders(cfg.InputDir) {
			bundled, err := s.deps.Converter.ConvertImageFolderToPdf(ctx, bundleDir)
			if err != nil {
				// Bundling is an enhancement, never a gate. Leave the folder alone and let the walk
				// below triage its photos individually rather than blocking readable documents.
				logWarn(s.deps.Log, moduleTriage, fmt.Sprintf(
					"Could not bundle folder '%s', its photos will be triaged individually: %s",
					bundleDir, err.Error()), nil)
				continue
			}
			logInfo(s.deps.Log, moduleTriage, fmt.Sprintf(
				"Bundled folder '%s' into a %d-page PDF", filepath.Base(bundleDir), bundled.PageCount),
				map[string]any{"bundleDir": bundleDir, "pdfPath": bundled.PdfPath})
			emitEvent(onProgress, Event{
				Type:     EventFileProgress,
				Filename: filepath.Base(bundled.PdfPath),
				Message:  fmt.Sprintf("Bundled %d photos from '%s' into one PDF", bundled.PageCount, filepath.Base(bundleDir)),
			})
		}
	}

	// --- walk ---------------------------------------------------------------------------------
	pdfFilePaths := pdfscanner.GetPDFsRecursively(cfg.InputDir, cfg.OutputRootDir)
	filenames := make([]string, 0, len(pdfFilePaths))
	for _, p := range pdfFilePaths {
		filenames = append(filenames, filepath.Base(p))
	}
	if err := s.deps.DB.PruneBlockedFiles(pdfFilePaths); err != nil {
		return Result{}, err
	}

	emitEvent(onProgress, Event{
		Type:       EventScanStarted,
		TotalFiles: intPtr(len(pdfFilePaths)),
		Files:      &filenames,
	})

	var (
		processedCount int
		skippedCount   int
		scannedCount   int
	)
	totalFiles := len(pdfFilePaths)
	items := []ResultItem{}

	abort := func() bool {
		if ctx.Err() != nil {
			return true
		}
		return shouldAbort != nil && shouldAbort()
	}

	for _, incomingPath := range pdfFilePaths {
		if abort() {
			logInfo(s.deps.Log, moduleTriage, fmt.Sprintf(
				"Scan aborted by user after %d of %d file(s). Remaining files stay in __raws for the next scan.",
				scannedCount, totalFiles), nil)
			break
		}
		scannedCount++

		st := &fileState{
			originalPath: incomingPath,
			file:         filepath.Base(incomingPath),
			activePath:   incomingPath,
			scannedCount: scannedCount,
			totalFiles:   totalFiles,
		}
		s.processFile(ctx, cfg, st, &processedCount, &skippedCount, &items, onProgress)

		// The TS `finally` runs for every iteration, including the `continue` branches above, so
		// this sleep is unconditional.
		sleepContext(ctx, perFileDelay)
	}

	CleanEmptyDirectories(cfg.InputDir, cfg.InputDir, s.deps.Log)
	if s.deps.Registry != nil {
		if err := s.deps.Registry.SyncJSONRegistry(); err != nil {
			return Result{}, err
		}
	}

	emitEvent(onProgress, Event{
		Type:           EventScanCompleted,
		ScannedCount:   intPtr(len(pdfFilePaths)),
		ProcessedCount: intPtr(processedCount),
		SkippedCount:   intPtr(skippedCount),
	})

	return Result{
		ScannedCount:   len(pdfFilePaths),
		ProcessedCount: processedCount,
		SkippedCount:   skippedCount,
		Items:          items,
	}, nil
}

// fileState tracks the per-file paths the TS re-points at the generated PDF when the incoming file
// is a photograph. originalPath and activePath mirror each other until image conversion; the catch
// block uses activePath so a quality-gate failure moves the artifact that is actually kept.
type fileState struct {
	originalPath string
	file         string
	activePath   string
	scannedCount int
	totalFiles   int
}

// processFile ports the body of the per-file `for` iteration (triage-scan.ts:143-523), including
// its try/catch. Per-file failures are reported as FILE_FAILED events and never escape, exactly as
// the TS catch swallows them.
func (s *Scanner) processFile(
	ctx context.Context,
	cfg settings.Config,
	st *fileState,
	processedCount, skippedCount *int,
	items *[]ResultItem,
	onProgress func(Event),
) {
	docLog := s.forDocument(st.file)
	docInfo(docLog, moduleTriage, fmt.Sprintf("Starting triage session for incoming document: %s", st.file), nil)

	fileStat, err := os.Stat(st.originalPath)
	if err != nil {
		s.fileFailure(onProgress, cfg, st, *processedCount, err)
		return
	}

	previouslyBlocked, err := s.deps.DB.GetBlockedFile(st.originalPath)
	if err != nil {
		s.fileFailure(onProgress, cfg, st, *processedCount, err)
		return
	}
	if previouslyBlocked != nil && previouslyBlocked.MtimeMs == mtimeMs(fileStat) && previouslyBlocked.Size == fileStat.Size() {
		// Same file content as last blocked attempt: skip re-extraction/re-classification and
		// re-logging so an unfixable file doesn't spam the log every auto-watcher tick.
		emitEvent(onProgress, Event{
			Type:           EventFileFailed,
			Filename:       st.file,
			Stage:          StageFailed,
			ScannedCount:   intPtr(st.scannedCount),
			ProcessedCount: intPtr(*processedCount),
			TotalFiles:     intPtr(st.totalFiles),
			Message:        previouslyBlocked.Message,
		})
		sleepContext(ctx, perFileDelay)
		return
	}
	if previouslyBlocked != nil {
		// File content changed since it was blocked (e.g. user replaced it) — retry fresh.
		if err := s.deps.DB.DeleteBlockedFile(st.originalPath); err != nil {
			s.fileFailure(onProgress, cfg, st, *processedCount, err)
			return
		}
	}

	emitEvent(onProgress, Event{
		Type:           EventFileProgress,
		Filename:       st.file,
		Stage:          StageExtractingText,
		ScannedCount:   intPtr(st.scannedCount),
		ProcessedCount: intPtr(*processedCount),
		TotalFiles:     intPtr(st.totalFiles),
		Message:        "Extracting content layer from file...",
	})

	// A photograph is not an archivable document: run the vision pipeline over it, keep the
	// resulting PDF in its place, and reuse the text that pipeline already read.
	var converted *convertimage.ConvertedImageDocument
	if s.deps.Converter != nil && convertimage.IsImageFile(st.originalPath) {
		c, convErr := s.deps.Converter.ConvertImageToPdf(ctx, st.originalPath)
		if convErr != nil {
			// Conversion is an enhancement, not a gate. Fall through and triage the photo as-is.
			docWarn(docLog, moduleTriage, fmt.Sprintf("Image-to-PDF conversion failed, triaging the photo as-is: %s", convErr.Error()), map[string]any{"filename": st.file})
		} else {
			converted = &c
			st.originalPath = c.PdfPath
			st.file = filepath.Base(c.PdfPath)
			st.activePath = c.PdfPath
		}
	}

	var (
		checksum        string
		rawText         string
		doclingMarkdown string
	)
	if converted != nil {
		checksum = converted.Checksum
		rawText = converted.RawText
	} else {
		extracted, extErr := s.deps.Extractor.ExtractPDFContent(st.originalPath)
		if extErr != nil {
			s.fileFailure(onProgress, cfg, st, *processedCount, extErr)
			return
		}
		checksum = extracted.Checksum
		rawText = extracted.RawText
		// pdf2w's structured Markdown rides along so Step C's LLM chunk-by-chunk conversion is
		// skipped; the pre-registration quality gate below still audits raw_text vs markdown.
		doclingMarkdown = extracted.Pdf2wMarkdown
	}

	// --- Golden Rule 3: no-text block ---------------------------------------------------------
	if guards.IsNoTextBlocked(rawText) {
		movedPath := st.originalPath
		if p, moveErr := MoveBlockedFileToBlockedFolder(cfg.InputDir, st.originalPath); moveErr == nil {
			movedPath = p
		}
		violation := guards.NoTextViolation()
		message := violation.Message
		docWarn(docLog, moduleTriage, "BLOCKED: No text extracted from PDF. Moved to __raws/blocked_files.", map[string]any{"originalPath": movedPath, "filename": st.file})
		if err := s.deps.DB.UpsertBlockedFile(database.BlockedFileEntry{
			OriginalPath: movedPath,
			Filename:     st.file,
			Reason:       violation.BlockedFileReason,
			Message:      message,
			MtimeMs:      mtimeMs(fileStat),
			Size:         fileStat.Size(),
		}); err != nil {
			s.fileFailure(onProgress, cfg, st, *processedCount, err)
			return
		}
		emitEvent(onProgress, Event{
			Type:     EventFileFailed,
			Filename: st.file,
			Stage:    StageFailed,
			Message:  message,
		})
		sleepContext(ctx, perFileDelay)
		return
	}

	// --- checksum pre-check dedup --------------------------------------------------------------
	preCheckOwner, err := s.deps.DB.GetDocumentByChecksum(checksum)
	if err != nil {
		s.fileFailure(onProgress, cfg, st, *processedCount, err)
		return
	}
	if guards.DecideChecksumDuplicate(preCheckOwner != nil) == guards.ChecksumSkipDuplicate {
		movedDupPath := st.originalPath
		if p, moveErr := MoveDuplicateFileToDuplicatesFolder(cfg.InputDir, st.originalPath); moveErr == nil {
			movedDupPath = p
			docInfo(docLog, moduleTriage, fmt.Sprintf("Moved duplicate file to __raws/.duplicates_files (Checksum in DB, ID: %d)", preCheckOwner.ID), map[string]any{"filename": st.file, "docId": preCheckOwner.ID})
		} else {
			docWarn(docLog, moduleTriage, fmt.Sprintf("Skipping duplicate file (Failed to move to .duplicates_files: %s)", moveErr.Error()), map[string]any{"filename": st.file})
		}

		subcategory := subcategoryOrGeneral(preCheckOwner.Subcategory)
		*skippedCount++
		*items = append(*items, ResultItem{
			Filename:    st.file,
			DocID:       preCheckOwner.ID,
			Title:       preCheckOwner.Title,
			Category:    preCheckOwner.Category,
			Subcategory: subcategory,
			NewPath:     movedDupPath,
			Status:      "SKIPPED_DUPLICATE",
		})
		emitEvent(onProgress, Event{
			Type:        EventFileCompleted,
			Filename:    st.file,
			Stage:       StageSkippedDuplicate,
			Message:     "Moved duplicate file to __raws/.duplicates_files (Already in database)",
			DocID:       int64Ptr(preCheckOwner.ID),
			Title:       preCheckOwner.Title,
			Category:    preCheckOwner.Category,
			Subcategory: subcategory,
			NewPath:     movedDupPath,
		})
		sleepContext(ctx, perFileDelay)
		return
	}

	// --- classify ----------------------------------------------------------------------------
	emitEvent(onProgress, Event{
		Type:     EventFileProgress,
		Filename: st.file,
		Stage:    StageAIClassifying,
		Message:  "Analyzing text, title, date & subcategory with Qwen 3.5 AI...",
	})

	var metadata documentschema.DocumentMetadata
	if strings.TrimSpace(doclingMarkdown) != "" {
		metadata, err = s.deps.Classifier.ClassifyPDFText(rawText, st.file, "", time.Time{}, doclingMarkdown)
	} else {
		metadata, err = s.deps.Classifier.ClassifyPDFText(rawText, st.file, "", time.Time{}, "")
	}
	if err != nil {
		s.fileFailure(onProgress, cfg, st, *processedCount, err)
		return
	}

	// --- Golden Rule 4: forbidden-subcategory block ------------------------------------------
	if violation := guards.StrictNoSubcategoryViolation(st.file, metadata.Subcategorie); violation != nil {
		movedPath := st.originalPath
		if p, moveErr := MoveBlockedFileToBlockedFolder(cfg.InputDir, st.originalPath); moveErr == nil {
			movedPath = p
		}
		message := violation.Message
		subcat := strings.ToLower(strings.TrimSpace(metadata.Subcategorie))
		docWarn(docLog, moduleTriage, fmt.Sprintf("BLOCKED: No specific subcategory detected (subcat='%s'). Moved to __raws/.blocked_files.", subcat), map[string]any{"originalPath": movedPath, "filename": st.file})
		if err := s.deps.DB.UpsertBlockedFile(database.BlockedFileEntry{
			OriginalPath: movedPath,
			Filename:     st.file,
			Reason:       violation.BlockedFileReason,
			Message:      message,
			MtimeMs:      mtimeMs(fileStat),
			Size:         fileStat.Size(),
		}); err != nil {
			s.fileFailure(onProgress, cfg, st, *processedCount, err)
			return
		}
		emitEvent(onProgress, Event{
			Type:     EventFileFailed,
			Filename: st.file,
			Stage:    StageFailed,
			Message:  message,
		})
		sleepContext(ctx, perFileDelay)
		return
	}

	// --- pre-registration quality gate ---------------------------------------------------------
	qualityReport := extractionqualitygate.AssessExtractionQuality(rawText, metadata.MarkdownContent)
	if !qualityReport.Pass {
		s.qualityGateFailure(onProgress, cfg, st, *processedCount,
			extractionqualitygate.NewExtractionQualityGateError(st.file, qualityReport))
		sleepContext(ctx, perFileDelay)
		return
	}

	embedding := s.deps.Ollama.GenerateEmbedding(rawText)

	// --- insert ------------------------------------------------------------------------------
	subcategory := subcategoryOrGeneral(metadata.Subcategorie)
	newDoc := database.NewDocument{
		Checksum:         checksum,
		Title:            metadata.Titre,
		Registre:         metadata.Registre,
		Date:             metadata.Date,
		Category:         metadata.Categorie,
		Subcategory:      subcategory,
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
		OriginalFilename: st.file,
		OriginalPath:     st.originalPath,
		Embedding:        embedding,
		SourceImagePath:  sourceImagePath(converted),
		Status:           "PENDING",
	}
	docID, insertErr := s.deps.DB.InsertDocumentRecord(newDoc)
	if insertErr != nil {
		if !guards.IsChecksumUniqueViolation(insertErr.Error()) {
			s.fileFailure(onProgress, cfg, st, *processedCount, insertErr)
			return
		}

		// Another checksum-owning row appeared between the pre-check and this insert. Treat it as a
		// duplicate: fetch the owner, move the incoming file to .duplicates_files and skip.
		collisionOwner, lookupErr := s.deps.DB.GetDocumentByChecksum(checksum)
		if lookupErr != nil {
			s.fileFailure(onProgress, cfg, st, *processedCount, lookupErr)
			return
		}
		movedDupPath := st.originalPath
		if p, moveErr := MoveDuplicateFileToDuplicatesFolder(cfg.InputDir, st.originalPath); moveErr == nil {
			movedDupPath = p
		} else {
			docWarn(docLog, moduleTriage, fmt.Sprintf("Skipping duplicate file (Failed to move to .duplicates_files: %s)", moveErr.Error()), map[string]any{"filename": st.file})
		}

		// The TS fallbacks are `existing?.id ?? -1`, `existing?.title ?? metadata.titre`,
		// `existing?.category ?? metadata.categorie` and `existing?.subcategory || 'general'`.
		// The last one means a missing owner reports 'general', not the freshly classified
		// subcategory, so both the item and the event start there.
		item := ResultItem{
			Filename:    st.file,
			DocID:       -1,
			Title:       metadata.Titre,
			Category:    metadata.Categorie,
			Subcategory: "general",
			NewPath:     movedDupPath,
			Status:      "SKIPPED_DUPLICATE",
		}
		event := Event{
			Type:        EventFileCompleted,
			Filename:    st.file,
			Stage:       StageSkippedDuplicate,
			Message:     "Moved duplicate file to __raws/.duplicates_files (Checksum collided with an existing document)",
			Subcategory: "general",
			NewPath:     movedDupPath,
		}
		if collisionOwner != nil {
			item.DocID = collisionOwner.ID
			item.Title = collisionOwner.Title
			item.Category = collisionOwner.Category
			item.Subcategory = subcategoryOrGeneral(collisionOwner.Subcategory)
			event.DocID = int64Ptr(collisionOwner.ID)
			event.Title = collisionOwner.Title
			event.Category = collisionOwner.Category
			event.Subcategory = item.Subcategory
		}

		*skippedCount++
		*items = append(*items, item)
		emitEvent(onProgress, event)
		sleepContext(ctx, perFileDelay)
		return
	}

	// --- relocalize + move --------------------------------------------------------------------
	emitEvent(onProgress, Event{
		Type:     EventFileProgress,
		Filename: st.file,
		Stage:    StageRelocalizing,
		Message:  fmt.Sprintf("Moving file to __archive/%s/%s/...", metadata.Categorie, subcategory),
	})

	relocated, relErr := s.deps.Relocalizer.RelocalizeFileIfNeeded(
		st.originalPath, metadata.Categorie, &metadata.Subcategorie, &metadata.Date, &metadata.Titre)
	if relErr != nil {
		s.fileFailure(onProgress, cfg, st, *processedCount, relErr)
		return
	}
	finalTargetPath := relocated.NewPath

	if _, err := s.deps.DB.UpdateDocumentRecord(docID, database.DocumentUpdates{
		NewPath: strPtr(finalTargetPath),
		Status:  strPtr("MOVED"),
	}); err != nil {
		s.fileFailure(onProgress, cfg, st, *processedCount, err)
		return
	}

	*processedCount++
	*items = append(*items, ResultItem{
		Filename:    st.file,
		DocID:       docID,
		Title:       metadata.Titre,
		Category:    metadata.Categorie,
		Subcategory: subcategory,
		NewPath:     finalTargetPath,
		Status:      "MOVED",
	})
	emitEvent(onProgress, Event{
		Type:           EventFileCompleted,
		Filename:       st.file,
		Stage:          StageCompleted,
		ScannedCount:   intPtr(st.scannedCount),
		ProcessedCount: intPtr(*processedCount),
		TotalFiles:     intPtr(st.totalFiles),
		Message:        "Successfully triaged & relocated",
		DocID:          int64Ptr(docID),
		Title:          metadata.Titre,
		Category:       metadata.Categorie,
		Subcategory:    subcategory,
		NewPath:        finalTargetPath,
	})
	logInfo(s.deps.Log, moduleTriage, fmt.Sprintf(
		"Successfully triaged '%s' -> ID: %d, Category: %s/%s",
		st.file, docID, metadata.Categorie, metadata.Subcategorie), nil)
}

// qualityGateFailure ports the ExtractionQualityGateError arm of the TS catch
// (triage-scan.ts:468-504). Nothing was inserted or moved when the gate ran, so this moves the
// active artifact to .blocked_files and records WHY.
func (s *Scanner) qualityGateFailure(
	onProgress func(Event),
	cfg settings.Config,
	st *fileState,
	processedCount int,
	gateErr *extractionqualitygate.ExtractionQualityGateError,
) {
	movedPath := st.activePath
	if p, err := MoveBlockedFileToBlockedFolder(cfg.InputDir, st.activePath); err == nil {
		movedPath = p
	}
	var mtime float64
	var size int64
	if info, err := os.Stat(movedPath); err == nil {
		mtime = mtimeMs(info)
		size = info.Size()
	}
	logWarn(s.deps.Log, moduleTriage, "BLOCKED by quality gate: "+gateErr.Error(), map[string]any{"originalPath": movedPath, "filename": st.file})
	if err := s.deps.DB.UpsertBlockedFile(database.BlockedFileEntry{
		OriginalPath: movedPath,
		Filename:     st.file,
		Reason:       "QUALITY_GATE",
		Message:      gateErr.Error(),
		MtimeMs:      mtime,
		Size:         size,
	}); err != nil {
		logWarn(s.deps.Log, moduleTriage, "Failed to record blocked file after quality gate: "+err.Error(), map[string]any{"filename": st.file})
	}
	emitEvent(onProgress, Event{
		Type:           EventFileFailed,
		Filename:       st.file,
		Stage:          StageFailed,
		ScannedCount:   intPtr(st.scannedCount),
		ProcessedCount: intPtr(processedCount),
		TotalFiles:     intPtr(st.totalFiles),
		Message:        gateErr.Error(),
	})
}

// fileFailure ports the generic arm of the TS catch (triage-scan.ts:505-519). An
// OllamaUnavailableError (Ollama dropped after the start gate passed) gets the reminder message
// and leaves the file in __raws — no rule-based fallback, no move.
func (s *Scanner) fileFailure(onProgress func(Event), cfg settings.Config, st *fileState, processedCount int, err error) {
	message := err.Error()
	var down *ollama.OllamaUnavailableError
	if errors.As(err, &down) {
		message = fmt.Sprintf(
			"⛔ Ollama is down — %s unreachable. Start Ollama, then re-scan. '%s' stays in __raws.",
			cfg.OllamaModel, st.file)
	}
	logError(s.deps.Log, moduleTriage, fmt.Sprintf("Error processing file %s: %s", st.file, message), nil)
	emitEvent(onProgress, Event{
		Type:           EventFileFailed,
		Filename:       st.file,
		Stage:          StageFailed,
		ScannedCount:   intPtr(st.scannedCount),
		ProcessedCount: intPtr(processedCount),
		TotalFiles:     intPtr(st.totalFiles),
		Message:        message,
	})
}

// --- small helpers ------------------------------------------------------------------------------

func (s *Scanner) nowTime() time.Time {
	if s.deps.Now != nil {
		return s.deps.Now()
	}
	return time.Now()
}

func (s *Scanner) forDocument(file string) DocumentLogger {
	if s.deps.Log == nil {
		return nil
	}
	return s.deps.Log.ForDocument(file)
}

// sourceImagePath is the TS `converted?.sourceImagePath || ”`.
func sourceImagePath(converted *convertimage.ConvertedImageDocument) string {
	if converted == nil {
		return ""
	}
	return converted.SourceImagePath
}

// subcategoryOrGeneral is the TS `x || 'general'`.
func subcategoryOrGeneral(subcategory string) string {
	if subcategory == "" {
		return "general"
	}
	return subcategory
}

func emitEvent(onProgress func(Event), event Event) {
	if onProgress != nil {
		onProgress(event)
	}
}

// sleepContext is the TS 50ms delay, made cancellable.
func sleepContext(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

func logInfo(l Logger, module, message string, meta any) {
	if l != nil {
		l.Info(module, message, meta)
	}
}

func logWarn(l Logger, module, message string, meta any) {
	if l != nil {
		l.Warn(module, message, meta)
	}
}

func logError(l Logger, module, message string, meta any) {
	if l != nil {
		l.Error(module, message, meta)
	}
}

func docInfo(l DocumentLogger, module, message string, meta any) {
	if l != nil {
		l.Info(module, message, meta)
	}
}

func docWarn(l DocumentLogger, module, message string, meta any) {
	if l != nil {
		l.Warn(module, message, meta)
	}
}
