//go:build windows

package platform

import "testing"

func TestWindowsDefaultLivesUnderProgramData(t *testing.T) {
	t.Setenv(DataDirEnv, "")
	t.Setenv("ProgramData", `D:\PD`)

	if got, want := DataDir(), `D:\PD\Overwatch Agent`; got != want {
		t.Fatalf("DataDir() = %q, want %q", got, want)
	}
	if got, want := DefaultLogFile(), `D:\PD\Overwatch Agent\logs\agent.log`; got != want {
		t.Errorf("DefaultLogFile() = %q, want %q", got, want)
	}
}
