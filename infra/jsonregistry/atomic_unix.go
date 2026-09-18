//go:build !windows

package jsonregistry

import (
	"errors"
	"io/fs"
	"syscall"
)

// isPermOrBusy is the `e.code === 'EPERM' || e.code === 'EBUSY'` check for the atomic-rename
// fallback. errors.Is unwraps the *os.LinkError that os.Rename returns. fs.ErrPermission is included
// because os.Rename in a read-only directory returns EACCES, which Node surfaces as EPERM-family.
func isPermOrBusy(err error) bool {
	return errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EBUSY) ||
		errors.Is(err, fs.ErrPermission)
}
