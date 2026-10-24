package main

import (
	"fmt"
	"strconv"
	"strings"

	"overwatch/agent/internal/ozonelib"
)

// The Windows installer opens the game server's LAN announcement port to the
// agent's executable, so that discovery (OZONE_WS_HOST=auto), the report-only
// listener and `overwatch-agent discover` hear it without Windows Firewall
// dropping it or asking anybody. The netsh arguments are built here, in an
// untagged file, so they are tested on every platform; only running netsh is
// Windows-only (service_windows.go).
//
// The rule is scoped to the program, so it covers the service (LocalService)
// and an elevated setup page or discover run from the same executable alike.
// It admits senders on a directly connected subnet only, which is where a
// broadcast comes from anyway, under every network profile: an isolated game
// LAN is often classed as Public ("Unidentified network").
//
// Its port is fixed at the default. A custom OZONE_LIB_PORT is not followed:
// the rule is made once, at install time, while the port can be changed in
// the configuration at any time afterwards, so an operator who moves it adds
// a rule for the new port by hand.
const (
	firewallRuleName = "Overwatch Agent - O-Zone discovery (UDP 12106)"
	firewallPort     = ozonelib.DefaultPort
)

// firewallDeleteArgs removes the rule. netsh deletes every rule of the name,
// so delete-then-add leaves exactly one however often the agent is
// reinstalled (an upgrade uninstalls first, then installs).
func firewallDeleteArgs() []string {
	return []string{"advfirewall", "firewall", "delete", "rule", "name=" + firewallRuleName}
}

// firewallShowArgs lists the rule. netsh exits non-zero when no rule has the
// name, which tells uninstall whether there is anything to remove without
// reading netsh's messages, which are in the system's language.
func firewallShowArgs() []string {
	return []string{"advfirewall", "firewall", "show", "rule", "name=" + firewallRuleName}
}

// firewallAddArgs adds the rule for program, the executable the service runs.
func firewallAddArgs(program string) []string {
	return []string{
		"advfirewall", "firewall", "add", "rule",
		"name=" + firewallRuleName,
		"dir=in",
		"action=allow",
		"program=" + program,
		"protocol=UDP",
		"localport=" + strconv.Itoa(firewallPort),
		"remoteip=localsubnet",
		"profile=any",
		"enable=yes",
	}
}

// firewallManualCommand is the add command for an operator to paste into an
// elevated prompt when the installer could not run it.
func firewallManualCommand(program string) string {
	return netshCommandLine("netsh", firewallAddArgs(program))
}

// firewallWarning explains a rule the installer could not add. It is a
// warning only: the service is installed, and works with a fixed host.
func firewallWarning(err error, program string) string {
	return fmt.Sprintf("the Windows Firewall rule that lets this computer hear the game server's\n"+
		"announcement on UDP %d could not be added:\n%v\n"+
		"Without it OZONE_WS_HOST=auto may never find the game server, and `overwatch-agent\n"+
		"discover` may hear nothing; a fixed OZONE_WS_HOST still works. Add it from an\n"+
		"elevated prompt:\n  %s",
		firewallPort, err, firewallManualCommand(program))
}

// netshCommandLine writes out a netsh command line in the form netsh's own
// documentation uses, tag="value with spaces", and the installer hands it to
// Windows verbatim (SysProcAttr.CmdLine).
//
// Left to itself, Go would quote each argument whole ("name=Overwatch Agent
// - O-Zone ..."), which suits a program that splits its command line the C
// runtime's way. netsh interprets its arguments itself (its documentation
// even writes "name = value", with spaces round the equals sign) and never
// documents a tag=value quoted whole, so the safe form is the one it does
// document and administrators type, with only the value quoted. The quoting
// within a value follows the C runtime's rules too, so a program that does
// split the standard way reads exactly the same arguments; the tests check
// that round trip. An executable path cannot contain a quote and never ends
// in a backslash, so in practice a value is only ever wrapped in quotes when
// it has a space, as in program="C:\Program Files\...\x.exe".
func netshCommandLine(netsh string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, quoteArg(netsh))
	for _, a := range args {
		if tag, value, ok := strings.Cut(a, "="); ok {
			parts = append(parts, tag+"="+quoteArg(value))
		} else {
			parts = append(parts, quoteArg(a))
		}
	}
	return strings.Join(parts, " ")
}

// quoteArg quotes s for a Windows command line, if it needs it, by the C
// runtime's rules (those of syscall.EscapeArg, which exists only on Windows):
// a quote is escaped with a backslash, and backslashes are doubled only where
// they come before a quote, including the closing one.
func quoteArg(s string) string {
	if s == "" {
		return `""`
	}
	if !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteByte(s[i])
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}
