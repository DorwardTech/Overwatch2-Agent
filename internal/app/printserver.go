package app

import (
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// Some game servers have no print server at all: nothing answers on the
// results port. Without telling that apart from a print server that is
// briefly down, every finished game was tried about ten times over two hours
// and then abandoned, each try logging a timeout, and the cache refresh waited
// out a dial timeout every minute between games. printServerPresence notices
// when the print server stops answering, stops the automatic work from
// dialling it, and checks again now and then, always between games.
//
// It is fed from the one place the agent dials the print server
// (resultsSession.connect), after the print-server gate has passed, so a check
// can never happen during a game.

// printServerRecheckBase is the first wait before a print server treated as
// absent is checked again. Each check that fails doubles the wait, up to
// PRINT_SERVER_RECHECK_MAX.
const printServerRecheckBase = 60 * time.Second

// printServerAbsentSpan is the shortest time over which the failed dials must
// be spread before the print server is treated as absent. A game server and
// the agent coming back from a venue power cut together can have the
// WebSocket API up minutes before the print server; three quick failures then
// must not stop the results of the first games of the day. The alternative,
// trusting a non-empty local cache as proof that a print server exists, was
// not chosen: the cache can be filled from central's failover store, and a
// venue running results without the cache has none.
const printServerAbsentSpan = 10 * time.Minute

// maxPendingWhileAbsent bounds the games kept waiting for a print server that
// answered earlier in this run and has stopped. The oldest queued are
// dropped first (counted in the report); backfill recovers them later.
const maxPendingWhileAbsent = 50

// errPrintServerAbsent is returned instead of dialling a print server that is
// being treated as absent and is not due a check. It is neither a fetch
// failure (no attempt is burnt) nor "not now" (the work is not retried on the
// next poll): the work waits until the print server answers again.
var errPrintServerAbsent = errors.New("print server not answering: treated as absent until its next check")

type printServerState int

const (
	psUnknown printServerState = iota
	psPresent
	psAbsent
	psDisabled
)

func (s printServerState) String() string {
	switch s {
	case psPresent:
		return "present"
	case psAbsent:
		return "absent"
	case psDisabled:
		return "disabled"
	default:
		return "unknown"
	}
}

// printServerPresence tracks whether the print server answers. The zero value
// has detection off (absentAfter 0): it reports, and never blocks a dial.
type printServerPresence struct {
	mu sync.Mutex

	// absentAfter is PRINT_SERVER_ABSENT_AFTER: consecutive failed dials that
	// make the print server absent (0 = never). recheckMax caps the wait
	// between checks. minSpan is printServerAbsentSpan (tests shorten it).
	absentAfter int
	recheckMax  time.Duration
	minSpan     time.Duration

	state          printServerState
	failures       int       // consecutive failed dials
	firstFailureAt time.Time // the first of them
	since          time.Time // when the current state began
	lastOK         time.Time // the last dial that connected (zero = none this run)
	lastErr        string
	backoff        time.Duration // the current wait between checks while absent
	nextCheck      time.Time     // automatic work may dial again from then
	everReached    bool          // the print server has answered in this run
	announced      bool          // "reached" has been logged in this run
	dropped        int           // queued games dropped while absent
}

// configure sets the tracker up. disabled is for an agent with nothing that
// uses the print server on (results, cache and message bus all off): its
// state stays "disabled", whatever an operator's command finds.
func (p *printServerPresence) configure(absentAfter int, recheckMax time.Duration, disabled bool, now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.absentAfter = max(absentAfter, 0)
	p.recheckMax = recheckMax
	p.minSpan = printServerAbsentSpan
	p.since = now
	if disabled {
		p.state = psDisabled
	}
}

// maxWait is the cap on the wait between checks.
func (p *printServerPresence) maxWait() time.Duration {
	if p.recheckMax < printServerRecheckBase {
		return printServerRecheckBase
	}
	return p.recheckMax
}

// allow reports whether a dial may go ahead. Only automatic work is held back,
// and only while the print server is absent and its next check is not due. A
// check that is due is claimed, so the work racing for it makes one dial, not
// several; the dial's outcome then sets the next one (see failed).
//
// It must be asked after the print-server gate, never instead of it.
func (p *printServerPresence) allow(now time.Time, manual bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if manual || p.state != psAbsent {
		return nil
	}
	if now.Before(p.nextCheck) {
		return errPrintServerAbsent
	}
	p.nextCheck = now.Add(p.backoff)
	return nil
}

// failed counts a dial that did not connect, made while the gate was open. It
// reports whether the print server is now treated as absent (it was already,
// or this failure made it so), in which case automatic work stands down
// instead of counting the failure against a game.
func (p *printServerPresence) failed(addr string, err error, now time.Time, manual bool) (absent bool) {
	p.mu.Lock()
	if p.state == psDisabled {
		p.mu.Unlock()
		return false
	}
	if manual && p.state != psAbsent {
		// An operator's failed command does not count towards absent. One
		// never reached in this run would then get no queued games, and with
		// the cache off nothing automatic would ever check it again.
		p.lastErr = err.Error()
		p.mu.Unlock()
		return false
	}
	if p.failures == 0 {
		p.firstFailureAt = now
	}
	p.failures++
	p.lastErr = err.Error()
	switch {
	case p.state == psAbsent:
		// A check that failed: wait longer before the next, without a word.
		// An operator's command is a check of its own and moves nothing.
		if !manual {
			p.backoff = min(p.backoff*2, p.maxWait())
			p.nextCheck = now.Add(p.backoff)
		}
		p.mu.Unlock()
		return true
	case p.absentAfter == 0, p.failures < p.absentAfter, now.Sub(p.firstFailureAt) < p.minSpan:
		p.mu.Unlock()
		return false
	}
	p.state, p.since = psAbsent, now
	p.backoff = min(printServerRecheckBase, p.maxWait())
	p.nextCheck = now.Add(p.backoff)
	line := fmt.Sprintf("[agent] print server: %s not answering after %d attempts over %s (%s) — treating it as absent: results and cache refresh paused, checking again between games (next in %s, at most every %s)",
		addr, p.failures, shortDuration(now.Sub(p.firstFailureAt)), p.lastErr, shortDuration(p.backoff), shortDuration(p.maxWait()))
	p.mu.Unlock()
	log.Print(line)
	return true
}

// reached records a dial that connected. It reports whether the print server
// had been treated as absent, so the caller can resume the work that waited.
// waiting counts the games waiting to be fetched, for the log; it is called
// outside the tracker's lock.
func (p *printServerPresence) reached(addr string, now time.Time, waiting func() int) bool {
	p.mu.Lock()
	wasReached := p.everReached
	p.failures, p.firstFailureAt, p.lastErr = 0, time.Time{}, ""
	p.lastOK, p.everReached = now, true
	var line string
	switch p.state {
	case psUnknown:
		p.state, p.since = psPresent, now
		if !p.announced {
			line = fmt.Sprintf("[agent] print server: reached at %s", addr)
		}
		p.announced = true
	case psAbsent:
		absentFor := now.Sub(p.since)
		p.state, p.since = psPresent, now
		p.backoff, p.nextCheck = 0, time.Time{}
		p.announced = true
		p.mu.Unlock()
		n := 0
		if waiting != nil {
			n = waiting()
		}
		again := "answering again"
		if !wasReached {
			again = "answering for the first time in this run"
		}
		log.Printf("[agent] print server: %s %s after %s — resuming (%d game(s) waiting)", addr, again, shortDuration(absentFor), n)
		return true
	}
	p.mu.Unlock()
	if line != "" {
		log.Print(line)
	}
	return false
}

// gameFinished makes the next check due now, while the print server is absent
// but has answered in this run: a game just ended, so the drain after it
// (which waits out GAME_FINISH_DELAY and the gate) checks at once, and a print
// server back from a short outage serves that game without waiting out the
// backoff. A print server never reached in this run is not checked after
// every game.
func (p *printServerPresence) gameFinished(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.state == psAbsent && p.everReached {
		p.nextCheck = now
	}
}

// queueing reports what queueFetch needs to know: whether the print server is
// absent, and whether it has answered in this run.
func (p *printServerPresence) queueing() (absent, everReached bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state == psAbsent, p.everReached
}

// addDropped counts queued games dropped while the print server was absent.
func (p *printServerPresence) addDropped(n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.dropped += n
}

// report is the self-report's (and the overview's) print_server block.
func (p *printServerPresence) report(now time.Time) map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	next := 0
	if p.state == psAbsent && now.Before(p.nextCheck) {
		next = int((p.nextCheck.Sub(now) + time.Second - 1) / time.Second)
	}
	r := map[string]any{
		"state":        p.state.String(),
		"since":        rfc3339OrNil(p.since),
		"last_ok_at":   rfc3339OrNil(p.lastOK),
		"failures":     p.failures,
		"next_check_s": next,
		"dropped":      p.dropped,
	}
	if p.state == psAbsent {
		r["last_error"] = p.lastErr
	}
	return r
}

func rfc3339OrNil(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// shortDuration writes d for a log line: whole seconds, without the zero
// units Duration.String leaves on ("15m", not "15m0s"; "1h", not "1h0m0s").
func shortDuration(d time.Duration) string {
	if d >= time.Minute {
		d = d.Round(time.Minute)
	} else {
		d = d.Round(time.Second)
	}
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
