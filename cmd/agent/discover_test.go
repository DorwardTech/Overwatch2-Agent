package main

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/ozonelib"
	"overwatch/agent/internal/ozonesim"
)

var receivedAt = time.Date(2026, 10, 24, 3, 4, 5, 0, time.UTC)

type announce struct {
	sid     string
	arena   int
	ip      string
	sender  string
	times   int
	noClock bool
}

// heard runs announcements through a real table, so the candidates (their
// usability and reasons included) are exactly what a scan would return.
func heard(t *testing.T, as ...announce) ([]ozonelib.Candidate, ozonelib.Counters) {
	t.Helper()
	tbl := ozonelib.NewTable(func() time.Time { return receivedAt })
	for _, a := range as {
		sid, raw, err := ozonelib.ParseSID(a.sid)
		if err != nil {
			t.Fatal(err)
		}
		p := ozonelib.Packet{
			IP: netip.MustParseAddr(a.ip), SID: sid, SIDRaw: raw, Arena: a.arena,
			Time:    ozonelib.WallClock{Year: 2026, Month: 10, Day: 24, Weekday: 6, Hour: 13, Min: 34, Sec: 5, Usec: 250000},
			HasTime: !a.noClock,
		}
		for range max(a.times, 1) {
			tbl.Observe(p, netip.MustParseAddr(a.sender), receivedAt)
		}
	}
	return tbl.Candidates(), tbl.Counters()
}

func selectors(t *testing.T, sid string, arena int) config.LIBSettings {
	t.Helper()
	l := config.LIBSettings{Port: ozonelib.DefaultPort}
	if sid != "" {
		n, raw, err := ozonelib.ParseSID(sid)
		if err != nil {
			t.Fatal(err)
		}
		l.SID, l.SIDRaw = &n, raw
	}
	if arena >= 0 {
		l.Arena = &arena
	}
	return l
}

func render(r discoverReport) (string, int) {
	var b bytes.Buffer
	code := writeDiscover(&b, r)
	return b.String(), code
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, s := range want {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func mustNotContain(t *testing.T, out string, unwanted ...string) {
	t.Helper()
	for _, s := range unwanted {
		if strings.Contains(out, s) {
			t.Errorf("output has %q:\n%s", s, out)
		}
	}
}

// row returns the table's header and the row whose IP column holds ip, and
// checks that the value under each header starts in the same column.
func row(t *testing.T, out, ip string) map[string]string {
	t.Helper()
	lines := strings.Split(out, "\n")
	hi := -1
	for i, l := range lines {
		if strings.HasPrefix(l, "SID ") {
			hi = i
			break
		}
	}
	if hi < 0 {
		t.Fatalf("no table header:\n%s", out)
	}
	header := lines[hi]
	cols := []string{"SID", "ARENA", "IP", "SENDER", "LOCAL", "PACKETS", "O-ZONE TIME", "RECEIVED (UTC)", "STATUS"}
	starts := make([]int, len(cols))
	for i, c := range cols {
		starts[i] = strings.Index(header, c)
		if starts[i] < 0 || (i > 0 && starts[i] <= starts[i-1]) {
			t.Fatalf("header %q: column %q missing or out of order", header, c)
		}
	}
	for _, l := range lines[hi+1:] {
		if l == "" {
			break
		}
		if len(l) <= starts[3] || strings.TrimSpace(l[starts[2]:starts[3]]) != ip {
			continue
		}
		got := map[string]string{}
		for i, c := range cols {
			end := len(l)
			if i+1 < len(cols) {
				end = starts[i+1]
			}
			if l[starts[i]] == ' ' || (starts[i] > 0 && l[starts[i]-1] != ' ') {
				t.Errorf("row %q: the %s value does not start under its header", l, c)
			}
			got[c] = strings.TrimSpace(l[starts[i]:end])
		}
		return got
	}
	t.Fatalf("no row for %s:\n%s", ip, out)
	return nil
}

func TestDiscoverNothingHeard(t *testing.T) {
	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			r := discoverReport{LIB: selectors(t, "", -1), Seconds: 11, Host: "auto", OS: goos}
			out, code := render(r)
			if code != exitFailure {
				t.Errorf("exit = %d, want %d", code, exitFailure)
			}
			mustContain(t, out,
				"No game server announcement was heard.",
				"Packets: 0 recorded, 0 rejected, 0 off-link dropped, 0 sender mismatch",
				"With OZONE_SID and OZONE_ARENA unset the agent would wait: no usable server was heard.",
				"No usable game server was heard. Check that:",
				"subnet or VLAN",
				"the game server is running",
			)
			mustNotContain(t, out, "\nSID ", "Interrupted")
			if goos == "windows" {
				mustContain(t, out, "Windows Firewall allows inbound UDP 12106", "block all incoming connections")
				mustNotContain(t, out, "host networking")
			} else {
				mustContain(t, out, "host networking (network_mode: host)", "bridge network")
				mustNotContain(t, out, "Windows Firewall")
			}
		})
	}
}

// Announcements that arrived but were all dropped are not "nothing heard":
// the network path works, so the advice is about why they were dropped.
func TestDiscoverEverythingDropped(t *testing.T) {
	for _, tc := range []struct {
		name     string
		n        ozonelib.Counters
		want     []string
		dontWant []string
	}{
		{"off-link", ozonelib.Counters{Offlink: 3}, []string{"3 from outside this machine's networks, 0 that could not be read", "subnet mask"}, []string{"not an announcement"}},
		{"unreadable", ozonelib.Counters{Rejected: 2}, []string{"0 from outside this machine's networks, 2 that could not be read", "not an announcement the agent can read"}, []string{"subnet mask"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, code := render(discoverReport{LIB: selectors(t, "", -1), Seconds: 11, Host: "auto", OS: "linux", Counters: tc.n})
			if code != exitFailure {
				t.Errorf("exit = %d, want %d", code, exitFailure)
			}
			mustContain(t, out, append(tc.want, "Announcements arrived, but none could be used", "No usable game server was heard. Check that:")...)
			mustNotContain(t, out, append(tc.dontWant, "No game server announcement was heard.", "host networking", "Windows Firewall")...)
		})
	}
}

func TestDiscoverOneUsableServer(t *testing.T) {
	cs, n := heard(t, announce{sid: "4242", arena: 1, ip: "192.0.2.10", sender: "192.0.2.10", times: 3})
	out, code := render(discoverReport{
		LIB: selectors(t, "4242", 1), Seconds: 11, Host: "auto", OS: "linux", Candidates: cs, Counters: n,
	})
	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	got := row(t, out, "192.0.2.10")
	want := map[string]string{
		"SID": "4242", "ARENA": "1", "IP": "192.0.2.10", "SENDER": "192.0.2.10", "LOCAL": "no", "PACKETS": "3",
		"O-ZONE TIME": "2026-10-24T13:34:05.250000", "RECEIVED (UTC)": "2026-10-24 03:04:05", "STATUS": "usable",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	mustContain(t, out,
		"Packets: 3 recorded, 0 rejected, 0 off-link dropped, 0 sender mismatch",
		"With OZONE_SID=4242 OZONE_ARENA=1 the agent would choose 192.0.2.10 (site 4242, arena 1, sender 192.0.2.10).",
	)
	mustNotContain(t, out, "Check that", "OZONE_WS_HOST is", "one run only")
}

func TestDiscoverZeroConfigChoiceIsForOneRun(t *testing.T) {
	cs, n := heard(t, announce{sid: "0042", arena: 2, ip: "192.0.2.10", sender: "192.0.2.10"})
	out, _ := render(discoverReport{LIB: selectors(t, "", -1), Host: "AUTO", OS: "linux", Candidates: cs, Counters: n})
	mustContain(t, out,
		"With OZONE_SID and OZONE_ARENA unset the agent would choose 192.0.2.10 (site 0042, arena 2, sender 192.0.2.10).",
		"\nIt would keep to that server for one run only; set OZONE_SID=0042 and OZONE_ARENA=2 to make that permanent.\n",
	)
}

func TestDiscoverTwoSites(t *testing.T) {
	cs, n := heard(t,
		announce{sid: "4242", arena: 1, ip: "192.0.2.10", sender: "192.0.2.10"},
		announce{sid: "4243", arena: 1, ip: "198.51.100.20", sender: "198.51.100.20"},
	)
	r := discoverReport{LIB: selectors(t, "", -1), Host: "auto", OS: "linux", Candidates: cs, Counters: n}
	out, code := render(r)
	if code != exitOK {
		t.Errorf("exit = %d, want %d: a usable server was heard", code, exitOK)
	}
	row(t, out, "192.0.2.10")
	row(t, out, "198.51.100.20")
	mustContain(t, out, "With OZONE_SID and OZONE_ARENA unset the agent would wait: 2 servers heard "+
		"(site 4242 arena 1, site 4243 arena 1) — set OZONE_SID and OZONE_ARENA to choose one.")
	mustNotContain(t, out, "would choose", "Check that")

	r.LIB = selectors(t, "4243", -1)
	out, _ = render(r)
	mustContain(t, out, "With OZONE_SID=4243 (OZONE_ARENA unset) the agent would choose 198.51.100.20 (site 4243, arena 1, sender 198.51.100.20).")

	r.LIB = selectors(t, "4244", 1)
	out, code = render(r)
	mustContain(t, out, "With OZONE_SID=4244 OZONE_ARENA=1 the agent would wait: none of the servers heard matches.")
	if code != exitOK {
		t.Errorf("exit = %d, want %d: usable servers were heard, just not the selected one", code, exitOK)
	}
}

func TestDiscoverConflict(t *testing.T) {
	cs, n := heard(t,
		announce{sid: "4242", arena: 1, ip: "192.0.2.10", sender: "192.0.2.10"},
		announce{sid: "4242", arena: 1, ip: "192.0.2.11", sender: "192.0.2.11"},
	)
	out, code := render(discoverReport{LIB: selectors(t, "4242", 1), Host: "auto", OS: "linux", Candidates: cs, Counters: n})
	if code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	mustContain(t, out, "the agent would wait: site 4242 arena 1 is announced from 2 addresses (192.0.2.10, 192.0.2.11)")
}

func TestDiscoverSenderMismatch(t *testing.T) {
	cs, n := heard(t, announce{sid: "4242", arena: 1, ip: "192.0.2.10", sender: "198.51.100.7", times: 2})
	out, code := render(discoverReport{LIB: selectors(t, "", -1), Host: "auto", OS: "linux", Candidates: cs, Counters: n})
	if code != exitFailure {
		t.Errorf("exit = %d, want %d: the only server heard is unusable", code, exitFailure)
	}
	got := row(t, out, "192.0.2.10")
	if got["SENDER"] != "198.51.100.7" || got["STATUS"] != "sent from 198.51.100.7, claims 192.0.2.10" {
		t.Errorf("row = %v", got)
	}
	mustContain(t, out,
		"Packets: 2 recorded, 0 rejected, 0 off-link dropped, 2 sender mismatch",
		"would wait: no usable server was heard.",
		"Check that:",
		"announced an address other than the one it sent from",
	)
}

func TestDiscoverLocalServerAndNoClock(t *testing.T) {
	cs, n := heard(t,
		announce{sid: "4242", arena: 0, ip: "127.0.0.1", sender: "127.0.0.1", noClock: true},
		announce{sid: "4242", arena: 1, ip: "192.0.2.10", sender: "192.0.2.10"},
	)
	local := map[netip.Addr]bool{netip.MustParseAddr("127.0.0.1"): true}
	out, _ := render(discoverReport{LIB: selectors(t, "4242", 0), Host: "auto", OS: "linux", Candidates: cs, Counters: n, Local: local})
	if got := row(t, out, "127.0.0.1"); got["LOCAL"] != "yes" || got["O-ZONE TIME"] != "-" || got["STATUS"] != "usable" {
		t.Errorf("local row = %v", got)
	}
	if got := row(t, out, "192.0.2.10"); got["LOCAL"] != "no" || got["O-ZONE TIME"] != "2026-10-24T13:34:05.250000" {
		t.Errorf("remote row = %v", got)
	}
}

func TestDiscoverFixedHost(t *testing.T) {
	cs, n := heard(t, announce{sid: "4242", arena: 1, ip: "192.0.2.10", sender: "192.0.2.10"})
	r := discoverReport{LIB: selectors(t, "", -1), Host: "192.0.2.10", OS: "linux", Candidates: cs, Counters: n}
	out, _ := render(r)
	const connects = ": the agent connects to that address whatever is announced.\n"
	mustContain(t, out,
		"OZONE_WS_HOST is 192.0.2.10"+connects+"That address was heard announcing site 4242 arena 1.\n",
		"In auto mode (OZONE_WS_HOST=auto), with OZONE_SID and OZONE_ARENA unset the agent would choose 192.0.2.10",
	)

	r.Host = "192.0.2.99"
	out, _ = render(r)
	mustContain(t, out, "OZONE_WS_HOST is 192.0.2.99"+connects+"No server heard announces that address.\n")

	// The default host is this machine, which a server announcing one of
	// this machine's addresses is.
	r.Host, r.Local = "", map[netip.Addr]bool{netip.MustParseAddr("192.0.2.10"): true}
	out, _ = render(r)
	mustContain(t, out, "OZONE_WS_HOST is 127.0.0.1 (the default)"+connects+"That address was heard announcing site 4242 arena 1.\n")

	// A host name cannot be compared with an announced address.
	r.Host = "ozone.example"
	out, _ = render(r)
	mustContain(t, out, "OZONE_WS_HOST is ozone.example"+connects+"In auto mode")
}

func TestDiscoverBindFailure(t *testing.T) {
	out, code := render(discoverReport{
		LIB: selectors(t, "", -1), Host: "auto", OS: "linux",
		BindErr: errors.New("listen udp4 :12106: bind: address already in use"),
	})
	if code != exitFailure {
		t.Errorf("exit = %d, want %d", code, exitFailure)
	}
	mustContain(t, out, "Cannot listen on UDP 12106: listen udp4 :12106: bind: address already in use", "hold the port exclusively")
	mustNotContain(t, out, "Packets:", "would")
}

func TestDiscoverCustomPortOnWindows(t *testing.T) {
	l := selectors(t, "", -1)
	l.Port = 12107
	out, _ := render(discoverReport{LIB: l, Host: "auto", OS: "windows"})
	mustContain(t, out, "inbound UDP 12107", "opens UDP 12106 only; OZONE_LIB_PORT=12107 needs a rule of its own")
}

// discover passes the port and duration through, says what it is doing
// first, and reports an interrupted scan as such.
func TestDiscoverRunsTheScan(t *testing.T) {
	cs, n := heard(t, announce{sid: "4242", arena: 1, ip: "192.0.2.10", sender: "192.0.2.10"})
	var gotPort int
	var gotD time.Duration
	scan := func(_ context.Context, port int, d time.Duration) ([]ozonelib.Candidate, ozonelib.Counters, error) {
		gotPort, gotD = port, d
		return cs, n, nil
	}
	var b bytes.Buffer
	l := selectors(t, "", -1)
	l.Port = 12999
	if code := discover(context.Background(), &b, l, " auto ", 7, scan); code != exitOK {
		t.Errorf("exit = %d, want %d", code, exitOK)
	}
	if gotPort != 12999 || gotD != 7*time.Second {
		t.Errorf("scan(port %d, %s), want (12999, 7s)", gotPort, gotD)
	}
	if !strings.HasPrefix(b.String(), "Listening on UDP 12999 for 7 s...\n") {
		t.Errorf("output does not start by saying what it does:\n%s", b.String())
	}
	mustNotContain(t, b.String(), "Interrupted", "OZONE_WS_HOST is")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.Reset()
	empty := func(context.Context, int, time.Duration) ([]ozonelib.Candidate, ozonelib.Counters, error) {
		return nil, ozonelib.Counters{}, nil
	}
	if code := discover(ctx, &b, l, "auto", 7, empty); code != exitFailure {
		t.Errorf("interrupted with nothing heard: exit = %d, want %d", code, exitFailure)
	}
	mustContain(t, b.String(), "Interrupted", "stopped early")
}

// The real thing: a fake game server announcing itself to a free port is
// heard by an actual scan.
func TestDiscoverHearsAnAnnouncement(t *testing.T) {
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()

	sender := &ozonesim.LIBSender{Target: net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), Source: "127.0.0.1"}
	sender.Start(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"}, 50*time.Millisecond)
	defer sender.Stop()

	var b bytes.Buffer
	l := selectors(t, "4242", 1)
	l.Port = port
	if code := discover(context.Background(), &b, l, "auto", 1, ozonelib.Scan); code != exitOK {
		t.Fatalf("exit = %d, want %d:\n%s", code, exitOK, b.String())
	}
	got := row(t, b.String(), "127.0.0.1")
	if got["SID"] != "4242" || got["ARENA"] != "1" || got["LOCAL"] != "yes" || got["STATUS"] != "usable" || got["O-ZONE TIME"] != "2024-08-06T12:24:59.129158" {
		t.Errorf("row = %v", got)
	}
	mustContain(t, b.String(), "the agent would choose 127.0.0.1 (site 4242, arena 1, sender 127.0.0.1).")
}
