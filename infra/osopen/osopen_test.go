package osopen

import (
	"runtime"
	"strings"
	"testing"
)

// Test cases below are ported case-for-case from pdf-triage's
// src/infrastructure/os-open.test.ts (all 10 cases; the upstream suite is GREEN at port time,
// `npx vitest run src/infrastructure/os-open.test.ts` -> 10 passed), plus added Go cases that pin
// seams the TS suite could not (the /mnt/c root edge case, the Runner seam, DefaultDeps, and the
// GOOS mapping).

// TestRevealInFileManager ports the four `revealInFileManager` cases.
func TestRevealInFileManager(t *testing.T) {
	t.Run("uses Explorer /select on native Windows (path already Windows-form)", func(t *testing.T) {
		got := RevealInFileManager(`C:\Users\you\__archive\a.pdf`, PlatformWindows, DefaultDeps())
		want := Plan{Cmd: "explorer.exe", Args: []string{"/select,", `C:\Users\you\__archive\a.pdf`}}
		assertPlanEqual(t, got, want)
	})

	t.Run("uses Finder reveal on macOS", func(t *testing.T) {
		got := RevealInFileManager("/Users/you/a.pdf", PlatformDarwin, DefaultDeps())
		want := Plan{Cmd: "open", Args: []string{"-R", "/Users/you/a.pdf"}}
		assertPlanEqual(t, got, want)
	})

	t.Run("converts a WSL mount path to Windows form before handing it to explorer.exe (interop)", func(t *testing.T) {
		got := RevealInFileManager("/mnt/c/Users/you/__archive/a.pdf", PlatformLinux, DefaultDeps())
		want := Plan{Cmd: "explorer.exe", Args: []string{"/select,", `C:\Users\you\__archive\a.pdf`}}
		assertPlanEqual(t, got, want)
	})

	t.Run("falls back to xdg-open with the parent dir for plain Linux paths", func(t *testing.T) {
		got := RevealInFileManager("/home/you/archive/a.pdf", PlatformLinux, DefaultDeps())
		want := Plan{Cmd: "xdg-open", Args: []string{"/home/you/archive"}}
		assertPlanEqual(t, got, want)
	})
}

// TestOpenDirectory ports the three `openDirectory` cases plus the /mnt/c root edge case.
func TestOpenDirectory(t *testing.T) {
	t.Run("opens the dir directly in Explorer on Windows", func(t *testing.T) {
		got := OpenDirectory(`C:\Users\you\__raws`, PlatformWindows, DefaultDeps())
		want := Plan{Cmd: "explorer.exe", Args: []string{`C:\Users\you\__raws`}}
		assertPlanEqual(t, got, want)
	})

	t.Run("converts a WSL mount dir for explorer.exe on Linux", func(t *testing.T) {
		got := OpenDirectory("/mnt/c/Users/you/__raws", PlatformLinux, DefaultDeps())
		want := Plan{Cmd: "explorer.exe", Args: []string{`C:\Users\you\__raws`}}
		assertPlanEqual(t, got, want)
	})

	t.Run("converts the bare /mnt/c root to C:\\ (pathconv behavior)", func(t *testing.T) {
		got := OpenDirectory("/mnt/c", PlatformLinux, DefaultDeps())
		want := Plan{Cmd: "explorer.exe", Args: []string{`C:\`}}
		assertPlanEqual(t, got, want)
	})

	t.Run("uses xdg-open for a plain Linux dir", func(t *testing.T) {
		got := OpenDirectory("/home/you/raws", PlatformLinux, DefaultDeps())
		want := Plan{Cmd: "xdg-open", Args: []string{"/home/you/raws"}}
		assertPlanEqual(t, got, want)
	})
}

// TestResolveChromeExecutable ports the three `resolveChromeExecutable` cases.
func TestResolveChromeExecutable(t *testing.T) {
	// TS `const exists = (paths: string[]) => (p: string) => paths.includes(p);`
	existsIn := func(paths ...string) map[string]bool {
		set := make(map[string]bool, len(paths))
		for _, p := range paths {
			set[p] = true
		}
		return set
	}

	t.Run("picks the first existing candidate (Program Files on Windows)", func(t *testing.T) {
		// path.join uses the HOST separator, so on a Linux test host the candidate is forward-slash
		// mixed form; on a real Windows host it is pure backslashes. Build the expected the same way.
		expected := nodePathJoinTest(`C:\Program Files`, "Google", "Chrome", "Application", "chrome.exe")
		if got := ResolveChromeExecutable(PlatformWindows, newDeps(existsIn(expected), nil)); got != expected {
			t.Fatalf("got %q, want %q", got, expected)
		}
	})

	t.Run("probes the /mnt/c mounts under WSL when the Program Files env vars are empty", func(t *testing.T) {
		// Simulate a Linux host: no ProgramFiles env; only the /mnt/c candidate exists.
		wslChrome := "/mnt/c/Program Files/Google/Chrome/Application/chrome.exe"
		if got := ResolveChromeExecutable(PlatformLinux, newDeps(existsIn(wslChrome), nil)); got != wslChrome {
			t.Fatalf("got %q, want %q", got, wslChrome)
		}
	})

	t.Run("falls back to the bare \"chrome\" command when nothing is found on disk", func(t *testing.T) {
		if got := ResolveChromeExecutable(PlatformLinux, newDeps(map[string]bool{}, nil)); got != "chrome" {
			t.Fatalf("got %q, want %q", got, "chrome")
		}
	})
}

// TestOpenInChrome ports the three `openInChrome` cases.
func TestOpenInChrome(t *testing.T) {
	t.Run("returns null when the target file does not exist", func(t *testing.T) {
		got := OpenInChrome("/mnt/c/x.pdf", PlatformLinux, newDeps(map[string]bool{}, nil))
		if got != nil {
			t.Fatalf("got %+v, want nil", got)
		}
	})

	t.Run("converts a WSL target path for chrome.exe", func(t *testing.T) {
		target := "/mnt/c/Users/you/__archive/a.pdf"
		wslChrome := "/mnt/c/Program Files/Google/Chrome/Application/chrome.exe"
		onlyWslChrome := newDeps(map[string]bool{target: true, wslChrome: true}, nil)
		got := OpenInChrome(target, PlatformLinux, onlyWslChrome)
		if got == nil {
			t.Fatal("got nil plan, want a chrome plan")
		}
		if got.Cmd != wslChrome {
			t.Fatalf("cmd = %q, want %q", got.Cmd, wslChrome)
		}
		assertArgsEqual(t, got.Args, []string{`C:\Users\you\__archive\a.pdf`})
	})

	t.Run("keeps an already-Windows path as-is", func(t *testing.T) {
		target := `C:\Users\you\a.pdf`
		deps := newDeps(map[string]bool{target: true}, nil)
		got := OpenInChrome(target, PlatformWindows, deps)
		if got == nil {
			t.Fatal("got nil plan, want a chrome plan")
		}
		assertArgsEqual(t, got.Args, []string{target})
	})
}

// TestRevealInFileManagerPreservesArgsAcrossCalls pins that plan construction copies the argument
// slice rather than sharing mutable state between calls.
func TestRevealInFileManagerPreservesArgsAcrossCalls(t *testing.T) {
	first := RevealInFileManager(`C:\Users\you\a.pdf`, PlatformWindows, DefaultDeps())
	second := RevealInFileManager(`C:\Users\you\b.pdf`, PlatformWindows, DefaultDeps())
	if first.Args[1] == second.Args[1] {
		t.Fatalf("second plan reused the first plan's args: %v", first.Args)
	}
	if second.Args[1] != `C:\Users\you\b.pdf` {
		t.Fatalf("second plan args = %v, want [%s]", second.Args, `C:\Users\you\b.pdf`)
	}
}

// TestRunnerSeam confirms tests can drive the Runner seam without launching a real process, and
// that ExecRunner satisfies the interface.
func TestRunnerSeam(t *testing.T) {
	var fr fakeRunner
	if err := fr.Run(Plan{Cmd: "explorer.exe", Args: []string{"/select,", `C:\a.pdf`}}); err != nil {
		t.Fatalf("fake runner returned %v", err)
	}
	if len(fr.calls) != 1 || fr.calls[0].Cmd != "explorer.exe" {
		t.Fatalf("fake runner captured %+v", fr.calls)
	}

	var _ Runner = ExecRunner{}
}

// TestDefaultDepsWiresRealSeams asserts the default Deps exposes working env/exists/converter seams
// and that PlatformFromGOOS translates Go's "windows" spelling to Node's "win32".
func TestDefaultDepsWiresRealSeams(t *testing.T) {
	deps := DefaultDeps()
	if deps.PathConv == nil {
		t.Fatal("deps.PathConv is nil, want a real converter")
	}
	if deps.Getenv == nil {
		t.Fatal("deps.Getenv is nil, want os.Getenv")
	}
	if deps.Exists == nil {
		t.Fatal("deps.Exists is nil, want the os.Stat probe")
	}
	if got := deps.PathConv.WSLToWindowsPath("/mnt/c/a"); got != `C:\a` {
		t.Fatalf("PathConv.WSLToWindowsPath = %q, want %q", got, `C:\a`)
	}
	if got := PlatformFromGOOS(runtime.GOOS); got == "" {
		t.Fatal("PlatformFromGOOS(runtime.GOOS) = empty, want a Node platform spelling")
	}
}

// TestPlatformFromGOOS pins the GOOS -> Node platform mapping, Windows being the one different name.
func TestPlatformFromGOOS(t *testing.T) {
	cases := map[string]string{
		"windows": "win32",
		"darwin":  "darwin",
		"linux":   "linux",
	}
	for goos, want := range cases {
		if got := PlatformFromGOOS(goos); got != want {
			t.Fatalf("PlatformFromGOOS(%q) = %q, want %q", goos, got, want)
		}
	}
}

// --- helpers ---------------------------------------------------------------------------------

// TestWindowsProgramNeverGetsPosixPath is the Golden Rule 21 regression the whole package exists
// for: every path handed to explorer.exe/chrome.exe must be Windows-form, never a POSIX `/mnt/...`.
// The converter is a spy that delegates to the real pathconv-backed conversion and records its
// calls, so a builder that skipped PathConv.WSLToWindowsPath would both leave the POSIX arg in the
// plan and never register a conversion.
func TestWindowsProgramNeverGetsPosixPath(t *testing.T) {
	mount := "/mnt/c/Users/you/__archive/a.pdf"
	spy := &spyConv{inner: RealPathConv()}
	deps := Deps{
		Getenv:   func(string) string { return "" },
		Exists:   func(string) bool { return true },
		PathConv: spy,
	}

	plans := []Plan{
		RevealInFileManager(mount, PlatformLinux, deps),
		OpenDirectory("/mnt/c/Users/you/__raws", PlatformLinux, deps),
	}
	open := OpenInChrome(mount, PlatformLinux, deps)
	if open == nil {
		t.Fatal("OpenInChrome returned nil, want a plan")
	}
	plans = append(plans, *open)

	for _, plan := range plans {
		// The resolved Chrome command can itself be a Windows path (C:\...\chrome.exe) or the bare
		// fallback; what matters for Golden Rule 21 is that no argument handed to it is POSIX /mnt.
		if !strings.HasSuffix(plan.Cmd, ".exe") && plan.Cmd != "chrome" {
			t.Fatalf("expected a Windows launcher, got %q", plan.Cmd)
		}
		for _, arg := range plan.Args {
			if strings.HasPrefix(arg, "/mnt/") {
				t.Fatalf("POSIX mount path %q handed to Windows launcher %q (Golden Rule 21)", arg, plan.Cmd)
			}
		}
	}
	if len(spy.converted) != 3 {
		t.Fatalf("WSLToWindowsPath called %d times, want 3 (reveal, open-directory, chrome)", len(spy.converted))
	}
}

// spyConv delegates to the real converter and records every WSLToWindowsPath input.
type spyConv struct {
	inner     PathConv
	converted []string
}

func (s *spyConv) IsWSLMountPath(path string) bool { return s.inner.IsWSLMountPath(path) }

func (s *spyConv) WSLToWindowsPath(path string) string {
	s.converted = append(s.converted, path)
	return s.inner.WSLToWindowsPath(path)
}

func (s *spyConv) DirName(path string) string { return s.inner.DirName(path) }

// newDeps builds the injected environment from fakes: the given existence set and env map, plus the
// real pathconv-backed converter (a pure function, no I/O).
func newDeps(existing map[string]bool, env map[string]string) Deps {
	return Deps{
		Getenv: func(key string) string {
			return env[key]
		},
		Exists: func(path string) bool {
			return existing[path]
		},
		PathConv: RealPathConv(),
	}
}

// fakeRunner captures Plans instead of executing them, so no test launches a process.
type fakeRunner struct {
	calls []Plan
}

func (r *fakeRunner) Run(plan Plan) error {
	r.calls = append(r.calls, plan)
	return nil
}

// --- assertions ------------------------------------------------------------------------------

func assertPlanEqual(t *testing.T, got, want Plan) {
	t.Helper()
	if got.Cmd != want.Cmd {
		t.Fatalf("cmd = %q, want %q", got.Cmd, want.Cmd)
	}
	assertArgsEqual(t, got.Args, want.Args)
}

func assertArgsEqual(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("args[%d] = %q, want %q (full: %#v)", i, got[i], want[i], got)
		}
	}
}

// nodePathJoinTest mirrors Node's path.join on the host platform: it joins segments with the host
// separator but leaves an embedded backslash (from the Windows-style first segment the TS tests
// pass) as a literal character. That is what makes the Program Files candidate
// `C:\Program Files/Google/Chrome/Application/chrome.exe` on a Linux test host. It duplicates the
// package's unexported nodeJoinPath because this package's tests are in the same package and could
// call it directly; keeping a separate copy here makes the TS parity intent explicit and avoids the
// test passing merely because it shares the implementation's bug.
func nodePathJoinTest(segments ...string) string {
	sep := "/"
	if runtime.GOOS == "windows" {
		sep = `\`
	}
	var kept []string
	for _, s := range segments {
		if s != "" {
			kept = append(kept, s)
		}
	}
	joined := strings.Join(kept, sep)
	for strings.Contains(joined, sep+sep) {
		joined = strings.ReplaceAll(joined, sep+sep, sep)
	}
	return joined
}
