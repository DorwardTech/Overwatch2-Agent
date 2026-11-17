package app

import (
	"context"
	"encoding/json"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// The game server link (agent 1.12.0). Telemetry reaches central only after a
// poll of the game server's WebSocket API works, and the self-report rides on
// the slow polls. So an agent that could not reach the game server sent
// central nothing at all: last_seen_at froze and the venue read "agent
// offline" while the agent was running and could have said what was wrong.
//
// ozoneLink tracks whether that connection works, for the self-report's and
// the overview's ozone_link block. While it does not, heartbeatLoop sends the
// self-report on its own at the idle rate, and fetches commands, so central
// sees an agent that is online but cannot reach its game server, and why. A
// heartbeat carries no server state and no packs: nothing was read, and an
// empty pack list would show central an empty rack.

// ozoneLinkState is the game server link's state. The zero value is
// "unreachable", so an App built without New reports the link as down.
type ozoneLinkState int

const (
	linkUnreachable ozoneLinkState = iota // a dial or a poll failed, or none has worked yet
	linkDiscovering                       // auto mode: no game server chosen yet
	linkConnected                         // the last poll worked
)

func (s ozoneLinkState) String() string {
	switch s {
	case linkConnected:
		return "connected"
	case linkDiscovering:
		return "discovering"
	default:
		return "unreachable"
	}
}

// maxLinkText bounds last_error. Central keeps no more than this, and the text
// is only ever a dial or poll error, or what discovery has heard: never data
// from the game server.
const maxLinkText = 200

// linkNotYet is last_error before the first attempt to reach a known address
// has an outcome.
const linkNotYet = "not connected yet"

// heartbeatKeyPrefix marks a heartbeat in the buffer: drainBuffer does not
// count one delivered as telemetry for /readyz. Central dedupes on the body's
// push_seq and ignores the key.
const heartbeatKeyPrefix = "hb-"

// ozoneLink tracks whether the WebSocket connection to the game server works.
// It is shaped like printServerPresence. The zero value reports the link as
// unreachable, with no start grace.
type ozoneLink struct {
	mu sync.Mutex

	state    ozoneLinkState
	since    time.Time // when the current state began
	lastOK   time.Time // the last poll that worked (zero = none this run)
	attempts int       // dials and polls failed since the last that worked
	lastErr  string    // why it is not connected (see maxLinkText)

	// No heartbeat goes until an attempt has failed or grace has passed since
	// started, so a normal first connection (or a normal discovery) is never
	// reported to central as a game server it cannot reach.
	started    time.Time
	grace      time.Duration
	hadFailure bool // an attempt has failed in this run

	// wake tells heartbeatLoop the link has just gone down, so the first
	// heartbeat goes at once rather than at the loop's next tick (see
	// lostLocked). Made on first use, so the zero value works.
	wake chan struct{}
}

// wakeC is the channel lostLocked signals.
func (l *ozoneLink) wakeC() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.wakeLocked()
}

// wakeLocked makes the wake channel on first use. l.mu held.
func (l *ozoneLink) wakeLocked() chan struct{} {
	if l.wake == nil {
		l.wake = make(chan struct{}, 1)
	}
	return l.wake
}

// lostLocked wakes heartbeatLoop: the link has just gone down (it was
// connected), or the first attempt of the run has failed. Waiting for the
// loop's next tick would leave central without word for up to a whole
// interval after the last push, on top of the poll that timed out: past the
// Agent offline alert's 30 seconds at the default timings. l.mu held.
func (l *ozoneLink) lostLocked() {
	select {
	case l.wakeLocked() <- struct{}{}:
	default: // already signalled and not yet taken
	}
}

// start sets the state before any attempt: discovering in auto mode, else
// unreachable with "not connected yet".
func (l *ozoneLink) start(discover bool, grace time.Duration, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.started, l.grace, l.since = now, grace, now
	if discover {
		l.state, l.lastErr = linkDiscovering, ""
	} else {
		l.state, l.lastErr = linkUnreachable, linkNotYet
	}
}

// set changes the state, and since with it when it changes. l.mu held.
func (l *ozoneLink) set(s ozoneLinkState, now time.Time) {
	if l.state != s {
		l.state, l.since = s, now
	}
}

// ok records a poll that worked: both replies of a collect came in.
func (l *ozoneLink) ok(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.set(linkConnected, now)
	l.lastOK, l.attempts, l.lastErr = now, 0, ""
}

// failed records a dial or a poll that failed.
func (l *ozoneLink) failed(err error, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == linkConnected || !l.hadFailure {
		l.lostLocked()
	}
	l.set(linkUnreachable, now)
	l.attempts++
	l.hadFailure = true
	l.lastErr = linkNotYet
	if err != nil {
		l.lastErr = shortLinkText(err.Error())
	}
}

// discovering records that auto mode has no game server yet, and why (what
// /readyz says: see ozonelib.Service.NotReadyReason).
func (l *ozoneLink) discovering(reason string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == linkConnected {
		l.lostLocked()
	}
	l.set(linkDiscovering, now)
	l.lastErr = shortLinkText(reason)
}

// stalled records that auto mode's discovery window has passed with no game
// server chosen (none heard, several, or none that matches). That counts as a
// failed attempt for the start grace, so the first heartbeat goes now rather
// than when the grace runs out: after a restart with the game server off,
// nothing is going to be chosen.
func (l *ozoneLink) stalled() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.hadFailure && l.state == linkDiscovering {
		l.hadFailure = true
		l.lostLocked()
	}
}

// graceLeft is how much of the start grace is still to run: 0 once it is over.
func (l *ozoneLink) graceLeft(now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return max(l.started.Add(l.grace).Sub(now), 0)
}

// chosen records that auto mode has chosen a game server: until the first
// dial or poll has an outcome, the link is "not connected yet".
func (l *ozoneLink) chosen(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == linkDiscovering {
		l.set(linkUnreachable, now)
		l.lastErr = linkNotYet
	}
}

// report is the self-report's (and the overview's) ozone_link block. Its
// times are absolute, so a heartbeat replayed from the buffer after an outage
// of central cannot pass for current.
func (l *ozoneLink) report() map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.reportLocked()
}

func (l *ozoneLink) reportLocked() map[string]any {
	r := map[string]any{
		"state":      l.state.String(),
		"since":      rfc3339OrNil(l.since),
		"last_ok_at": rfc3339OrNil(l.lastOK),
		"attempts":   l.attempts,
	}
	if l.state != linkConnected {
		r["last_error"] = l.lastErr
	}
	return r
}

// whileDown calls send with the link's report when the link is not connected
// and the start grace is over, and reports whether it did. It holds the
// tracker while send runs, so ok cannot land between the decision and the
// send: a heartbeat that found the link down is queued before the push of the
// poll that brought it back, never after it, and central never takes a stale
// "unreachable" for the latest word. send must not call the tracker.
func (l *ozoneLink) whileDown(now time.Time, send func(link map[string]any)) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state == linkConnected {
		return false
	}
	if !l.hadFailure && now.Sub(l.started) < l.grace {
		return false
	}
	send(l.reportLocked())
	return true
}

// shortLinkText is s on one line, cut to maxLinkText characters.
func shortLinkText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxLinkText {
		return s
	}
	return string([]rune(s)[:maxLinkText-1]) + "…"
}

// maxStartGrace caps the start grace with a fixed game server address, so
// that after a restart with the game server off (a dial that hangs, say) the
// first heartbeat still reaches central inside the Agent offline alert's 30
// seconds.
const maxStartGrace = 20 * time.Second

// startGrace is how long the agent waits at start before telling central the
// game server is unreachable, unless an attempt fails (or discovery stalls)
// first: two heartbeat intervals, at most maxStartGrace. In auto mode it is
// at least two discovery windows and a margin: a server first heard a little
// after the agent starts listening gets a window of its own (it is chosen at
// 11 to 17 s), and a normal discovery is never reported. A discovery that
// hears nothing, or nothing it may choose, stalls at the first window's end
// and reports at once (stalled); only a listener that cannot start waits
// the whole grace.
func (a *App) startGrace() time.Duration {
	g := min(2*a.heartbeatEvery(), maxStartGrace)
	if a.cfg.OzoneDiscover {
		g = max(g, 2*a.libWindow+2*time.Second)
	}
	return g
}

// heartbeatEvery is how often a heartbeat goes while the game server is
// unreachable: the idle poll interval (15 s by default), which stays under
// the Agent offline alert's default of 30 seconds, as the idle polls do.
func (a *App) heartbeatEvery() time.Duration {
	if a.cfg.IdlePollInterval > 0 {
		return a.cfg.IdlePollInterval
	}
	return 15 * time.Second
}

// commandsEvery is how often commands are fetched while the game server is
// unreachable: the slow poll interval, as when it answers.
func (a *App) commandsEvery() time.Duration {
	if a.cfg.SlowPollInterval > 0 {
		return a.cfg.SlowPollInterval
	}
	return time.Minute
}

// heartbeatLoop sends a heartbeat every heartbeatEvery while the game server
// is unreachable (see heartbeat), the first lossDelay after the link goes
// down (or the run's first attempt fails), and fetches commands every commandsEvery
// meanwhile: once heartbeats make the venue online, central lets operators
// queue commands, and only a slow poll would otherwise fetch them. A command
// that reaches the print server is deferred by the gate, which stays shut
// (nothing here reads or writes the server state); reboot_agent,
// agent_overview and cache_purge run as usual.
//
// Started by Run before anything waits for the game server, and stopped with
// it.
func (a *App) heartbeatLoop(ctx context.Context) {
	every := a.heartbeatEvery()
	tick := time.NewTicker(every)
	defer tick.Stop()
	wake := a.ozLink.wakeC()
	// The start grace may end between ticks: a heartbeat waiting on it goes
	// then, not at the next tick.
	graceOver := time.NewTimer(a.ozLink.graceLeft(time.Now()))
	defer graceOver.Stop()
	// A lost link gets lossDelay to come back before its first heartbeat:
	// the run redials at once, and a reconnect inside a second or two (one
	// missed reply, a reset socket) is not an outage worth a red venue.
	lost := time.NewTimer(time.Hour)
	lost.Stop()
	defer lost.Stop()
	var lastCommands time.Time
	sending := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-wake:
			// The link has just gone down: the first heartbeat goes after
			// lossDelay if it is still down, and the next a whole interval
			// after that.
			lost.Reset(a.lossDelay)
			continue
		case <-lost.C:
			tick.Reset(every)
		case <-graceOver.C:
			tick.Reset(every)
		}
		now := time.Now()
		if !a.heartbeat(now) {
			sending = false
			continue
		}
		if !sending {
			sending = true
			log.Printf("[agent] not connected to O-Zone: sending central a heartbeat every %s until it answers", shortDuration(a.heartbeatEvery()))
		}
		if a.cfg.CommandsEnabled && (lastCommands.IsZero() || now.Sub(lastCommands) >= a.commandsEvery()) {
			lastCommands = now
			go a.processCommands()
		}
	}
}

// heartbeat queues a heartbeat if the game server is unreachable and the
// start grace is over, and reports whether it did.
func (a *App) heartbeat(now time.Time) bool {
	return a.ozLink.whileDown(now, func(link map[string]any) {
		a.sendHeartbeat(now, link)
	})
}

// sendHeartbeat queues {push_seq, client_ts, agent} for central, through the
// buffer like any push but in place of any heartbeat still queued, and
// returns its push_seq. It carries the whole
// self-report (central replaces what it keeps of it on each push), and no
// server_state or packs keys at all. It reads nothing from the game server
// and touches none of the server-state readings the print-server gate and
// discovery go by, nor pushDue's throttle.
func (a *App) sendHeartbeat(now time.Time, link map[string]any) int64 {
	seq := a.nextSeq()
	data, err := json.Marshal(map[string]any{
		"push_seq":  seq,
		"client_ts": now.UTC().Format(time.RFC3339),
		"agent":     a.selfReportWith(link),
	})
	if err != nil {
		log.Printf("[agent] heartbeat not sent: %v", err)
		return seq
	}
	// Only the latest heartbeat means anything to central, so it replaces any
	// still queued: a long outage of central leaves one, not one per tick
	// pushing the telemetry ahead of it out of the buffer.
	a.buf.PushReplacing(heartbeatKeyPrefix, heartbeatKeyPrefix+strconv.FormatInt(seq, 10), data)
	a.signalDelivery()
	return seq
}

// isHeartbeat reports whether a buffered entry is a heartbeat.
func isHeartbeat(key string) bool { return strings.HasPrefix(key, heartbeatKeyPrefix) }
