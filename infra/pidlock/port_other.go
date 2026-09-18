//go:build !windows

package pidlock

// KillProcessOnPort is a no-op on non-Windows platforms: the netstat -ano -p tcp / taskkill port
// takeover is Windows-only (this app targets Windows, see AGENTS.md's dist:exe target). The parsing
// and orchestration remain testable on every OS through ParsePidOnPort and KillProcessOnPortWith.
func KillProcessOnPort(port int) bool {
	return false
}
