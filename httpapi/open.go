// POST /api/open-location and POST /api/open-chrome, ported from web-server.ts:116-181.
//
// Both routes validate a `{ targetPath: z.string().min(1) }` body, normalize the path, stat it, and
// hand the launcher decision to os-open.ts (platform branching + WSL->Windows path conversion). The
// launcher itself is injected as the Opener interface, so httpapi neither imports infra/osopen (being
// ported concurrently) nor ever launches a real process in tests. The spawn is `detached` and
// fire-and-forget, matching every other GUI-helper spawn() in web-server.ts.
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (s *server) registerOpenLocation(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/open-location", s.openLocationHandler)
}

func (s *server) registerOpenChrome(mux *http.ServeMux) {
	mux.HandleFunc("POST /api/open-chrome", s.openChromeHandler)
}

// openLocationHandler ports web-server.ts:118-145.
func (s *server) openLocationHandler(w http.ResponseWriter, r *http.Request) {
	targetPath, zodErr := parseTargetPath(r)
	if zodErr != "" {
		writeError(w, 400, zodErr)
		return
	}

	normalized := filepath.Clean(targetPath)
	if info, err := os.Stat(normalized); err == nil {
		// Existence/stat checks run against the POSIX path; the Opener hands Windows programs the
		// Windows form (a POSIX /mnt path makes Windows Explorer fall back to the user's Documents).
		var launch Launch
		var ok bool
		if info.IsDir() {
			launch, ok = s.openDirectory(normalized)
		} else {
			launch, ok = s.revealInFileManager(normalized)
		}
		if ok {
			s.spawn(launch)
		}
		writeJSON(w, 200, map[string]any{"message": "Windows Explorer opened", "path": normalized})
		return
	}

	parentDir := filepath.Dir(normalized)
	if _, err := os.Stat(parentDir); err == nil {
		launch, ok := s.openDirectory(parentDir)
		if ok {
			s.spawn(launch)
		}
		writeJSON(w, 200, map[string]any{"message": "Opened parent directory", "path": parentDir})
		return
	}
	writeError(w, 404, "Path does not exist: "+normalized)
}

// openChromeHandler ports web-server.ts:148-181.
func (s *server) openChromeHandler(w http.ResponseWriter, r *http.Request) {
	targetPath, zodErr := parseTargetPath(r)
	if zodErr != "" {
		writeError(w, 400, zodErr)
		return
	}

	normalized := filepath.Clean(targetPath)
	if _, err := os.Stat(normalized); err != nil {
		writeError(w, 404, "File path does not exist: "+normalized)
		return
	}

	launch, ok := s.openInChrome(normalized)
	if !ok {
		// Chrome executable not found (os-open.ts's resolveChromeExecutable returned null).
		writeError(w, 500, "Chrome executable not found")
		return
	}
	// Launch Chrome directly with the file path as an argv entry — Chrome's own single-instance IPC
	// forwards this to the existing window as a new tab. Spawn never invokes a shell, so shell
	// metacharacters in `normalized` (attacker-controllable request-body input) cannot matter.
	s.spawn(launch)
	writeJSON(w, 200, map[string]any{"success": true, "message": "Opened document in Chrome tab", "path": normalized})
}

// spawn runs a resolved launch through the injected Spawner. A nil Spawner is a test/server that
// chose not to launch anything; the response is unaffected, matching TS's fire-and-forget spawn.
func (s *server) spawn(launch Launch) {
	if s.deps.Spawner != nil {
		s.deps.Spawner.Spawn(launch.Cmd, launch.Args)
	}
}

func (s *server) openDirectory(path string) (Launch, bool) {
	if s.deps.Opener == nil {
		return Launch{}, false
	}
	return s.deps.Opener.OpenDirectory(path)
}

func (s *server) revealInFileManager(path string) (Launch, bool) {
	if s.deps.Opener == nil {
		return Launch{}, false
	}
	return s.deps.Opener.RevealInFileManager(path)
}

func (s *server) openInChrome(path string) (Launch, bool) {
	if s.deps.Opener == nil {
		return Launch{}, false
	}
	return s.deps.Opener.OpenInChrome(path)
}

// processSpawner is the production Spawner: detached, stdio ignored, never waited on.
type processSpawner struct{}

// Spawn starts cmd and releases the process so the server does not reap it.
func (processSpawner) Spawn(cmd string, args []string) {
	c := exec.Command(cmd, args...)
	if err := c.Start(); err != nil {
		return
	}
	_ = c.Process.Release()
}

// parseTargetPath reproduces `z.object({ targetPath: z.string().min(1) }).parse(req.body)` and
// returns Zod's pretty-printed issue array on failure, so the error body matches TS byte-for-byte
// for the two cases the ported tests exercise (missing key, empty string).
func parseTargetPath(r *http.Request) (string, string) {
	raw := bodyBytes(r)
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "", zodRequired("targetPath")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err.Error()
	}
	value, present := m["targetPath"]
	if !present {
		return "", zodRequired("targetPath")
	}
	var s string
	if err := json.Unmarshal(value, &s); err != nil {
		return "", zodInvalidType("targetPath")
	}
	if len(s) < 1 {
		return "", zodTooSmall("targetPath")
	}
	return s, ""
}

func zodRequired(field string) string {
	return fmt.Sprintf("[\n  {\n    \"code\": \"invalid_type\",\n    \"expected\": \"string\",\n    \"received\": \"undefined\",\n    \"path\": [\n      %q\n    ],\n    \"message\": \"Required\"\n  }\n]", field)
}

func zodInvalidType(field string) string {
	return fmt.Sprintf("[\n  {\n    \"code\": \"invalid_type\",\n    \"expected\": \"string\",\n    \"received\": \"number\",\n    \"path\": [\n      %q\n    ],\n    \"message\": \"Expected string, received number\"\n  }\n]", field)
}

func zodTooSmall(field string) string {
	return fmt.Sprintf("[\n  {\n    \"code\": \"too_small\",\n    \"minimum\": 1,\n    \"type\": \"string\",\n    \"inclusive\": true,\n    \"exact\": false,\n    \"message\": \"String must contain at least 1 character(s)\",\n    \"path\": [\n      %q\n    ]\n  }\n]", field)
}
