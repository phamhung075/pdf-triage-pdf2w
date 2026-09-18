//go:build windows

package pidlock

import (
	"errors"
	"syscall"
)

// IsProcessRunning is the Windows counterpart of `process.kill(pid, 0)`. There is no signal 0 on
// Windows, so it opens the process with query access: a successful open means the PID is live, and
// ERROR_ACCESS_DENIED (5) means it exists but is not ours - the same distinction Node's
// process.kill returns as EPERM. A nonexistent PID reports ERROR_INVALID_PARAMETER (87) and is
// false.
func IsProcessRunning(pid int) bool {
	const processQueryInformation = 0x0400
	handle, err := syscall.OpenProcess(processQueryInformation, false, uint32(pid))
	if err == nil {
		syscall.CloseHandle(handle)
		return true
	}
	return errors.Is(err, syscall.Errno(5)) // ERROR_ACCESS_DENIED
}
