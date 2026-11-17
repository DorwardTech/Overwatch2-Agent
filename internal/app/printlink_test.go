package app

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/ozonefix"
	"overwatch/agent/internal/push"
)

// keepOpen turns PRINT_SERVER_KEEP_OPEN on (the default outside tests: a
// Config built in a test has it off).
func keepOpen(c *config.Config) { c.PrintServerKeepOpen = true }

func withAll(fs ...func(*config.Config)) func(*config.Config) {
	return func(c *config.Config) {
		for _, f := range fs {
			f(c)
		}
	}
}

func startGame(a *App) {
	a.noteServerMode(4)
	a.gameState.Store(stateActive)
}

func endGame(a *App) {
	a.noteServerMode(1)
	a.gameState.Store(stateIdle)
}

func TestClassifyFrame(t *testing.T) {
	cases := []struct {
		raw  string
		kind linkFrameKind
		game int
	}{
		{string(ozonefix.Compact(ozonefix.AllResponseJSON())), frameGame, 9},
		{`{"game":{"gamenum":"12"},"players":{},"events":[]}`, frameGame, 12},
		{`{"game":{},"players":{},"events":[]}`, frameGame, 0},
		// Part of a game: the details without the players or the events.
		{`{"game":{"gamenum":12},"events":[]}`, framePartGame, 12},
		{`{"game":{"gamenum":12},"players":{}}`, framePartGame, 12},
		{`{"game":{"gamenum":12},"players":null,"events":[]}`, framePartGame, 12},
		// Events null: whole as a reply (marked noEvents; see take).
		{`{"game":{"gamenum":9},"players":{},"events":null}`, frameGame, 9},
		{`{"game":{},"players":{}}`, framePartGame, 0},
		{`{"gamelist":[]}`, frameList, 0},
		{`{"error":"Game not found"}`, frameError, 0},
		{`{"gamenumber":3,"command":"player","ids":["5"]}`, framePrint, 3},
		{`{"autoprint":1}`, frameAutoprint, 0},
		{`{"success":true}`, frameAck, 0},
		{string(ozonefix.Compact(ozonefix.TextsBannerJSON())), frameBanner, 0},
		{string(ozonefix.Compact(ozonefix.EventTypesBannerJSON())), frameBanner, 0},
		{`{"game":null,"players":{}}`, frameOther, 0},
		{`{"something":1}`, frameOther, 0},
		{`not json`, frameOther, 0},
	}
	for _, c := range cases {
		f := classifyFrame([]byte(c.raw))
		if f.kind != c.kind || f.game != c.game {
			short := c.raw
			if len(short) > 40 {
				short = short[:40]
			}
			t.Errorf("classifyFrame(%s) = (%s, %d), want (%s, %d)", short, f.kind, f.game, c.kind, c.game)
		}
	}
}

// The reply a request waits for is its own and nothing else: the whole game
// naming the game asked for, the list for the list, a refusal for either.
// Never another game, part of a game, game data naming none, or a frame the
// agent does not recognise.
func TestWaiterWants(t *testing.T) {
	game9 := &linkWaiter{game: 9}
	list := &linkWaiter{}
	cases := []struct {
		w    *linkWaiter
		f    linkFrame
		want bool
	}{
		{game9, linkFrame{kind: frameGame, game: 9}, true},
		{game9, linkFrame{kind: frameGame, game: 0}, false},
		{game9, linkFrame{kind: frameGame, game: 8}, false},
		{game9, linkFrame{kind: frameError}, true},
		{game9, linkFrame{kind: frameOther}, false},
		{game9, linkFrame{kind: framePartGame, game: 9}, false},
		{game9, linkFrame{kind: framePartGame, game: 8}, false},
		{game9, linkFrame{kind: framePartGame, game: 0}, false},
		{game9, linkFrame{kind: frameList}, false},
		{game9, linkFrame{kind: framePrint, game: 9}, false},
		{game9, linkFrame{kind: frameAutoprint}, false},
		{game9, linkFrame{kind: frameAck}, false},
		{list, linkFrame{kind: frameList}, true},
		{list, linkFrame{kind: frameGame, game: 9}, false},
		{list, linkFrame{kind: frameError}, true},
		{list, linkFrame{kind: frameBanner}, false},
		{list, linkFrame{kind: frameOther}, false},
	}
	for i, c := range cases {
		if got := c.w.wants(c.f); got != c.want {
			t.Errorf("case %d: waiter for game %d wants %s #%d = %t, want %t", i, c.w.game, c.f.kind, c.f.game, got, c.want)
		}
	}
}

// One connection serves every piece of work, and stays open after it; an
// open connection is not work, so polling goes back to the idle rate.
func TestOneConnectionServesEveryFetchAndStaysOpen(t *testing.T) {
	a, ps, d, pushed := presenceApp(t, keepOpen)
	ps.AddGame(10, gamePayload(10))

	for _, n := range []int{9, 10} {
		if err := a.pullGameResults(n, false); err != nil {
			t.Fatalf("game %d: %v", n, err)
		}
	}
	if got := d.dials(); got != 1 {
		t.Errorf("dials = %d, want 1", got)
	}
	if got := ps.Connections(); got != 1 {
		t.Errorf("connections = %d, want 1", got)
	}
	if got := ps.OpenConnections(); got != 1 {
		t.Errorf("open connections = %d, want 1 (kept open)", got)
	}
	if got := pushedNumbers(pushed); len(got) != 2 || got[0] != 9 || got[1] != 10 {
		t.Errorf("central was sent %v, want [9 10]", got)
	}
	if got := a.printServerBusy.Load(); got != 0 {
		t.Errorf("printServerBusy = %d with only the connection open, want 0", got)
	}
	if got := a.nextPollInterval(); got != a.cfg.IdlePollInterval {
		t.Errorf("poll interval = %s with only the connection open, want the idle rate %s", got, a.cfg.IdlePollInterval)
	}
	if r := a.printServerReport(); r["connection"] != "open" || r["connections"] != 1 {
		t.Errorf("report = %v, want connection open, 1 opened", r)
	}
}

// PRINT_SERVER_KEEP_OPEN=false: a connection for each piece of work, closed
// when it ends, as before 1.11.0; and nothing opens one on its own.
func TestKeepOpenOffClosesTheConnectionAfterEachPieceOfWork(t *testing.T) {
	a, ps, d, _ := presenceApp(t, nil)
	ps.AddGame(10, gamePayload(10))
	for _, n := range []int{9, 10} {
		if err := a.pullGameResults(n, false); err != nil {
			t.Fatalf("game %d: %v", n, err)
		}
	}
	if got := d.dials(); got != 2 {
		t.Errorf("dials = %d, want 2", got)
	}
	waitUntil(t, "the connections to close", 2*time.Second, func() bool { return ps.OpenConnections() == 0 })
	a.psLink().maintain()
	if got := d.dials(); got != 2 {
		t.Errorf("dials after maintain = %d, want 2 (nothing opens a connection on its own)", got)
	}
}

// The connection stays open through a game, and nothing is sent down it:
// not a request, not an acknowledgement. Afterwards the same connection
// carries on.
func TestNothingIsSentDuringAGameOverTheOpenConnection(t *testing.T) {
	a, ps, d, _ := presenceApp(t, keepOpen)
	if err := a.pullGameResults(9, false); err != nil {
		t.Fatal(err)
	}
	alls, acks := ps.Requests("all"), ps.Requests("ack")

	startGame(a)
	err := a.pullGameResults(9, true) // even an operator's request waits
	if !errors.Is(err, errGameActive) {
		t.Fatalf("request during a game: err = %v, want errGameActive", err)
	}
	a.psLink().acknowledge(currentGen(a))
	a.psLink().maintain()
	time.Sleep(50 * time.Millisecond)
	if ps.Requests("all") != alls || ps.Requests("ack") != acks {
		t.Fatalf("sent during a game: all %d→%d, ack %d→%d", alls, ps.Requests("all"), acks, ps.Requests("ack"))
	}
	if got := ps.OpenConnections(); got != 1 {
		t.Fatalf("open connections during the game = %d, want 1 (held open)", got)
	}

	endGame(a)
	if err := a.pullGameResults(9, false); err != nil {
		t.Fatal(err)
	}
	if got := d.dials(); got != 1 {
		t.Errorf("dials = %d, want 1 (the same connection after the game)", got)
	}
}

// The agent connects on its own only once the print server has answered,
// only between games, and after the game server drops the connection only
// once the wait is over.
func TestMaintainOpensTheConnectionBetweenGamesOnly(t *testing.T) {
	a, ps, d, _ := presenceApp(t, keepOpen)
	l := a.psLink()

	// Not answered yet in this run: the first dial is left to work.
	l.maintain()
	if got := d.dials(); got != 0 {
		t.Fatalf("dials before the print server has answered = %d, want 0", got)
	}
	if err := a.pullGameResults(9, false); err != nil {
		t.Fatal(err)
	}
	if !l.connected() || d.dials() != 1 {
		t.Fatalf("after the first fetch: connected=%t dials=%d, want connected after 1 dial", l.connected(), d.dials())
	}
	l.maintain()
	if got := d.dials(); got != 1 {
		t.Fatalf("dials with the connection open = %d, want 1", got)
	}

	ps.DropConnections()
	waitUntil(t, "the agent to see the connection end", 2*time.Second, func() bool { return !l.connected() })
	l.maintain()
	if got := d.dials(); got != 1 {
		t.Fatalf("dials straight after the drop = %d, want 1 (waits first)", got)
	}
	l.mu.Lock()
	wait := l.retryWait
	l.retryAt = time.Now().Add(-time.Second)
	l.mu.Unlock()
	if wait != linkRetryBase {
		t.Errorf("wait after a drop = %s, want %s", wait, linkRetryBase)
	}
	startGame(a)
	l.maintain()
	if got := d.dials(); got != 1 {
		t.Fatalf("dials during the next game = %d, want 1", got)
	}
	endGame(a)
	l.maintain()
	if !l.connected() || d.dials() != 2 {
		t.Fatalf("after the wait: connected=%t dials=%d, want connected after 2 dials", l.connected(), d.dials())
	}
}

// A connection that keeps ending soon after it opens is opened less and less
// often; one that lasted starts the wait again from the shortest.
func TestTheWaitBeforeReconnectingGrows(t *testing.T) {
	a, _, _, _ := presenceApp(t, keepOpen)
	l := a.psLink()
	now := time.Now()
	for _, want := range []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second} {
		l.backOff(now)
		if l.retryWait != want {
			t.Fatalf("wait = %s, want %s", l.retryWait, want)
		}
	}
	for i := 0; i < 10; i++ {
		l.backOff(now)
	}
	if l.retryWait != linkRetryMax {
		t.Fatalf("wait = %s, want at most %s", l.retryWait, linkRetryMax)
	}
}

// Nothing automatic uses the print server: no connection is held.
func TestMaintainOpensNothingWhenNothingUsesThePrintServer(t *testing.T) {
	a, _, d, _ := presenceApp(t, withAll(keepOpen, func(c *config.Config) { c.ResultsEnabled = false }))
	a.psLink().maintain()
	if got := d.dials(); got != 0 {
		t.Fatalf("dials = %d, want 0", got)
	}
}

// The game server sends a game's data unasked as it ends: it is cached, sent
// to central and acknowledged, and never asked for.
func TestAGameSentUnaskedIsSyncedAndAcknowledged(t *testing.T) {
	a, ps, _, pushed := presenceApp(t, withAll(keepOpen, withCache(t)))
	ps.AddGame(12, gamePayload(12))
	a.queueFetch(12) // a poll saw it finish
	connectLink(t, a)
	if !a.psLink().connected() {
		t.Fatal("not connected")
	}
	if n := ps.PushGame(12); n != 1 {
		t.Fatalf("pushed to %d connections, want 1", n)
	}

	waitUntil(t, "game 12 synced", 2*time.Second, func() bool { return a.isFetched(12) })
	if got := pushedNumbers(pushed); len(got) != 1 || got[0] != 12 {
		t.Fatalf("central was sent %v, want [12]", got)
	}
	if !a.store.HasRaw(12) {
		t.Error("game 12 was not cached")
	}
	if got := a.pendingCount(); got != 0 {
		t.Errorf("games waiting = %d, want 0", got)
	}
	waitUntil(t, "the acknowledgement", 2*time.Second, func() bool { return ps.Requests("ack") == 1 })
	if got := ps.Requests("all"); got != 0 {
		t.Errorf(`"all" requests = %d, want 0 (never asked for)`, got)
	}
	if r := a.printServerReport(); r["games_sent_unasked"] != 1 {
		t.Errorf("report games_sent_unasked = %v, want 1", r["games_sent_unasked"])
	}

	// The game's later drain finds nothing to do.
	if a.drainPendingFetches() {
		t.Error("drain deferred")
	}
	if got := ps.Requests("all"); got != 0 {
		t.Errorf(`"all" requests after the drain = %d, want 0`, got)
	}
}

// Game data that arrives while a game is on (the next one started at once)
// is synced, but acknowledged only once the game is over.
func TestAGameSentUnaskedDuringAGameIsAcknowledgedAfterIt(t *testing.T) {
	a, ps, _, _ := presenceApp(t, keepOpen)
	ps.AddGame(12, gamePayload(12))
	connectLink(t, a)
	startGame(a)
	ps.PushGame(12)
	waitUntil(t, "game 12 synced", 2*time.Second, func() bool { return a.isFetched(12) })
	time.Sleep(50 * time.Millisecond)
	if got := ps.Requests("ack"); got != 0 {
		t.Fatalf("acknowledgements during the game = %d, want 0", got)
	}
	endGame(a)
	a.psLink().maintain()
	waitUntil(t, "the acknowledgement", 2*time.Second, func() bool { return ps.Requests("ack") == 1 })
}

// A game sent unasked while the agent waits for another game's reply is
// taken as what it is, and the reply still reaches its request.
func TestAGameSentUnaskedWhileAnotherIsAskedFor(t *testing.T) {
	a, ps, _, pushed := presenceApp(t, keepOpen)
	ps.AddGame(12, gamePayload(12))
	sent := false
	ps.OnRequest(func(command string) {
		if command == "all" && !sent {
			sent = true
			ps.PushGame(12) // arrives before the reply
		}
	})
	if err := a.pullGameResults(9, false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "game 12 synced", 2*time.Second, func() bool { return a.isFetched(12) })
	got := map[int]int{}
	for _, n := range pushedNumbers(pushed) {
		got[n]++
	}
	if got[9] != 1 || got[12] != 1 || len(got) != 2 {
		t.Fatalf("central was sent %v, want game 9 and game 12 once each", pushedNumbers(pushed))
	}
}

// Print commands and the auto-print setting are for a printer: left alone,
// and never taken for a reply.
func TestPrintCommandsAreLeftAlone(t *testing.T) {
	a, ps, _, pushed := presenceApp(t, keepOpen)
	sent := false
	ps.OnRequest(func(command string) {
		if command == "all" && !sent {
			sent = true
			ps.SendToAll([]byte(`{"gamenumber":3,"command":"team","ids":["1"]}`))
			ps.SendToAll([]byte(`{"autoprint":1}`))
		}
	})
	if err := a.pullGameResults(9, false); err != nil {
		t.Fatal(err)
	}
	if got := pushedNumbers(pushed); len(got) != 1 || got[0] != 9 {
		t.Fatalf("central was sent %v, want [9]", got)
	}
	if got := ps.Requests("all"); got != 1 {
		t.Errorf(`"all" requests = %d, want 1 (a print command asks the agent for nothing)`, got)
	}
	if r := a.printServerReport(); r["messages_left"] != 2 {
		t.Errorf("messages_left = %v, want 2", r["messages_left"])
	}
}

// Central does not take a game sent unasked: it is asked for between games
// instead, with the usual back-off.
func TestAGameSentUnaskedThatCentralRefusesIsAskedForLater(t *testing.T) {
	a, ps, _, _ := presenceApp(t, keepOpen)
	a.pusher = push.New("http://127.0.0.1:1/api/agent/ingest", "test")
	ps.AddGame(12, gamePayload(12))
	connectLink(t, a)
	ps.PushGame(12)
	waitUntil(t, "game 12 queued", 3*time.Second, func() bool {
		a.mu.Lock()
		defer a.mu.Unlock()
		p := a.pendingFetch[12]
		return p != nil && p.attempts == 1
	})
	if a.isFetched(12) {
		t.Error("game 12 marked fetched though central did not take it")
	}
}

// Game data naming no game, with no request waiting, cannot be filed: left.
func TestGameDataWithNoNumberIsLeft(t *testing.T) {
	a, ps, _, pushed := presenceApp(t, keepOpen)
	connectLink(t, a)
	ps.SendToAll([]byte(`{"game":{"gamename":"x"},"players":{}}`))
	waitUntil(t, "the frame to be read", 2*time.Second, func() bool { return a.printServerReport()["messages_left"] == 1 })
	if got := pushedNumbers(pushed); len(got) != 0 {
		t.Fatalf("central was sent %v, want nothing", got)
	}
}

// The agent stopping closes the connection.
func TestCloseEndsTheConnection(t *testing.T) {
	a, ps, _, _ := presenceApp(t, keepOpen)
	connectLink(t, a)
	if ps.OpenConnections() != 1 {
		t.Fatal("not connected")
	}
	a.psLink().close()
	waitUntil(t, "the connection to close", 2*time.Second, func() bool { return ps.OpenConnections() == 0 })
	l := a.psLink()
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.retryAt.IsZero() {
		t.Error("closing on purpose set a reconnect wait")
	}
}

func currentGen(a *App) uint64 {
	l := a.psLink()
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.gen
}

// Part of a game sent unasked (its details and events, no players) is never
// taken as its results: nothing is cached or sent to central, and the game is
// asked for whole between games, once the finish delay is over.
func TestPartOfAGameSentUnaskedIsAskedForWhole(t *testing.T) {
	a, ps, _, pushed := presenceApp(t, withAll(keepOpen, withCache(t), func(c *config.Config) { c.GameFinishDelay = time.Hour }))
	ps.AddGame(12, gamePayload(12))
	connectLink(t, a)
	ps.SendToAll([]byte(`{"game":{"gamenum":12,"gamename":"g"},"events":[]}`))
	waitUntil(t, "game 12 queued", 2*time.Second, func() bool { return a.pendingCount() == 1 })

	if a.isFetched(12) || a.store.HasRaw(12) || len(pushedNumbers(pushed)) != 0 {
		t.Fatalf("part of a game was taken as its results: fetched=%t cached=%t central=%v", a.isFetched(12), a.store.HasRaw(12), pushedNumbers(pushed))
	}
	// Inside the finish delay: not asked for yet.
	a.drainPendingFetches()
	if got := ps.Requests("all"); got != 0 {
		t.Fatalf(`"all" requests inside the finish delay = %d, want 0`, got)
	}
	// Once it is over, the whole game is asked for, over the same connection.
	a.mu.Lock()
	a.pendingFetch[12].notBefore = time.Time{}
	a.finishHold = nil
	a.mu.Unlock()
	a.drainPendingFetches()
	if got := pushedNumbers(pushed); len(got) != 1 || got[0] != 12 {
		t.Fatalf("central was sent %v, want [12]", got)
	}
	raw, ok, _ := a.store.GameRaw(12)
	if !ok || string(raw) != string(gamePayload(12)) {
		t.Fatalf("cached game 12 = %s, want the whole game", raw)
	}
	if ps.Connections() != 1 {
		t.Errorf("connections = %d, want 1", ps.Connections())
	}
	if r := a.printServerReport(); r["parts_sent_unasked"] != 1 || r["games_sent_unasked"] != 0 {
		t.Errorf("report = %v, want parts_sent_unasked 1, games_sent_unasked 0", r)
	}
	// Nothing is acknowledged: not the part, and not the whole game, which
	// was asked for.
	time.Sleep(50 * time.Millisecond)
	if got := ps.Requests("ack"); got != 0 {
		t.Errorf("acknowledgements = %d, want 0", got)
	}
}

// Part of the game being asked for, arriving before the reply, is not taken as
// the reply: it is handled as sent unasked, and the request waits for the
// whole game, which is what is cached and sent.
func TestPartOfTheGameAskedForIsNotTakenAsTheReply(t *testing.T) {
	a, ps, _, pushed := presenceApp(t, withAll(keepOpen, withCache(t)))
	sent := false
	ps.OnRequest(func(command string) {
		if command == "all" && !sent {
			sent = true
			ps.SendToAll([]byte(`{"game":{"gamenum":9},"events":[]}`))
		}
	})
	if err := a.pullGameResults(9, false); err != nil {
		t.Fatal(err)
	}
	raw, ok, _ := a.store.GameRaw(9)
	if !ok || classifyFrame(raw).kind != frameGame {
		t.Fatalf("cached game 9 is not the whole game: %.80s", raw)
	}
	if got := pushedNumbers(pushed); len(got) != 1 || got[0] != 9 {
		t.Fatalf("central was sent %v, want [9]", got)
	}
	if r := a.printServerReport(); r["parts_sent_unasked"] != 1 {
		t.Errorf("parts_sent_unasked = %v, want 1", r["parts_sent_unasked"])
	}
}

// Part of a game, then the game server's refusal of that game: the refusal
// answers that game's own request, never the next one.
func TestARefusalAfterPartOfTheGameAnswersItsOwnRequest(t *testing.T) {
	a, ps, _, _ := presenceApp(t, keepOpen)
	sent := false
	ps.OnRequest(func(command string) {
		if command == "all" && !sent {
			sent = true
			ps.SendToAll([]byte(`{"game":{"gamenum":12},"events":[]}`)) // then "Game not found"
		}
	})
	if err := a.pullGameResults(12, false); !errors.Is(err, errNotGameData) {
		t.Fatalf("game 12: err = %v, want errNotGameData (refused)", err)
	}
	status, result := a.syncGames(map[string]any{}, true)
	if status != "acked" || result["synced"] != 1 {
		t.Fatalf("the next request: sync = %s %v, want acked with game 9 synced", status, result)
	}
}

// A frame the agent does not recognise, arriving while a game is asked for,
// is logged and left; the request still gets its reply.
func TestAnUnrecognisedFrameDuringAGameRequestIsLeft(t *testing.T) {
	a, ps, _, pushed := presenceApp(t, keepOpen)
	sent := false
	ps.OnRequest(func(command string) {
		if command == "all" && !sent {
			sent = true
			ps.SendToAll([]byte(`{"gamedump":{"gamenum":9}}`))
		}
	})
	if err := a.pullGameResults(9, false); err != nil {
		t.Fatal(err)
	}
	if got := pushedNumbers(pushed); len(got) != 1 || got[0] != 9 {
		t.Fatalf("central was sent %v, want [9]", got)
	}
	if r := a.printServerReport(); r["messages_left"] != 1 {
		t.Errorf("messages_left = %v, want 1", r["messages_left"])
	}
}

// A frame the agent does not recognise, arriving while the game list is
// awaited, is left; the list still reaches the request.
func TestAnUnrecognisedFrameIsNotTakenAsTheList(t *testing.T) {
	a, ps, _, _ := presenceApp(t, keepOpen)
	sent := false
	ps.OnRequest(func(command string) {
		if command == "list" && !sent {
			sent = true
			ps.SendToAll([]byte(`{"gamedump":{"gamenum":9}}`))
		}
	})
	status, result := a.syncGames(map[string]any{}, true)
	if status != "acked" || result["synced"] != 1 {
		t.Fatalf("sync = %s %v, want acked with game 9 synced", status, result)
	}
	if r := a.printServerReport(); r["messages_left"] != 1 {
		t.Errorf("messages_left = %v, want 1", r["messages_left"])
	}
}

// Data that came on a connection since closed is not acknowledged on the next
// one.
func TestAnAcknowledgementGoesOnlyOnTheConnectionTheDataCameOn(t *testing.T) {
	a, ps, _, _ := presenceApp(t, keepOpen)
	l := a.psLink()
	connectLink(t, a)
	first := currentGen(a)
	ps.DropConnections()
	waitUntil(t, "the drop", 2*time.Second, func() bool { return !l.connected() })
	l.mu.Lock()
	l.retryAt = time.Time{}
	l.mu.Unlock()
	l.maintain()
	if !l.connected() || currentGen(a) == first {
		t.Fatal("no second connection")
	}
	l.acknowledge(first)
	time.Sleep(50 * time.Millisecond)
	if got := ps.Requests("ack"); got != 0 {
		t.Fatalf("acknowledgements on the new connection for the old one's data = %d, want 0", got)
	}
	l.acknowledge(currentGen(a))
	waitUntil(t, "the acknowledgement", 2*time.Second, func() bool { return ps.Requests("ack") == 1 })
}

// connectLink opens the held connection the way work does (here an operator's
// command): the agent connects on its own only once the print server has
// answered.
func connectLink(t *testing.T, a *App) {
	t.Helper()
	s := a.newResultsSession(true)
	if err := s.connect(); err != nil {
		t.Fatal(err)
	}
	s.retire()
	if !a.psLink().connected() {
		t.Fatal("not connected")
	}
}

// A print server down when the agent starts is not written off by the agent's
// own reconnecting before any game: nothing is dialled on its own until it has
// answered, so the first finished game is still queued and fetched once it
// does, as before 1.11.0.
func TestAPrintServerDownAtStartIsNotCheckedBeforeAnyGame(t *testing.T) {
	a, _, d, pushed := presenceApp(t, keepOpen)
	d.failWith(errDialTimeout)
	l := a.psLink()
	for i := 0; i < 10; i++ {
		l.mu.Lock()
		l.retryAt = time.Time{}
		l.mu.Unlock()
		l.maintain()
	}
	if got := d.dials(); got != 0 {
		t.Fatalf("dials before any game = %d, want 0", got)
	}
	if got := presenceState(a); got == "absent" {
		t.Fatal("the print server was treated as absent before any game")
	}
	// A game ends; the print server answers by the time it is fetched.
	d.failWith(nil)
	a.checkGameResults(map[string]any{"SERVERMODE": 7, "GAMENUM": 9})
	waitDrainIdle(t, a)
	if got := pushedNumbers(pushed); len(got) != 1 || got[0] != 9 {
		t.Fatalf("central was sent %v, want [9]", got)
	}
}

// A whole game sent unasked replaces a different payload cached under its
// number (the game server can use a number again), as a fetch does.
func TestAGameSentUnaskedReplacesADifferentCachedGame(t *testing.T) {
	a, ps, _, _ := presenceApp(t, withAll(keepOpen, withCache(t)))
	old := []byte(`{"game":{"gamenum":12,"gamename":"old"},"players":{},"events":[]}`)
	a.cacheGame(12, old)
	ps.AddGame(12, gamePayload(12))
	connectLink(t, a)
	ps.PushGame(12)
	waitUntil(t, "game 12 synced", 2*time.Second, func() bool { return a.isFetched(12) })
	raw, ok, _ := a.store.GameRaw(12)
	if !ok || string(raw) != string(gamePayload(12)) {
		t.Fatalf("cached game 12 = %s, want the game just sent", raw)
	}
}

// With results and the cache off, a connection an operator's command opens is
// closed when the command is done: nothing else would close it.
func TestAnOperatorsConnectionClosesWhenNothingElseUsesThePrintServer(t *testing.T) {
	a, ps, _, _ := presenceApp(t, withAll(keepOpen, func(c *config.Config) { c.ResultsEnabled = false }))
	if err := a.pullGameResults(9, true); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the connection to close", 2*time.Second, func() bool { return ps.OpenConnections() == 0 })
	if a.psLink().connected() {
		t.Fatal("still connected")
	}
}

// Game data naming no game, arriving while a game is asked for, is not taken
// as its reply: it could be any game. The reply naming the game is.
func TestGameDataNamingNoGameIsNotTakenAsTheReply(t *testing.T) {
	a, ps, _, pushed := presenceApp(t, withAll(keepOpen, withCache(t)))
	sent := false
	ps.OnRequest(func(command string) {
		if command == "all" && !sent {
			sent = true
			ps.SendToAll([]byte(`{"game":{"gamename":"another"},"players":{},"events":[]}`))
		}
	})
	if err := a.pullGameResults(9, false); err != nil {
		t.Fatal(err)
	}
	raw, ok, _ := a.store.GameRaw(9)
	if !ok || payloadGameNumberOf(raw) != 9 {
		t.Fatalf("cached game 9 = %.80s, want the reply naming game 9", raw)
	}
	got := pushed.all()
	if len(got) != 1 {
		t.Fatalf("central was sent %d games, want 1", len(got))
	}
	data, _ := got[0]["data"].(map[string]any)
	game, _ := data["game"].(map[string]any)
	if n, _ := game["gamenum"].(float64); n != 9 {
		t.Fatalf("central was sent game %v as game 9, want game 9's own data", game)
	}
}

func payloadGameNumberOf(raw []byte) int {
	n, _ := payloadGameNumber(raw)
	return n
}

// A refusal of the game list is not an empty list: a backfill fails, rather
// than reporting success having synced nothing.
func TestAListRefusalIsNotAnEmptyList(t *testing.T) {
	a, ps, _, _ := presenceApp(t, keepOpen)
	ps.SetList([]byte(`{"error":"Busy"}`))
	status, result := a.syncGames(map[string]any{}, true)
	if status != "failed" {
		t.Fatalf("sync = %s %v, want failed", status, result)
	}
}

// Part of a game the message bus has already said finished does not put off
// the finish work that game is waiting for.
func TestPartOfAGameDoesNotPutOffAGameAlreadyWaiting(t *testing.T) {
	a, ps, _, _ := presenceApp(t, withAll(keepOpen, func(c *config.Config) { c.GameFinishDelay = time.Hour }))
	connectLink(t, a)
	due := time.Now().Add(time.Minute)
	a.queueFetch(12)
	a.notBefore(12, due)
	ps.SendToAll([]byte(`{"game":{"gamenum":12},"events":[]}`))
	waitUntil(t, "the part to be read", 2*time.Second, func() bool { return a.printServerReport()["parts_sent_unasked"] == 1 })
	time.Sleep(100 * time.Millisecond) // counted just before it is handled
	a.mu.Lock()
	got, hold := a.pendingFetch[12].notBefore, a.finishHold[12]
	a.mu.Unlock()
	if !got.Equal(due) || !hold.Equal(due) {
		t.Fatalf("wait moved to %s (hold %s), want it kept at %s", got, hold, due)
	}
}

// The message bus fetches a finished game whatever ENABLE_GAME_RESULTS says,
// so with only the bus on the connection is held like any other use.
func TestABusOnlyVenueKeepsTheConnectionOpen(t *testing.T) {
	a, ps, _, _ := presenceApp(t, withAll(keepOpen, func(c *config.Config) {
		c.ResultsEnabled = false
		c.MsgBusEnabled = true
	}))
	if err := a.pullGameResults(9, false); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if got := ps.OpenConnections(); got != 1 {
		t.Fatalf("open connections after the fetch = %d, want 1 (kept)", got)
	}
}

// With only the message bus on, a whole game sent unasked is sent to central,
// as the bus's own finish would fetch and send it.
func TestABusOnlyVenueSyncsAGameSentUnasked(t *testing.T) {
	a, ps, _, pushed := presenceApp(t, withAll(keepOpen, func(c *config.Config) {
		c.ResultsEnabled = false
		c.MsgBusEnabled = true
	}))
	ps.AddGame(12, gamePayload(12))
	connectLink(t, a)
	ps.PushGame(12)
	waitUntil(t, "game 12 synced", 2*time.Second, func() bool { return a.isFetched(12) })
	if got := pushedNumbers(pushed); len(got) != 1 || got[0] != 12 {
		t.Fatalf("central was sent %v, want [12]", got)
	}
}

// Sent unasked with its events null, a game is not taken as whole: its events
// may not be written yet. It is asked for whole after the finish delay. The
// same shape is whole as the reply to a request.
func TestAGameSentUnaskedWithNullEventsIsAskedForWhole(t *testing.T) {
	a, ps, _, pushed := presenceApp(t, withAll(keepOpen, withCache(t), func(c *config.Config) { c.GameFinishDelay = time.Hour }))
	connectLink(t, a)
	ps.SendToAll([]byte(`{"game":{"gamenum":12},"players":{},"events":null}`))
	waitUntil(t, "game 12 queued", 2*time.Second, func() bool { return a.pendingCount() == 1 })
	if a.isFetched(12) || a.store.HasRaw(12) || len(pushedNumbers(pushed)) != 0 {
		t.Fatal("a game sent unasked with null events was taken as whole")
	}
	ps.AddGame(9, []byte(`{"game":{"gamenum":9},"players":{},"events":null}`))
	if err := a.pullGameResults(9, false); err != nil {
		t.Fatalf("the same shape as a reply: %v", err)
	}
}

// With only the message bus on, a game sent unasked that central does not
// take stays queued with no attempt used, so the bus's one finish run after
// the finish delay fetches and sends it.
func TestABusOnlyVenueLeavesARefusedGameForTheFinishRun(t *testing.T) {
	a, ps, _, _ := presenceApp(t, withAll(keepOpen, func(c *config.Config) {
		c.ResultsEnabled = false
		c.MsgBusEnabled = true
	}))
	a.pusher = push.New("http://127.0.0.1:1/api/agent/ingest", "test")
	ps.AddGame(12, gamePayload(12))
	connectLink(t, a)
	ps.PushGame(12)
	waitUntil(t, "game 12 queued", 3*time.Second, func() bool { return a.pendingCount() == 1 })
	time.Sleep(50 * time.Millisecond)
	a.mu.Lock()
	p := a.pendingFetch[12]
	attempts, retry := p.attempts, p.nextRetry
	a.mu.Unlock()
	if attempts != 0 || !retry.IsZero() {
		t.Fatalf("attempts=%d nextRetry=%s, want none (the finish run must not skip it)", attempts, retry)
	}
}

// A game the agent asks for is never acknowledged, with the connection held
// or closed after the work: the game server's print-server API has the client
// acknowledge only the game data it sends unasked (agent 1.12.1).
func TestAGameAskedForIsNotAcknowledged(t *testing.T) {
	for name, mode := range map[string]func(*config.Config){"held": keepOpen, "closed": func(*config.Config) {}} {
		t.Run(name, func(t *testing.T) {
			a, ps, _, pushed := presenceApp(t, mode)
			if err := a.pullGameResults(9, false); err != nil {
				t.Fatal(err)
			}
			if got := pushedNumbers(pushed); len(got) != 1 || got[0] != 9 {
				t.Fatalf("central was sent %v, want [9]", got)
			}
			time.Sleep(50 * time.Millisecond)
			if got := ps.Requests("ack"); got != 0 {
				t.Fatalf("acknowledgements = %d, want 0", got)
			}
		})
	}
}

// A cache refresh, once a minute between games, acknowledges nothing: not the
// list, and not the games it fetches. Before 1.12.1 every refresh ended with
// {"success": true}, even one that fetched nothing.
func TestACacheRefreshAcknowledgesNothing(t *testing.T) {
	for name, mode := range map[string]func(*config.Config){"held": keepOpen, "closed": func(*config.Config) {}} {
		t.Run(name, func(t *testing.T) {
			a, ps, _, _ := presenceApp(t, withAll(mode, withCache(t)))
			if err := a.refreshCache(false); err != nil { // fetches game 9
				t.Fatal(err)
			}
			if !a.store.HasRaw(9) {
				t.Fatal("game 9 was not cached")
			}
			if err := a.refreshCache(false); err != nil { // nothing to fetch
				t.Fatal(err)
			}
			time.Sleep(50 * time.Millisecond)
			if l, all, ack := ps.Requests("list"), ps.Requests("all"), ps.Requests("ack"); l != 2 || all != 1 || ack != 0 {
				t.Fatalf("list %d, all %d, ack %d; want 2, 1, 0", l, all, ack)
			}
		})
	}
}

// A game sent unasked while a request waits for its reply is acknowledged only
// once that request is answered, so nothing the game server says back to the
// acknowledgement could be taken as the reply. The acknowledgement goes with
// the next poll's upkeep.
func TestNoAcknowledgementGoesWhileARequestWaits(t *testing.T) {
	a, ps, _, _ := presenceApp(t, keepOpen)
	ps.AddGame(12, gamePayload(12))
	connectLink(t, a)
	release := make(chan struct{})
	var once sync.Once
	ps.OnRequest(func(command string) {
		if command == "list" {
			once.Do(func() {
				ps.PushGame(12) // game 12 ends while the list is asked for
				<-release       // and the list's reply is held back
			})
		}
	})
	done := make(chan error, 1)
	go func() {
		s := a.newResultsSession(false)
		defer s.retire()
		_, err := s.list()
		done <- err
	}()
	waitUntil(t, "game 12 synced", 2*time.Second, func() bool { return a.isFetched(12) })
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The game server reads frames in order: an acknowledgement written while
	// the list waited would be counted now, right after the list's reply.
	time.Sleep(50 * time.Millisecond)
	if got := ps.Requests("ack"); got != 0 {
		t.Fatalf("acknowledgements sent while the list waited = %d, want 0", got)
	}
	a.psLink().maintain()
	waitUntil(t, "the acknowledgement after the reply", 2*time.Second, func() bool { return ps.Requests("ack") == 1 })
}

// A game server that answers an acknowledgement (as one that mistakes it for
// a request might, with an error) does not have that answer taken as the reply
// to the agent's next request: the request waits a moment after it.
func TestAnAnswerToAnAcknowledgementIsNotTakenAsTheNextReply(t *testing.T) {
	logs := captureLog(t)
	a, ps, _, _ := presenceApp(t, keepOpen)
	ps.AddGame(12, gamePayload(12))
	ps.AnswerAcks([]byte(`{"error":"Game not found"}`), 200*time.Millisecond)
	connectLink(t, a)
	ps.PushGame(12)
	waitUntil(t, "the acknowledgement", 2*time.Second, func() bool { return ps.Requests("ack") == 1 })

	s := a.newResultsSession(false)
	defer s.retire()
	if _, err := s.list(); err != nil {
		t.Fatalf("the list after an acknowledgement: %v", err)
	}
	// A second game, a second acknowledgement and a second answer on the same
	// connection: the answer is logged once per connection, in the game
	// server's words, and both are counted as left.
	ps.AddGame(13, gamePayload(13))
	ps.PushGame(13)
	waitUntil(t, "the second answer", 3*time.Second, func() bool {
		return a.printServerReport()["messages_left"] == 2
	})
	if got := strings.Count(logs.String(), `sent an error no request was waiting for ("Game not found")`); got != 1 {
		t.Fatalf("logged the answer %d times, want 1:\n%s", got, logs.String())
	}
}

// An acknowledgement held back while a request waited is sent before the agent
// closes the connection itself (PRINT_SERVER_KEEP_OPEN=false, the work done),
// not dropped with it.
func TestAnAcknowledgementHeldBackGoesBeforeTheConnectionCloses(t *testing.T) {
	a, ps, _, _ := presenceApp(t, func(*config.Config) {})
	ps.AddGame(12, gamePayload(12))
	release := make(chan struct{})
	var once sync.Once
	ps.OnRequest(func(command string) {
		if command == "list" {
			once.Do(func() {
				ps.PushGame(12)
				<-release
			})
		}
	})
	done := make(chan error, 1)
	go func() {
		s := a.newResultsSession(false)
		_, err := s.list()
		s.retire()
		done <- err
	}()
	waitUntil(t, "game 12 synced", 2*time.Second, func() bool { return a.isFetched(12) })
	time.Sleep(50 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the acknowledgement", 2*time.Second, func() bool { return ps.Requests("ack") == 1 })
	waitUntil(t, "the connection to close", 2*time.Second, func() bool { return ps.OpenConnections() == 0 })
}
