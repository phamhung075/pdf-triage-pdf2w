// Package pidlock is a Go port of pdf-triage's src/infrastructure/pid-lock.ts (72 lines):
// isProcessRunning, readActiveLockHolder, acquireProcessLock and killProcessOnPort.
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/pid-lock.test.ts` -> 12 passed), so no upstream case is pinned
// red. All twelve cases are ported.
//
// The process-existence and port-takeover halves split along the platform boundary the objective
// requires:
//
//   - IsProcessRunning uses signal 0 semantics on unix (proclock_unix.go) and OpenProcess on Windows
//     (proclock_windows.go), behind `//go:build` tags.
//   - ParsePidOnPort, the netstat `-ano -p tcp` line parser, is a pure function in this file and is
//     tested on every OS.
//   - KillProcessOnPortWith is the portable orchestration (run netstat, parse, run taskkill) and is
//     tested on every OS through an injected CommandRunner. The real exec runner and the exported
//     KillProcessOnPort live in port_windows.go; on other platforms KillProcessOnPort is a no-op
//     (port_other.go), because the netstat/taskkill takeover is Windows-only by design.
//
// Deviations, all resolved in favor of matching the TS acceptance bar:
//
//  1. isProcessRunning on unix: process.kill(pid, 0) returns true on success and true for EPERM
//     (the process exists but is not ours); every other errno (ESRCH, EINVAL) is false. syscall.Kill
//     with signal 0 is the same probe.
//  2. Optional arguments. TS `killProcessOnPort(port)` returns Promise<boolean>; the Go port exposes
//     both the portable KillProcessOnPortWith(port, run) and the platform-specific
//     KillProcessOnPort(port). exec errors are swallowed exactly as the TS `(err, stdout)` callbacks
//     swallow them, so the boolean is the only result.
//  3. parseInt. TS reads the lock with `parseInt(text.trim(), 10)`, which accepts a numeric prefix
//     ("12abc" -> 12) and ignores trailing junk. parseIntPrefix reproduces that prefix scan; a value
//     that overflows Go's int is treated as not-running (Node would reject the out-of-range PID).
//  4. String(process.pid) is strconv.Itoa(os.Getpid()); the release closure ignores every error
//     (the TS catch {}), and only removes the file when the trimmed content still equals this PID.
//  5. The TS `exec(cmd, {windowsHide:true}, cb)` single command string maps to a name+args pair, so
//     the test asserts the taskkill argv instead of the rendered string.
package pidlock

import (
	"os"
	"strconv"
	"strings"
)

// CommandRunner runs a command and returns its stdout. It abstracts child_process.exec so the
// netstat/taskkill orchestration is testable on every OS.
type CommandRunner func(name string, args ...string) ([]byte, error)

// ReadActiveLockHolder returns the existing lock holder's PID if the lock is currently held by a
// still-running OTHER process, or ok=false if the lock is free, stale (holder no longer running), or
// held by this same process. It is the Go equivalent of the TS `number | null` return.
func ReadActiveLockHolder(lockFilePath string) (int, bool) {
	raw, err := os.ReadFile(lockFilePath)
	if err != nil {
		return 0, false
	}
	pid, ok := parseIntPrefix(strings.TrimSpace(string(raw)))
	if !ok || pid == os.Getpid() || !IsProcessRunning(pid) {
		return 0, false
	}
	return pid, true
}

// AcquireProcessLock writes this process's PID to lockFilePath and returns a release function that
// removes the lock file - but only if it still belongs to this process (avoids deleting a lock
// another process has since acquired). The error replaces the synchronous write exception.
func AcquireProcessLock(lockFilePath string) (func(), error) {
	pid := strconv.Itoa(os.Getpid())
	if err := os.WriteFile(lockFilePath, []byte(pid), 0o644); err != nil {
		return nil, err
	}
	return func() {
		raw, err := os.ReadFile(lockFilePath)
		if err != nil {
			return // includes "file no longer exists" - TS catch {}
		}
		if strings.TrimSpace(string(raw)) == pid {
			_ = os.Remove(lockFilePath)
		}
	}, nil
}

// ParsePidOnPort is the pure `netstat -ano -p tcp` parser: it returns the PID of the LISTENING
// socket whose local address ends in ":port", or ok=false when there is none. The parse is exactly
// the TS `stdout.split('\n').find(...)`: split each line on whitespace, require columns[1] to end
// with ":port" and columns[3] to be "LISTENING", then take the last column as the PID.
func ParsePidOnPort(netstatOutput string, port int) (int, bool) {
	if netstatOutput == "" {
		return 0, false
	}
	suffix := ":" + strconv.Itoa(port)
	for _, line := range strings.Split(netstatOutput, "\n") {
		cols := strings.Fields(line)
		if len(cols) < 4 {
			continue
		}
		if !strings.HasSuffix(cols[1], suffix) || cols[3] != "LISTENING" {
			continue
		}
		pid, ok := parseIntPrefix(cols[len(cols)-1])
		if !ok {
			return 0, false
		}
		return pid, true
	}
	return 0, false
}

// KillProcessOnPortWith runs `netstat -ano -p tcp`, finds the PID listening on port and force-kills
// it with `taskkill /PID <pid> /F`. It returns true if a process was found and killed, false if
// nothing was listening (so the caller knows a retry is pointless). Errors are swallowed, matching
// the TS exec callbacks.
func KillProcessOnPortWith(port int, run CommandRunner) bool {
	out, err := run("netstat", "-ano", "-p", "tcp")
	if err != nil || len(out) == 0 {
		return false
	}
	pid, ok := ParsePidOnPort(string(out), port)
	if !ok {
		return false
	}
	_, _ = run("taskkill", "/PID", strconv.Itoa(pid), "/F")
	return true
}

// parseIntPrefix reproduces JavaScript's `parseInt(s, 10)` numeric-prefix scan: optional leading
// whitespace, an optional sign, then as many decimal digits as possible; ok=false when there are no
// digits or the value does not fit in an int.
func parseIntPrefix(s string) (int, bool) {
	i := 0
	for i < len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\v', '\f', '\r':
			i++
			continue
		}
		break
	}
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digitsStart := i
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	if i == digitsStart {
		return 0, false
	}
	n, err := strconv.Atoi(s[:i])
	if err != nil {
		return 0, false
	}
	return n, true
}
