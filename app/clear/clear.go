// Package clear is a Go port of pdf-triage's src/application/clear-registry.ts (95 lines): the
// DELETE /api/documents "Clear Registry" use case that moves every __archive file back to __raws,
// purges the SQLite documents + FTS index, relocates orphan archive files that have no DB row,
// prunes the now-empty directory skeleton, and re-syncs the JSON registry.
//
// Exported surface (the TS names in parentheses):
//
//   - Deps.ClearRegistryAndMoveArchiveToRaws (clear-registry.ts:10)
//   - ClearResult                            (the TS `{ countMoved }` return)
//   - Event, NewClearStartedEvent, NewFileProgressEvent
//     (the CLEAR_STARTED / FILE_PROGRESS payloads at clear-registry.ts:19 and :32)
//
// # Golden Rules pinned
//
//   - Golden Rule 15 (Clear Registry semantics): DELETE /api/documents moves every __archive file
//     back to __raws, cleans empty folders and purges the SQLite DB. No path in this package ever
//     calls os.Remove on a document FILE — the only os.Remove calls are on an EMPTY directory.
//     Nothing here calls a delete/trash helper: the move-back is app/relocalize's MoveBackToRaws,
//     exactly as clear-registry.ts:41 / :69 call moveBackToRaws.
//   - The archive-boundary check is taxonomy.IsPathInsideDir on CONFIG.OUTPUT_ROOT_DIR, verbatim
//     from clear-registry.ts:30.
//   - The file locator is app/guards.FindActualFileOnDisk (the one shared guard; migration design
//     decision 6 forbids reimplementing it). clear-registry.ts:29 imported the same
//     findActualFileOnDisk the relocalize module re-exports.
//
// Golden Rules 4/5 are not exercised here: the clear pipeline does no classification and creates no
// taxonomy branch, so there is no forbidden-subcategory or pre-move auto-create decision to make.
//
// # The scan lock is acquired by the CALLER (deliberate deviation)
//
// clear-registry.ts:11 acquires and releases the cross-process scan lock itself, inside a
// try/finally. This port does NOT touch app/scanlock: the composition root / HTTP handler acquires
// the lock BEFORE calling ClearRegistryAndMoveArchiveToRaws and releases it after, so the lock
// ordering in migration-design decision 7 stays visible at the one place that also owns the
// in-memory isAutoScanning guard and the task-state machine (web-server.ts:1348-1371). The method
// must only be called while the caller holds that lock; a concurrent scan/repair/clear is then
// rejected by the caller before this method runs.
//
// Consequence for the test port: the upstream case "propagates ScanInProgressError when the scan
// lock is held by another process" (clear-registry.test.ts:140) is NOT ported here, because this
// method never acquires the lock. That case belongs to the caller/composition-root suite, and
// app/scanlock's own suite (scanlock_test.go) already pins the *ScanInProgressError shape, its
// message and its errors.As contract.
//
// # Injected collaborators (no globals)
//
// The TypeScript imported infrastructure singletons directly; the port takes a Deps struct whose
// fields are small interfaces, so tests substitute fakes and the production composition root wires
// the real stores:
//
//   - Config   (settings.Config): InputDir / OutputRootDir / JSONRegistryPath.
//   - DB       (DocumentStore, *store/database.Store): GetAllDocuments + PurgeAll — the named
//     replacements for the TS raw `DELETE FROM documents` / `documents_fts` (store/database's
//     PurgeAll does both, with the FTS delete best-effort exactly as the TS try/catch makes it).
//   - Mover    (FileMover, app/relocalize.Deps): MoveBackToRaws. The clear pipeline always passes
//     checksum=nil, exactly like the TS one-argument moveBackToRaws(actualPath) call, so the mover
//     never un-registers a row (the DB is purged wholesale afterwards).
//   - Registry (RegistrySyncer, app/relocalize.JSONRegistrySync): the JSON mirror.
//   - Log      (*infra/logger.Logger): the TS console.log / console.warn messages. Nil is silent.
//   - EnsureDirectories: ensureDirectoriesExist(). Nil defaults to settings.EnsureDirectoriesExist.
//
// NewRelocalizeMover / NewJSONRegistrySync build the production adapters over app/relocalize, so
// the composition root does not re-implement the move or the mirror.
//
// # TS-vs-Go gaps, each resolved to MATCH the TypeScript surface
//
//  1. `console.log` / `console.warn` carry no logger module. They map to Log.Info / Log.Warn with
//     the module name "CLEAR"; a nil Log is silent, the convention of app/relocalize.
//  2. `reloadConfigFromDisk()` is not called here. The TS reads the live module-level CONFIG getter
//     after reloading; Go's settings.Config is a value, so reloading inside this package would not
//     update a captured Deps.Config and would leave the injected Mover/Registry on stale paths. The
//     caller owns the settings store (as it owns the scan lock), reloads it, and builds Deps from
//     the refreshed snapshot. EnsureDirectories IS still called, at the same two points as TS.
//  3. The TS inner async closure moveOrphansAndRemoveEmptyDirs is the unexported
//     Deps.moveOrphansAndRemoveEmptyDirs method. Every failure is swallowed exactly as the TS
//     `try { ... } catch (e) {}` does, so one unreadable directory cannot abort the purge.
//  4. `fs.readdirSync` + `fs.lstatSync(...).isDirectory()` is os.ReadDir + DirEntry.IsDir; neither
//     follows a symlink, so a symlinked directory is treated as a file and moved back to __raws,
//     matching lstat rather than a stat-following walk.
//  5. The root guard `dirPath.toLowerCase() !== path.normalize(OUTPUT_ROOT_DIR).toLowerCase()` is
//     filepath.Clean + strings.EqualFold, so __archive itself is never removed even when it ends up
//     empty.
//  6. `Promise<{ countMoved: number }>` is ClearResult, and a rejected promise is a Go error. A move
//     failure for an individual file is warned and that file is simply not counted, exactly as TS
//     catches it; a DB-purge or registry-sync failure is returned as an error (the TS lets both
//     reject the promise).
//  7. The TS optional callback `onProgress?: (event: any) => void` becomes a nil-able
//     `onProgress func(Event)` method parameter. The Event struct carries pointer fields for the
//     FILE_PROGRESS-only keys so a CLEAR_STARTED marshal omits them entirely while TotalFiles and
//     Message — present on BOTH payloads — are always emitted, including TotalFiles: 0 for an empty
//     registry. This reproduces the two TS object shapes byte-for-byte when marshalled.
package clear

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/app/guards"
	"github.com/phamhung075/pdf-triage-pdf2w/app/relocalize"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/logger"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/settings"
	"github.com/phamhung075/pdf-triage-pdf2w/store/database"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// moduleClear is the logger module name for the TS console.log / console.warn messages.
const moduleClear = "CLEAR"

// Event type and stage constants, exactly the TS literals.
const (
	EventClearStarted = "CLEAR_STARTED"
	EventFileProgress = "FILE_PROGRESS"
	StageClearing     = "CLEARING"
)

// DocumentStore is the narrow store/database surface this package needs. *store/database.Store
// satisfies it. Every write goes through a named method, so this package issues no raw SQL.
type DocumentStore interface {
	GetAllDocuments() ([]database.DocumentRecord, error)
	PurgeAll() error
}

// FileMover is the injected move-back-to-__raws helper (relocalize-document.ts:108). app/relocalize
// Deps satisfies it; NewRelocalizeMover builds the production value. The clear pipeline always
// passes a nil checksum, matching the TS one-argument call.
type FileMover interface {
	MoveBackToRaws(filePath string, checksum *string) (string, error)
}

// RegistrySyncer is the injected JSON-mirror writer. app/relocalize.JSONRegistrySync satisfies it.
type RegistrySyncer interface {
	SyncJSONRegistry() error
}

// Logger is the slice of *infra/logger.Logger this package logs through (the TS console.log /
// console.warn calls). Nil is silent, matching app/relocalize's convention.
type Logger interface {
	Info(moduleName, message string, meta any, filename ...string)
	Warn(moduleName, message string, meta any, filename ...string)
}

// Compile-time proof that the real collaborators satisfy the injected seams.
var (
	_ DocumentStore  = (*database.Store)(nil)
	_ FileMover      = relocalize.Deps{}
	_ RegistrySyncer = relocalize.JSONRegistrySync{}
	_ Logger         = (*logger.Logger)(nil)
)

// Event is one progress payload emitted through onProgress. It is a single struct covering both
// event types; the FILE_PROGRESS-only keys are pointers so a CLEAR_STARTED value omits them on
// marshal (the TS object has no such properties), while TotalFiles and Message are present on both
// payloads and therefore carry no omitempty — an empty registry must still emit "totalFiles":0.
//
// Marshalled field order matches the TS object literal order:
//
//	CLEAR_STARTED -> {"type","totalFiles","message"}
//	FILE_PROGRESS -> {"type","filename","scannedCount","processedCount","totalFiles","stage","message"}
type Event struct {
	Type           string  `json:"type"`
	Filename       *string `json:"filename,omitempty"`
	ScannedCount   *int    `json:"scannedCount,omitempty"`
	ProcessedCount *int    `json:"processedCount,omitempty"`
	TotalFiles     int     `json:"totalFiles"`
	Stage          *string `json:"stage,omitempty"`
	Message        string  `json:"message"`
}

// NewClearStartedEvent builds the CLEAR_STARTED payload (clear-registry.ts:19-23).
func NewClearStartedEvent(totalFiles int) Event {
	return Event{
		Type:       EventClearStarted,
		TotalFiles: totalFiles,
		Message:    fmt.Sprintf("Clearing registry records and moving %d physical file(s) back to __raws...", totalFiles),
	}
}

// NewFileProgressEvent builds the FILE_PROGRESS payload (clear-registry.ts:32-40). filename is
// `doc.original_filename || doc.title`; title is `doc.title` for the message.
func NewFileProgressEvent(filename string, index, totalFiles int, title string) Event {
	scannedCount := index
	processedCount := index
	stage := StageClearing
	return Event{
		Type:           EventFileProgress,
		Filename:       &filename,
		ScannedCount:   &scannedCount,
		ProcessedCount: &processedCount,
		TotalFiles:     totalFiles,
		Stage:          &stage,
		Message:        fmt.Sprintf("Moving file %d/%d back to __raws: %s", index, totalFiles, title),
	}
}

// ClearResult mirrors the TS return `{ countMoved }`.
type ClearResult struct {
	CountMoved int `json:"countMoved"`
}

// Deps is the explicit injected surface. Construct it in the composition root; tests build it with
// fakes and a temp SQLite store.
type Deps struct {
	Config   settings.Config
	DB       DocumentStore
	Mover    FileMover
	Registry RegistrySyncer
	Log      Logger

	// EnsureDirectories is ensureDirectoriesExist(). Nil defaults to
	// settings.EnsureDirectoriesExist(Deps.Config).
	EnsureDirectories func() error
}

// NewRelocalizeMover wires the production move-back helper: app/relocalize's Deps with only the
// Config and document store MoveBackToRaws reads (checksum is always nil, so the checksum lookup
// path is never reached).
func NewRelocalizeMover(cfg settings.Config, db *database.Store) FileMover {
	return relocalize.Deps{Config: cfg, DB: db}
}

// NewJSONRegistrySync wires the production JSON mirror: app/relocalize's JSONRegistrySync adapter,
// the port of syncJSONRegistry.
func NewJSONRegistrySync(db *database.Store, path string) RegistrySyncer {
	return relocalize.JSONRegistrySync{DB: db, Path: path}
}

func (d Deps) ensureDirectories() error {
	if d.EnsureDirectories != nil {
		return d.EnsureDirectories()
	}
	return settings.EnsureDirectoriesExist(d.Config)
}

func (d Deps) info(message string, meta any) {
	if d.Log != nil {
		d.Log.Info(moduleClear, message, meta)
	}
}

func (d Deps) warn(message string, meta any) {
	if d.Log != nil {
		d.Log.Warn(moduleClear, message, meta)
	}
}

// ClearRegistryAndMoveArchiveToRaws ports clearRegistryAndMoveArchiveToRaws
// (clear-registry.ts:10). It walks every stored document, moves its archived file back to __raws
// when that file exists and lies inside __archive, purges the whole documents + FTS index, then
// moves any remaining orphan file under __archive back to __raws and prunes the empty directory
// skeleton, and finally re-syncs the JSON registry.
//
// The caller MUST already hold the cross-process scan lock (app/scanlock); see the package
// comment. onProgress may be nil.
func (d Deps) ClearRegistryAndMoveArchiveToRaws(onProgress func(Event)) (ClearResult, error) {
	if d.DB == nil {
		return ClearResult{}, errors.New("clear: Deps.DB is required")
	}
	if d.Mover == nil {
		return ClearResult{}, errors.New("clear: Deps.Mover is required (the app/relocalize moveBackToRaws helper)")
	}

	cfg := d.Config

	if err := d.ensureDirectories(); err != nil {
		return ClearResult{}, err
	}

	existingDocs, err := d.DB.GetAllDocuments()
	if err != nil {
		return ClearResult{}, err
	}

	d.info(fmt.Sprintf("Clearing registry (%d records) and moving all physical files from __archive to __raws...", len(existingDocs)), nil)

	emit(onProgress, NewClearStartedEvent(len(existingDocs)))

	countMoved := 0
	processedIndex := 0
	for _, doc := range existingDocs {
		processedIndex++
		actualPath := guards.FindActualFileOnDisk(
			guards.DocumentLocation{
				NewPath:          doc.NewPath,
				OriginalPath:     doc.OriginalPath,
				OriginalFilename: doc.OriginalFilename,
			},
			cfg.InputDir,
			cfg.OutputRootDir,
		)
		if actualPath == "" || !pathExists(actualPath) || !taxonomy.IsPathInsideDir(actualPath, cfg.OutputRootDir) {
			continue
		}

		filename := doc.OriginalFilename
		if filename == "" {
			filename = doc.Title
		}
		emit(onProgress, NewFileProgressEvent(filename, processedIndex, len(existingDocs), doc.Title))

		if _, err := d.Mover.MoveBackToRaws(actualPath, nil); err != nil {
			d.warn(fmt.Sprintf("Error moving file %s back to __raws: %s", actualPath, err.Error()), nil)
			continue
		}
		countMoved++
	}

	// TS deletes documents (authoritative) then documents_fts (best-effort); PurgeAll does both.
	if err := d.DB.PurgeAll(); err != nil {
		return ClearResult{CountMoved: countMoved}, err
	}

	// Any files still left under __archive at this point have no matching DB row (e.g. a
	// repair/insert that never completed). Never delete a PDF — move these orphans back to __raws
	// too, same as tracked files, then remove the now-empty folder skeleton so the next scan
	// reconstructs it cleanly. Every failure is swallowed, exactly as the TS try/catch — and, as in
	// TS, the final ensureDirectoriesExist() runs only when the walk itself did not abort.
	if err := d.moveOrphansAndRemoveEmptyDirs(cfg.OutputRootDir, cfg.OutputRootDir, &countMoved); err == nil {
		_ = d.ensureDirectories()
	}

	if d.Registry != nil {
		if err := d.Registry.SyncJSONRegistry(); err != nil {
			return ClearResult{CountMoved: countMoved}, err
		}
	}

	d.info(fmt.Sprintf("Clear Registry Completed: Purged DB & moved %d physical files from __archive back to __raws.", countMoved), nil)

	return ClearResult{CountMoved: countMoved}, nil
}

// moveOrphansAndRemoveEmptyDirs is the TS inner moveOrphansAndRemoveEmptyDirs closure
// (clear-registry.ts:60-83). It returns an error only for a directory it could not list; the caller
// swallows it, and every per-file move failure is warned and skipped.
func (d Deps) moveOrphansAndRemoveEmptyDirs(dirPath, rootDir string, countMoved *int) error {
	if !pathExists(dirPath) {
		return nil
	}
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		curPath := filepath.Join(dirPath, entry.Name())
		if entry.IsDir() {
			// lstat semantics: a symlinked directory is not IsDir and is handled as a file.
			_ = d.moveOrphansAndRemoveEmptyDirs(curPath, rootDir, countMoved)
			continue
		}
		if _, err := d.Mover.MoveBackToRaws(curPath, nil); err != nil {
			d.warn(fmt.Sprintf("Error moving orphaned file %s back to __raws: %s", curPath, err.Error()), nil)
			continue
		}
		*countMoved = *countMoved + 1
	}
	if !samePath(dirPath, rootDir) && pathExists(dirPath) && dirIsEmpty(dirPath) {
		_ = os.Remove(dirPath)
	}
	return nil
}

func emit(onProgress func(Event), event Event) {
	if onProgress != nil {
		onProgress(event)
	}
}

// samePath is the TS `dirPath.toLowerCase() === path.normalize(root).toLowerCase()` comparison.
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// dirIsEmpty is `fs.existsSync(dir) && fs.readdirSync(dir).length === 0`: a missing or unreadable
// directory is not empty.
func dirIsEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(entries) == 0
}

// pathExists is `fs.existsSync`: true only when os.Stat succeeds (a broken symlink is "missing",
// exactly as Node reports it).
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
