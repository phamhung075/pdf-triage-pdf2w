package relocalize

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// renameAtomicNoOverwrite is a direct port of relocalize-document.ts:21-45.
//
// The TypeScript WHY comment, preserved verbatim (relocalize-document.ts:15-20):
//
// Moves sourcePath to desiredTargetPath without the check-then-act race a plain
// `existsSync` + `renameSync` has: fs.linkSync fails atomically with EEXIST if the
// target already exists (unlike renameSync, which would silently overwrite it on
// Windows), so a genuine collision always gets a fresh unique suffix instead of
// clobbering another file. Falls back to a plain rename across filesystem/volume
// boundaries (EXDEV), where an atomic link isn't possible.
//
// It returns the path the file actually landed at, which on a collision is the unique-suffixed
// candidate, not desiredTargetPath.
func renameAtomicNoOverwrite(sourcePath, desiredTargetPath string, maxAttempts int) (string, error) {
	dir := filepath.Dir(desiredTargetPath)
	ext := filepath.Ext(desiredTargetPath)
	base := strings.TrimSuffix(filepath.Base(desiredTargetPath), ext)

	candidate := desiredTargetPath
	for attempt := 0; attempt < maxAttempts; attempt++ {
		err := os.Link(sourcePath, candidate)
		if err == nil {
			if rmErr := os.Remove(sourcePath); rmErr != nil {
				return "", rmErr
			}
			return candidate, nil
		}
		if errors.Is(err, fs.ErrExist) {
			// `${base}_${Date.now()}_${attempt}${ext}` — UnixMilli is Date.now()'s millisecond clock.
			candidate = filepath.Join(dir, base+"_"+strconv.FormatInt(time.Now().UnixMilli(), 10)+"_"+strconv.Itoa(attempt)+ext)
			continue
		}
		if isCrossDeviceError(err) {
			// A plain rename across filesystem/volume boundaries, where link cannot work.
			if rnErr := os.Rename(sourcePath, candidate); rnErr != nil {
				return "", rnErr
			}
			return candidate, nil
		}
		return "", err
	}
	return "", fmt.Errorf("Failed to move '%s' to a unique path after %d attempts", sourcePath, maxAttempts)
}

// removeEmptyDirAndParent is the shared post-move cleanup in relocalize-document.ts (:94-103,
// :128-137, :364-373): remove the now-empty source directory, and then its parent when that is empty
// too. Every failure is swallowed, exactly as the TS `try { ... } catch (e) {}` does — a directory
// still holding another file is simply left in place.
func removeEmptyDirAndParent(filePath string) {
	oldDir := filepath.Dir(filePath)
	if !dirIsEmpty(oldDir) {
		return
	}
	if err := os.Remove(oldDir); err != nil {
		return
	}
	oldParent := filepath.Dir(oldDir)
	if dirIsEmpty(oldParent) {
		_ = os.Remove(oldParent)
	}
}

// dirIsEmpty is `fs.existsSync(dir) && fs.readdirSync(dir).length === 0`: a missing or unreadable
// directory is not empty.
func dirIsEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	return len(entries) == 0
}

// pathExists is `fs.existsSync`: true only when os.Stat succeeds (a broken symlink is "missing",
// exactly as Node reports it).
func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
