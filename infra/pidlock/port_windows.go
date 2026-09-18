//go:build windows

package pidlock

import (
	"os/exec"
	"syscall"
)

// KillProcessOnPort finds and force-kills whatever process is listening on port, so a caller that
// just hit EADDRINUSE can retry binding instead of requiring a manual PID hunt. Always kills
// whatever holds the port, with no check that it is a previous instance of this same app - by
// design, see the port-takeover design doc. Returns true if a process was found and killed, false
// if nothing was listening. Windows-only: the netstat/taskkill takeover is the platform mechanism.
func KillProcessOnPort(port int) bool {
	return KillProcessOnPortWith(port, windowsCommandRunner)
}

// windowsCommandRunner is the real CommandRunner: exec with windowsHide, matching the TS
// `{ windowsHide: true }` option.
func windowsCommandRunner(name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	return cmd.Output()
}
