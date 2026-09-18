//go:build !windows

package ollama

import "os/exec"

// configureOllamaServe is a no-op off Windows; Node's windowsHide option has no equivalent there.
func configureOllamaServe(cmd *exec.Cmd) {}
