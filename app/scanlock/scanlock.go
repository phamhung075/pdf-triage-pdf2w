// Package scanlock is a Go port of pdf-triage's src/application/scan-lock.ts (54 lines): the
// cross-process .scan.lock guard plus the in-process ownership tracking that stops two
// scan/repair/clear pipelines running against the same __raws/__archive pair.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/application/scan-lock.test.ts` -> 9 passed), so no upstream case is pinned
// red. All nine cases are ported, plus cases for the lock-file path shape, the errors.As contract,
// the exact error message, a locker write failure, and concurrent Acquire calls exercised under
// `go test -race`.
//
// Why the lock exists (src/application/scan-lock.ts:5-14, preserved verbatim):
//
//	Cross-process guard: the web server's own auto-watcher/manual-scan/repair/clear
//	routes already serialize themselves via an in-memory flag, but that can't stop a
//	SEPARATE process (e.g. the MCP server, `npm run scan`, or a stray second server
//	instance) from concurrently running one of these against the same __raws/__archive
//	files. This file-based lock makes that cross-process case fail fast instead of racing.
//	DATA_DIR, not BASE_DIR — same reasoning as the server lock in web-server.ts. The lock exists to
//	serialize work over ONE __raws/__archive pair, and which pair that is comes from the data
//	directory, not from which copy of the application files is running. Keyed on BASE_DIR, two
//	installs sharing an app folder blocked each other, and a packaged install wrote its lock into
//	resources/app — a folder upgrades replace and a real install may not let it write to.
//
// Why the in-process half is tracked separately (src/application/scan-lock.ts:23-32, preserved
// verbatim):
//
//	In-process ownership, tracked separately from the lock FILE. readActiveLockHolder()
//	deliberately reports "free" when the lock file is held by this same PID (see pid-lock.ts), so
//	the file alone cannot stop a second scan starting inside the SAME process — which is exactly
//	what happened when the user pressed Stop (which cleared the isAutoScanning flag without
//	cancelling the running loop) and then Scan again. Two runTriageScan() loops then walked the
//	same __raws listing: one moved a file to __archive between the other's directory read and its
//	statSync (ENOENT), the loser of a classify race hit `UNIQUE constraint failed:
//	documents.checksum` and shunted an already-archived user document into .duplicates_files, and
//	whichever loop finished first deleted .scan.lock while the other was still running — which then
//	let `npm run scan` or the MCP server start a third pipeline on top.
//
// The ordering that prevents the concurrent-scan UNIQUE-checksum failure is restated and preserved
// (migration-design §7): the auto-watcher claims the in-memory guard synchronously before any
// await (web-server.ts:1434) and the scan path holds the cross-process scan lock for the whole run,
// so two scans can never both insert a new checksum. src/application/triage-scan.ts:373-384 is the
// call site that documents the failure this lock prevents: another checksum-owning row can appear
// between the pre-check and the insert because classifyPDFText's Step A/C/D round-trip takes tens of
// seconds, and without serialization the loser hits `SQLITE_CONSTRAINT: UNIQUE constraint failed:
// documents.checksum` for every subsequent file. The guard denies a second acquisition before that
// window can open, both in one process (heldInProcess) and across processes (the .scan.lock file).
//
// The data directory is an explicit parameter, not a package global: New/NewWithLocker take the
// dir and derive DATA_DIR/.scan.lock with filepath.Join, mirroring TS `path.join(DATA_DIR,
// '.scan.lock')`. The file-lock backend is a small injected interface (FileLocker) so tests can
// supply a fake; PidLockFiles is the production adapter over infra/pidlock's
// ReadActiveLockHolder/AcquireProcessLock.
//
// Deviations, all resolved in favor of matching the TS acceptance bar:
//
//  1. Return shape. TS `acquireScanLock()` returns `() => void` and throws. Go cannot throw, so
//     `Acquire` returns `(release func(), err error)`; a `*ScanInProgressError` is returned when
//     held. Everything else, including an acquire/write failure, is returned as a plain error.
//  2. Typed error. TS `ScanInProgressError extends Error` with a readonly `holderPid`; Go has a
//     `ScanInProgressError` struct with exported `HolderPID`, the same `Error()` text, and an
//     `errors.As` contract replacing `instanceof`.
//  3. In-process ownership. TS keeps one module-level `heldInProcess` boolean; Go keeps it per
//     `Guard`. The composition root holds one Guard per data dir, and AcquireScanLock keeps the TS
//     process-wide singleton behavior through a per-lock-path guard registry. `heldInProcess` is
//     the TS name preserved for reviewability; field is unexported.
//  4. `process.pid` is os.Getpid(); `path.join` is filepath.Join.
//  5. Idempotent release. TS guards with a local `released` boolean; Go uses a sync.Once per
//     acquisition, so a stale handle stays a no-op even after a later run re-acquires the lock.
//  6. `readActiveLockHolder(): number | null` is `(holderPid int, held bool)`; "null" is held=false.
//     AcquireProcessLock's synchronous fs throw is the `err` return.
//  7. Concurrency. TS relies on Node's single-threaded event loop; Go callers are concurrent, so
//     the heldInProcess check + file check + file acquire are one mutex-guarded critical section,
//     making concurrent Acquire calls race-clean. A FileLocker must not re-enter its owning Guard.
package scanlock

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/phamhung075/pdf-triage-pdf2w/infra/pidlock"
)

// ScanLockFileName is the TS `'.scan.lock'` basename.
const ScanLockFileName = ".scan.lock"

// FileLocker abstracts the two infra/pidlock operations this package needs, so tests can inject a
// fake and production wires PidLockFiles. It is intentionally the exact shape of
// pidlock.ReadActiveLockHolder / pidlock.AcquireProcessLock.
type FileLocker interface {
	// ReadActiveHolder returns the existing lock holder's PID and true when the lock is held by a
	// still-running OTHER process; false when it is free, stale, or held by this same process.
	ReadActiveHolder(lockFilePath string) (holderPID int, held bool)
	// AcquireProcessLock writes this process's PID to lockFilePath and returns a release function
	// that removes it only while it still belongs to this process.
	AcquireProcessLock(lockFilePath string) (release func(), err error)
}

// PidLockFiles is the production FileLocker, backed directly by infra/pidlock.
type PidLockFiles struct{}

// ReadActiveHolder delegates to pidlock.ReadActiveLockHolder.
func (PidLockFiles) ReadActiveHolder(lockFilePath string) (int, bool) {
	return pidlock.ReadActiveLockHolder(lockFilePath)
}

// AcquireProcessLock delegates to pidlock.AcquireProcessLock.
func (PidLockFiles) AcquireProcessLock(lockFilePath string) (func(), error) {
	return pidlock.AcquireProcessLock(lockFilePath)
}

// ScanInProgressError is the Go form of the TS `ScanInProgressError` class. It is returned by
// Acquire when another scan/repair/clear already owns the lock, and is recoverable with
// `var target *ScanInProgressError; errors.As(err, &target)`.
type ScanInProgressError struct {
	// HolderPID is the PID reported for the operation already holding the lock. It is os.Getpid()
	// when the holder is this same process (the in-process re-entry case).
	HolderPID int
}

// Error renders the same message as the TS class constructor.
func (e *ScanInProgressError) Error() string {
	return fmt.Sprintf(
		"A scan/repair/clear operation is already in progress (held by process %d). Try again shortly.",
		e.HolderPID,
	)
}

// ScanInProgressError implements the error interface (TS `extends Error`).
var _ error = (*ScanInProgressError)(nil)

// LockFilePath returns the TS `path.join(DATA_DIR, '.scan.lock')` path for a data directory.
func LockFilePath(dataDir string) string {
	return filepath.Join(dataDir, ScanLockFileName)
}

// Guard owns the cross-process scan lock for one data directory plus its in-process ownership flag.
// It is safe for concurrent use.
type Guard struct {
	lockFilePath string
	files        FileLocker

	mu            sync.Mutex
	heldInProcess bool
}

// New returns a Guard for dataDir backed by the real infra/pidlock file lock.
func New(dataDir string) *Guard {
	return &Guard{lockFilePath: LockFilePath(dataDir), files: PidLockFiles{}}
}

// NewWithLocker returns a Guard for dataDir using an injected FileLocker (tests, or an alternate
// backend). A nil locker falls back to PidLockFiles.
func NewWithLocker(dataDir string, files FileLocker) *Guard {
	if files == nil {
		files = PidLockFiles{}
	}
	return &Guard{lockFilePath: LockFilePath(dataDir), files: files}
}

// LockFilePath returns the lock file this Guard owns.
func (g *Guard) LockFilePath() string { return g.lockFilePath }

// Acquire claims the scan lock and returns an idempotent release function. It returns a
// *ScanInProgressError when the lock is already held in this process or by another running process;
// the returned release function is nil on error.
func (g *Guard) Acquire() (release func(), err error) {
	g.mu.Lock()
	defer g.mu.Unlock()

	// In-process ownership first, exactly as TS checks heldInProcess before touching the file. The
	// file alone cannot stop a same-process re-entry because readActiveLockHolder deliberately
	// reports "free" for this process's own PID.
	if g.heldInProcess {
		return nil, &ScanInProgressError{HolderPID: os.Getpid()}
	}

	if holderPID, held := g.files.ReadActiveHolder(g.lockFilePath); held {
		return nil, &ScanInProgressError{HolderPID: holderPID}
	}

	releaseFile, err := g.files.AcquireProcessLock(g.lockFilePath)
	if err != nil {
		return nil, err
	}
	g.heldInProcess = true

	// Idempotent: a double release must not delete a lock file a LATER run now owns.
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.heldInProcess = false
			g.mu.Unlock()
			releaseFile()
		})
	}, nil
}

// defaultGuards preserves the TS module-singleton `heldInProcess` across call sites that do not
// hold a Guard: one Guard per lock-file path, so AcquireScanLock(dataDir) is process-wide and
// race-clean exactly like the TS module state. Tests use New/NewWithLocker for isolation.
var (
	defaultGuardsMu sync.Mutex
	defaultGuards   = map[string]*Guard{}
)

func defaultGuard(dataDir string) *Guard {
	key := LockFilePath(dataDir)
	defaultGuardsMu.Lock()
	defer defaultGuardsMu.Unlock()
	if g := defaultGuards[key]; g != nil {
		return g
	}
	g := New(dataDir)
	defaultGuards[key] = g
	return g
}

// AcquireScanLock is the Go form of the TS module-level `acquireScanLock()`: it takes the data
// directory as a parameter and returns the release function or a *ScanInProgressError.
func AcquireScanLock(dataDir string) (func(), error) {
	return defaultGuard(dataDir).Acquire()
}
