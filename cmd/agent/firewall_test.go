package main

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const programFiles = `C:\Program Files\Overwatch Agent\overwatch-agent.exe`

func TestFirewallRule(t *testing.T) {
	if firewallRuleName != "Overwatch Agent - O-Zone discovery (UDP 12106)" {
		t.Errorf("rule name = %q", firewallRuleName)
	}
	if !strings.Contains(firewallRuleName, "UDP "+strconv.Itoa(firewallPort)+")") {
		t.Errorf("rule name %q does not name the port it opens (%d)", firewallRuleName, firewallPort)
	}

	name := "name=" + firewallRuleName
	if got, want := firewallDeleteArgs(), []string{"advfirewall", "firewall", "delete", "rule", name}; !slices.Equal(got, want) {
		t.Errorf("delete = %q, want %q", got, want)
	}
	if got, want := firewallShowArgs(), []string{"advfirewall", "firewall", "show", "rule", name}; !slices.Equal(got, want) {
		t.Errorf("show = %q, want %q", got, want)
	}
	want := []string{
		"advfirewall", "firewall", "add", "rule", name, "dir=in", "action=allow",
		"program=" + programFiles, "protocol=UDP", "localport=12106",
		"remoteip=localsubnet", "profile=any", "enable=yes",
	}
	if got := firewallAddArgs(programFiles); !slices.Equal(got, want) {
		t.Errorf("add = %q,\nwant %q", got, want)
	}
}

// The command line is the one netsh's documentation writes, value quoted and
// tag bare, both for running it and for the operator to paste.
func TestNetshCommandLine(t *testing.T) {
	want := `netsh advfirewall firewall add rule name="Overwatch Agent - O-Zone discovery (UDP 12106)" ` +
		`dir=in action=allow program="C:\Program Files\Overwatch Agent\overwatch-agent.exe" ` +
		`protocol=UDP localport=12106 remoteip=localsubnet profile=any enable=yes`
	if got := firewallManualCommand(programFiles); got != want {
		t.Errorf("manual command =\n  %s\nwant\n  %s", got, want)
	}

	got := netshCommandLine(`C:\Windows\System32\netsh.exe`, firewallDeleteArgs())
	if want := `C:\Windows\System32\netsh.exe advfirewall firewall delete rule name="Overwatch Agent - O-Zone discovery (UDP 12106)"`; got != want {
		t.Errorf("delete command =\n  %s\nwant\n  %s", got, want)
	}
	if got := netshCommandLine(`C:\Odd Root\System32\netsh.exe`, nil); got != `"C:\Odd Root\System32\netsh.exe"` {
		t.Errorf("an executable path with a space was not quoted: %s", got)
	}
}

// A program that splits its command line the standard way reads back exactly
// the arguments that were written, whatever the path holds.
func TestNetshCommandLineRoundTrip(t *testing.T) {
	for _, program := range []string{
		programFiles,
		`C:\Program Files (x86)\Overwatch & Co\agent.exe`,
		`C:\Agent\overwatch-agent.exe`,
		`C:\ends in a slash\`,
		`C:\two slashes\\`,
		`C:\a "quoted" name\x.exe`,
		`C:\slash before quote\"x.exe`,
		"C:\\tab\there\\x.exe",
		"",
	} {
		args := firewallAddArgs(program)
		line := netshCommandLine(`C:\Windows\System32\netsh.exe`, args)
		got := splitCommandLine(line)
		want := append([]string{`C:\Windows\System32\netsh.exe`}, args...)
		if !slices.Equal(got, want) {
			t.Errorf("program %q:\n  line %s\n  read %q\n  want %q", program, line, got, want)
		}
	}
}

func TestFirewallWarning(t *testing.T) {
	w := firewallWarning(errors.New("netsh.exe: exit status 1\nThe requested operation requires elevation."), programFiles)
	for _, s := range []string{"requires elevation", "UDP 12106", "elevated prompt", firewallManualCommand(programFiles)} {
		if !strings.Contains(w, s) {
			t.Errorf("warning does not mention %q:\n%s", s, w)
		}
	}
}

// splitCommandLine splits a Windows command line by the C runtime's rules
// (those of CommandLineToArgvW for every argument after the first): space
// and tab separate arguments outside quotes; a quote toggles quoting; 2n
// backslashes before a quote are n backslashes and the quote toggles, 2n+1
// are n backslashes and a literal quote; other backslashes are literal.
func splitCommandLine(s string) []string {
	var out []string
	var cur strings.Builder
	inQuote, inArg := false, false
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == '\\':
			n := 0
			for i < len(s) && s[i] == '\\' {
				n++
				i++
			}
			if i < len(s) && s[i] == '"' {
				cur.WriteString(strings.Repeat(`\`, n/2))
				if n%2 == 1 {
					cur.WriteByte('"')
					i++
				}
			} else {
				cur.WriteString(strings.Repeat(`\`, n))
			}
			inArg = true
			continue
		case c == '"':
			inQuote, inArg = !inQuote, true
		case (c == ' ' || c == '\t') && !inQuote:
			if inArg {
				out = append(out, cur.String())
				cur.Reset()
				inArg = false
			}
		default:
			cur.WriteByte(c)
			inArg = true
		}
		i++
	}
	if inArg {
		out = append(out, cur.String())
	}
	return out
}
