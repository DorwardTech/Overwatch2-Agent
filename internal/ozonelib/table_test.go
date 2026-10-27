package ozonelib

import (
	"net/netip"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestTable() (*Table, *fakeClock) {
	c := &fakeClock{t: time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)}
	return NewTable(c.now), c
}

func pkt(ip string, sid uint32, arena int) Packet {
	return Packet{IP: netip.MustParseAddr(ip), SID: sid, SIDRaw: "x", Arena: arena}
}

// hear records n announcements of p, sent from p's own address.
func hear(t *Table, c *fakeClock, p Packet, n int) {
	for i := 0; i < n; i++ {
		t.Observe(p, p.IP, c.now())
	}
}

func u32(v uint32) *uint32 { return &v }
func intp(v int) *int      { return &v }

func TestSelectOneServer(t *testing.T) {
	tb, c := newTestTable()
	hear(tb, c, pkt("192.0.2.10", 4242, 1), 1)
	r := tb.Select(Selector{})
	if r.Outcome != Selected || r.Selected.IP != netip.MustParseAddr("192.0.2.10") || r.Selected.Key != (Key{4242, 1}) {
		t.Fatalf("Select() = %+v", r)
	}
}

func TestSelectRefusesToGuess(t *testing.T) {
	cases := map[string]struct {
		heard []Packet
		sel   Selector
		want  Outcome
	}{
		"nothing heard":         {nil, Selector{}, None},
		"two sites":             {[]Packet{pkt("192.0.2.10", 4242, 1), pkt("192.0.2.11", 4343, 1)}, Selector{}, Ambiguous},
		"site picks one":        {[]Packet{pkt("192.0.2.10", 4242, 1), pkt("192.0.2.11", 4343, 1)}, Selector{SID: u32(4343)}, Selected},
		"two arenas, one IP":    {[]Packet{pkt("192.0.2.10", 4242, 0), pkt("192.0.2.10", 4242, 1)}, Selector{}, Ambiguous},
		"arena picks one":       {[]Packet{pkt("192.0.2.10", 4242, 0), pkt("192.0.2.10", 4242, 1)}, Selector{Arena: intp(0)}, Selected},
		"one server, two IPs":   {[]Packet{pkt("192.0.2.10", 4242, 1), pkt("192.0.2.11", 4242, 1)}, Selector{}, Conflict},
		"selector matches none": {[]Packet{pkt("192.0.2.10", 4242, 1)}, Selector{SID: u32(1)}, None},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tb, c := newTestTable()
			for _, p := range tc.heard {
				hear(tb, c, p, 1)
			}
			if r := tb.Select(tc.sel); r.Outcome != tc.want {
				t.Fatalf("Select() = %v (%+v), want %v", r.Outcome, r.Candidates, tc.want)
			}
		})
	}
}

func TestSelectForgetsAServerAfterTheTTL(t *testing.T) {
	tb, c := newTestTable()
	hear(tb, c, pkt("192.0.2.10", 4242, 1), 1)
	c.advance(entryTTL + time.Second)
	hear(tb, c, pkt("192.0.2.11", 4343, 1), 1)
	r := tb.Select(Selector{})
	if r.Outcome != Selected || r.Selected.SID != 4343 {
		t.Fatalf("Select() = %+v, want only the server still heard", r)
	}
}

// A server heard again after a silence longer than the TTL starts afresh: one
// new announcement is not "heard twice" on the strength of old ones.
func TestALapsedEntryStartsAfresh(t *testing.T) {
	tb, c := newTestTable()
	p := pkt("192.0.2.20", 4242, 1)
	hear(tb, c, p, 3)
	c.advance(entryTTL + time.Second)
	hear(tb, c, p, 1)
	cs := tb.Candidates()
	if len(cs) != 1 || cs[0].Packets != 1 || !cs[0].FirstSeen.Equal(c.now()) {
		t.Fatalf("Candidates() = %+v, want one fresh entry", cs)
	}
	if _, ok := tb.Moved(Key{4242, 1}, netip.MustParseAddr("192.0.2.10")); ok {
		t.Fatal("moved on a single announcement after a silence")
	}
}

// A packet naming an address it was not sent from is listed and counted, but
// never chosen, and never spoils the genuine server's entry.
func TestASenderMismatchIsNeverChosen(t *testing.T) {
	tb, c := newTestTable()
	spoof := pkt("192.0.2.10", 4242, 1)
	tb.Observe(spoof, netip.MustParseAddr("192.0.2.66"), c.now())
	if r := tb.Select(Selector{}); r.Outcome != None {
		t.Fatalf("Select() = %+v, want None", r)
	}
	if tb.Counters().SenderMismatch != 1 {
		t.Fatalf("Counters() = %+v", tb.Counters())
	}
	cs := tb.Candidates()
	if len(cs) != 1 || cs[0].Usable || cs[0].Reason != "sent from 192.0.2.66, claims 192.0.2.10" {
		t.Fatalf("Candidates() = %+v", cs)
	}

	hear(tb, c, spoof, 1) // the genuine server
	tb.Observe(spoof, netip.MustParseAddr("192.0.2.66"), c.now())
	if r := tb.Select(Selector{}); r.Outcome != Selected || r.Selected.Sender != spoof.IP {
		t.Fatalf("Select() = %+v, want the genuine server", r)
	}
}

func TestTheTableIsBounded(t *testing.T) {
	tb, c := newTestTable()
	for i := 0; i < maxEntries+4; i++ {
		c.advance(time.Millisecond)
		hear(tb, c, pkt("192.0.2.10", uint32(i), 1), 1)
	}
	cs := tb.Candidates()
	if len(cs) != maxEntries {
		t.Fatalf("%d entries, want %d", len(cs), maxEntries)
	}
	if cs[0].SID != 4 {
		t.Fatalf("oldest kept is site %d, want 4 (the four heard first evicted)", cs[0].SID)
	}
}

func TestMoved(t *testing.T) {
	pin := Key{4242, 1}
	old := netip.MustParseAddr("192.0.2.10")

	tb, c := newTestTable()
	hear(tb, c, pkt("192.0.2.20", 4242, 1), 1)
	if _, ok := tb.Moved(pin, old); ok {
		t.Fatal("moved on a single announcement")
	}
	hear(tb, c, pkt("192.0.2.20", 4242, 1), 1)
	if ip, ok := tb.Moved(pin, old); !ok || ip != netip.MustParseAddr("192.0.2.20") {
		t.Fatalf("Moved() = %v, %v; want 192.0.2.20", ip, ok)
	}

	// Still heard at the old address too: a conflict, not a move.
	hear(tb, c, pkt("192.0.2.10", 4242, 1), 2)
	if _, ok := tb.Moved(pin, old); ok {
		t.Fatal("moved while the server was also heard at its current address")
	}

	// A spoofed claim of the new address does not count.
	tb2, c2 := newTestTable()
	for i := 0; i < 3; i++ {
		tb2.Observe(pkt("192.0.2.20", 4242, 1), netip.MustParseAddr("192.0.2.66"), c2.now())
	}
	if _, ok := tb2.Moved(pin, old); ok {
		t.Fatal("moved on announcements sent from another address")
	}
}

func TestIdentityLost(t *testing.T) {
	pin := Key{4242, 1}
	cur := netip.MustParseAddr("192.0.2.10")

	tb, c := newTestTable()
	hear(tb, c, pkt("192.0.2.10", 4242, 1), 2)
	hear(tb, c, pkt("192.0.2.10", 5555, 0), 2)
	if tb.IdentityLost(pin, cur) {
		t.Fatal("identity lost while the pinned server is still heard at the address")
	}

	c.advance(entryTTL / 2)
	hear(tb, c, pkt("192.0.2.10", 5555, 0), 1)
	c.advance(entryTTL/2 + time.Second)
	hear(tb, c, pkt("192.0.2.10", 5555, 0), 1)
	if !tb.IdentityLost(pin, cur) {
		t.Fatal("identity not lost once only another server was heard at the address")
	}

	tb2, c2 := newTestTable()
	hear(tb2, c2, pkt("192.0.2.10", 5555, 0), 1)
	if tb2.IdentityLost(pin, cur) {
		t.Fatal("identity lost on a single announcement")
	}
}

func TestSelectFromAScan(t *testing.T) {
	tb, c := newTestTable()
	hear(tb, c, pkt("192.0.2.10", 4242, 0), 1)
	hear(tb, c, pkt("192.0.2.10", 4242, 1), 1)
	cs := tb.Candidates()
	if r := SelectFrom(cs, Selector{}); r.Outcome != Ambiguous {
		t.Fatalf("SelectFrom() = %v, want ambiguous", r.Outcome)
	}
	if r := SelectFrom(cs, Selector{Arena: intp(1)}); r.Outcome != Selected || r.Selected.Arena != 1 {
		t.Fatalf("SelectFrom(arena 1) = %+v", r)
	}
}
