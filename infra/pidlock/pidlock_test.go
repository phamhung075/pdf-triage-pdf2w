package pidlock

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These cases are ported from pdf-triage's src/infrastructure/pid-lock.test.ts (12 cases: 2 for
// isProcessRunning, 5 for readActiveLockHolder, 3 for acquireProcessLock, 2 for killProcessOnPort).
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/pid-lock.test.ts` -> 12 passed), so no upstream case is pinned
// red.
//
// The upstream suite mocks child_process.exec for the two killProcessOnPort cases; this port injects
// a CommandRunner into KillProcessOnPortWith, so those two cases run on every OS even though the real
// netstat/taskkill takeover is Windows-only. The pure parser is tested directly as well.

const netstatFixture = "Active Connections\n" +
	"\n" +
	"  Proto  Local Address          Foreign Address        State           PID\n" +
	"  TCP    0.0.0.0:135            0.0.0.0:0              LISTENING       1044\n" +
	"  TCP    0.0.0.0:3971           0.0.0.0:0              LISTENING       2152\n"

// TestPIDLockHelperProcess is the "other running process" the process-based cases need. It is a
// normal Go test that does nothing unless the parent starts it with PIDLOCK_HELPER=1, which makes it
// sleep instead of exiting so it has an observable live PID.
func TestPIDLockHelperProcess(t *testing.T) {
	if os.Getenv("PIDLOCK_HELPER") != "1" {
		return
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

// spawnChild starts a second copy of this test binary and returns it. It is registered for cleanup
// so a failing test can never leave the helper behind.
func spawnChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestPIDLockHelperProcess")
	cmd.Env = append(os.Environ(), "PIDLOCK_HELPER=1")
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

// killAndWait terminates a helper and reaps it, reproducing the TS `child.kill(); once('exit')`.
func killAndWait(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

func TestIsProcessRunning(t *testing.T) {
	t.Run("returns true for the current process", func(t *testing.T) {
		if !IsProcessRunning(os.Getpid()) {
			t.Fatal("IsProcessRunning(self) = false, want true")
		}
	})

	t.Run("returns false for a process that has already exited", func(t *testing.T) {
		child := spawnChild(t)
		pid := child.Process.Pid
		killAndWait(t, child)
		if IsProcessRunning(pid) {
			t.Fatalf("IsProcessRunning(%d) = true, want false after exit", pid)
		}
	})
}

func TestReadActiveLockHolder(t *testing.T) {
	t.Run("returns null when the lock file does not exist", func(t *testing.T) {
		lockFile := filepath.Join(t.TempDir(), ".scan.lock")
		if pid, ok := ReadActiveLockHolder(lockFile); ok {
			t.Fatalf("ReadActiveLockHolder = %d, true; want false", pid)
		}
	})

	t.Run("returns null when the lock file contains garbage (non-numeric) content", func(t *testing.T) {
		lockFile := filepath.Join(t.TempDir(), ".scan.lock")
		if err := os.WriteFile(lockFile, []byte("not-a-pid"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if pid, ok := ReadActiveLockHolder(lockFile); ok {
			t.Fatalf("ReadActiveLockHolder = %d, true; want false", pid)
		}
	})

	t.Run("returns null when the lock file holds this process's own PID", func(t *testing.T) {
		lockFile := filepath.Join(t.TempDir(), ".scan.lock")
		if err := os.WriteFile(lockFile, []byte(strconv.Itoa(os.Getpid())), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if pid, ok := ReadActiveLockHolder(lockFile); ok {
			t.Fatalf("ReadActiveLockHolder = %d, true; want false", pid)
		}
	})

	t.Run("returns null when the lock file holds a PID of a process that has already exited (stale lock)", func(t *testing.T) {
		lockFile := filepath.Join(t.TempDir(), ".scan.lock")
		child := spawnChild(t)
		pid := child.Process.Pid
		killAndWait(t, child)
		if err := os.WriteFile(lockFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if got, ok := ReadActiveLockHolder(lockFile); ok {
			t.Fatalf("ReadActiveLockHolder = %d, true; want false", got)
		}
	})

	t.Run("returns the holder PID when the lock file holds a genuinely running other process", func(t *testing.T) {
		lockFile := filepath.Join(t.TempDir(), ".scan.lock")
		child := spawnChild(t)
		pid := child.Process.Pid
		if err := os.WriteFile(lockFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		got, ok := ReadActiveLockHolder(lockFile)
		if !ok || got != pid {
			t.Fatalf("ReadActiveLockHolder = %d, %v; want %d, true", got, ok, pid)
		}
	})
}

func TestAcquireProcessLock(t *testing.T) {
	t.Run("writes this process's PID to the lock file", func(t *testing.T) {
		lockFile := filepath.Join(t.TempDir(), ".scan.lock")
		release, err := AcquireProcessLock(lockFile)
		if err != nil {
			t.Fatalf("AcquireProcessLock: %v", err)
		}
		defer release()
		raw, err := os.ReadFile(lockFile)
		if err != nil {
			t.Fatalf("ReadFile: %v", err)
		}
		if got := strings.TrimSpace(string(raw)); got != strconv.Itoa(os.Getpid()) {
			t.Fatalf("lock contents = %q, want %d", got, os.Getpid())
		}
	})

	t.Run("release() removes the lock file", func(t *testing.T) {
		lockFile := filepath.Join(t.TempDir(), ".scan.lock")
		release, err := AcquireProcessLock(lockFile)
		if err != nil {
			t.Fatalf("AcquireProcessLock: %v", err)
		}
		release()
		if _, err := os.Stat(lockFile); !os.IsNotExist(err) {
			t.Fatalf("lock file still exists after release, stat err = %v", err)
		}
	})

	t.Run("release() does not remove the lock file if its content no longer matches this process", func(t *testing.T) {
		lockFile := filepath.Join(t.TempDir(), ".scan.lock")
		release, err := AcquireProcessLock(lockFile)
		if err != nil {
			t.Fatalf("AcquireProcessLock: %v", err)
		}
		if err := os.WriteFile(lockFile, []byte("999999"), 0o600); err != nil { // simulate another process having taken over
			t.Fatalf("WriteFile: %v", err)
		}
		release()
		if _, err := os.Stat(lockFile); err != nil {
			t.Fatalf("lock file removed despite foreign owner: %v", err)
		}
	})
}

func TestParsePidOnPort(t *testing.T) {
	cases := []struct {
		name string
		out  string
		port int
		want int
		ok   bool
	}{
		{"finds the listening PID", netstatFixture, 3971, 2152, true},
		{"finds a different listener", netstatFixture, 135, 1044, true},
		{"returns false for a port with no listener", netstatFixture, 9999, 0, false},
		{"returns false for empty output", "", 3971, 0, false},
		{"ignores a non-LISTENING state", "  TCP    0.0.0.0:3971   0.0.0.0:0   ESTABLISHED   2152\n", 3971, 0, false},
		{"ignores a header line", "  Proto  Local Address  Foreign Address  State  PID\n", 3971, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParsePidOnPort(tc.out, tc.port)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ParsePidOnPort(%d) = %d, %v; want %d, %v", tc.port, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestKillProcessOnPort(t *testing.T) {
	t.Run("finds and kills the process listening on the given port", func(t *testing.T) {
		var names []string
		var taskkillArgs []string
		run := func(name string, args ...string) ([]byte, error) {
			names = append(names, name)
			switch name {
			case "netstat":
				return []byte(netstatFixture), nil
			case "taskkill":
				taskkillArgs = args
				return nil, nil
			default:
				return nil, errors.New("unexpected command " + name)
			}
		}

		killed := KillProcessOnPortWith(3971, run)

		if !killed {
			t.Fatal("KillProcessOnPortWith = false, want true")
		}
		if !equalStrings(taskkillArgs, []string{"/PID", "2152", "/F"}) {
			t.Fatalf("taskkill args = %v, want [/PID 2152 /F]", taskkillArgs)
		}
	})

	t.Run("returns false when nothing is listening on the port", func(t *testing.T) {
		var names []string
		run := func(name string, args ...string) ([]byte, error) {
			names = append(names, name)
			if name == "netstat" {
				return []byte(strings.Replace(netstatFixture, "  TCP    0.0.0.0:3971           0.0.0.0:0              LISTENING       2152\n", "", 1)), nil
			}
			return nil, errors.New("unexpected command " + name)
		}

		killed := KillProcessOnPortWith(3971, run)

		if killed {
			t.Fatal("KillProcessOnPortWith = true, want false")
		}
		if len(names) != 1 { // only netstat ran - no taskkill attempted
			t.Fatalf("commands run = %v, want only [netstat]", names)
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
