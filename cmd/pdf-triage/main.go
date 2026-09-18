package main

import (
	"os"
)

// main is the only package-level entry point. It dispatches on the first argument exactly like
// src/index.ts:9-22 (`scan`, `mcp`, else web) with the added `vision-lab` command that used to be
// a separate entrypoint (src/vision-lab-main.ts).
func main() {
	os.Exit(run(os.Args[1:]))
}

// run dispatches the subcommand and returns the process exit code. Anything other than `scan`,
// `mcp` or `vision-lab` serves the web dashboard, exactly as src/index.ts:11-21 falls through to
// startWebServer for every other first argument.
func run(args []string) int {
	command := ""
	if len(args) > 0 {
		command = args[0]
	}
	switch command {
	case "scan":
		return runScanCommand()
	case "mcp":
		return runMCPCommand()
	case "vision-lab":
		return runVisionLabCommand()
	default:
		return runServeCommand()
	}
}
