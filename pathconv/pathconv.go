// Package pathconv is a Go port of pdf-triage's src/domain/path-conversion.ts:
// windowsToWslPath, wslToWindowsPath and isWslMountPath.
//
// Windows <-> WSL path conversion — the single source of truth for every path-form problem.
//
// Why this module exists (history, so it never regresses):
//
//  1. A settings.json holding a Windows-style path (`\mnt\C:\Users\...` with backslashes) was
//     passed to Node's fs on Linux, where backslash is a LEGAL filename character. The app never
//     resolved it to the OneDrive folder; instead it created real directories literally named
//     `\mnt\C:\Users\...\__raws` inside the project root and scanned those empty stubs — "the
//     system config cannot see files on Windows".
//  2. The reverse happened in the other direction: "Open Incoming"/"Open Archive" handed the
//     POSIX path `/mnt/c/Users/...` straight to Windows Explorer. Explorer is a Windows program; it
//     cannot resolve a POSIX path and silently fell back to opening C:\Users\<user>\Documents.
//
// The two rules that follow from this:
//   - Paths the APP reads/writes with Node's fs on WSL must be `/mnt/<drive>/...` (POSIX).
//   - Paths handed to WINDOWS programs (Windows Explorer, Google Chrome) must be `X:\...` (Windows).
//
// Pure functions, zero I/O — safe to call from domain code and unit-test anywhere.
//
// Two deviations from the TS signatures are intentional and preserve its behavior:
//
//  1. `raw: unknown` + `String(raw ?? ”)` becomes a plain `string` parameter. Go's type system
//     makes the coercion the caller's responsibility, and a Go string cannot be null/undefined;
//     every test case passes an already-string value, so behavior is identical.
//  2. TS's `platform = process.platform` default parameter has no Go equivalent. WindowsToWSLPath
//     takes the platform explicitly (Node's spelling: "linux", "darwin", "win32", ...), and
//     WindowsToWSLPathHost supplies the process.platform default by translating runtime.GOOS —
//     including the one name that differs, Go's "windows" vs Node's "win32".
package pathconv

import (
	"regexp"
	"runtime"
	"strings"
)

var (
	// Mangled-WSL and drive forms all collapse onto the same /mnt/<lower-drive><rest>.
	wslColonDriveRe = regexp.MustCompile(`^/mnt/([A-Za-z]):(/.*)$`)
	wslDriveRe      = regexp.MustCompile(`^/mnt/([A-Za-z])(/.*)$`)
	driveRe         = regexp.MustCompile(`^([A-Za-z]):(/.*)$`)

	// isWslMountPath / wslToWindowsPath: /mnt/<letter> with an optional remainder.
	mountRe = regexp.MustCompile(`^/mnt/([A-Za-z])(/.*)?$`)
)

// IsWSLMountPath reports whether the path is a WSL mount path: `/mnt/<letter>` or
// `/mnt/<letter>/...`.
func IsWSLMountPath(input string) bool {
	return mountRe.MatchString(strings.TrimSpace(input))
}

// WindowsToWSLPath converts any Windows spelling into the POSIX `/mnt/<drive>/...` form this host's
// fs needs.
//
// On native Windows the input is already correct (Node accepts both separators) — no-op.
// On POSIX/WSL:
//
//	C:\Users\you\...      -> /mnt/c/Users/you/...
//	\mnt\C:\Users\you\... -> /mnt/c/Users/you/...   (mangled WSL spelling from the UI)
//	/mnt/C/Users/you/...  -> /mnt/c/Users/you/...   (uppercase drive letter)
//	/custom/path          -> /custom/path           (plain POSIX path untouched)
//
// The `platform` param exists only so the unit tests can exercise the conversion on any host; pass
// "win32" for the native-Windows no-op.
func WindowsToWSLPath(raw, platform string) string {
	value := strings.TrimSpace(raw)
	if value == "" || platform == "win32" {
		return value
	}
	forward := strings.ReplaceAll(value, `\`, "/")
	if m := wslColonDriveRe.FindStringSubmatch(forward); m != nil {
		return "/mnt/" + strings.ToLower(m[1]) + m[2]
	}
	if m := wslDriveRe.FindStringSubmatch(forward); m != nil {
		return "/mnt/" + strings.ToLower(m[1]) + m[2]
	}
	if m := driveRe.FindStringSubmatch(forward); m != nil {
		return "/mnt/" + strings.ToLower(m[1]) + m[2]
	}
	return forward
}

// WindowsToWSLPathHost supplies the TS default (`platform = process.platform`) by translating
// runtime.GOOS into Node's platform spelling. The only differing name is Windows: Go reports
// "windows" where Node reports "win32".
func WindowsToWSLPathHost(raw string) string {
	platform := runtime.GOOS
	if platform == "windows" {
		platform = "win32"
	}
	return WindowsToWSLPath(raw, platform)
}

// WSLToWindowsPath is the reverse of WindowsToWSLPath: it converts a WSL mount path into the Windows
// path form that Windows programs expect, so it can be handed to Windows Explorer / Google Chrome
// spawned from WSL.
//
// Pure string transform, no I/O: only `/mnt/<drive>/...` prefixes are rewritten
// (`/mnt/c/Users/you/__raws` -> `C:\Users\you\__raws`); every other path is returned unchanged,
// which keeps native-Windows installs (already Windows-form) and plain POSIX paths untouched.
func WSLToWindowsPath(input string) string {
	value := strings.TrimSpace(input)
	if value == "" {
		return value
	}
	if m := mountRe.FindStringSubmatch(value); m != nil {
		rest := m[2]
		if rest == "" {
			rest = "/"
		}
		rest = strings.ReplaceAll(rest, "/", `\`)
		return strings.ToUpper(m[1]) + ":" + rest
	}
	return value
}
