package platform

import (
	"path/filepath"
	"testing"
)

func TestDefaultsFollowDataDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(DataDirEnv, dir)

	if got := DataDir(); got != dir {
		t.Fatalf("DataDir() = %q, want %q", got, dir)
	}
	cases := map[string]struct{ got, want string }{
		"cache":   {DefaultCacheDir(), filepath.Join(dir, "cache")},
		"buffer":  {DefaultBufferFile(), filepath.Join(dir, "buffer.json")},
		"pack-ir": {DefaultPackIRBufferFile(), filepath.Join(dir, "packir-buffer.json")},
		"log":     {DefaultLogFile(), filepath.Join(dir, "logs", "agent.log")},
		"env":     {DefaultEnvFile(), filepath.Join(dir, "agent.env")},
	}
	for name, c := range cases {
		if c.got != c.want {
			t.Errorf("%s default = %q, want %q", name, c.got, c.want)
		}
	}
}
