//go:build windows

package settings

import (
	"errors"
	"io/fs"
	"syscall"
)

// isPermOrBusy is the `e.code === 'EPERM' || e.code === 'EBUSY'` check for the atomic-rename
// fallback, matching infra/jsonregistry. On Windows os.Rename reports the raw Win32 error through
// *os.LinkError; the codes Node's libuv translates to EPERM/EBUSY are ERROR_ACCESS_DENIED (5),
// ERROR_SHARING_VIOLATION (32) and ERROR_LOCK_VIOLATION (33). The syscall package does not export
// the latter two, so they are compared as raw Errno values.
func isPermOrBusy(err error) bool {
	const (
		errorAccessDenied     = syscall.Errno(5)
		errorSharingViolation = syscall.Errno(32)
		errorLockViolation    = syscall.Errno(33)
	)
	return errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EBUSY) ||
		errors.Is(err, fs.ErrPermission) ||
		errors.Is(err, errorAccessDenied) ||
		errors.Is(err, errorSharingViolation) ||
		errors.Is(err, errorLockViolation)
}
