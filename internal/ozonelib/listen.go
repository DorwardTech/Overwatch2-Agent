package ozonelib

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

// Listen binds the announcement port on every IPv4 interface (a broadcast is
// only delivered to a wildcard bind), sharing it with any other program that
// also binds it for reuse: the game server's own tools may listen too.
func Listen(ctx context.Context, port int) (net.PacketConn, error) {
	lc := net.ListenConfig{Control: setReuse}
	return lc.ListenPacket(ctx, "udp4", ":"+strconv.Itoa(port))
}

// readLoop reads announcements from conn into t until conn is closed. Each is
// stamped with its arrival time as soon as it is read, from the table's clock:
// time.Now, keeping its monotonic reading, so how long ago a server was heard
// is measured on a clock that a step of the wall clock does not move. Reports
// convert a stamp to UTC only when they format it. A datagram from
// a sender outside every network this machine is on is dropped: a broadcast
// never crosses a router, so one that did is not a game server on this LAN.
// heard, if set, runs after each recorded announcement.
func readLoop(conn net.PacketConn, t *Table, links *linkCache, heard func(Packet, netip.Addr)) {
	buf := make([]byte, MaxPacket+1)
	for {
		n, addr, err := conn.ReadFrom(buf)
		at := t.now()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if isMsgSize(err) {
				t.Reject()
				continue
			}
			// Some platforms report an earlier send's failure on the next
			// read; it says nothing about the socket. Pause so a persistent
			// error cannot spin.
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if n > MaxPacket {
			t.Reject()
			continue
		}
		sender := senderOf(addr)
		if !sender.IsValid() || !links.onLink(sender) {
			t.Offlink()
			continue
		}
		p, err := Parse(buf[:n])
		if err != nil {
			t.Reject()
			continue
		}
		t.Observe(p, sender, at)
		if heard != nil {
			heard(p, sender)
		}
	}
}

func senderOf(addr net.Addr) netip.Addr {
	if u, ok := addr.(*net.UDPAddr); ok {
		if a, ok := netip.AddrFromSlice(u.IP); ok {
			return a.Unmap()
		}
	}
	return netip.Addr{}
}

// linkCache answers whether an address is on, or is, one of this machine's
// networks, re-reading the interfaces at most every linkCacheTTL.
type linkCache struct {
	mu       sync.Mutex
	read     func() ([]net.Addr, error)
	readAt   time.Time
	prefixes []netip.Prefix
}

const linkCacheTTL = 30 * time.Second

func newLinkCache() *linkCache { return &linkCache{read: net.InterfaceAddrs} }

func (l *linkCache) refresh() {
	if l.prefixes != nil && time.Since(l.readAt) < linkCacheTTL {
		return
	}
	addrs, err := l.read()
	if err != nil {
		return
	}
	l.prefixes = l.prefixes[:0]
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip, ok := netip.AddrFromSlice(ipn.IP)
		if !ok {
			continue
		}
		ones, bits := ipn.Mask.Size()
		ip = ip.Unmap()
		if ip.Is4() && bits == 128 {
			ones -= 96 // a 16-byte mask on an IPv4 address
		}
		if p := netip.PrefixFrom(ip, ones); p.IsValid() {
			l.prefixes = append(l.prefixes, p)
		}
	}
	if l.prefixes == nil {
		l.prefixes = []netip.Prefix{}
	}
	l.readAt = time.Now()
}

// onLink reports whether ip lies within a network of one of this machine's
// interfaces. Loopback always passes.
func (l *linkCache) onLink(ip netip.Addr) bool {
	if ip.IsLoopback() {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refresh()
	for _, p := range l.prefixes {
		if p.Masked().Contains(ip) {
			return true
		}
	}
	return false
}

// isLocal reports whether ip is one of this machine's own addresses.
func (l *linkCache) isLocal(ip netip.Addr) bool {
	if ip.IsLoopback() {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refresh()
	for _, p := range l.prefixes {
		if p.Addr() == ip {
			return true
		}
	}
	return false
}

var localLinks = newLinkCache()

// IsLocal reports whether ip is one of this machine's own addresses, such as
// a game server announcing itself from the computer the agent runs on.
func IsLocal(ip netip.Addr) bool { return localLinks.isLocal(ip) }

// OnLink reports whether ip lies in one of this machine's networks, or is
// loopback: an address reached directly, not through a router.
func OnLink(ip netip.Addr) bool { return localLinks.onLink(ip) }
