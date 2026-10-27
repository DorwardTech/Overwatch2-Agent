package app

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"overwatch/agent/internal/ozone"
)

// frameSummaryEvery is the longest the agent goes between summaries of the
// messages it discarded from the game server's WebSocket API, while there is
// anything to sum up. A constant, not a setting: the counts themselves are in
// every self-report, and the log only needs to say that they are there.
const frameSummaryEvery = time.Hour

// unsettledWarnEvery is the shortest time between two warnings that polls lost
// a message mid-poll.
const unsettledWarnEvery = 5 * time.Minute

// frameExplanation is logged once per process, the first time a message is
// discarded for answering another client's request or for arriving between
// this agent's requests. O-Zone sends every client's replies to every
// connection, so with another program using the game server's API this
// happens on every poll, all day. That is normal, and logging it once a minute
// (as 1.6.5 to 1.7.1 did) only buried the lines that matter.
const frameExplanation = "[agent] O-Zone: discarding messages that answer other clients' requests or arrived between this agent's requests — normal when other programs use the game server; counted in the self-report (agent.frames) and summarised every hour"

// frameCounts is one reading of an ozone.Client's discard counters: frames
// that answered another command, frames that arrived between requests, and
// frames the reader dropped with its queue full.
type frameCounts struct{ Foreign, Stale, Overflow int64 }

func (c frameCounts) add(d frameCounts) frameCounts {
	return frameCounts{c.Foreign + d.Foreign, c.Stale + d.Stale, c.Overflow + d.Overflow}
}

func (c frameCounts) zero() bool { return c == frameCounts{} }

// frameCountsOf reads a connection's counters. They are atomics, so this is
// cheap enough for every poll.
func frameCountsOf(c *ozone.Client) frameCounts {
	return frameCounts{Foreign: c.ForeignFrames(), Stale: c.StaleFrames(), Overflow: c.OverflowFrames()}
}

// grown is how far a counter rose from was to now. A counter lower than when
// last read belongs to a new connection, which counts from zero, so all of it
// is new. Never negative.
func grown(now, was int64) int64 {
	switch {
	case now < 0:
		return 0
	case now < was:
		return now
	}
	return now - was
}

// frameTally keeps the discard counts for the life of the process, across
// reconnects (each connection's counters start from zero), and decides what to
// log about them. The zero value is ready to use. The poll loop calls
// connected, sample, lostPoll and lines; report may be called from any
// goroutine (the admin API's overview runs on its own).
type frameTally struct {
	mu sync.Mutex
	// total is every frame discarded since the process started, and pending
	// those not yet logged about (see lines).
	total   frameCounts
	pending frameCounts
	// seen is this connection's counters at the last sample.
	seen frameCounts
	// unsettledLost counts polls left unsettled by a frame lost unread during
	// the poll, since the process started; warnedLost is its value at the last
	// warning.
	unsettledLost int64
	warnedLost    int64
	// explained is set once the explanatory line has been logged.
	explained bool
	// windowStart is when the explanation or the last summary was logged: the
	// start of the window the next summary covers. Zero until then.
	windowStart time.Time
	// unsettledWarned is when the last warning was logged (zero: never).
	unsettledWarned time.Time
}

// connected starts counting a new connection, whose counters start at zero.
func (t *frameTally) connected() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.seen = frameCounts{}
}

// sample adds what the connection's counters c have counted since the last
// sample. A counter that went backwards is taken for a new connection's.
func (t *frameTally) sample(c frameCounts) {
	t.mu.Lock()
	defer t.mu.Unlock()
	d := frameCounts{
		Foreign:  grown(c.Foreign, t.seen.Foreign),
		Stale:    grown(c.Stale, t.seen.Stale),
		Overflow: grown(c.Overflow, t.seen.Overflow),
	}
	t.seen = c
	t.total = t.total.add(d)
	t.pending = t.pending.add(d)
}

// lostPoll records a poll left unsettled because a frame was lost unread
// during it. Of all the discards, only that one costs anything: the lost frame
// could have been the server state, so the poll cannot confirm its reading and
// the print server stays off-limits until a clean poll. A frame dropped with
// the queue full between polls costs nothing, since the next poll starts
// afresh, so overflow alone never warns.
func (t *frameTally) lostPoll() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.unsettledLost++
}

// lines returns what to log now, and logs nothing itself:
//
//   - the explanation, once per process, the first time a frame is discarded
//     for answering another client's request or arriving between requests;
//   - after that, a summary at most once per every, and only when something
//     was discarded since the last line;
//   - a warning, at most once per unsettledWarnEvery, when more polls have
//     been left unsettled by a frame lost mid-poll.
func (t *frameTally) lines(now time.Time, every time.Duration) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	switch {
	case !t.explained && (t.total.Foreign > 0 || t.total.Stale > 0):
		t.explained = true
		out = append(out, frameExplanation)
		// The explanation stands for what was discarded so far. Frames
		// dropped with the queue full are not in it, so they wait for the
		// summary.
		t.pending.Foreign, t.pending.Stale = 0, 0
		t.windowStart = now
	case !t.pending.zero() && (t.windowStart.IsZero() || now.Sub(t.windowStart) >= every):
		// windowStart is zero here only when nothing but queue-full drops has
		// been counted: those get a line at once.
		p, s := t.pending, t.total
		out = append(out, fmt.Sprintf("[agent] O-Zone: in %s discarded %d answering another command and %d that arrived between requests; dropped %d with the queue full (since start: %d answering another command, %d between requests, %d dropped)",
			summarySpan(t.windowStart, now, every), p.Foreign, p.Stale, p.Overflow, s.Foreign, s.Stale, s.Overflow))
		t.pending = frameCounts{}
		t.windowStart = now
	}
	if t.unsettledLost > t.warnedLost && (t.unsettledWarned.IsZero() || now.Sub(t.unsettledWarned) >= unsettledWarnEvery) {
		out = append(out, fmt.Sprintf("[agent] O-Zone: WARNING: %d poll(s) lost a message unread mid-poll (%d since start). Such a poll cannot confirm the server state it read, so the print server stays off-limits until a clean poll. More traffic is arriving than the agent keeps; check what else is using the game server",
			t.unsettledLost-t.warnedLost, t.unsettledLost))
		t.warnedLost = t.unsettledLost
		t.unsettledWarned = now
	}
	return out
}

// summarySpan names the window a summary covers: from the last line to now.
// That is about an hour while slow polls run, but far longer when none did
// (the game server off overnight, say), and then the counts include what was
// discarded before the gap; a window that has not started yet (only queue-full
// drops counted) is the run so far.
func summarySpan(windowStart, now time.Time, every time.Duration) string {
	if windowStart.IsZero() {
		return "this run so far"
	}
	if d := now.Sub(windowStart).Round(time.Minute); d > every+every/2 {
		return "the last " + strings.TrimSuffix(d.String(), "0s")
	}
	return "the last hour"
}

// report returns the counts since the process started, for the self-report
// (agent.frames) and the admin overview (frames).
func (t *frameTally) report() map[string]any {
	t.mu.Lock()
	defer t.mu.Unlock()
	return map[string]any{
		"foreign":        t.total.Foreign,
		"stale":          t.total.Stale,
		"overflow":       t.total.Overflow,
		"unsettled_lost": t.unsettledLost,
	}
}
