//go:build !windows

package settings

import (
	"errors"
	"io/fs"
	"syscall"
)

// isPermOrBusy is the `e.code === 'EPERM' || e.code === 'EBUSY'` check for the atomic-rename
// fallback, matching infra/jsonregistry. errors.Is unwraps the *os.LinkError that os.Rename
// returns; fs.ErrPermission covers a read-only-directory EACCES.
func isPermOrBusy(err error) bool {
	return errors.Is(err, syscall.EPERM) ||
		errors.Is(err, syscall.EBUSY) ||
		errors.Is(err, fs.ErrPermission)
}
