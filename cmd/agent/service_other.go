//go:build !windows

package main

import (
	"fmt"
	"os"
)

// The service commands exist only on Windows. Everywhere else the container
// runtime or systemd supervises the agent, and the commands say so plainly
// rather than pretend.

func isWindowsService() bool { return false }

func runAsService(options) int { return exitFailure }

func serviceCommand(cmd string, _ options) int {
	fmt.Fprintf(os.Stderr, "overwatch-agent: %q manages the Windows service and is only available on Windows\n", cmd)
	return exitUsage
}

func reportServiceError(string) {}
