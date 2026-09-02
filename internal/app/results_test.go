package app

import (
	"net"
	"testing"
	"time"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/ozonefix"
	"overwatch/agent/internal/ozonesim"
	"overwatch/agent/internal/push"
)

// newSimApp starts a fake print server and returns an idle agent wired to it.
func newSimApp(t *testing.T) (*App, *ozonesim.PrintServer) {
	t.Helper()
	ps := ozonesim.NewPrintServer()
	if err := ps.Start(0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	host, port, _ := net.SplitHostPort(ps.Addr())

	a := New(config.Config{
		OzoneHost:        host,
		OzoneResultsPort: port,
		ResultsHandshake: 2,
		CentralURL:       "http://central.invalid",
		Token:            "test",
	})
	a.noteServerMode(1) // idle
	a.gameState.Store(stateIdle)
	return a, ps
}

// A backfill or resync passes through the command-queue gate before it blocks
// on the results lock, and both readings that gate consults are written only by
// the poll loop. If the O-Zone link has dropped, "idle" is simply the last
// thing the agent saw — so the batch must defer, and say why, rather than dial
// the print server on a memory.
func TestSyncGamesDefersOnAStaleLink(t *testing.T) {
	a, ps := newSimApp(t)
	ps.SetList([]byte(`{"gamelist":[{"gamenum":9,"gamename":"g9","duration":601,` +
		`"starttime":"s","endtime":"e","playercount":2,"valid":1}]}`))

	// Last seen idle — but long enough ago that it means nothing now.
	a.serverModeAt.Store(time.Now().Add(-maxServerModeAge - time.Second).UnixNano())

	status, result := a.syncGames(map[string]any{}, true)

	if status != "deferred" {
		t.Fatalf("status = %q, want deferred on a stale O-Zone link", status)
	}
	if got, _ := result["reason"].(string); got != errLinkStale.Error() {
		t.Errorf("reason = %q, want %q — an outage must not be reported as a game", got, errLinkStale)
	}
	if ps.Connections() != 0 {
		t.Fatalf("print server was contacted on a stale reading (connections=%d)", ps.Connections())
	}
}

// A backfill/resync must stop the moment a game starts mid-batch — the print
// server is never pulled from during active play, even inside a long batch that
// passed the safety gate when it began.
func TestSyncGamesAbortsWhenGameStartsMidBatch(t *testing.T) {
	a, ps := newSimApp(t)

	// Two valid games; the first "all" fetch flips the agent's game state to
	// active, exactly as a GAME_START bus event would mid-batch.
	ps.AddGame(10, ozonefix.AllResponseJSON())
	ps.SetList([]byte(`{"gamelist":[` +
		`{"gamenum":9,"gamename":"g9","duration":601,"starttime":"s","endtime":"e","playercount":2,"valid":1},` +
		`{"gamenum":10,"gamename":"g10","duration":601,"starttime":"s","endtime":"e","playercount":2,"valid":1}]}`))
	ps.OnRequest(func(cmd string) {
		if cmd == "all" {
			a.gameState.Store(stateActive)
		}
	})

	status, result := a.syncGames(map[string]any{}, true)

	if status != "deferred" {
		t.Fatalf("status = %q, want deferred (batch must stop when a game starts)", status)
	}
	if got := ps.Requests("all"); got != 1 {
		t.Fatalf("print server received %d 'all' fetches after a game started, want exactly 1", got)
	}
	if result["reason"] == nil {
		t.Fatal("deferred result should carry a reason")
	}
}

// refetch_game re-checks the idle gate at the moment of connection: a game that
// starts between the command-queue gate and the fetch defers the command
// instead of touching the print server.
func TestRefetchGameDefersWhenGameActive(t *testing.T) {
	a, ps := newSimApp(t)
	a.gameState.Store(stateActive)

	status, result := a.runCommand(push.Command{Type: "refetch_game", Payload: map[string]any{"game_number": 9}})

	if status != "deferred" {
		t.Fatalf("status = %q, want deferred", status)
	}
	if result["reason"] != errGameActive.Error() {
		t.Fatalf("result = %v, want a game-in-progress reason", result)
	}
	if ps.Connections() != 0 {
		t.Fatalf("print server must not be contacted during a game (connections=%d)", ps.Connections())
	}
}

// pullGameResults is the single chokepoint: it refuses to dial while a game is
// active, whatever the caller previously checked.
func TestPullGameResultsRefusesDuringGame(t *testing.T) {
	a, ps := newSimApp(t)
	a.noteServerMode(6) // game in progress

	if err := a.pullGameResults(9); err != errGameActive {
		t.Fatalf("err = %v, want errGameActive", err)
	}
	if ps.Connections() != 0 {
		t.Fatalf("print server must not be contacted during a game (connections=%d)", ps.Connections())
	}
}

// A game whose fetch keeps failing backs off exponentially and is abandoned
// after maxFetchAttempts — it must not be re-fetched on every poll forever
// (the game #347 failure mode).
func TestFetchFailureBacksOffAndGivesUp(t *testing.T) {
	a := New(config.Config{CentralURL: "http://central.invalid", Token: "t"})

	a.queueFetch(347)
	for i := 1; i < maxFetchAttempts; i++ {
		a.noteFetchFailure(347, errTest)
		a.mu.Lock()
		p := a.pendingFetch[347]
		a.mu.Unlock()
		if p == nil {
			t.Fatalf("game should still be pending after %d attempts", i)
		}
		if p.attempts != i {
			t.Fatalf("attempts = %d, want %d", p.attempts, i)
		}
		if !p.nextRetry.After(time.Now()) {
			t.Fatalf("attempt %d: nextRetry should be in the future", i)
		}
	}

	// Re-noticing the finish must not reset the backoff state.
	a.queueFetch(347)
	a.mu.Lock()
	attempts := a.pendingFetch[347].attempts
	a.mu.Unlock()
	if attempts != maxFetchAttempts-1 {
		t.Fatalf("queueFetch reset attempts to %d", attempts)
	}

	// The final failure abandons the game.
	a.noteFetchFailure(347, errTest)
	pending, givenUp := a.resultsBacklog()
	if pending != 0 {
		t.Fatalf("pending = %d, want 0 after giving up", pending)
	}
	if len(givenUp) != 1 || givenUp[0] != 347 {
		t.Fatalf("givenUp = %v, want [347]", givenUp)
	}

	// An abandoned game is not silently re-queued by the finish detector...
	a.queueFetch(347)
	if pending, _ := a.resultsBacklog(); pending != 0 {
		t.Fatal("an abandoned game must not be re-queued automatically")
	}
	// ...but a successful manual refetch recovers it.
	a.markFetched(347)
	if _, givenUp := a.resultsBacklog(); len(givenUp) != 0 {
		t.Fatalf("givenUp = %v, want empty after a successful refetch", givenUp)
	}
}

// While a game is backing off, drainPendingFetches must not touch the print
// server for it.
func TestDrainSkipsGameInBackoff(t *testing.T) {
	a, ps := newSimApp(t)

	a.queueFetch(9)
	a.noteFetchFailure(9, errTest) // schedules a future retry

	a.drainPendingFetches()
	if ps.Connections() != 0 {
		t.Fatalf("a backing-off game was fetched anyway (connections=%d)", ps.Connections())
	}

	// Once due, it is fetched again.
	a.mu.Lock()
	a.pendingFetch[9].nextRetry = time.Time{}
	a.mu.Unlock()
	a.drainPendingFetches()
	if ps.Connections() == 0 {
		t.Fatal("a due game should be fetched")
	}
}

func TestFetchBackoffSchedule(t *testing.T) {
	cases := map[int]time.Duration{
		1: 30 * time.Second,
		2: time.Minute,
		3: 2 * time.Minute,
		7: 30 * time.Minute, // capped
		9: 30 * time.Minute,
	}
	for attempts, want := range cases {
		if got := fetchBackoff(attempts); got != want {
			t.Errorf("fetchBackoff(%d) = %s, want %s", attempts, got, want)
		}
	}
}

var errTest = errTestType{}

type errTestType struct{}

func (errTestType) Error() string { return "simulated fetch failure" }

// A backfill/resync that waited on the results mutex must re-check the idle gate
// before it dials: the command-queue gate ran before the wait, so a game can
// have started while we were blocked. Without the re-check the agent connects,
// drains the handshake and issues "list" during active play.
func TestSyncGamesDefersWhenGameStartsDuringLockWait(t *testing.T) {
	a, ps := newSimApp(t)

	// Hold the results mutex, as a mid-flight refreshCache would, and start a
	// game while syncGames is blocked on it.
	a.resultsMu.Lock()
	done := make(chan struct{})
	var status string
	var result map[string]any
	go func() {
		defer close(done)
		status, result = a.syncGames(map[string]any{}, true)
	}()

	// Give the goroutine time to block on the lock, then start a game.
	time.Sleep(50 * time.Millisecond)
	a.gameState.Store(stateActive)
	a.resultsMu.Unlock()
	<-done

	if status != "deferred" {
		t.Fatalf("status = %q, want deferred (a game started during the lock wait)", status)
	}
	if result["reason"] != errGameActive.Error() {
		t.Fatalf("result = %v, want a game-in-progress reason", result)
	}
	if ps.Connections() != 0 {
		t.Fatalf("print server must not be contacted during a game (connections=%d)", ps.Connections())
	}
}

// The pending-results drain must not run on the poll goroutine. collect() is the
// only writer of serverMode (and, without the Message Bus, of gameState), so a
// synchronous drain freezes the very idle signal each queued fetch re-checks:
// a game starting mid-drain goes unnoticed and the games queued behind it are
// pulled from the print server during live play.
func TestCheckGameResultsDrainsOffThePollGoroutine(t *testing.T) {
	a, ps := newSimApp(t)
	a.cfg.ResultsEnabled = true

	// Two finished games are queued and the first fetch is slow, so the drain is
	// still running when the poll loop next reads the server state.
	ps.AddGame(10, ozonefix.AllResponseJSON())
	a.queueFetch(9)
	a.queueFetch(10)
	fetching := make(chan struct{}, 1)
	ps.OnRequest(func(cmd string) {
		if cmd == "all" {
			select {
			case fetching <- struct{}{}:
			default:
			}
			time.Sleep(300 * time.Millisecond)
		}
	})

	// The poll goroutine's call must return promptly rather than block for the
	// whole drain (that alone would stall telemetry past the offline window)...
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		a.checkGameResults(map[string]any{"SERVERMODE": 1, "GAMENUM": 0})
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("checkGameResults blocked the poll goroutine for the whole drain")
	}

	// ...so that the poll loop can still record a game starting while the drain
	// is in flight. With a synchronous drain this update could not land until
	// every queued game had already been pulled.
	<-fetching
	a.noteServerMode(6) // what collect() would store on the next poll

	time.Sleep(time.Second)
	if got := ps.Requests("all"); got != 1 {
		t.Fatalf("print server received %d 'all' fetches after a game started, want exactly 1", got)
	}
}
