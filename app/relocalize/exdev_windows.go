//go:build windows

package relocalize

import (
	"errors"
	"syscall"
)

// errorNotSameDevice is Win32 ERROR_NOT_SAME_DEVICE (17), the cross-volume failure os.Link reports
// on Windows. The syscall package does not export a name for it, so it is compared as a raw Errno;
// syscall.EXDEV is kept too for the case where the runtime maps it.
const errorNotSameDevice = syscall.Errno(17)

// isCrossDeviceError is the `err.code === 'EXDEV'` check for renameAtomicNoOverwrite's cross-volume
// fallback on Windows.
func isCrossDeviceError(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, errorNotSameDevice)
}
