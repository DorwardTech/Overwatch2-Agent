//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Installing rewrites the data directory's permissions, so it must refuse a
// directory that is not the agent's to lock down.
func TestCheckDataDir(t *testing.T) {
	t.Run("a fresh folder is fine", func(t *testing.T) {
		if err := checkDataDir(filepath.Join(t.TempDir(), "Overwatch Agent")); err != nil {
			t.Errorf("checkDataDir: %v", err)
		}
	})

	t.Run("a folder holding only the agent's own files is fine", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "agent.env"), []byte("x=1"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := checkDataDir(dir); err != nil {
			t.Errorf("checkDataDir: %v", err)
		}
	})

	t.Run("somebody else's folder is refused", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "payroll.xlsx"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := checkDataDir(dir); err == nil {
			t.Error("a directory with unrelated files in it was accepted")
		}
	})

	t.Run("a drive root is refused", func(t *testing.T) {
		if err := checkDataDir(`C:\`); err == nil {
			t.Error("the drive root was accepted")
		}
	})

	t.Run("ProgramData itself is refused", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("ProgramData", dir)
		if err := checkDataDir(dir); err == nil {
			t.Error("%ProgramData% itself was accepted")
		}
	})

	t.Run("an empty path is refused", func(t *testing.T) {
		if err := checkDataDir(""); err == nil {
			t.Error("an empty data directory was accepted")
		}
	})
}

func TestUnderDir(t *testing.T) {
	cases := []struct {
		dir, path string
		want      bool
	}{
		{`C:\ProgramData\Overwatch Agent`, `C:\ProgramData\Overwatch Agent\agent.env`, true},
		{`C:\ProgramData\Overwatch Agent`, `C:\ProgramData\Overwatch Agent\logs\agent.log`, true},
		{`C:\ProgramData\Overwatch Agent`, `C:\ProgramData\Overwatch Agent`, true},
		{`C:\ProgramData\Overwatch Agent`, `C:\Temp\agent.env`, false},
		{`C:\ProgramData\Overwatch Agent`, `C:\ProgramData\Overwatch Agent Backup\agent.env`, false},
	}
	for _, c := range cases {
		if got := underDir(c.dir, c.path); got != c.want {
			t.Errorf("underDir(%q, %q) = %v, want %v", c.dir, c.path, got, c.want)
		}
	}
}
