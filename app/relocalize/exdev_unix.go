//go:build !windows

package relocalize

import (
	"errors"
	"syscall"
)

// isCrossDeviceError is the `err.code === 'EXDEV'` check for renameAtomicNoOverwrite's cross-volume
// fallback. errors.Is unwraps the *os.LinkError that os.Link returns.
func isCrossDeviceError(err error) bool {
	return errors.Is(err, syscall.EXDEV)
}
