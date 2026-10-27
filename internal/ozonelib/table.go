package ozonelib

import (
	"cmp"
	"net/netip"
	"slices"
	"sync"
	"time"
)

const (
	// entryTTL is how long a server stays known after its last announcement:
	// six missed broadcasts at the usual five-second period.
	entryTTL = 30 * time.Second
	// maxEntries bounds the table; the server heard least recently goes first.
	maxEntries = 16
)

// Candidate is one game server heard recently, as selection and reports see it.
type Candidate struct {
	Key
	IP        netip.Addr // the address it announced
	Sender    netip.Addr // the address the announcement came from
	SIDRaw    string
	Packets   int
	FirstSeen time.Time
	// LastSeen is when its latest announcement was received, on this
	// machine's clock with its monotonic reading kept (see readLoop): call
	// .UTC() to format it.
	LastSeen time.Time
	Last     Packet
	// Usable is false when the announcement came from an address other than
	// the one it names. Such a server is listed, with Reason, but never chosen.
	Usable bool
	Reason string
}

// Counters tally what the listener has seen since it started.
type Counters struct {
	Packets        int64 // announcements parsed and recorded
	Rejected       int64 // datagrams too large, or that did not parse
	Offlink        int64 // from a sender outside every local network: dropped
	SenderMismatch int64 // recorded, but from an address other than the one named
}

// Outcome is the result of choosing a server.
type Outcome int

const (
	None      Outcome = iota // nothing usable heard
	Selected                 // exactly one server, at exactly one address
	Ambiguous                // more than one server (site/arena) heard
	Conflict                 // one server announced from more than one address
)

func (o Outcome) String() string {
	switch o {
	case Selected:
		return "selected"
	case Ambiguous:
		return "ambiguous"
	case Conflict:
		return "conflict"
	default:
		return "none"
	}
}

// Result is what Select found: the chosen server, or the ones it could not
// choose between.
type Result struct {
	Outcome    Outcome
	Selected   Candidate
	Candidates []Candidate // the usable servers that matched the selector
}

// Selector narrows the choice to a site, an arena, or both. A nil field
// matches anything.
type Selector struct {
	SID   *uint32
	Arena *int
	// SIDRaw is OZONE_SID as written, for messages; SID is used when empty.
	SIDRaw string
}

func (s Selector) matches(k Key) bool {
	return (s.SID == nil || *s.SID == k.SID) && (s.Arena == nil || *s.Arena == k.Arena)
}

// entryKey separates every (server, announced address, sender) combination, so
// a packet naming a server's address from somewhere else never alters the
// genuine entry: it is kept apart, unusable.
type entryKey struct {
	key    Key
	ip     netip.Addr
	sender netip.Addr
}

type entry struct {
	last      Packet
	packets   int
	firstSeen time.Time
	lastSeen  time.Time
}

// Table remembers the servers heard in the last entryTTL and chooses among
// them. It is safe for concurrent use.
type Table struct {
	mu       sync.Mutex
	now      func() time.Time
	ttl      time.Duration
	entries  map[entryKey]*entry
	counters Counters
}

// NewTable returns an empty table reading time from now.
func NewTable(now func() time.Time) *Table {
	return &Table{now: now, ttl: entryTTL, entries: map[entryKey]*entry{}}
}

// Observe records an announcement received from sender at at. Entries that
// have lapsed are dropped first, so a server heard again after a silence
// longer than the TTL starts afresh: its earlier packets do not count towards
// being heard twice.
func (t *Table) Observe(p Packet, sender netip.Addr, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.counters.Packets++
	if sender != p.IP {
		t.counters.SenderMismatch++
	}
	t.expireLocked()
	k := entryKey{key: p.Key(), ip: p.IP, sender: sender}
	e := t.entries[k]
	if e == nil {
		e = &entry{firstSeen: at}
		t.entries[k] = e
	}
	e.last = p
	e.packets++
	e.lastSeen = at
	for len(t.entries) > maxEntries {
		var oldest entryKey
		var oldestAt time.Time
		first := true
		for k, e := range t.entries {
			if first || e.lastSeen.Before(oldestAt) {
				oldest, oldestAt, first = k, e.lastSeen, false
			}
		}
		delete(t.entries, oldest)
	}
}

// Reject counts a datagram that was too large or did not parse.
func (t *Table) Reject() { t.mu.Lock(); t.counters.Rejected++; t.mu.Unlock() }

// Offlink counts a datagram dropped because its sender is on no local network.
func (t *Table) Offlink() { t.mu.Lock(); t.counters.Offlink++; t.mu.Unlock() }

// Counters returns the tallies so far.
func (t *Table) Counters() Counters {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.counters
}

func (t *Table) expireLocked() {
	cutoff := t.now().Add(-t.ttl)
	for k, e := range t.entries {
		if e.lastSeen.Before(cutoff) {
			delete(t.entries, k)
		}
	}
}

func candidate(k entryKey, e *entry) Candidate {
	c := Candidate{
		Key: k.key, IP: k.ip, Sender: k.sender, SIDRaw: e.last.SIDRaw,
		Packets: e.packets, FirstSeen: e.firstSeen, LastSeen: e.lastSeen, Last: e.last,
		Usable: k.sender == k.ip,
	}
	if !c.Usable {
		c.Reason = "sent from " + k.sender.String() + ", claims " + k.ip.String()
	}
	return c
}

// Candidates returns every server heard in the last entryTTL, usable or not,
// sorted by site, arena, address and sender.
func (t *Table) Candidates() []Candidate {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expireLocked()
	out := make([]Candidate, 0, len(t.entries))
	for k, e := range t.entries {
		out = append(out, candidate(k, e))
	}
	slices.SortFunc(out, func(a, b Candidate) int {
		return cmp.Or(cmp.Compare(a.SID, b.SID), cmp.Compare(a.Arena, b.Arena), a.IP.Compare(b.IP), a.Sender.Compare(b.Sender))
	})
	return out
}

func usable(cs []Candidate, keep func(Candidate) bool) []Candidate {
	var out []Candidate
	for _, c := range cs {
		if c.Usable && keep(c) {
			out = append(out, c)
		}
	}
	return out
}

// Select chooses the one server that matches sel. It refuses to guess: two
// servers (sites or arenas, including two arenas at one address) is
// Ambiguous, and one server at two addresses is Conflict.
func (t *Table) Select(sel Selector) Result { return SelectFrom(t.Candidates(), sel) }

// SelectFrom applies Select's rule to a list of candidates, such as a Scan's.
func SelectFrom(cs []Candidate, sel Selector) Result {
	matched := usable(cs, func(c Candidate) bool { return sel.matches(c.Key) })
	r := Result{Candidates: matched}
	if len(matched) == 0 {
		return r
	}
	keys := map[Key]bool{}
	ips := map[netip.Addr]bool{}
	for _, c := range matched {
		keys[c.Key] = true
		ips[c.IP] = true
	}
	switch {
	case len(keys) > 1:
		r.Outcome = Ambiguous
	case len(ips) > 1:
		r.Outcome = Conflict
	default:
		r.Outcome, r.Selected = Selected, matched[0]
	}
	return r
}

// Moved reports the server's new address when it has plainly moved: the
// pinned server is announced, usably, from exactly one address, that address
// is not current, it has been heard at least twice, and the server is no
// longer heard at current.
func (t *Table) Moved(pin Key, current netip.Addr) (netip.Addr, bool) {
	at := usable(t.Candidates(), func(c Candidate) bool { return c.Key == pin })
	if len(at) != 1 || at[0].IP == current || at[0].Packets < 2 {
		return netip.Addr{}, false
	}
	return at[0].IP, true
}

// IdentityLost reports that the machine at current has become a different
// server: current is announcing, usably and at least twice, a site or arena
// other than pin, and pin has not been heard at current within entryTTL.
func (t *Table) IdentityLost(pin Key, current netip.Addr) bool {
	cs := usable(t.Candidates(), func(c Candidate) bool { return c.IP == current })
	other := false
	for _, c := range cs {
		if c.Key == pin {
			return false
		}
		if c.Packets >= 2 {
			other = true
		}
	}
	return other
}
