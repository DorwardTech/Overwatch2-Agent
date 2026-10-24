//go:build !windows

package platform

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func withStateDir(t *testing.T, dir string) {
	t.Helper()
	old := stateDir
	stateDir = dir
	t.Cleanup(func() { stateDir = old })
}

// The image's state directory, when the agent can write to it, is where
// everything goes: the cache, and both spill files under the names the compose
// files use, so a volume they mounted keeps working.
func TestWritableStateDirHoldsTheCacheAndSpillFiles(t *testing.T) {
	t.Setenv(DataDirEnv, "")
	dir := t.TempDir()
	withStateDir(t, dir)

	cases := map[string]struct{ got, want string }{
		"cache":   {DefaultCacheDir(), dir},
		"buffer":  {DefaultBufferFile(), filepath.Join(dir, "buffer.json")},
		"pack-ir": {DefaultPackIRBufferFile(), filepath.Join(dir, "pack-ir-buffer.json")},
		"log":     {DefaultLogFile(), ""},
		"env":     {DefaultEnvFile(), ""},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("%s default = %q, want %q", name, c.got, c.want)
		}
	}
	if p := StateDirProblem(); p != "" {
		t.Errorf("StateDirProblem() = %q, want empty", p)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Errorf("write probe left %d file(s) behind", len(left))
	}
}

// A root-owned host directory mounted over it must not switch the cache off:
// the historical defaults apply, and the agent is told what to chown.
func TestUnwritableStateDirKeepsHistoricalDefaultsAndSaysWhy(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root can write to any directory")
	}
	t.Setenv(DataDirEnv, "")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	withStateDir(t, dir)

	if got := DefaultCacheDir(); got != "./cache" {
		t.Errorf("DefaultCacheDir() = %q, want ./cache", got)
	}
	if got := DefaultBufferFile(); got != "" {
		t.Errorf("DefaultBufferFile() = %q, want empty", got)
	}
	if p := StateDirProblem(); !strings.Contains(p, dir) || !strings.Contains(p, "chown") {
		t.Errorf("StateDirProblem() = %q, want it to name %s and say chown", p, dir)
	}
}

// An explicit data directory still wins, and no state directory is no problem.
func TestDataDirWinsOverStateDir(t *testing.T) {
	data := t.TempDir()
	t.Setenv(DataDirEnv, data)
	withStateDir(t, t.TempDir())

	if got := DefaultCacheDir(); got != filepath.Join(data, "cache") {
		t.Errorf("DefaultCacheDir() = %q, want %q", got, filepath.Join(data, "cache"))
	}

	withStateDir(t, filepath.Join(t.TempDir(), "absent"))
	if p := StateDirProblem(); p != "" {
		t.Errorf("StateDirProblem() with no state directory = %q, want empty", p)
	}
}
