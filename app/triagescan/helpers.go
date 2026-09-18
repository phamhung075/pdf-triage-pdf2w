package triagescan

// This file ports the three filesystem helpers exported by src/application/triage-scan.ts:
//
//	moveDuplicateFileToDuplicatesFolder  (triage-scan.ts:546-574)
//	cleanEmptyDirectories                (triage-scan.ts:576-618)
//	moveBlockedFileToBlockedFolder       (triage-scan.ts:620-648)
//
// The only signature change is the platform convention this repo's Go port already uses: the
// input directory arrives as an explicit parameter instead of the settings module's CONFIG global,
// so the helpers are pure filesystem functions with no hidden dependency.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MoveDuplicateFileToDuplicatesFolder is moveDuplicateFileToDuplicatesFolder. It parks an incoming
// duplicate under <inputDir>/.duplicates_files, appending a `_dupN` suffix when the name is taken,
// and returns the resting path.
func MoveDuplicateFileToDuplicatesFolder(inputDir, originalPath string) (string, error) {
	return moveToCollisionDir(inputDir, ".duplicates_files", "_dup", originalPath)
}

// MoveBlockedFileToBlockedFolder is moveBlockedFileToBlockedFolder. It parks a blocked document
// under <inputDir>/.blocked_files, appending a `_blockedN` suffix when the name is taken, and
// returns the resting path.
func MoveBlockedFileToBlockedFolder(inputDir, originalPath string) (string, error) {
	return moveToCollisionDir(inputDir, ".blocked_files", "_blocked", originalPath)
}

// moveToCollisionDir is the shared body of the two movers. The TS functions are byte-for-byte
// identical apart from the folder name and the suffix, so the port factors them. A rename failure
// falls back to copy-then-unlink exactly as the TS catch block does (EXDEV between __raws and a
// temp location is the real case).
func moveToCollisionDir(inputDir, dirName, suffix, originalPath string) (string, error) {
	file := filepath.Base(originalPath)
	dir := filepath.Join(inputDir, dirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	targetPath := filepath.Join(dir, file)
	if fileExists(targetPath) && targetPath != originalPath {
		ext := filepath.Ext(file)
		base := strings.TrimSuffix(file, ext)
		counter := 1
		for fileExists(targetPath) {
			targetPath = filepath.Join(dir, fmt.Sprintf("%s%s%d%s", base, suffix, counter, ext))
			counter++
		}
	}

	if originalPath != targetPath {
		if err := os.Rename(originalPath, targetPath); err != nil {
			if err := copyFile(originalPath, targetPath); err != nil {
				return "", err
			}
			if err := os.Remove(originalPath); err != nil {
				return "", err
			}
		}
	}

	return targetPath, nil
}

// CleanEmptyDirectories is cleanEmptyDirectories. It removes nested empty directories under dir,
// keeping baseInputDir itself and every hidden/duplicates/blocked directory. `log` may be nil; the
// TS version logs through the module logger.
func CleanEmptyDirectories(dir, baseInputDir string, log Logger) {
	if !fileExists(dir) {
		return
	}

	items, err := os.ReadDir(dir)
	if err != nil {
		return
	}

	for _, item := range items {
		if !item.IsDir() {
			continue
		}
		name := item.Name()
		fullPath := filepath.Join(dir, name)

		if samePath(fullPath, baseInputDir) ||
			strings.HasPrefix(name, ".") ||
			name == "duplicates_files" || name == "duplicates" ||
			name == "blocked_files" || name == "blocked" {
			continue
		}

		CleanEmptyDirectories(fullPath, baseInputDir, log)

		if !fileExists(fullPath) {
			continue
		}

		remainingItems := []string{}
		subEntries, readErr := os.ReadDir(fullPath)
		if readErr != nil {
			continue
		}
		for _, sub := range subEntries {
			n := sub.Name()
			if n == "Thumbs.db" || n == ".DS_Store" || n == "desktop.ini" {
				continue
			}
			remainingItems = append(remainingItems, n)
		}

		if len(remainingItems) == 0 {
			if err := os.RemoveAll(fullPath); err != nil {
				logWarn(log, moduleTriage, fmt.Sprintf("Failed to remove empty directory %s: %s", fullPath, err.Error()), nil)
			} else {
				logInfo(log, moduleTriage, "Cleaned up empty directory in __raws: "+fullPath, nil)
			}
		}
	}
}

// fileExists is `fs.existsSync`: true only when os.Stat succeeds, so a broken symlink is missing.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// samePath is `path.resolve(a) === path.resolve(b)` for the paths this package handles.
func samePath(a, b string) bool {
	ca, errA := filepath.Abs(a)
	cb, errB := filepath.Abs(b)
	if errA == nil && errB == nil {
		return ca == cb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

// copyFile is the TS catch-block fallback (`fs.copyFileSync` then `fs.unlinkSync`).
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

// mtimeMs is Node's `fs.Stats.mtimeMs`: milliseconds since the epoch as a float, carrying the
// filesystem's sub-millisecond precision. Go's FileInfo exposes nanoseconds, so the conversion is
// the closest exact representation and round-trips on the same file.
func mtimeMs(info os.FileInfo) float64 {
	return float64(info.ModTime().UnixNano()) / 1e6
}
