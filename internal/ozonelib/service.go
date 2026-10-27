package ozonelib

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config says what a Service is for.
type Config struct {
	Port int // UDP port to listen on (0 picks a free one; tests only)
	// Auto means discovery chooses the game server. Otherwise the service only
	// reports what it hears: it never chooses or changes anything.
	Auto      bool
	Selector  Selector // narrows the choice in auto mode
	FixedHost string   // the configured host, when not in auto mode
	// TTL is how long a server stays known after its last announcement
	// (default 30s). Tests shorten it.
	TTL time.Duration

	// now replaces the clock, and tick Wait's polling period (tests only).
	now  func() time.Time
	tick time.Duration
}

// retryBind is how often auto mode retries a port it could not bind.
const retryBind = 30 * time.Second

// maxReported bounds the candidates a report lists.
const maxReported = 8

// maxListed bounds the servers a not-ready reason names.
const maxListed = 4

// Service listens for announcements for the life of the process and answers
// what it has heard. It is safe for concurrent use.
type Service struct {
	cfg   Config
	table *Table
	links *linkCache
	now   func() time.Time

	mu          sync.Mutex
	conn        net.PacketConn
	loopDone    chan struct{}
	port        int
	listenedAt  time.Time
	bindErr     string
	closed      bool
	selected    *Candidate
	selectedAt  time.Time
	loggedOther string // the other server last logged in report-only mode
	// waiting is set once Wait has been called, with the listening window it
	// was given, so a not-ready reason can tell "nothing heard yet" from
	// "nothing heard all window".
	waiting bool
	window  time.Duration
}

// New returns a service that has not started listening.
func New(cfg Config) *Service {
	now := cfg.now
	if now == nil {
		now = time.Now
	}
	t := NewTable(now)
	if cfg.TTL > 0 {
		t.ttl = cfg.TTL
	}
	return &Service{cfg: cfg, table: t, links: newLinkCache(), now: now, port: cfg.Port}
}

// Start binds the port and starts reading. A failure is returned and kept for
// reports. In auto mode the bind is retried every retryBind until it works,
// since discovery has no other way to find the server. The service closes
// when ctx ends.
func (s *Service) Start(ctx context.Context) error {
	err := s.bind(ctx)
	if err != nil && s.cfg.Auto {
		go s.retry(ctx)
	}
	go func() {
		<-ctx.Done()
		s.Close()
	}()
	return err
}

func (s *Service) bind(ctx context.Context) error {
	conn, err := Listen(ctx, s.cfg.Port)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		if conn != nil {
			_ = conn.Close()
		}
		return net.ErrClosed
	}
	if err != nil {
		s.bindErr = err.Error()
		return err
	}
	s.conn, s.bindErr, s.listenedAt = conn, "", s.now()
	if u, ok := conn.LocalAddr().(*net.UDPAddr); ok {
		s.port = u.Port
	}
	done := make(chan struct{})
	s.loopDone = done
	go func() {
		defer close(done)
		readLoop(conn, s.table, s.links, s.heard)
	}()
	return nil
}

func (s *Service) retry(ctx context.Context) {
	t := time.NewTicker(retryBind)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.bind(ctx); err == nil {
				log.Printf("[agent] O-Zone discovery: now listening on UDP %d", s.Port())
				return
			}
		}
	}
}

// Close stops listening. It is safe to call more than once.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	conn, done := s.conn, s.loopDone
	s.conn = nil
	s.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
		<-done
	}
}

// Port is the UDP port listened on (the bound one once listening).
func (s *Service) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// Select chooses among the servers heard, by the configured selector.
func (s *Service) Select() Result { return s.table.Select(s.cfg.Selector) }

// Wait returns the one server the selector picks, once the service has been
// listening for at least minWindow (long enough to hear every server on the
// LAN at least twice) and that same server, at the same address, has been the
// only one heard for at least minWindow without a break. Any look that finds
// no single server, or a different one, starts that count again. So a server
// heard alone just after the others' entries lapsed (their broadcasts paused,
// say) is not chosen until the others have had a whole window to be heard
// again, and a server first heard late in the window is given a window of its
// own. Until then onTick, if set, is called regularly with the current result
// and whether the listening window has passed. It returns false when ctx ends.
func (s *Service) Wait(ctx context.Context, minWindow time.Duration, onTick func(r Result, windowDone bool)) (Candidate, bool) {
	tick := s.cfg.tick
	if tick <= 0 {
		tick = min(max(minWindow/10, 10*time.Millisecond), time.Second)
	}
	s.mu.Lock()
	s.waiting, s.window = true, minWindow
	s.mu.Unlock()
	t := time.NewTicker(tick)
	defer t.Stop()
	var held stableChoice
	for {
		now := s.now()
		s.mu.Lock()
		windowDone := s.conn != nil && now.Sub(s.listenedAt) >= minWindow
		s.mu.Unlock()
		r := s.Select()
		heldFor := held.hold(r, now)
		if r.Outcome == Selected && heldFor >= minWindow && windowDone {
			return r.Selected, true
		}
		if onTick != nil {
			onTick(r, windowDone)
		}
		select {
		case <-ctx.Done():
			return Candidate{}, false
		case <-t.C:
		}
	}
}

// stableChoice tracks how long one server, at one address, has been Select's
// choice without a break.
type stableChoice struct {
	held  bool
	key   Key
	ip    netip.Addr
	since time.Time
}

// hold records r, seen at now, and returns how long its choice has held
// unbroken. Unless r is Selected it forgets the choice and returns 0; a
// different server or address starts the count again from now.
func (c *stableChoice) hold(r Result, now time.Time) time.Duration {
	if r.Outcome != Selected {
		*c = stableChoice{}
		return 0
	}
	if !c.held || c.key != r.Selected.Key || c.ip != r.Selected.IP {
		*c = stableChoice{held: true, key: r.Selected.Key, ip: r.Selected.IP, since: now}
	}
	return now.Sub(c.since)
}

// SetSelected records the server chosen for this run, for reports.
func (s *Service) SetSelected(c Candidate) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.selected, s.selectedAt = &c, s.now().UTC()
}

// Moved reports the pinned server's new address once it has plainly moved;
// see Table.Moved.
func (s *Service) Moved(pin Key, current netip.Addr) (netip.Addr, bool) {
	return s.table.Moved(pin, current)
}

// IdentityLost reports that current now belongs to a different server; see
// Table.IdentityLost.
func (s *Service) IdentityLost(pin Key, current netip.Addr) bool {
	return s.table.IdentityLost(pin, current)
}

// heard runs on the reader after each announcement. In report-only mode it
// logs, once per change, a lone server that is not the configured host: the
// configuration may be pointing at the wrong machine.
func (s *Service) heard(Packet, netip.Addr) {
	if s.cfg.Auto {
		return
	}
	fixed, err := netip.ParseAddr(s.cfg.FixedHost)
	if err != nil {
		return
	}
	cs := usable(s.table.Candidates(), func(Candidate) bool { return true })
	if len(cs) != 1 || s.isConfigured(cs[0].IP, fixed.Unmap()) {
		return
	}
	c := cs[0]
	note := fmt.Sprintf("%s/%d/%d", c.IP, c.SID, c.Arena)
	s.mu.Lock()
	changed := s.loggedOther != note
	s.loggedOther = note
	s.mu.Unlock()
	if changed {
		log.Printf("[agent] LIB: the game server announcing itself on this network is %s (site %s, arena %d), not OZONE_WS_HOST=%s",
			c.IP, c.SIDRaw, c.Arena, s.cfg.FixedHost)
	}
}

// isConfigured reports whether an announced address is the configured host:
// the same address, or, for a loopback host, one of this machine's addresses.
func (s *Service) isConfigured(ip, fixed netip.Addr) bool {
	return ip == fixed || (fixed.IsLoopback() && s.links.isLocal(ip))
}

// state names what the service is doing, for reports and readiness.
func (s *Service) state(listening bool, bindErr string, selected bool) string {
	switch {
	case !listening && bindErr != "":
		return "bind_failed"
	case !s.cfg.Auto:
		return "report_only"
	case selected:
		return "selected"
	}
	switch s.Select().Outcome {
	case Ambiguous:
		return "ambiguous"
	case Conflict:
		return "conflict"
	default:
		return "waiting"
	}
}

// NotReadyReason says why auto mode has no server yet, or "" when it has one
// (or the service only reports). It names what was heard, so an operator can
// tell a selector that matches nothing, or announcements the agent will not
// trust, from a network that delivers nothing; only the last gets the advice
// about host networking and the firewall.
func (s *Service) NotReadyReason() string {
	s.mu.Lock()
	listening, bindErr, selected, port := s.conn != nil, s.bindErr, s.selected != nil, s.port
	windowDone := listening && s.waiting && s.now().Sub(s.listenedAt) >= s.window
	s.mu.Unlock()
	if !s.cfg.Auto || selected {
		return ""
	}
	if s.state(listening, bindErr, selected) == "bind_failed" {
		return fmt.Sprintf("cannot listen for the game server's announcement on UDP %d: %s", port, bindErr)
	}
	return s.waitDetail(port, windowDone)
}

// waitDetail says what auto mode has heard, against its selector, while it
// has no server. The text depends only on which servers are heard where, not
// on packet counts or times, so a caller can log it once per change.
func (s *Service) waitDetail(port int, windowDone bool) string {
	cs := s.table.Candidates()
	r := SelectFrom(cs, s.cfg.Selector)
	switch r.Outcome {
	case Selected:
		return "heard " + serverAt(r.Selected) + "; listening a little longer to be sure it is the only game server"
	case Ambiguous:
		var servers []string
		for _, c := range r.Candidates {
			servers = appendNew(servers, serverAt(c))
		}
		return "more than one game server announced (" + joinAnd(servers) + "); set OZONE_SID and OZONE_ARENA"
	case Conflict:
		var ips []string
		for _, c := range r.Candidates {
			ips = appendNew(ips, c.IP.String())
		}
		c := r.Candidates[0]
		return fmt.Sprintf("site %s arena %d is announced from %s; set OZONE_WS_HOST to the right one", c.SIDRaw, c.Arena, joinAnd(ips))
	}
	if len(cs) == 0 {
		if !windowDone {
			return fmt.Sprintf("waiting for the game server's announcement on UDP %d", port)
		}
		return fmt.Sprintf("no game server announcement heard on UDP %d — in Docker this needs host networking; on Windows allow inbound UDP %d; or set OZONE_WS_HOST to the game server's IP", port, port)
	}
	var heard, untrusted []string
	for _, c := range cs {
		if c.Usable {
			heard = appendNew(heard, serverAt(c))
		} else {
			untrusted = appendNew(untrusted, c.Reason)
		}
	}
	if len(heard) > 0 {
		return "heard " + joinAnd(heard) + ", but " + selectorText(s.cfg.Selector)
	}
	if len(untrusted) > maxListed {
		untrusted = append(untrusted[:maxListed:maxListed], fmt.Sprintf("%d more", len(untrusted)-maxListed))
	}
	return "heard only announcements sent from another address than they name (" + strings.Join(untrusted, "; ") + "); set OZONE_WS_HOST"
}

// serverAt names a server and the address it announced.
func serverAt(c Candidate) string {
	return "site " + c.SIDRaw + " arena " + strconv.Itoa(c.Arena) + " at " + c.IP.String()
}

// selectorText says that the configured selector matches none of the
// servers heard.
func selectorText(sel Selector) string {
	var set []string
	if sel.SID != nil {
		sid := sel.SIDRaw
		if sid == "" {
			sid = strconv.FormatUint(uint64(*sel.SID), 10)
		}
		set = append(set, "OZONE_SID="+sid)
	}
	if sel.Arena != nil {
		set = append(set, "OZONE_ARENA="+strconv.Itoa(*sel.Arena))
	}
	switch len(set) {
	case 0:
		return "none of them can be chosen"
	case 1:
		return set[0] + " selects no server"
	default:
		return strings.Join(set, " and ") + " select no server"
	}
}

// appendNew appends v to list unless it is already there.
func appendNew(list []string, v string) []string {
	if slices.Contains(list, v) {
		return list
	}
	return append(list, v)
}

// joinAnd joins items as prose ("a", "a and b", "a, b and c"), naming at most
// maxListed and counting the rest.
func joinAnd(items []string) string {
	if len(items) > maxListed {
		items = append(items[:maxListed:maxListed], fmt.Sprintf("%d more", len(items)-maxListed))
	}
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	default:
		return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
	}
}

// Report is the service's state for the agent's self-report.
func (s *Service) Report() map[string]any {
	s.mu.Lock()
	listening, bindErr, port := s.conn != nil, s.bindErr, s.port
	var sel *Candidate
	if s.selected != nil {
		c := *s.selected
		sel = &c
	}
	selAt := s.selectedAt
	s.mu.Unlock()

	cs := s.table.Candidates()
	n := s.table.Counters()
	mode := "static"
	if s.cfg.Auto {
		mode = "auto"
	}
	r := map[string]any{
		"mode":            mode,
		"port":            port,
		"listening":       listening,
		"error":           bindErr,
		"state":           s.state(listening, bindErr, sel != nil),
		"packets":         n.Packets,
		"rejected":        n.Rejected,
		"offlink":         n.Offlink,
		"sender_mismatch": n.SenderMismatch,
		"selected":        nil,
		"last":            nil,
	}
	if s.cfg.Auto {
		selector := map[string]any{"sid": nil, "arena": nil}
		if s.cfg.Selector.SID != nil {
			selector["sid"] = *s.cfg.Selector.SID
		}
		if s.cfg.Selector.Arena != nil {
			selector["arena"] = *s.cfg.Selector.Arena
		}
		r["selector"] = selector
	}
	if sel != nil {
		r["selected"] = map[string]any{
			"ip": sel.IP.String(), "sender": sel.Sender.String(), "sid": sel.SID, "sid_raw": sel.SIDRaw,
			"arena": sel.Arena, "selected_at": selAt.Format(time.RFC3339),
		}
	}

	// The latest announcement from the server this agent talks to: the one it
	// chose, or the configured host.
	var last *Candidate
	fixed, fixedErr := netip.ParseAddr(s.cfg.FixedHost)
	for i := range cs {
		c := &cs[i]
		if !c.Usable {
			continue
		}
		mine := false
		switch {
		case sel != nil:
			mine = c.Key == sel.Key && c.IP == sel.IP
		case !s.cfg.Auto && fixedErr == nil:
			mine = s.isConfigured(c.IP, fixed.Unmap())
		}
		if mine && (last == nil || c.LastSeen.After(last.LastSeen)) {
			last = c
		}
	}
	if !s.cfg.Auto && fixedErr == nil {
		r["configured_host_heard"] = last != nil
	}
	if last != nil {
		l := map[string]any{
			"ip": last.IP.String(), "sender": last.Sender.String(), "sid": last.SID, "arena": last.Arena,
			"ozone_time": nil, "ozone_time_text": "", "time_ok": last.Last.HasTime,
			"received_at": last.LastSeen.UTC().Format(time.RFC3339Nano),
		}
		if last.Last.HasTime {
			w := last.Last.Time
			l["ozone_time"] = map[string]any{
				"year": w.Year, "month": w.Month, "day": w.Day, "weekday": w.Weekday,
				"hour": w.Hour, "min": w.Min, "sec": w.Sec, "usec": w.Usec,
			}
			l["ozone_time_text"] = w.Text()
		}
		r["last"] = l
	}

	list := make([]map[string]any, 0, min(len(cs), maxReported))
	for _, c := range cs[:min(len(cs), maxReported)] {
		list = append(list, map[string]any{
			"ip": c.IP.String(), "sender": c.Sender.String(), "sid": c.SID, "sid_raw": c.SIDRaw, "arena": c.Arena,
			"packets": c.Packets, "last_heard_at": c.LastSeen.UTC().Format(time.RFC3339), "usable": c.Usable,
			"reason": c.Reason,
		})
	}
	r["candidates"] = list
	return r
}

// Status is Report, for the local admin overview.
func (s *Service) Status() map[string]any { return s.Report() }

// Describe renders a candidate for a log line.
func Describe(c Candidate) string {
	return c.IP.String() + " (site " + c.SIDRaw + ", arena " + strconv.Itoa(c.Arena) + ", sender " + c.Sender.String() + ")"
}
