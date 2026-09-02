//go:build !windows

package main

import (
	"fmt"
	"os"

	"overwatch/agent/internal/setupui"
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

// serviceControl is what the setup page can drive. There is no service manager
// to speak to here, so it reports itself unsupported and the page confines
// itself to writing the configuration.
func serviceControl(options) setupui.Service { return nil }

// prepareStorage makes sure the data directory exists. On Windows the
// equivalent also locks its permissions down; here the deployment (a container
// with a mounted volume, or systemd) owns that.
func prepareStorage(dataDir string) error {
	if dataDir == "" {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", dataDir, err)
	}
	return nil
}
