package ozonelib

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"overwatch/agent/internal/ozonesim"
)

func startService(t *testing.T, cfg Config) (*Service, *ozonesim.LIBSender) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := New(cfg)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, &ozonesim.LIBSender{Target: "127.0.0.1:" + strconv.Itoa(s.Port()), Source: "127.0.0.1"}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServiceHearsAnAnnouncement(t *testing.T) {
	s, send := startService(t, Config{Auto: true})
	before := time.Now().UTC()
	if err := send.Send(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the announcement", func() bool { return s.Select().Outcome == Selected })
	c := s.Select().Selected
	if c.Key != (Key{4242, 1}) || c.IP != netip.MustParseAddr("127.0.0.1") || c.Sender != c.IP {
		t.Fatalf("selected %+v", c)
	}
	if c.LastSeen.Before(before) || c.LastSeen.Location() != time.UTC {
		t.Fatalf("received at %v, want a UTC time after %v", c.LastSeen, before)
	}
}

// Wait chooses only once the window has passed: a second server heard inside
// it must be able to stop a choice.
func TestWaitHoldsForTheWindow(t *testing.T) {
	s, send := startService(t, Config{Auto: true})
	_ = send.Send(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	var sawEarly bool
	c, ok := s.Wait(ctx, 300*time.Millisecond, func(r Result, windowDone bool) {
		if !windowDone && r.Outcome == Selected {
			sawEarly = true
		}
	})
	if !ok || c.SID != 4242 {
		t.Fatalf("Wait() = %+v, %v", c, ok)
	}
	if time.Since(start) < 300*time.Millisecond-50*time.Millisecond || !sawEarly {
		t.Fatalf("Wait returned after %s (saw the server early: %v), want after the window", time.Since(start), sawEarly)
	}

	_ = send.Send(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 2, SID: "4242"})
	waitFor(t, "a second arena", func() bool { return s.Select().Outcome == Ambiguous })
	ctx2, cancel2 := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel2()
	if c, ok := s.Wait(ctx2, 0, nil); ok {
		t.Fatalf("Wait() chose %+v with two arenas heard", c)
	}
}

func TestTheSecondReuseBindSucceeds(t *testing.T) {
	ctx := context.Background()
	a, err := Listen(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	port := a.LocalAddr().(*net.UDPAddr).Port
	b, err := Listen(ctx, port)
	if err != nil {
		t.Fatalf("a second listener could not share UDP %d: %v", port, err)
	}
	b.Close()
}

func TestBadDatagramsAreCountedNotRecorded(t *testing.T) {
	s, send := startService(t, Config{Auto: true})
	_ = send.SendRaw([]byte(strings.Repeat("x", MaxPacket+1)))
	_ = send.SendRaw([]byte("<lib><server/></lib>"))
	waitFor(t, "two rejections", func() bool { return s.table.Counters().Rejected == 2 })
	if n := len(s.table.Candidates()); n != 0 {
		t.Fatalf("%d candidates recorded from bad datagrams", n)
	}
}

func TestABindFailureIsReportedAndNotFatal(t *testing.T) {
	held, err := net.ListenPacket("udp4", ":0") // bound without reuse
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.LocalAddr().(*net.UDPAddr).Port

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := New(Config{Port: port, Auto: true})
	defer s.Close()
	if err := s.Start(ctx); err == nil {
		t.Skip("this platform let a reuse bind share a port held without reuse")
	}
	r := s.Report()
	if r["state"] != "bind_failed" || r["listening"] != false || r["error"] == "" {
		t.Fatalf("Report() = %v", r)
	}
	if reason := s.NotReadyReason(); !strings.Contains(reason, "cannot listen") {
		t.Fatalf("NotReadyReason() = %q", reason)
	}
}

func TestReportInAutoMode(t *testing.T) {
	sid, arena := uint32(4242), 1
	s, send := startService(t, Config{Auto: true, Selector: Selector{SID: &sid, Arena: &arena}})
	if r := s.Report(); r["state"] != "waiting" || r["selected"] != nil || r["last"] != nil {
		t.Fatalf("Report() before anything heard = %v", r)
	}
	if !strings.Contains(s.NotReadyReason(), "waiting") {
		t.Fatalf("NotReadyReason() = %q", s.NotReadyReason())
	}
	_ = send.Send(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "04242"})
	waitFor(t, "the announcement", func() bool { return s.Select().Outcome == Selected })
	s.SetSelected(s.Select().Selected)

	r := s.Report()
	if r["mode"] != "auto" || r["state"] != "selected" || r["listening"] != true {
		t.Fatalf("Report() = %v", r)
	}
	if sel := r["selector"].(map[string]any); sel["sid"] != sid || sel["arena"] != arena {
		t.Fatalf("selector = %v", sel)
	}
	selected := r["selected"].(map[string]any)
	if selected["ip"] != "127.0.0.1" || selected["sid_raw"] != "04242" || selected["sid"] != uint32(4242) {
		t.Fatalf("selected = %v", selected)
	}
	last := r["last"].(map[string]any)
	if last["ozone_time_text"] != "2024-08-06T12:24:59.129158" || last["time_ok"] != true || last["received_at"] == "" {
		t.Fatalf("last = %v", last)
	}
	if _, ok := r["configured_host_heard"]; ok {
		t.Fatal("auto mode reported configured_host_heard")
	}
	if s.NotReadyReason() != "" {
		t.Fatalf("NotReadyReason() = %q once selected", s.NotReadyReason())
	}
	if cs := r["candidates"].([]map[string]any); len(cs) != 1 || cs[0]["usable"] != true {
		t.Fatalf("candidates = %v", cs)
	}
}

func TestReportInReportOnlyMode(t *testing.T) {
	heard, send := startService(t, Config{FixedHost: "127.0.0.1"})
	_ = send.Send(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 0, SID: "4242"})
	waitFor(t, "the announcement", func() bool { return len(heard.table.Candidates()) == 1 })
	r := heard.Report()
	if r["mode"] != "static" || r["state"] != "report_only" || r["configured_host_heard"] != true || r["last"] == nil {
		t.Fatalf("Report() = %v", r)
	}
	if _, ok := r["selector"]; ok {
		t.Fatal("report-only mode reported a selector")
	}
	if heard.NotReadyReason() != "" {
		t.Fatal("report-only mode is never not-ready")
	}

	other, send2 := startService(t, Config{FixedHost: "192.0.2.10"})
	_ = send2.Send(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 0, SID: "4242"})
	waitFor(t, "the announcement", func() bool { return len(other.table.Candidates()) == 1 })
	if r := other.Report(); r["configured_host_heard"] != false || r["last"] != nil {
		t.Fatalf("Report() = %v", r)
	}

	named, send3 := startService(t, Config{FixedHost: "ozone.lan"})
	_ = send3.Send(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 0, SID: "4242"})
	waitFor(t, "the announcement", func() bool { return len(named.table.Candidates()) == 1 })
	if _, ok := named.Report()["configured_host_heard"]; ok {
		t.Fatal("a host name is not an address: configured_host_heard must be absent")
	}
}

func TestTheLinkCache(t *testing.T) {
	l := &linkCache{read: func() ([]net.Addr, error) {
		_, n, _ := net.ParseCIDR("192.0.2.0/24")
		n.IP = net.ParseIP("192.0.2.7").To4()
		return []net.Addr{n}, nil
	}}
	for ip, want := range map[string]bool{"192.0.2.10": true, "198.51.100.1": false, "127.0.0.2": true} {
		if got := l.onLink(netip.MustParseAddr(ip)); got != want {
			t.Errorf("onLink(%s) = %v, want %v", ip, got, want)
		}
	}
	if !l.isLocal(netip.MustParseAddr("192.0.2.7")) || l.isLocal(netip.MustParseAddr("192.0.2.10")) {
		t.Error("isLocal did not tell this machine's address from a neighbour's")
	}
}

func TestScanCollectsForTheWindow(t *testing.T) {
	probe, err := net.ListenPacket("udp4", ":0")
	if err != nil {
		t.Fatal(err)
	}
	port := probe.LocalAddr().(*net.UDPAddr).Port
	probe.Close()

	send := &ozonesim.LIBSender{Target: "127.0.0.1:" + strconv.Itoa(port), Source: "127.0.0.1"}
	send.Start(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 3, SID: "4242"}, 20*time.Millisecond)
	defer send.Stop()
	cs, n, err := Scan(context.Background(), port, 300*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 1 || cs[0].Arena != 3 || n.Packets < 2 {
		t.Fatalf("Scan() = %+v, %+v", cs, n)
	}
}

// fakePacketConn hands readLoop prepared datagrams, then reports closed.
type fakePacketConn struct {
	net.PacketConn
	datagrams []struct {
		from netip.Addr
		data []byte
	}
}

func (f *fakePacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if len(f.datagrams) == 0 {
		return 0, nil, net.ErrClosed
	}
	d := f.datagrams[0]
	f.datagrams = f.datagrams[1:]
	return copy(b, d.data), net.UDPAddrFromAddrPort(netip.AddrPortFrom(d.from, 12106)), nil
}

// An announcement from beyond every local network is dropped and counted,
// whatever it says: a broadcast never crosses a router.
func TestReadLoopDropsOffLinkSenders(t *testing.T) {
	links := &linkCache{read: func() ([]net.Addr, error) {
		_, n, _ := net.ParseCIDR("192.0.2.0/24")
		return []net.Addr{n}, nil
	}}
	pkt := func(ip string) []byte {
		return ozonesim.LIBPacket(ozonesim.LIBAnnouncement{IP: ip, Arena: 1, SID: "4242"})
	}
	conn := &fakePacketConn{}
	conn.datagrams = append(conn.datagrams,
		struct {
			from netip.Addr
			data []byte
		}{netip.MustParseAddr("198.51.100.7"), pkt("198.51.100.7")},
		struct {
			from netip.Addr
			data []byte
		}{netip.MustParseAddr("192.0.2.10"), pkt("192.0.2.10")},
	)
	tb := NewTable(time.Now)
	readLoop(conn, tb, links, nil)

	if n := tb.Counters(); n.Offlink != 1 || n.Packets != 1 {
		t.Fatalf("Counters() = %+v, want one dropped off-link and one recorded", n)
	}
	cs := tb.Candidates()
	if len(cs) != 1 || cs[0].IP != netip.MustParseAddr("192.0.2.10") {
		t.Fatalf("Candidates() = %+v", cs)
	}
}
