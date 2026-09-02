// Package platform resolves where the agent keeps its state when nobody has
// told it. In a container the answer is fixed by the image — a mounted /data
// volume, paths set in the compose file. On a plain machine, a Windows service
// in particular, the agent has to choose for itself, and every default that
// used to be "the current directory" or "disabled" becomes a path under one
// data directory.
package platform

import (
	"os"
	"path/filepath"
)

// DataDirEnv names the environment variable that overrides the data directory.
const DataDirEnv = "AGENT_DATA_DIR"

// DataDir returns the agent's data directory: AGENT_DATA_DIR when set,
// otherwise the operating system's default (on Windows, "Overwatch Agent"
// under %ProgramData%). It is empty where there is no default — Linux and the
// container image — and every Default* function then returns the value the
// agent has always used, so nothing changes for an existing deployment.
func DataDir() string {
	if v := os.Getenv(DataDirEnv); v != "" {
		return v
	}
	return defaultDataDir()
}

// DefaultCacheDir is the print-server cache location: <data>/cache, or the
// historical ./cache when no data directory is known.
func DefaultCacheDir() string {
	if d := DataDir(); d != "" {
		return filepath.Join(d, "cache")
	}
	return "./cache"
}

// DefaultBufferFile is where unsent telemetry is spilled across restarts.
// Empty (disabled) when no data directory is known, as before.
func DefaultBufferFile() string { return inDataDir("buffer.json") }

// DefaultPackIRBufferFile is the spill file for unsent pack-IR readings.
func DefaultPackIRBufferFile() string { return inDataDir("packir-buffer.json") }

// DefaultLogFile is the agent's log file. Empty (log to the console) when no
// data directory is known.
func DefaultLogFile() string { return inDataDir("logs", "agent.log") }

// DefaultEnvFile is the configuration file the agent reads at startup when no
// other is named. Empty when no data directory is known.
func DefaultEnvFile() string { return inDataDir("agent.env") }

func inDataDir(elem ...string) string {
	d := DataDir()
	if d == "" {
		return ""
	}
	return filepath.Join(append([]string{d}, elem...)...)
}
