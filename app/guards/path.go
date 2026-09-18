package guards

import (
	"path"
	"regexp"

	"github.com/phamhung075/pdf-triage-pdf2w/pathconv"
	"github.com/phamhung075/pdf-triage-pdf2w/taxonomy"
)

// windowsDrivePathRe matches a Windows drive-letter path: C:\..., C:/..., c:\...
var windowsDrivePathRe = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

// ResolveManagedPath is the ONE implementation of the resolveManagedPath path-boundary guard.
// It returns the cleaned absolute path when candidate is inside inputDir (__raws) or outputDir
// (__archive), and ("", *GuardViolation) when it escapes.
//
// The TypeScript WHY comment, preserved verbatim (web-server.ts:29-37):
//
// Resolves a caller-supplied path and asserts it is inside a managed directory.
//
// Every path this server legitimately touches lives under INPUT_DIR (__raws) or OUTPUT_ROOT_DIR
// (__archive). Endpoints that took a path straight from the request body and only checked
// existsSync would happily read any file the Node process can — an SSH key, another app's .env —
// and, for the PDF tools, write a derivative of it into __raws, where the auto-watcher then
// classifies and archives it into the searchable registry. Returns null when the path escapes.
//
// The boundary test itself is taxonomy.IsPathInsideDir, whose own TS comment is preserved in that
// package: a plain string-prefix check would also match an unrelated sibling that merely shares a
// prefix (e.g. "__archive" vs "__archive_old").
//
// # WSL / Windows path forms
//
// settings.ts normalizes the managed roots through pathconv on WSL, so on the real deployment they
// are `/mnt/<drive>/...` POSIX paths (settings.ts:489; NormalizePathInput). When either managed
// root IS a WSL mount path, a Windows-spelled candidate (`C:\...`, `C:/...`, or the mangled
// `\mnt\C:\...` form) is translated by pathconv.WindowsToWSLPathHost into the same form before the
// boundary check. When the roots are not WSL mount paths, a drive-letter candidate is REJECTED:
// Node's path.resolve on the POSIX target treats it as a relative path and resolves it against the
// process cwd (verified: path.isAbsolute("C:/x") === false on Linux), so it can never equal a
// drive-form root. web-server.test.ts's "returns 404 if file path by path query does not exist"
// expects 404 for exactly such a candidate but the live TS server returns 403; this guard pins the
// ACTUAL 403 verdict. See path_test.go and the package comment.
//
// The check is purely lexical: it does not resolve symlinks, exactly like Node's path.normalize, so
// a symlink whose textual path is inside a root is accepted (a symlink cannot widen the managed
// boundary because the host filesystem still resolves it under that path).
func ResolveManagedPath(candidate, inputDir, outputDir string) (string, *GuardViolation) {
	if jsTrim(candidate) == "" {
		return "", PathOutsideManagedDirectoriesViolation()
	}

	abs := candidate
	rootsAreWSL := pathconv.IsWSLMountPath(inputDir) || pathconv.IsWSLMountPath(outputDir)
	if rootsAreWSL {
		abs = pathconv.WindowsToWSLPathHost(abs)
	} else if windowsDrivePathRe.MatchString(abs) && !path.IsAbs(abs) {
		// Node path.resolve on POSIX prepends cwd to a drive-letter path; it cannot be inside a
		// drive-form managed root. Reject without touching cwd or the filesystem.
		return "", PathOutsideManagedDirectoriesViolation()
	}

	if !path.IsAbs(abs) {
		// Node path.resolve would make this absolute against the process cwd. No legitimate caller
		// supplies a bare relative managed path, and guessing the cwd here would make the guard
		// depend on process state, so reject.
		return "", PathOutsideManagedDirectoriesViolation()
	}

	abs = path.Clean(abs)
	if taxonomy.IsPathInsideDir(abs, inputDir) || taxonomy.IsPathInsideDir(abs, outputDir) {
		return abs, nil
	}
	return "", PathOutsideManagedDirectoriesViolation()
}

// PathOutsideManagedDirectoriesViolation returns the 403 violation web-server.ts:1125 returns for
// a path outside INPUT_DIR/OUTPUT_ROOT_DIR. The exact message is preserved.
func PathOutsideManagedDirectoriesViolation() *GuardViolation {
	return newViolation(CodePathOutsideManaged,
		"Path is outside the managed input/output directories — not allowed.", 403)
}
