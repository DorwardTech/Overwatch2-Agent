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

// DefaultCacheDir is the print-server cache location: <data>/cache; else the
// container state directory when it is usable (StateDir); else the historical
// ./cache.
func DefaultCacheDir() string {
	if d := DataDir(); d != "" {
		return filepath.Join(d, "cache")
	}
	if d := StateDir(); d != "" {
		return d
	}
	return "./cache"
}

// DefaultBufferFile is where unsent telemetry is spilled across restarts:
// under the data directory, else in the container state directory. Empty
// (disabled) when neither is available, as before.
func DefaultBufferFile() string {
	if f := inDataDir("buffer.json"); f != "" {
		return f
	}
	return inStateDir("buffer.json")
}

// DefaultPackIRBufferFile is the spill file for unsent pack-IR readings. In
// the state directory it takes the name the compose files have always given
// it, so a volume they set up keeps its file.
func DefaultPackIRBufferFile() string {
	if f := inDataDir("packir-buffer.json"); f != "" {
		return f
	}
	return inStateDir("pack-ir-buffer.json")
}

// DefaultLogFile is the agent's log file. Empty (log to the console) when no
// data directory is known.
func DefaultLogFile() string { return inDataDir("logs", "agent.log") }

// DefaultEnvFile is the configuration file the agent reads at startup when no
// other is named. Empty when no data directory is known.
func DefaultEnvFile() string { return inDataDir("agent.env") }

func inStateDir(name string) string {
	d := StateDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, name)
}

func inDataDir(elem ...string) string {
	d := DataDir()
	if d == "" {
		return ""
	}
	return filepath.Join(append([]string{d}, elem...)...)
}
