package main

import (
	"os/exec"
	"runtime"
	"time"

	"github.com/phamhung075/pdf-triage-pdf2w/httpapi"
	"github.com/phamhung075/pdf-triage-pdf2w/infra/osopen"
	"github.com/phamhung075/pdf-triage-pdf2w/mcpserver"
)

// This file holds the small adapters between packages whose interfaces deliberately do not line
// up, plus the real process-launch seams. They live in the composition root (not in the ported
// packages) per the migration design: a package that does not need to know about another layer's
// shape is not edited to satisfy it.

// osLauncher adapts infra/osopen's pure Plan builders to the httpapi.Opener and
// httpapi.DocumentOpener seams (Launch is httpapi's {Cmd,Args} shape). It never launches anything;
// the Spawner does that.
type osLauncher struct {
	platform string
	deps     osopen.Deps
}

// newOSLauncher returns the production launcher for the host platform.
func newOSLauncher() osLauncher {
	return osLauncher{
		platform: osopen.PlatformFromGOOS(runtime.GOOS),
		deps:     osopen.DefaultDeps(),
	}
}

// OpenDirectory builds the OS file-manager plan for a directory.
func (o osLauncher) OpenDirectory(path string) (httpapi.Launch, bool) {
	plan := osopen.OpenDirectory(path, o.platform, o.deps)
	return httpapi.Launch{Cmd: plan.Cmd, Args: plan.Args}, plan.Cmd != ""
}

// RevealInFileManager builds the OS file-manager reveal plan for a file.
func (o osLauncher) RevealInFileManager(path string) (httpapi.Launch, bool) {
	plan := osopen.RevealInFileManager(path, o.platform, o.deps)
	return httpapi.Launch{Cmd: plan.Cmd, Args: plan.Args}, plan.Cmd != ""
}

// OpenInChrome builds the Chrome plan for a local PDF, or reports not-found when Chrome is absent.
func (o osLauncher) OpenInChrome(path string) (httpapi.Launch, bool) {
	plan := osopen.OpenInChrome(path, o.platform, o.deps)
	if plan == nil {
		return httpapi.Launch{}, false
	}
	return httpapi.Launch{Cmd: plan.Cmd, Args: plan.Args}, true
}

// detachedSpawner is the production httpapi.Spawner: it starts the resolved program detached and
// releases it, matching the TS `spawn(cmd, args, { detached: true, stdio: 'ignore' }).unref()`.
// Tests inject a recording no-op so no Explorer/Chrome process is ever started.
type detachedSpawner struct{}

// Spawn starts cmd and releases the process so this server never reaps it.
func (detachedSpawner) Spawn(cmd string, args []string) {
	command := exec.Command(cmd, args...)
	if err := command.Start(); err != nil {
		return
	}
	_ = command.Process.Release()
}

// noopSpawner is the test/injected Spawner: it records nothing and launches nothing.
type noopSpawner struct{}

// Spawn does nothing.
func (noopSpawner) Spawn(string, []string) {}

// noopRunner is the test/injected mcpserver.Runner / osopen.Runner: it launches nothing.
type noopRunner struct{}

// Run does nothing.
func (noopRunner) Run(osopen.Plan) error { return nil }

// startOllamaServe is the POST /api/ollama/start action: `ollama serve` fire-and-forget, exactly
// like the TS exec() whose callback only logs a warning. Tests inject a no-op so no real Ollama is
// started.
func startOllamaServe() error {
	command := exec.Command("ollama", "serve")
	if err := command.Start(); err != nil {
		return err
	}
	return command.Process.Release()
}

// mcpToolLister adapts mcpserver's SDK tool list to httpapi's McpToolLister seam for
// GET /api/mcp/status.
type mcpToolLister struct{}

// ListTools projects the SDK tool definitions onto httpapi's name/description pairs.
func (mcpToolLister) ListTools() ([]httpapi.McpToolInfo, error) {
	definitions := mcpserver.ToolDefinitions()
	tools := make([]httpapi.McpToolInfo, 0, len(definitions))
	for _, definition := range definitions {
		if definition == nil {
			continue
		}
		tools = append(tools, httpapi.McpToolInfo{
			Name:        definition.Name,
			Description: definition.Description,
		})
	}
	return tools, nil
}

// newRealMCPOpener returns the production mcpserver.Opener (over infra/osopen).
func newRealMCPOpener() mcpserver.Opener {
	return mcpserver.RealOpener{}
}

// timeNowOr returns now when set, else time.Now. It keeps every injected clock in one place.
func timeNowOr(now func() time.Time) func() time.Time {
	if now != nil {
		return now
	}
	return time.Now
}

// compile-time assertions that the adapters satisfy the seams they are wired into.
var (
	_ httpapi.Opener         = osLauncher{}
	_ httpapi.DocumentOpener = osLauncher{}
	_ httpapi.Spawner        = detachedSpawner{}
	_ httpapi.Spawner        = noopSpawner{}
	_ mcpserver.Runner       = noopRunner{}
	_ osopen.Runner          = noopRunner{}
	_ httpapi.McpToolLister  = mcpToolLister{}
	_ mcpserver.Opener       = mcpserver.RealOpener{}
	_ mcpserver.Runner       = osopen.ExecRunner{}
)
