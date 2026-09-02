//go:build !windows

package platform

import "testing"

// Without a data directory the agent must behave exactly as it always has:
// the container image sets every path it needs, and a default that suddenly
// pointed somewhere else would move a fleet's caches.
func TestNoDataDirKeepsHistoricalDefaults(t *testing.T) {
	t.Setenv(DataDirEnv, "")

	if got := DataDir(); got != "" {
		t.Fatalf("DataDir() = %q, want empty", got)
	}
	if got := DefaultCacheDir(); got != "./cache" {
		t.Errorf("DefaultCacheDir() = %q, want ./cache", got)
	}
	for name, got := range map[string]string{
		"buffer":  DefaultBufferFile(),
		"pack-ir": DefaultPackIRBufferFile(),
		"log":     DefaultLogFile(),
		"env":     DefaultEnvFile(),
	} {
		if got != "" {
			t.Errorf("%s default = %q, want empty (disabled)", name, got)
		}
	}
}
