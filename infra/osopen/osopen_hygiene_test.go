package osopen

import (
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Regression guard for the WSL path discipline (see pathconv and infra/osopen): the ONLY Go source
// file allowed to know about OS-specific launcher executables is infra/osopen/osopen.go. Any other
// non-test file naming one of ForbiddenLauncherLiterals is exactly how "Open Incoming" ended up
// passing a POSIX /mnt path to explorer.exe, which cannot resolve it and silently fell back to
// C:\Users\<user>\Documents.
//
// This is a case-for-case port of pdf-triage's src/infrastructure/os-open.hygiene.test.ts, adapted
// from `src/**/*.ts` to this Go module's non-test `*.go` files:
//
//   - The owner file src/infrastructure/os-open.ts becomes this package's directory infra/osopen,
//     which is skipped in full (source plus its own tests, mirroring `rel === OWNER_FILE`).
//   - `*.test.ts` is exempt because tests must assert on the command names; `*_test.go` is exempt
//     for the same reason. This includes os-open.hygiene.test.ts's own Go port.
//   - The forbidden atom set lives in osopen.go as ForbiddenLauncherLiterals, so this test file
//     contains no launcher literal itself (otherwise the guard would have to exempt itself).
//   - The TS SKIP_DIRS {node_modules, dist, vendor} and dot-directories stay; for Go, "testdata"
//     also carries no package source, so it is skipped rather than walked.

// skipDirs mirrors the TS SKIP_DIRS plus "testdata" (Go convention: no package source).
var skipDirs = map[string]bool{
	"node_modules": true,
	"dist":         true,
	"vendor":       true,
	"testdata":     true,
}

func TestOSLauncherHygiene(t *testing.T) {
	moduleRoot := findModuleRoot(t)

	collect := func() []string {
		files := collectGoSourceFiles(t, moduleRoot, filepath.Join(moduleRoot, "infra", "osopen"))
		if len(files) <= 50 {
			t.Fatalf("found only %d non-test Go source files under %s; the hygiene test would not "+
				"meaningfully cover the module (want > 50)", len(files), moduleRoot)
		}
		return files
	}

	t.Run("finds non-test Go source files to check", func(t *testing.T) {
		_ = collect()
	})

	t.Run("all OS launcher executables live only in infra/osopen", func(t *testing.T) {
		var offenders []string
		for _, file := range collect() {
			content, err := os.ReadFile(file)
			if err != nil {
				continue
			}
			if line, literal, found := findForbiddenLiteral(string(content), ForbiddenLauncherLiterals); found {
				rel, relErr := filepath.Rel(moduleRoot, file)
				if relErr != nil {
					rel = file
				}
				offenders = append(offenders, rel+":"+strconv.Itoa(line)+": "+literal)
			}
		}
		if len(offenders) > 0 {
			t.Fatalf("launcher-executable literals found outside infra/osopen (Golden Rule 21): %v", offenders)
		}
	})
}

// findModuleRoot walks up from the test's working directory (the package directory) until it finds
// the go.mod that declares this module. It fails the test if none is found.
func findModuleRoot(t *testing.T) string {
	t.Helper()

	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found at or above %s", dir)
		}
		dir = parent
	}
}

// collectGoSourceFiles walks the module tree and returns every non-test `*.go` file, skipping the
// owner package directory and the directories the TS hygiene test skipped. Walk errors are skipped,
// matching the TS `catch { return; }`.
func collectGoSourceFiles(t *testing.T, moduleRoot, ownerDir string) []string {
	t.Helper()

	ownerDir = filepath.Clean(ownerDir)
	var files []string
	walkErr := filepath.WalkDir(moduleRoot, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			// TS: `if (entry.isSymbolicLink()) continue;`
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path == moduleRoot {
				return nil
			}
			name := entry.Name()
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			if filepath.Clean(path) == ownerDir {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(entry.Name(), ".go") && !strings.HasSuffix(entry.Name(), "_test.go") {
			files = append(files, path)
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk %s: %v", moduleRoot, walkErr)
	}
	return files
}

// findForbiddenLiteral returns the 1-based line and the offending literal of the first match, so
// the failure message names the offending file:line like the TS test's offender list.
func findForbiddenLiteral(content string, literals []string) (line int, literal string, found bool) {
	for _, candidate := range literals {
		index := strings.Index(content, candidate)
		if index < 0 {
			continue
		}
		return strings.Count(content[:index], "\n") + 1, candidate, true
	}
	return 0, "", false
}
