package platform

import (
	"fmt"
	"os"
	"path/filepath"
)

// stateDir is where the container image keeps the agent's state. The image
// creates it owned by the user the agent runs as, and the compose files mount
// the persistent volume over it. A variable so tests can point it elsewhere.
var stateDir = "/data/cache"

// StateDir returns the container state directory when the agent can write to
// it, and "" otherwise.
//
// The image's own paths used to come only from the compose files. A
// deployment that ran the image any other way (a Coolify "Docker image"
// application, a bare docker run) got the historical defaults instead: the
// cache in ./cache inside the container and both spill files switched off. It
// then re-downloaded its whole cache from central and dropped any buffered
// telemetry on every redeploy, even with a volume mounted at /data/cache.
//
// Unwritable is treated like absent, so the historical defaults still apply.
// A volume mounted from a host directory is owned by root unless someone
// chowns it, and choosing it anyway would switch the cache off rather than
// merely leave it unpersisted. StateDirProblem says what to fix.
func StateDir() string {
	if stateDirProblem() != "" {
		return ""
	}
	return stateDir
}

// StateDirProblem explains why a state directory that exists is not being
// used, for the agent to log at startup. Empty when it is used, or when there
// is none (not running from the image).
func StateDirProblem() string {
	p := stateDirProblem()
	if p == "absent" {
		return ""
	}
	return p
}

func stateDirProblem() string {
	info, err := os.Stat(stateDir)
	if err != nil || !info.IsDir() {
		return "absent"
	}
	probe, err := os.CreateTemp(stateDir, ".write-probe-*")
	if err != nil {
		return fmt.Sprintf("%s is not writable by uid %d: chown the volume's host directory to %d:%d. Until then the "+
			"default cache and spill paths fall back to ./cache and no spill file, and a CACHE_DIR, BUFFER_FILE or "+
			"PACK_IR_BUFFER_FILE set to a path in it fails (%v)",
			stateDir, os.Getuid(), os.Getuid(), os.Getgid(), err)
	}
	name := probe.Name()
	_ = probe.Close()
	_ = os.Remove(filepath.Clean(name))
	return ""
}
