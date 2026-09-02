//go:build windows

package main

import "os/exec"

// openBrowser hands the address to whatever the machine uses for links. It
// goes through url.dll rather than `cmd /c start`, which would put the address
// through the command interpreter's own quoting rules.
func openBrowser(url string) error {
	return exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", url).Start()
}
