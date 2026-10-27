package ozonelib

import (
	"context"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
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
	before := time.Now()
	if err := send.Send(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the announcement", func() bool { return s.Select().Outcome == Selected })
	c := s.Select().Selected
	if c.Key != (Key{4242, 1}) || c.IP != netip.MustParseAddr("127.0.0.1") || c.Sender != c.IP {
		t.Fatalf("selected %+v", c)
	}
	if c.LastSeen.Before(before) {
		t.Fatalf("received at %v, want a time after %v", c.LastSeen, before)
	}
}

// An announcement's arrival is stamped with the monotonic clock kept, so how
// long ago a server was heard (its 30 s entry, a move, a change of identity)
// is not moved by a step of the wall clock. Reports still give UTC.
func TestStampsKeepTheMonotonicClock(t *testing.T) {
	s, send := startService(t, Config{Auto: true})
	if err := send.Send(ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the announcement", func() bool { return s.Select().Outcome == Selected })
	c := s.Select().Selected
	if !strings.Contains(c.LastSeen.String(), " m=") || !strings.Contains(c.FirstSeen.String(), " m=") {
		t.Fatalf("stamps %v / %v carry no monotonic reading: a wall-clock step would age or freshen them", c.FirstSeen, c.LastSeen)
	}
	s.SetSelected(c)
	r := s.Report()
	heard := r["candidates"].([]map[string]any)[0]["last_heard_at"].(string)
	received := r["last"].(map[string]any)["received_at"].(string)
	if !strings.HasSuffix(heard, "Z") || !strings.HasSuffix(received, "Z") {
		t.Fatalf("report times %q / %q are not UTC", heard, received)
	}
}

// syncClock is a fake clock a service can share with its reader goroutine.
type syncClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *syncClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *syncClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// fakeClockService starts an auto-mode service on a free port that reads the
// time from a fake clock, and whose Wait looks every millisecond. A test's
// onTick advances the clock between looks.
func fakeClockService(t *testing.T, sel Selector) (*Service, *syncClock) {
	t.Helper()
	clk := &syncClock{t: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s := New(Config{Auto: true, Selector: sel, now: clk.now, tick: time.Millisecond})
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s, clk
}

func announced(ip string, sid uint32, arena int) Packet {
	return Packet{IP: netip.MustParseAddr(ip), SID: sid, SIDRaw: strconv.FormatUint(uint64(sid), 10), Arena: arena}
}

// Two servers are heard, then the clock jumps far enough to expire both
// entries (or both pause for longer than an entry lasts). The first to be
// heard again is alone for a few seconds, and the window has long passed: it
// must not be chosen, because the other has not had a window to be heard.
// When the other is heard again, the agent is back to not choosing.
func TestWaitNeedsAStableChoice(t *testing.T) {
	s, clk := fakeClockService(t, Selector{})
	bind := clk.now()
	a, b := announced("192.0.2.10", 4242, 1), announced("192.0.2.11", 4343, 1)
	hear := func(p Packet) { s.table.Observe(p, p.IP, clk.now()) }
	hear(a)
	hear(b)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var aloneLooks int
	var reason string
	c, ok := s.Wait(ctx, 11*time.Second, func(r Result, windowDone bool) {
		at := clk.now().Sub(bind) // when the look that found r was made
		switch {
		case at < 20*time.Second && windowDone && r.Outcome != Ambiguous:
			t.Errorf("at %s: %v with both servers heard", at, r.Outcome)
		case at >= 80*time.Second && at < 86*time.Second:
			if r.Outcome == Selected && windowDone {
				aloneLooks++
				reason = s.NotReadyReason()
			}
		case at >= 86*time.Second && r.Outcome != Ambiguous:
			t.Errorf("at %s: %v once the other server was heard again", at, r.Outcome)
		}
		if at >= 110*time.Second {
			cancel()
			return
		}
		if at == 20*time.Second {
			clk.advance(60 * time.Second) // the jump: both entries lapse
		} else {
			clk.advance(time.Second)
		}
		sec := int(clk.now().Sub(bind) / time.Second)
		if sec%5 == 0 {
			hear(a)
		}
		if (sec <= 20 && sec%5 == 0) || (sec >= 86 && (sec-86)%5 == 0) {
			hear(b)
		}
	})
	if ok {
		t.Fatalf("Wait chose %+v at %s", c, clk.now().Sub(bind))
	}
	if aloneLooks == 0 {
		t.Fatal("the test never had one server heard alone after the jump")
	}
	if !strings.Contains(reason, "heard site 4242 arena 1 at 192.0.2.10; listening a little longer") {
		t.Fatalf("NotReadyReason() while confirming = %q", reason)
	}
}

// A server is chosen only after it has been the only one heard for a whole
// window: from the end of the listening window if it was heard at once, and
// from when it was first heard if that came later.
func TestWaitChoosesAfterAFullStableWindow(t *testing.T) {
	const window = 11 * time.Second
	for _, tc := range []struct {
		name       string
		firstHeard time.Duration // after the bind
		chosenAt   time.Duration // after the bind
	}{
		{"heard at once", 0, window},
		{"first heard late in the window", 8 * time.Second, 8*time.Second + window},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, clk := fakeClockService(t, Selector{})
			bind := clk.now()
			a := announced("192.0.2.10", 4242, 1)
			hearIfDue := func() {
				if at := clk.now().Sub(bind); at >= tc.firstHeard && (at-tc.firstHeard)%(5*time.Second) == 0 {
					s.table.Observe(a, a.IP, clk.now())
				}
			}
			hearIfDue()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			c, ok := s.Wait(ctx, window, func(Result, bool) {
				if clk.now().Sub(bind) > time.Minute {
					cancel()
				}
				clk.advance(time.Second)
				hearIfDue()
			})
			if !ok || c.Key != a.Key() || c.IP != a.IP {
				t.Fatalf("Wait() = %+v, %v", c, ok)
			}
			if got := clk.now().Sub(bind); got != tc.chosenAt {
				t.Fatalf("chosen %s after the bind, want %s", got, tc.chosenAt)
			}
		})
	}
}

func TestNotReadyReasonNamesUnmatchedServers(t *testing.T) {
	s := New(Config{Auto: true, Selector: Selector{Arena: intp(1)}})
	p := Packet{IP: netip.MustParseAddr("192.168.1.182"), SID: 564, SIDRaw: "0564", Arena: 0}
	s.table.Observe(p, p.IP, time.Now())
	want := "heard site 0564 arena 0 at 192.168.1.182, but OZONE_ARENA=1 selects no server"
	if got := s.NotReadyReason(); got != want {
		t.Fatalf("NotReadyReason() = %q, want %q", got, want)
	}

	both := New(Config{Auto: true, Selector: Selector{SID: u32(564), SIDRaw: "0564", Arena: intp(1)}})
	for i := 0; i < 6; i++ {
		q := announced("192.0.2."+strconv.Itoa(10+i), 600+uint32(i), 0)
		both.table.Observe(q, q.IP, time.Now())
	}
	want = "heard site 600 arena 0 at 192.0.2.10, site 601 arena 0 at 192.0.2.11, site 602 arena 0 at 192.0.2.12, " +
		"site 603 arena 0 at 192.0.2.13 and 2 more, but OZONE_SID=0564 and OZONE_ARENA=1 select no server"
	if got := both.NotReadyReason(); got != want {
		t.Fatalf("NotReadyReason() = %q, want %q", got, want)
	}
}

func TestNotReadyReasonUntrustedOnly(t *testing.T) {
	s := New(Config{Auto: true})
	p := announced("192.0.2.10", 4242, 1)
	s.table.Observe(p, netip.MustParseAddr("192.0.2.66"), time.Now())
	s.table.Observe(p, netip.MustParseAddr("192.0.2.66"), time.Now())
	want := "heard only announcements sent from another address than they name (sent from 192.0.2.66, claims 192.0.2.10); set OZONE_WS_HOST"
	if got := s.NotReadyReason(); got != want {
		t.Fatalf("NotReadyReason() = %q, want %q", got, want)
	}
}

func TestNotReadyReasonConflictAdvisesAFixedHost(t *testing.T) {
	s := New(Config{Auto: true})
	for _, ip := range []string{"192.0.2.11", "192.0.2.10"} {
		p := announced(ip, 4242, 1)
		s.table.Observe(p, p.IP, time.Now())
	}
	want := "site 4242 arena 1 is announced from 192.0.2.10 and 192.0.2.11; set OZONE_WS_HOST to the right one"
	if got := s.NotReadyReason(); got != want {
		t.Fatalf("NotReadyReason() = %q, want %q", got, want)
	}

	two := New(Config{Auto: true})
	for _, arena := range []int{1, 0} {
		p := announced("192.0.2.10", 4242, arena)
		two.table.Observe(p, p.IP, time.Now())
	}
	want = "more than one game server announced (site 4242 arena 0 at 192.0.2.10 and site 4242 arena 1 at 192.0.2.10); set OZONE_SID and OZONE_ARENA"
	if got := two.NotReadyReason(); got != want {
		t.Fatalf("NotReadyReason() = %q, want %q", got, want)
	}
}

// The advice about host networking and the firewall is for a network that
// delivers nothing: it is given once a whole window has passed with nothing
// heard, and not when something was heard that cannot be used.
func TestNotReadyReasonGivesNetworkAdviceOnlyWhenNothingIsHeard(t *testing.T) {
	s, clk := fakeClockService(t, Selector{})
	if got := s.NotReadyReason(); got != "waiting for the game server's announcement on UDP "+strconv.Itoa(s.Port()) {
		t.Fatalf("NotReadyReason() at the start = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s.Wait(ctx, 11*time.Second, func(_ Result, windowDone bool) {
		if windowDone {
			cancel()
			return
		}
		clk.advance(time.Second)
	})
	got := s.NotReadyReason()
	if !strings.HasPrefix(got, "no game server announcement heard on UDP "+strconv.Itoa(s.Port())) || !strings.Contains(got, "host networking") {
		t.Fatalf("NotReadyReason() after a silent window = %q", got)
	}
	p := announced("192.0.2.10", 4242, 1)
	s.table.Observe(p, netip.MustParseAddr("192.0.2.66"), clk.now())
	if got := s.NotReadyReason(); strings.Contains(got, "host networking") || !strings.Contains(got, "sent from 192.0.2.66") {
		t.Fatalf("NotReadyReason() with an untrusted announcement heard = %q", got)
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

// A is heard alone, then with B (Ambiguous), then B's entry lapses. A must not
// be chosen until it has been alone for a whole window after B lapsed.
func TestWaitRestartsAfterAnAmbiguousLook(t *testing.T) {
	const window = 11 * time.Second
	s, clk := fakeClockService(t, Selector{})
	bind := clk.now()
	a, b := announced("192.0.2.10", 4242, 1), announced("192.0.2.11", 4343, 1)
	hear := func() {
		sec := int(clk.now().Sub(bind) / time.Second)
		if sec%5 == 0 {
			s.table.Observe(a, a.IP, clk.now())
		}
		if sec >= 3 && sec <= 18 && (sec-3)%5 == 0 {
			s.table.Observe(b, b.IP, clk.now())
		}
	}
	hear()
	var aloneFrom time.Duration = -1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, ok := s.Wait(ctx, window, func(r Result, _ bool) {
		at := clk.now().Sub(bind)
		if at > 3*time.Second && r.Outcome == Selected && aloneFrom < 0 {
			aloneFrom = at
		}
		if at > 2*time.Minute {
			cancel()
		}
		clk.advance(time.Second)
		hear()
	})
	got := clk.now().Sub(bind)
	if !ok || c.Key != a.Key() {
		t.Fatalf("Wait() = %+v, %v", c, ok)
	}
	if aloneFrom < 0 {
		// chosen on the very look that first found A alone again
		t.Fatalf("chosen at %s on the first look that found A alone after B lapsed; want a whole window later", got)
	}
	if got < aloneFrom+window {
		t.Fatalf("chosen at %s, A alone again from %s: want no earlier than %s", got, aloneFrom, aloneFrom+window)
	}
	t.Logf("A alone again from %s, chosen at %s", aloneFrom, got)
}

// A is Selected at .10; the clock jumps so .10's entry lapses and A is heard at
// .11 instead. The new address must hold for a whole window.
func TestWaitRestartsOnANewAddress(t *testing.T) {
	const window = 11 * time.Second
	s, clk := fakeClockService(t, Selector{})
	bind := clk.now()
	a10, a11 := announced("192.0.2.10", 4242, 1), announced("192.0.2.11", 4242, 1)
	s.table.Observe(a10, a10.IP, clk.now())
	var movedAt time.Duration = -1
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, ok := s.Wait(ctx, window, func(Result, bool) {
		at := clk.now().Sub(bind)
		if at > 2*time.Minute {
			cancel()
		}
		if at == 5*time.Second {
			clk.advance(31 * time.Second) // .10's entry lapses
		} else {
			clk.advance(time.Second)
		}
		now := clk.now().Sub(bind)
		if now >= 36*time.Second && (now-36*time.Second)%(5*time.Second) == 0 {
			if movedAt < 0 {
				movedAt = now
			}
			s.table.Observe(a11, a11.IP, clk.now())
		}
	})
	got := clk.now().Sub(bind)
	if !ok || c.IP != a11.IP {
		t.Fatalf("Wait() = %+v, %v at %s", c, ok, got)
	}
	if got < movedAt+window {
		t.Fatalf("chosen at .11 at %s, first heard there at %s: want no earlier than %s", got, movedAt, movedAt+window)
	}
	t.Logf("first heard at .11 at %s, chosen at %s", movedAt, got)
}
