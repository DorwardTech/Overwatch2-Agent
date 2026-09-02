//go:build windows

package platform

import (
	"os"
	"path/filepath"
)

// defaultDataDir is %ProgramData%\Overwatch Agent: machine-wide, present on
// every Windows installation, and the conventional home for a service's state.
// Windows always defines ProgramData; the literal only guards a stripped-down
// environment.
func defaultDataDir() string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		return filepath.Join(pd, "Overwatch Agent")
	}
	return `C:\ProgramData\Overwatch Agent`
}
