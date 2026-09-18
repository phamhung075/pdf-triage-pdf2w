//go:build windows

package ollama

import (
	"os/exec"
	"syscall"
)

// configureOllamaServe mirrors Node's `exec('ollama serve', { windowsHide: true })`: create the
// process without a console window.
func configureOllamaServe(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x08000000, // CREATE_NO_WINDOW
	}
}
