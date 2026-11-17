package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"overwatch/agent/internal/results"
)

// printLink is the agent's one connection to the game server's print server
// (agent 1.11.0).
//
// Before it, every piece of print-server work dialled a connection of its own
// and closed it when done: each finished game, the cache refresh every minute,
// an operator's command, each check of a print server treated as absent. The
// game server shows every one of those connecting and disconnecting.
//
// Now one connection is opened between games and kept open, through games
// too: a connection that sends nothing asks nothing of the game server, and a
// print server's own client stays connected the same way. What stays off
// limits during a game is everything written to it: every request and every
// acknowledgement passes the print-server gate at the moment it is sent, and
// a connection that drops is only dialled again between games.
//
// Acknowledgements ({"success": true}) go only for a whole game the game
// server sent unasked, which is the data its print-server API has the client
// acknowledge (agent 1.12.1). Never for a list or a game the agent asked for:
// no other client does that, and before 1.12.1 the agent did it after every
// cache refresh, once a minute between games. Never while a request waits for
// its reply, and the next request waits linkAckSettle after one, so anything
// the game server sends back to an acknowledgement promptly arrives with no
// request waiting and is not taken as a reply (an error is logged once per
// connection). Acknowledgements are best-effort: one owed is sent when the
// gate is open and no request is waiting, by the next poll's upkeep or before
// the last piece of work closes the connection (release), and is lost if the
// connection ends first, whatever ends it.
//
// One reader takes every frame the game server sends and sorts it by what it
// is, not by when it came:
//   - the reply to the request waiting for one: a game list for "list"; for
//     "all", the game it names when that is the game asked for (or one that
//     names none), or the game server's refusal;
//   - a whole game nobody asked for: the game server sends each game's data
//     unasked as the game ends. When it carries the players and events a
//     reply to "all" does, it is cached and sent to central like a fetched
//     game, and acknowledged once the gate is open;
//   - part of a game nobody asked for (game details without the players or
//     the events): taken only as word that the game has ended, never as its
//     results, which a partial payload would stand in for for good (cached,
//     marked fetched, never asked for again). The game is asked for whole
//     between games, after GAME_FINISH_DELAY. So is a game whose data never
//     comes unasked, as before;
//   - print commands and the auto-print setting, which are for a printer: the
//     agent prints nothing, so they are noted and left;
//   - anything else, noted once per connection by its keys alone (game data
//     carries players' names).
//
// Sorting by content also means a reply that arrives after its request gave
// up is never read as the answer to the next one: a game reply is only ever
// taken for the game it names, and a frame the agent does not recognise is
// never taken as a reply.
//
// PRINT_SERVER_KEEP_OPEN=false closes the connection when each piece of work
// ends, as before 1.11.0.
type printLink struct {
	app *App

	dialMu sync.Mutex // one dial at a time
	reqMu  sync.Mutex // one request waiting for a reply at a time; acknowledgements wait for it too
	wmu    sync.Mutex // one write at a time

	mu          sync.Mutex
	conn        *results.Client
	gen         uint64        // counts connections; a reader of an older one is ignored
	done        chan struct{} // closed when the current connection ends
	endErr      error         // why the last connection ended
	waiter      *linkWaiter   // the request waiting for its reply, if any
	users       int           // sessions using the connection (see release)
	connectedAt time.Time
	addr        string
	acksOwed    int                    // game data taken on this connection, not acknowledged yet
	noted       map[linkFrameKind]bool // frame kinds logged on this connection
	// Between games, maintain dials again from retryAt; the wait doubles after
	// a dial that failed or a connection that did not last.
	retryAt   time.Time
	retryWait time.Duration
	// For the report.
	opened  int // connections opened in this run
	pushed  int // whole games taken as sent unasked
	partial int // parts of games sent unasked, taken as word a game ended
	ignored int // print commands and other frames left alone

	maintaining atomic.Bool
	pushing     chan struct{} // bounds the games being handled at once
	// replyWait is how long a request waits for its reply (tests shorten it).
	replyWait time.Duration
	// ackSettle is linkAckSettle (tests may change it).
	ackSettle time.Duration
}

const (
	// linkReplyWait is how long a request waits for its reply.
	linkReplyWait = 20 * time.Second
	// linkRetryBase and linkRetryMax bound the wait before maintain dials
	// again after a failed dial or a connection that ended early.
	linkRetryBase = 5 * time.Second
	linkRetryMax  = 5 * time.Minute
	// linkSettled is how long a connection must last for the wait to start
	// again from linkRetryBase.
	linkSettled = 10 * time.Minute
	// maxPushedAtOnce bounds the games sent unasked being handled at once;
	// one beyond it is left to be asked for between games.
	maxPushedAtOnce = 4
	// linkAckSettle is how long the next request waits after an
	// acknowledgement (see the type's comment).
	linkAckSettle = 500 * time.Millisecond
)

// errLinkClosed is a request made when the connection has just closed.
var errLinkClosed = errors.New("print server connection closed")

type linkFrameKind int

const (
	frameOther     linkFrameKind = iota
	frameGame                    // a whole game: a "game" section, players and events
	framePartGame                // a "game" section without the players or the events
	frameList                    // a game list
	frameError                   // the game server's refusal: {"error": ...}
	frameAck                     // {"success": ...}
	framePrint                   // a print command for a printer
	frameAutoprint               // the auto-print setting
	frameBanner                  // the event types or texts sent on connect
)

func (k linkFrameKind) String() string {
	switch k {
	case frameGame:
		return "game data"
	case framePartGame:
		return "part of a game"
	case frameList:
		return "game list"
	case frameError:
		return "error"
	case frameAck:
		return "acknowledgement"
	case framePrint:
		return "print command"
	case frameAutoprint:
		return "auto-print setting"
	case frameBanner:
		return "event types or texts"
	default:
		return "unrecognised message"
	}
}

// linkFrame is what classifyFrame makes of a frame.
type linkFrame struct {
	kind linkFrameKind
	game int      // frameGame, framePartGame: the game it names (0 = none); framePrint: the game to print
	keys []string // frameOther, framePartGame: its top-level keys, for the log
	// noEvents marks a frameGame whose events entry is null: whole as the
	// reply to a request, but part of a game when sent unasked (see take).
	noEvents bool
}

// classifyFrame sorts a frame by its content. A "game" section makes it game
// data, the same rule payloadGameNumber applies to a reply; it is a whole game
// only with the players and the events too, which every reply to "all"
// carries (they are what central and the scoresheet program read). An events
// entry that is null is marked (noEvents): a reply asked for after the finish
// delay is whole with it, but a game sent unasked as it ends may not have its
// events yet, so take treats that as part of a game.
func classifyFrame(raw []byte) linkFrame {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil || top == nil {
		return linkFrame{kind: frameOther}
	}
	if g, ok := top["game"]; ok {
		var game map[string]any
		if json.Unmarshal(g, &game) == nil && game != nil {
			n := toInt(game["gamenum"])
			if events, ok := top["events"]; ok && present(top["players"]) {
				return linkFrame{kind: frameGame, game: n, noEvents: !present(events), keys: topKeys(top)}
			}
			return linkFrame{kind: framePartGame, game: n, keys: topKeys(top)}
		}
	}
	has := func(k string) bool { _, ok := top[k]; return ok }
	switch {
	case has("gamelist"):
		return linkFrame{kind: frameList}
	case has("error"):
		return linkFrame{kind: frameError}
	case has("command"):
		var n any
		_ = json.Unmarshal(top["gamenumber"], &n)
		return linkFrame{kind: framePrint, game: toInt(n)}
	case has("autoprint"):
		return linkFrame{kind: frameAutoprint}
	case has("success"):
		return linkFrame{kind: frameAck}
	case has("texts"), has("event_types"):
		return linkFrame{kind: frameBanner}
	}
	return linkFrame{kind: frameOther, keys: topKeys(top)}
}

// present reports whether a JSON value is there and not null.
func present(v json.RawMessage) bool {
	return len(v) > 0 && strings.TrimSpace(string(v)) != "null"
}

// topKeys is a frame's top-level keys, sorted, at most eight, for the log
// (never a value: game data carries players' names).
func topKeys(top map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(top))
	for k := range top {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) > 8 {
		keys = append(keys[:8], "…")
	}
	return keys
}

// linkWaiter is a request waiting for its reply.
type linkWaiter struct {
	game  int // the game asked for; 0 for the game list
	reply chan []byte
}

// wants reports whether f is the reply: its own reply and nothing else, so a
// request ends only on that reply (or a timeout, which closes the connection)
// and no reply is ever left owing for the next request to take. That is the
// list for the list; for a game, the whole game naming that game; and the game
// server's refusal for either (which it sends only as a reply). Never game
// data for another game, part of a game, or a frame the agent does not
// recognise: what the game server sends unasked as a game ends, handled as
// such, with the request still waiting. Never game data naming no game
// either: it could be any game, and filed under the wrong number it would
// never be put right.
func (w *linkWaiter) wants(f linkFrame) bool {
	switch f.kind {
	case frameError:
		return true
	case frameList:
		return w.game == 0
	case frameGame:
		return w.game != 0 && f.game == w.game
	}
	return false
}

func newPrintLink(a *App) *printLink {
	return &printLink{app: a, pushing: make(chan struct{}, maxPushedAtOnce), replyWait: linkReplyWait, ackSettle: linkAckSettle}
}

// connected reports whether a connection is open.
func (l *printLink) connected() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.conn != nil
}

// keepOpen reports whether the connection is held between pieces of work.
func (l *printLink) keepOpen() bool { return l.app.cfg.PrintServerKeepOpen }

// wanted reports whether anything automatic uses the print server, so a
// connection is worth holding: game results, the cache, or the message bus,
// whose finish fetches a game whatever ENABLE_GAME_RESULTS says (the same
// three the presence tracker counts; see New).
func (l *printLink) wanted() bool {
	return l.app.cfg.ResultsEnabled || l.app.store != nil || l.app.bus != nil
}

// ensure makes sure a connection is open, dialling one if not. It is the only
// place the agent dials the print server, and it dials only with the gate
// open, and, for automatic work, only when a print server treated as absent is
// due a check (manual: an operator's request; see resultsSession).
func (l *printLink) ensure(manual bool) error {
	l.dialMu.Lock()
	defer l.dialMu.Unlock()
	if l.connected() {
		return nil
	}
	a := l.app
	// The address is read once, before the gate: no address yet (auto mode
	// still choosing) is "not now", like a stale reading.
	host := a.ozoneAddr()
	if host == "" {
		return errLinkStale
	}
	if err := a.printServerGate(); err != nil {
		return err
	}
	// After the gate, never instead of it: a print server treated as absent
	// is left alone by automatic work until its next check is due, and the
	// check itself is a dial like this one, so it too only ever happens
	// between games.
	if err := a.ps.allow(time.Now(), manual); err != nil {
		return err
	}
	port := a.cfg.OzoneResultsPort
	addr := net.JoinHostPort(host, port)
	// Dialling is print-server work; see nextPollInterval.
	a.printServerBusy.Add(1)
	defer a.printServerBusy.Add(-1)
	rc, err := a.dial(host, port, 5*time.Second)
	if err != nil {
		l.backOff(time.Now())
		// Only a dial that failed with the gate still open counts against
		// the print server: one that failed while it closed under it (the
		// link dropped, a game started) says nothing about it. Automatic work
		// then stands down if the print server is now treated as absent; an
		// operator is told what the dial found.
		if gateErr := a.printServerGate(); gateErr != nil {
			// A check of a print server treated as absent, cut short by the
			// gate, is "not now": the work is put off (the gate error is
			// deferrable), so no attempt is used up and nothing is logged.
			// Only while absent, so the other states behave as before.
			if absent, _ := a.ps.queueing(); absent {
				return fmt.Errorf("%w (%w)", gateErr, err)
			}
			return err
		}
		if a.ps.failed(addr, err, time.Now(), manual) && !manual {
			return fmt.Errorf("%w (%v)", errPrintServerAbsent, err)
		}
		return err
	}
	if a.ps.reached(addr, time.Now(), a.pendingCount) {
		a.onPrintServerBack()
	}
	// O-Zone pushes a fixed handshake (texts + event_types) on connect; it is
	// consumed before the reader starts, as before.
	rc.Drain(a.cfg.ResultsHandshake, 5*time.Second)
	l.attach(rc, addr)
	return nil
}

// attach makes rc the connection and starts its reader.
func (l *printLink) attach(rc *results.Client, addr string) {
	l.mu.Lock()
	l.gen++
	gen := l.gen
	l.conn, l.done, l.endErr = rc, make(chan struct{}), nil
	l.connectedAt, l.addr = time.Now(), addr
	l.acksOwed, l.noted = 0, nil
	l.opened++
	l.mu.Unlock()
	if l.keepOpen() {
		log.Printf("[agent] print server: connected to %s; keeping the connection open (nothing is sent to it during a game)", addr)
	}
	go l.read(rc, gen)
}

// read takes every frame the connection brings until it ends.
func (l *printLink) read(rc *results.Client, gen uint64) {
	for {
		raw, err := rc.Next()
		if err != nil {
			l.end(gen, err, false)
			return
		}
		l.take(gen, raw)
	}
}

// end closes connection gen, if it is still the current one. asked marks a
// close the agent chose (work done with PRINT_SERVER_KEEP_OPEN off, or the
// agent stopping); anything else delays the next dial (see backOff).
func (l *printLink) end(gen uint64, why error, asked bool) {
	l.mu.Lock()
	if gen != l.gen || l.conn == nil {
		l.mu.Unlock()
		return
	}
	rc := l.conn
	lasted := time.Since(l.connectedAt)
	addr := l.addr
	l.conn, l.waiter, l.acksOwed, l.endErr = nil, nil, 0, why
	close(l.done)
	if !asked {
		if lasted >= linkSettled {
			l.retryWait = 0
		}
		l.backOffLocked(time.Now())
	}
	l.mu.Unlock()
	rc.Close()
	if l.keepOpen() && !asked {
		log.Printf("[agent] print server: connection to %s ended after %s (%v); connecting again between games", addr, shortDuration(lasted), why)
	}
}

// backOff delays maintain's next dial (see printLink.retryAt).
func (l *printLink) backOff(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.backOffLocked(now)
}

func (l *printLink) backOffLocked(now time.Time) {
	if l.retryWait < linkRetryBase {
		l.retryWait = linkRetryBase
	} else {
		l.retryWait = min(l.retryWait*2, linkRetryMax)
	}
	l.retryAt = now.Add(l.retryWait)
}

// take handles one frame from connection gen.
func (l *printLink) take(gen uint64, raw []byte) {
	f := classifyFrame(raw)
	l.mu.Lock()
	if gen != l.gen || l.conn == nil {
		l.mu.Unlock()
		return
	}
	if w := l.waiter; w != nil && w.wants(f) {
		l.waiter = nil
		l.mu.Unlock()
		w.reply <- raw // buffered; the waiter may have given up
		return
	}
	// Sent unasked with its events null: they may not be written yet, so it
	// is part of a game, asked for whole after the finish delay.
	if f.kind == frameGame && f.noEvents {
		f.kind = framePartGame
	}
	if f.kind == frameGame && f.game > 0 {
		l.pushed++
		l.mu.Unlock()
		l.takePushed(gen, f.game, raw)
		return
	}
	if f.kind == framePartGame && f.game > 0 {
		l.partial++
		first := !l.noted[framePartGame]
		if l.noted == nil {
			l.noted = map[linkFrameKind]bool{}
		}
		l.noted[framePartGame] = true
		l.mu.Unlock()
		if first {
			log.Printf("[agent] print server: the game server sent part of game #%d (keys: %s); taken as word that the game has ended, and the whole game is asked for between games (said once per connection)", f.game, strings.Join(f.keys, ", "))
		}
		l.app.gameEndedUnasked(f.game)
		return
	}
	if f.kind != frameBanner && f.kind != frameAck {
		l.ignored++
	}
	first := !l.noted[f.kind]
	if l.noted == nil {
		l.noted = map[linkFrameKind]bool{}
	}
	l.noted[f.kind] = true
	l.mu.Unlock()
	if !first {
		return
	}
	switch f.kind {
	case framePrint:
		log.Printf("[agent] print server: the game server asked for game #%d to be printed; the agent does not print, so it was left (said once per connection)", f.game)
	case frameAutoprint:
		log.Print("[agent] print server: the game server sent its auto-print setting; the agent does not print, so it was left (said once per connection)")
	case frameGame, framePartGame:
		log.Print("[agent] print server: the game server sent game data with no game number; it was left (said once per connection)")
	case frameError:
		// Seen after an acknowledgement, it says the game server answers them
		// (agent 1.12.1). The game server's own words, cut short: an error
		// carries no game data.
		log.Printf("[agent] print server: the game server sent an error no request was waiting for (%q); it was left (said once per connection)", errorText(raw))
	case frameOther:
		log.Printf("[agent] print server: the game server sent a message the agent does not recognise (keys: %s); it was left (said once per connection)", strings.Join(f.keys, ", "))
	}
}

// errorText is an error frame's message, on one line and at most 120
// characters, or "" when it is not a string.
func errorText(raw []byte) string {
	var top struct {
		Error any `json:"error"`
	}
	_ = json.Unmarshal(raw, &top)
	s, _ := top.Error.(string)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 120 {
		s = string(r[:120]) + "…"
	}
	return s
}

// takePushed hands a game the game server sent unasked to the App, off the
// reader, so a slow push to central never holds up a reply. Past
// maxPushedAtOnce it is left, to be asked for between games.
func (l *printLink) takePushed(gen uint64, game int, raw []byte) {
	select {
	case l.pushing <- struct{}{}:
	default:
		log.Printf("[agent] print server: game #%d arrived while %d others were still being handled; it will be asked for between games", game, maxPushedAtOnce)
		return
	}
	go func() {
		defer func() { <-l.pushing }()
		l.app.takePushedGame(game, raw)
		l.acknowledge(gen)
	}()
}

// request sends cmd and waits for its reply: game is the game asked for, or 0
// for the game list. The gate is checked as it is sent, whatever passed it
// before: the connection is held through games, so having one proves nothing.
// A reply is never acknowledged (see the type's comment).
func (l *printLink) request(cmd map[string]any, game int) ([]byte, error) {
	l.reqMu.Lock()
	defer l.reqMu.Unlock()
	if err := l.app.printServerGate(); err != nil {
		return nil, err
	}
	w := &linkWaiter{game: game, reply: make(chan []byte, 1)}
	l.mu.Lock()
	rc, gen, done := l.conn, l.gen, l.done
	if rc == nil {
		l.mu.Unlock()
		return nil, errLinkClosed
	}
	l.waiter = w
	l.mu.Unlock()

	if err := l.write(rc, cmd); err != nil {
		l.end(gen, err, false)
		return nil, err
	}
	t := time.NewTimer(l.replyWait)
	defer t.Stop()
	select {
	case raw := <-w.reply:
		return raw, nil
	case <-done:
		select {
		case raw := <-w.reply:
			return raw, nil // the reply came just before the connection ended
		default:
		}
		l.mu.Lock()
		why := l.endErr
		l.mu.Unlock()
		if why == nil {
			return nil, errLinkClosed
		}
		return nil, fmt.Errorf("%w: %v", errLinkClosed, why)
	case <-t.C:
		// No reply: the connection is not trusted for the next request.
		err := fmt.Errorf("no reply from the print server within %s", shortDuration(l.replyWait))
		l.end(gen, err, false)
		return nil, err
	}
}

// write sends one frame.
func (l *printLink) write(rc *results.Client, cmd map[string]any) error {
	l.wmu.Lock()
	defer l.wmu.Unlock()
	return rc.Send(cmd, 5*time.Second)
}

// acknowledge tells the game server a whole game it sent unasked on
// connection gen was received, if the gate is open (best-effort). Data from a
// connection that has since ended is not acknowledged on the next one.
func (l *printLink) acknowledge(gen uint64) {
	l.mu.Lock()
	if l.conn != nil && gen == l.gen {
		l.acksOwed++
	}
	l.mu.Unlock()
	l.flushAcks()
}

// flushAcks sends the acknowledgements owed on the current connection, if
// the gate is open and no request is waiting for its reply; otherwise they
// wait for maintain, after the next poll. Having sent any, it holds off the
// next request for ackSettle (see the type's comment).
func (l *printLink) flushAcks() {
	if l.app.printServerGate() != nil {
		return
	}
	if !l.reqMu.TryLock() {
		return
	}
	defer l.reqMu.Unlock()
	l.mu.Lock()
	rc, n := l.conn, l.acksOwed
	l.acksOwed = 0
	l.mu.Unlock()
	if rc == nil || n == 0 {
		return
	}
	for i := 0; i < n; i++ {
		if err := l.write(rc, map[string]any{"success": true}); err != nil {
			return // the reader sees the connection end
		}
	}
	time.Sleep(l.ackSettle)
}

// use and release count the sessions using the connection. The last to finish
// closes it with PRINT_SERVER_KEEP_OPEN off, and when nothing automatic uses
// the print server (an operator's command at a venue with results and the
// cache off), which nothing would ever close otherwise.
func (l *printLink) use() {
	l.mu.Lock()
	l.users++
	l.mu.Unlock()
}

func (l *printLink) release() {
	l.mu.Lock()
	l.users--
	closeIt := l.users <= 0 && (!l.keepOpen() || !l.wanted())
	gen := l.gen
	l.mu.Unlock()
	if closeIt {
		// An acknowledgement held back while a request waited goes before
		// the agent closes the connection (with the gate open, as always).
		l.flushAcks()
		l.end(gen, errors.New("work done"), true)
	}
}

// maintain runs after a poll that found the gate open: it sends the
// acknowledgements owed, and with PRINT_SERVER_KEEP_OPEN on (the default)
// opens the connection if it is not open, once the wait after the last one
// is over. Single-flighted.
//
// It dials on its own only once the print server has answered in this run.
// Until then the first dial is left to work that needs it (a finished game,
// the cache refresh, a command), as before 1.11.0: dials of its own would
// count towards treating a print server that is down at start-up as absent
// before any game is played, and while one never reached is absent finished
// games are not queued at all (see queueFetch).
func (l *printLink) maintain() {
	if !l.maintaining.CompareAndSwap(false, true) {
		return
	}
	defer l.maintaining.Store(false)
	if l.connected() {
		l.flushAcks()
		return
	}
	if !l.keepOpen() || !l.wanted() || !l.app.ps.answeredThisRun() {
		return
	}
	l.mu.Lock()
	wait := time.Now().Before(l.retryAt)
	l.mu.Unlock()
	if wait {
		return
	}
	_ = l.ensure(false) // the presence tracker says what a failed dial found
}

// close ends the connection (the agent is stopping).
func (l *printLink) close() {
	l.mu.Lock()
	gen := l.gen
	l.mu.Unlock()
	l.end(gen, errors.New("agent stopping"), true)
}

// report adds the connection to the print_server block.
func (l *printLink) report(r map[string]any) map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn != nil {
		r["connection"] = "open"
		r["connected_since"] = rfc3339OrNil(l.connectedAt)
	} else {
		r["connection"] = "closed"
	}
	r["keep_open"] = l.keepOpen()
	r["connections"] = l.opened
	r["games_sent_unasked"] = l.pushed
	r["parts_sent_unasked"] = l.partial
	r["messages_left"] = l.ignored
	return r
}
