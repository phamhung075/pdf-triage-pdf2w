// Package osopen is a Go port of pdf-triage's src/infrastructure/os-open.ts (91 lines):
// RevealInFileManager, OpenDirectory, ResolveChromeExecutable and OpenInChrome, plus the
// LaunchSpec -> Plan rename and a Runner seam for the actual exec.
//
// Host-aware OS launching — the ONLY module allowed to know how to open a file manager or Chrome.
//
// Every GUI-launch in the app (web-server routes, MCP tools) must go through these builders so the
// "Windows program gets a POSIX /mnt path and silently falls back to Documents" bug (see
// path-conversion.ts) can never be reintroduced. The hygiene test osopen_hygiene_test.go scans the
// Go module and fails the build if a launcher-executable literal appears in a non-test source file
// outside this package.
//
// These builders return a spawn-ready Plan; the CALLER owns the actual spawn (so the web-server
// tests can keep mocking child_process). Fire-and-forget spawning is the caller's job:
// spawn(spec.cmd, spec.args, { detached: true, stdio: 'ignore' }).unref().
//
// The upstream TypeScript suite is GREEN at port time
// (`npx vitest run src/infrastructure/os-open.test.ts` -> 10 passed; the hygiene suite
// `os-open.hygiene.test.ts` -> 2 passed), so no upstream case is pinned red. All 10 functional
// cases are ported, plus four added Go cases (documented in the test file).
//
// # Deviations from the TS source, each resolved to MATCH its behavior
//
//  1. `LaunchSpec` is renamed `Plan`. The objective names the shape a Plan type; fields keep the
//     `Cmd`/`Args` spelling for the `{cmd,args}` pair, and the zero value is inert. `Plan` has no
//     `Wait`/`Detached` field: the TS type is exactly `{cmd,args}`, and detached spawning stays the
//     caller's concern.
//  2. `process.platform` defaults are replaced by an injected `platform` argument plus the
//     `DefaultDeps()` seam. `platformFromGOOS` translates runtime.GOOS with the one differing name:
//     Go's "windows" is Node's "win32".
//  3. Node's `path.join` separator semantics are reproduced by `nodeJoinPath`. On a Linux test host
//     `path.join('C:\\Program Files','Google',...)` yields `C:\Program Files/Google/...` (the
//     embedded backslash is a literal, not a separator), which the TS test asserts. A Go
//     `path.Join` would differ on Windows and a plain slash-join would differ from Node's
//     separator collapsing, so both candidate construction and the parity test use nodeJoinPath.
//     It deliberately joins with `/` on non-Windows hosts; these are existence-probe candidates,
//     never paths handed to a Windows program, so Golden Rule 21 is not implicated (Windows
//     programs only ever receive paths through the PathConv seam below).
//  4. `fs.existsSync` and `process.env` become `Deps.Exists` and `Deps.Getenv`. `chromeCandidates`
//     reads `LOCALAPPDATA`, `ProgramFiles` and `ProgramFiles(x86)` at call time (not package init)
//     and fills the same `C:\Program Files` / `C:\Program Files (x86)` defaults when unset, exactly
//     as the TS `||` fallback does. Tests inject fakes, so no test touches the real filesystem.
//  5. Golden Rule 21 (WSL path discipline): the path handed to a Windows program is converted with
//     the existing pathconv package through the PathConv seam (`WSLToWindowsPath`), imported rather
//     than reimplemented. A POSIX `/mnt/...` path is never passed through to explorer.exe/chrome.exe.
//  6. `Runner` is the new exec seam the objective requires. It is intentionally not used by the
//     builders; `ExecRunner` is the minimal real implementation for callers that do not need the TS
//     `detached: true, stdio: 'ignore'` fire-and-forget behavior, which a caller can still provide
//     by spawning the Plan itself. Tests always use a fake Runner, so no test launches a process.
//  7. Only `Runner`'s `ExecRunner.Run` starts a process; the exported builders are pure. The
//     launcher literals this package owns are exported as `ForbiddenLauncherLiterals` so the
//     hygiene test can assert on the same list without its own source containing a literal.
package osopen

import (
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/phamhung075/pdf-triage-pdf2w/pathconv"
)

// Platform constants use Node's process.platform spelling, not runtime.GOOS, because the builder
// arguments are the TS `platform: NodeJS.Platform` values. Go constants are untyped so callers may
// compare them with a plain string.
const (
	// PlatformWindows is Node's "win32" (Go's runtime.GOOS is "windows").
	PlatformWindows = "win32"
	// PlatformDarwin is Node's "darwin" (Go's runtime.GOOS is "darwin").
	PlatformDarwin = "darwin"
	// PlatformLinux is Node's "linux" (Go's runtime.GOOS is "linux").
	PlatformLinux = "linux"
)

// ForbiddenLauncherLiterals is the set of OS-launcher executable names that may appear ONLY in this
// package's non-test source. It is the same list the hygiene test enforces and the same list Golden
// Rule 21 documents.
//
// The first three are the TS os-open.hygiene.test.ts FORBIDDEN_LITERALS exactly. The remaining two
// widen the guard to the remaining OS-launch spellings that would otherwise be a new bypass:
// `osascript` (macOS GUI scripting) and `cmd.exe` (Windows shell). `open` is deliberately NOT
// listed: it is also an ordinary word ("open", the function name), so scanning for it would produce
// false positives without adding real coverage.
var ForbiddenLauncherLiterals = []string{
	"explorer.exe",
	"chrome.exe",
	"xdg-open",
	"osascript",
	"cmd.exe",
}

// Plan is the spawn-ready `{cmd, args}` pair the TS source calls `LaunchSpec`. The caller owns the
// actual spawn.
type Plan struct {
	Cmd  string
	Args []string
}

// Runner executes a Plan. It exists so the actual exec is a seam: tests inject a fake and never
// launch a real process.
type Runner interface {
	Run(plan Plan) error
}

// ExecRunner is the minimal real Runner: exec.Command(...).Run(), i.e. it waits for the process.
// It carries no globals and is safe to use as a zero value.
type ExecRunner struct{}

// Run executes plan and waits for it to finish. It launches the plan exactly as given; the builders
// are the only sanctioned producers of a plan.
func (ExecRunner) Run(plan Plan) error {
	return exec.Command(plan.Cmd, plan.Args...).Run()
}

// PathConv is the path-form collaborator Golden Rule 21 depends on. It is the subset of the
// pathconv package this package needs; the real implementation is pathconvConv. Keeping it an
// interface (rather than calling pathconv directly) lets tests inject a converter without touching
// the real path package, and documents the only conversions this package is allowed to use.
type PathConv interface {
	// IsWSLMountPath reports whether a path is a POSIX `/mnt/<drive>/...` WSL mount path.
	IsWSLMountPath(path string) bool
	// WSLToWindowsPath converts `/mnt/<drive>/...` to the `X:\...` form a Windows program needs.
	WSLToWindowsPath(path string) string
	// DirName returns the parent directory using the host platform's separator, mirroring Node's
	// path.dirname on the host. It is only used for the Linux xdg-open reveal fallback.
	DirName(path string) string
}

// Deps is the injected environment for the plan builders: the environment lookup, the
// file-existence probe, and the WSL path converter. The host platform is NOT stored here: each
// builder takes it as an explicit argument (the TS `platform` parameter), so a stale field could not
// silently disagree with the call. No package globals are involved, so tests build a Deps with fakes.
type Deps struct {
	// Getenv reads an environment variable (process.env[...]).
	Getenv func(string) string
	// Exists probes whether a path exists on disk (fs.existsSync).
	Exists func(string) bool
	// PathConv provides the WSL mount path converter (Golden Rule 21).
	PathConv PathConv
}

// pathconvConv is the real PathConv, delegating to the ported pathconv package.
type pathconvConv struct{}

// RealPathConv returns the pathconv-backed PathConv the package uses by default. It is exported so
// tests and callers can build a Deps that uses the real converter with fake env/exists seams.
func RealPathConv() PathConv { return pathconvConv{} }

func (pathconvConv) IsWSLMountPath(path string) bool { return pathconv.IsWSLMountPath(path) }

func (pathconvConv) WSLToWindowsPath(path string) string { return pathconv.WSLToWindowsPath(path) }

func (pathconvConv) DirName(path string) string { return nodeDirName(path, runtime.GOOS) }

// DefaultDeps returns the real seams: os.Getenv, os.Stat-based Exists, and the pathconv-backed
// converter. The host platform is passed to the builders explicitly; use PlatformFromGOOS to derive
// it from runtime.GOOS.
func DefaultDeps() Deps {
	return Deps{
		Getenv:   os.Getenv,
		Exists:   fileExists,
		PathConv: RealPathConv(),
	}
}

// PlatformFromGOOS maps runtime.GOOS to Node's process.platform spelling. The only differing name
// is Windows: Go reports "windows" where Node reports "win32".
func PlatformFromGOOS(goos string) string {
	if goos == "windows" {
		return PlatformWindows
	}
	return goos
}

// fileExists is the default fs.existsSync equivalent: os.Stat succeeds for any path that exists,
// which is the same boolean Node's existsSync yields.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// RevealInFileManager reveals a FILE in the OS file manager (Explorer /select, macOS Reveal /
// xdg-open), converting WSL mount paths to Windows form before handing them to explorer.exe (WSL
// interop).
func RevealInFileManager(filePath, platform string, deps Deps) Plan {
	if platform == "win32" {
		return Plan{Cmd: "explorer.exe", Args: []string{"/select,", filePath}}
	}
	if platform == "darwin" {
		return Plan{Cmd: "open", Args: []string{"-R", filePath}}
	}
	if deps.PathConv.IsWSLMountPath(filePath) {
		return Plan{Cmd: "explorer.exe", Args: []string{"/select,", deps.PathConv.WSLToWindowsPath(filePath)}}
	}
	return Plan{Cmd: "xdg-open", Args: []string{deps.PathConv.DirName(filePath)}}
}

// OpenDirectory opens a DIRECTORY in the OS file manager (no /select), converting WSL mounts for
// explorer.exe.
func OpenDirectory(dirPath, platform string, deps Deps) Plan {
	if platform == "win32" {
		return Plan{Cmd: "explorer.exe", Args: []string{dirPath}}
	}
	if platform == "darwin" {
		return Plan{Cmd: "open", Args: []string{dirPath}}
	}
	if deps.PathConv.IsWSLMountPath(dirPath) {
		return Plan{Cmd: "explorer.exe", Args: []string{deps.PathConv.WSLToWindowsPath(dirPath)}}
	}
	return Plan{Cmd: "xdg-open", Args: []string{dirPath}}
}

// ResolveChromeExecutable locates the Chrome executable. On native Windows the ProgramFiles env vars
// are set; under WSL they are empty, so the /mnt/c candidates are probed too. exists is injectable
// so unit tests don't touch the real fs. Returns "" when nothing is found.
//
// The candidate order is exactly the TS one: Program Files, Program Files (x86), LOCALAPPDATA (when
// set), the two /mnt/c mounts, then the bare `chrome` command.
func ResolveChromeExecutable(platform string, deps Deps) string {
	return resolveChromeExecutableFrom(platform, deps, chromeCandidates(deps))
}

// resolveChromeExecutableFrom is the candidate-probing core; it exists so the candidate list can be
// constructed with a different (e.g. fake) environment than the one used to probe the filesystem.
func resolveChromeExecutableFrom(platform string, deps Deps, candidates []string) string {
	if platform == "darwin" {
		macChrome := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
		if deps.Exists(macChrome) {
			return macChrome
		}
		return "open"
	}
	for _, candidate := range candidates {
		// TS `if (c === 'chrome' || exists(c)) return c;` — the bare command is returned even when
		// it is not found on disk.
		if candidate == "chrome" || deps.Exists(candidate) {
			return candidate
		}
	}
	return ""
}

// chromeCandidates builds the TS `candidates` array for deps' environment.
func chromeCandidates(deps Deps) []string {
	// TS defaults: `process.env['ProgramFiles'] || 'C:\\Program Files'`; LOCALAPPDATA defaults to ''
	// and its joined candidate is dropped by `.filter(Boolean)`.
	localAppData := deps.Getenv("LOCALAPPDATA")
	programFiles := deps.Getenv("ProgramFiles")
	if programFiles == "" {
		programFiles = `C:\Program Files`
	}
	programFilesX86 := deps.Getenv("ProgramFiles(x86)")
	if programFilesX86 == "" {
		programFilesX86 = `C:\Program Files (x86)`
	}

	candidates := []string{
		nodeJoinPath(programFiles, "Google", "Chrome", "Application", "chrome.exe"),
		nodeJoinPath(programFilesX86, "Google", "Chrome", "Application", "chrome.exe"),
	}
	if localAppData != "" {
		candidates = append(candidates, nodeJoinPath(localAppData, "Google", "Chrome", "Application", "chrome.exe"))
	}
	// WSL: the env vars above are Linux-side and empty, so look on the /mnt/c mounts.
	candidates = append(candidates,
		`/mnt/c/Program Files/Google/Chrome/Application/chrome.exe`,
		`/mnt/c/Program Files (x86)/Google/Chrome/Application/chrome.exe`,
		"chrome",
	)
	return candidates
}

// OpenInChrome launches Chrome on a local PDF path. Returns nil when the file doesn't exist or no
// Chrome executable is found. The target path is converted to Windows form for chrome.exe (WSL
// interop); the conversion is a no-op for already-Windows or plain POSIX paths.
func OpenInChrome(filePath, platform string, deps Deps) *Plan {
	if !deps.Exists(filePath) {
		return nil
	}
	cmd := ResolveChromeExecutable(platform, deps)
	if cmd == "" {
		return nil
	}
	return &Plan{Cmd: cmd, Args: []string{wslOrNative(filePath, platform, deps)}}
}

// wslOrNative is the TS `wslOrNative(filePath, platform)`: on native Windows the path is already
// Windows-form; everywhere else it goes through wslToWindowsPath, which is a no-op for non-mount
// paths.
func wslOrNative(filePath, platform string, deps Deps) string {
	if platform == "win32" {
		return filePath
	}
	return deps.PathConv.WSLToWindowsPath(filePath)
}

// nodeJoinPath mirrors Node's path.join on the host platform for the Chrome candidate paths. See the
// package doc comment, deviation 3.
func nodeJoinPath(segments ...string) string {
	separator := "/"
	if runtime.GOOS == "windows" {
		separator = `\`
	}
	kept := make([]string, 0, len(segments))
	for _, segment := range segments {
		if segment != "" {
			kept = append(kept, segment)
		}
	}
	joined := strings.Join(kept, separator)
	// Node's path.join normalizes runs of separators (e.g. `a//b` -> `a/b`).
	for strings.Contains(joined, separator+separator) {
		joined = strings.ReplaceAll(joined, separator+separator, separator)
	}
	return joined
}

// nodeDirName mirrors Node's path.dirname on the host platform: the Windows build treats both `/`
// and `\` as separators, the POSIX build treats only `/`. Go's filepath.Dir is close but its Clean
// step diverges for some root-relative paths; this faithful scan matches Node's documented behavior
// instead. It is only reached by the xdg-open fallback.
func nodeDirName(path, goos string) string {
	if path == "" {
		return "."
	}
	hasRoot := strings.HasPrefix(path, "/")
	normalized := path
	if goos == "windows" {
		normalized = strings.ReplaceAll(path, `\`, "/")
		hasRoot = strings.HasPrefix(normalized, "/")
	}
	end := strings.LastIndex(normalized, "/")
	if end < 0 {
		return "."
	}
	head := normalized[:end]
	if !hasRoot && end == 0 {
		return "."
	}
	if head == "" {
		return "/"
	}
	return head
}
