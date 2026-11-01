//go:build windows

package ozonelib

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The holder is found in the system's table of UDP sockets and named by its
// executable: here that is this test binary.
func TestUDPPortHoldersNamesTheProgram(t *testing.T) {
	held, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.LocalAddr().(*net.UDPAddr).Port

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Base(exe) + ", PID " + strconv.Itoa(os.Getpid())
	got := udpPortHolders(port)
	if len(got) == 0 || !strings.EqualFold(got[0], want) {
		t.Fatalf("udpPortHolders(%d) = %q, want [%q]", port, got, want)
	}

	// And the real refusal names it.
	conn, lerr := Listen(t.Context(), port)
	if lerr == nil {
		_ = conn.Close()
		t.Skip("a reuse bind shared a port held without reuse")
	}
	if msg := BindProblem(port, lerr); !strings.Contains(strings.ToLower(msg), strings.ToLower(want)) {
		t.Fatalf("BindProblem(%v) = %q, does not name %q", lerr, msg, want)
	}
}

// A socket bound to one address shares the port with the agent's wildcard
// bind, so it is never the program in the way, and is not named.
func TestUDPPortHoldersLeavesOutASingleAddressSocket(t *testing.T) {
	one, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer one.Close()
	port := one.LocalAddr().(*net.UDPAddr).Port
	if got := udpPortHolders(port); len(got) != 0 {
		t.Fatalf("udpPortHolders(%d) = %q for a socket bound to 127.0.0.1 only", port, got)
	}
}

func TestUDPPortHoldersOfAFreePort(t *testing.T) {
	probe, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	_ = probe.Close()
	if got := udpPortHolders(port); len(got) != 0 {
		t.Fatalf("udpPortHolders(%d) = %q for a closed port", port, got)
	}
}
