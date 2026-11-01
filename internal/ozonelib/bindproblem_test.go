package ozonelib

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// heldPort binds a UDP port the way a program that does not share it would,
// and returns the error Listen then gives for it.
func heldPort(t *testing.T) (int, error) {
	t.Helper()
	held, err := net.ListenPacket("udp4", ":0") // bound without reuse
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Close() })
	port := held.LocalAddr().(*net.UDPAddr).Port
	conn, err := Listen(context.Background(), port)
	if err == nil {
		_ = conn.Close()
		t.Skip("this platform let a reuse bind share a port held without reuse")
	}
	return port, err
}

func TestBindProblemNamesWhoHoldsThePort(t *testing.T) {
	port, err := heldPort(t)
	defer func(f func(int) []string) { portHolders = f }(portHolders)
	var asked int
	portHolders = func(p int) []string { asked = p; return []string{"scoring.exe, PID 4242"} }

	got := BindProblem(port, err)
	if asked != port {
		t.Errorf("asked for the holders of %d, want %d", asked, port)
	}
	for _, want := range []string{"(scoring.exe, PID 4242)", "holds UDP", "without sharing it"} {
		if !strings.Contains(got, want) {
			t.Errorf("BindProblem() = %q, missing %q", got, want)
		}
	}
	// A named holder is the answer: a reserved range is not suggested too.
	if strings.Contains(got, "reserved") {
		t.Errorf("BindProblem() = %q, suggests a reserved range when the holder is known", got)
	}
}

func TestBindProblemWithoutAName(t *testing.T) {
	port, err := heldPort(t)
	defer func(f func(int) []string) { portHolders = f }(portHolders)
	portHolders = func(int) []string { return nil }

	got := BindProblem(port, err)
	if !strings.Contains(got, "Another program on this computer holds UDP") {
		t.Errorf("BindProblem() = %q", got)
	}
	// Windows answers access denied, which a reserved range also gives.
	if accessDenied(err) != strings.Contains(got, "excludedportrange") {
		t.Errorf("BindProblem() = %q: the reserved-range hint belongs with access denied only (denied=%v)", got, accessDenied(err))
	}
}

func TestBindProblemSaysNothingItDoesNotKnow(t *testing.T) {
	defer func(f func(int) []string) { portHolders = f }(portHolders)
	portHolders = func(int) []string { t.Error("holders looked up for an unrelated error"); return nil }
	for _, err := range []error{nil, errors.New("listen udp4 :12106: some other failure")} {
		if got := BindProblem(12106, err); got != "" {
			t.Errorf("BindProblem(%v) = %q, want empty", err, got)
		}
	}
}
