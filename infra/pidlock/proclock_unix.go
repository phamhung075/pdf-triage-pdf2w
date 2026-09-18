//go:build !windows

package pidlock

import (
	"errors"
	"syscall"
)

// IsProcessRunning is `process.kill(pid, 0)`: signal 0 performs the permission/existence check
// without delivering a signal. A nil error means the process exists; EPERM means it exists but
// belongs to another user; ESRCH (and anything else) means it does not exist or the PID is invalid.
func IsProcessRunning(pid int) bool {
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return errors.Is(err, syscall.EPERM)
}
