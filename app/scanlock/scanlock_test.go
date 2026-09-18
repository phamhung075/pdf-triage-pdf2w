package scanlock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// These cases are ported from pdf-triage's src/application/scan-lock.test.ts (9 cases: 5 for
// acquireScanLock, 4 for same-process re-entry). The upstream TypeScript suite is GREEN at port
// time (`npx vitest run src/application/scan-lock.test.ts` -> 9 passed), so no upstream case is
// pinned red.
//
// The TS suite resets the module with vi.resetModules() before each case to get a fresh module-level
// `heldInProcess`; the Go port uses a fresh Guard per case (New(t.TempDir())), which is the same
// isolation. Cases that need a genuinely running "other process" spawn a second copy of this test
// binary via TestScanLockHelperProcess, the same technique the pidlock port uses.

// TestScanLockHelperProcess is the "other running process" the cross-process case needs. It does
// nothing unless the parent starts it with SCANLOCK_HELPER=1, which makes it sleep so it has an
// observable live PID.
func TestScanLockHelperProcess(t *testing.T) {
	if os.Getenv("SCANLOCK_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// spawnScanLockChild starts a second copy of this test binary and registers cleanup so a failing
// case can never leave the helper behind.
func spawnScanLockChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestScanLockHelperProcess")
	cmd.Env = append(os.Environ(), "SCANLOCK_HELPER=1")
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting helper process: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func writeLockFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

func TestAcquireScanLock(t *testing.T) {
	t.Run("acquires the lock and writes this process's PID to the lock file", func(t *testing.T) {
		guard := New(t.TempDir())
		release, err := guard.Acquire()
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		defer release()

		raw, err := os.ReadFile(guard.LockFilePath())
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if got := strings.TrimSpace(string(raw)); got != strconv.Itoa(os.Getpid()) {
			t.Fatalf("lock contents = %q, want %d", got, os.Getpid())
		}
	})

	t.Run("release() removes the lock file", func(t *testing.T) {
		guard := New(t.TempDir())
		release, err := guard.Acquire()
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		release()
		if _, err := os.Stat(guard.LockFilePath()); !os.IsNotExist(err) {
			t.Fatalf("lock file still exists after release, stat err = %v", err)
		}
	})

	t.Run("release() does not remove the lock file if it no longer belongs to this process", func(t *testing.T) {
		guard := New(t.TempDir())
		release, err := guard.Acquire()
		if err != nil {
			t.Fatalf("Acquire: %v", err)
		}
		writeLockFile(t, guard.LockFilePath(), "999999") // another process has since taken over
		release()
		if _, err := os.Stat(guard.LockFilePath()); err != nil {
			t.Fatalf("lock file removed despite foreign owner: %v", err)
		}
	})

	t.Run("treats a lock file written with this process's own PID as free, not blocking", func(t *testing.T) {
		dataDir := t.TempDir()
		writeLockFile(t, LockFilePath(dataDir), strconv.Itoa(os.Getpid()))

		guard := New(dataDir)
		release, err := guard.Acquire()
		if err != nil {
			t.Fatalf("Acquire: %v, want nil (own PID is free)", err)
		}
		release()
	})

	t.Run("throws ScanInProgressError with the holder PID when the lock is held by another actually-running process", func(t *testing.T) {
		child := spawnScanLockChild(t)
		dataDir := t.TempDir()
		writeLockFile(t, LockFilePath(dataDir), strconv.Itoa(child.Process.Pid))

		guard := New(dataDir)
		release, err := guard.Acquire()
		if err == nil {
			release()
			t.Fatal("Acquire = nil error, want *ScanInProgressError")
		}
		var target *ScanInProgressError
		if !errors.As(err, &target) {
			t.Fatalf("errors.As(err, *ScanInProgressError) = false; err = %v", err)
		}
		if target.HolderPID != child.Process.Pid {
			t.Fatalf("HolderPID = %d, want %d", target.HolderPID, child.Process.Pid)
		}
		if !strings.Contains(err.Error(), strconv.Itoa(child.Process.Pid)) {
			t.Fatalf("error %q does not contain holder PID %d", err.Error(), child.Process.Pid)
		}
	})
}

func TestAcquireScanLockSameProcessReentry(t *testing.T) {
	t.Run("rejects a SECOND acquisition while the first is still held", func(t *testing.T) {
		guard := New(t.TempDir())
		release, err := guard.Acquire()
		if err != nil {
			t.Fatalf("first Acquire: %v", err)
		}
		defer release()

		_, err = guard.Acquire()
		if err == nil {
			t.Fatal("second Acquire = nil error, want *ScanInProgressError")
		}
		var target *ScanInProgressError
		if !errors.As(err, &target) {
			t.Fatalf("errors.As(err, *ScanInProgressError) = false; err = %v", err)
		}
		if target.HolderPID != os.Getpid() {
			t.Fatalf("HolderPID = %d, want own PID %d", target.HolderPID, os.Getpid())
		}
		if !strings.Contains(err.Error(), strconv.Itoa(os.Getpid())) {
			t.Fatalf("error %q does not contain own PID %d", err.Error(), os.Getpid())
		}
	})

	t.Run("allows a new acquisition once the first is released", func(t *testing.T) {
		guard := New(t.TempDir())
		release, err := guard.Acquire()
		if err != nil {
			t.Fatalf("first Acquire: %v", err)
		}
		release()

		release2, err := guard.Acquire()
		if err != nil {
			t.Fatalf("second Acquire = %v, want nil", err)
		}
		release2()
	})

	t.Run("makes release idempotent, so a stale handle cannot delete a lock a later run owns", func(t *testing.T) {
		guard := New(t.TempDir())

		staleRelease, err := guard.Acquire()
		if err != nil {
			t.Fatalf("first Acquire: %v", err)
		}
		staleRelease()
		staleRelease() // no-op

		laterRelease, err := guard.Acquire()
		if err != nil {
			t.Fatalf("later Acquire: %v", err)
		}
		if _, err := os.Stat(guard.LockFilePath()); err != nil {
			t.Fatalf("lock file missing after later acquisition: %v", err)
		}

		staleRelease()
		if _, err := os.Stat(guard.LockFilePath()); err != nil {
			t.Fatalf("stale release removed a lock owned by a later run: %v", err)
		}

		laterRelease()
		if _, err := os.Stat(guard.LockFilePath()); !os.IsNotExist(err) {
			t.Fatalf("lock file still exists after later release, stat err = %v", err)
		}
	})

	t.Run("frees in-process ownership even when the scan body throws", func(t *testing.T) {
		guard := New(t.TempDir())
		release, err := guard.Acquire()
		if err != nil {
			t.Fatalf("first Acquire: %v", err)
		}

		func() {
			defer func() { _ = recover() }()
			defer release()
			panic("scan blew up")
		}()

		release2, err := guard.Acquire()
		if err != nil {
			t.Fatalf("Acquire after thrown scan body = %v, want nil", err)
		}
		release2()
	})
}

// Added cases:

func TestLockFilePath(t *testing.T) {
	dataDir := filepath.Join("tmp", "pdf-triage-data")
	want := filepath.Join(dataDir, ".scan.lock")
	if got := LockFilePath(dataDir); got != want {
		t.Fatalf("LockFilePath = %q, want %q", got, want)
	}
}

func TestScanInProgressErrorMessage(t *testing.T) {
	err := &ScanInProgressError{HolderPID: 4242}
	want := "A scan/repair/clear operation is already in progress (held by process 4242). Try again shortly."
	if got := err.Error(); got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

// fakeLocker lets a case pin holder and write-failure behavior without a real second process.
type fakeLocker struct {
	holderPID int
	held      bool
	writeErr  error
	acquired  int
}

func (f *fakeLocker) ReadActiveHolder(string) (int, bool) { return f.holderPID, f.held }

func (f *fakeLocker) AcquireProcessLock(string) (func(), error) {
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	f.acquired++
	return func() {}, nil
}

func TestAcquireWithInjectedLocker(t *testing.T) {
	t.Run("reports the holder PID supplied by the backend", func(t *testing.T) {
		files := &fakeLocker{holderPID: 777, held: true}
		guard := NewWithLocker(t.TempDir(), files)

		_, err := guard.Acquire()
		var target *ScanInProgressError
		if !errors.As(err, &target) || target.HolderPID != 777 {
			t.Fatalf("Acquire err = %v, want *ScanInProgressError{HolderPID: 777}", err)
		}
		if files.acquired != 0 {
			t.Fatalf("backend AcquireProcessLock called %d times, want 0", files.acquired)
		}
	})

	t.Run("propagates a backend write failure and leaves ownership free", func(t *testing.T) {
		writeErr := errors.New("disk full")
		files := &fakeLocker{writeErr: writeErr}
		guard := NewWithLocker(t.TempDir(), files)

		if _, err := guard.Acquire(); !errors.Is(err, writeErr) {
			t.Fatalf("Acquire err = %v, want %v", err, writeErr)
		}

		// The failed acquisition must not leave heldInProcess set.
		files.writeErr = nil
		release, err := guard.Acquire()
		if err != nil {
			t.Fatalf("Acquire after failure = %v, want nil", err)
		}
		if files.acquired != 1 {
			t.Fatalf("backend AcquireProcessLock called %d times, want 1", files.acquired)
		}
		release()
	})
}

func TestConcurrentAcquireOnlyOneWins(t *testing.T) {
	files := &fakeLocker{}
	guard := NewWithLocker(t.TempDir(), files)

	const goroutines = 32
	var wg sync.WaitGroup
	var successes int64
	var inProgress int64
	releases := make(chan func(), goroutines)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := guard.Acquire()
			if err == nil {
				atomic.AddInt64(&successes, 1)
				releases <- release
				return
			}
			var target *ScanInProgressError
			if errors.As(err, &target) {
				atomic.AddInt64(&inProgress, 1)
			}
		}()
	}
	wg.Wait()
	close(releases)

	if successes != 1 {
		t.Fatalf("successful Acquire calls = %d, want exactly 1", successes)
	}
	if inProgress != goroutines-1 {
		t.Fatalf("*ScanInProgressError count = %d, want %d", inProgress, goroutines-1)
	}
	for release := range releases {
		release()
	}
}

func TestAcquireScanLockSingleton(t *testing.T) {
	dataDir := t.TempDir()

	release, err := AcquireScanLock(dataDir)
	if err != nil {
		t.Fatalf("first AcquireScanLock: %v", err)
	}

	if _, err := AcquireScanLock(dataDir); err == nil {
		t.Fatal("second AcquireScanLock = nil error, want *ScanInProgressError")
	} else {
		var target *ScanInProgressError
		if !errors.As(err, &target) {
			t.Fatalf("errors.As(err, *ScanInProgressError) = false; err = %v", err)
		}
	}

	release()

	release2, err := AcquireScanLock(dataDir)
	if err != nil {
		t.Fatalf("AcquireScanLock after release = %v, want nil", err)
	}
	release2()
}
