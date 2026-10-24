package app

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"overwatch/agent/internal/buffer"
	"overwatch/agent/internal/config"
	"overwatch/agent/internal/msgbus"
	"overwatch/agent/internal/ozone"
	"overwatch/agent/internal/ozonesim"
)

// A poll whose replies were out of step may record its reading and may mark a
// game active, but it may not clear one or open the print-server gate.
func TestUnsettledPollCannotClearAGameOrOpenTheGate(t *testing.T) {
	a := &App{}
	a.noteServerMode(1)
	a.gameState.Store(stateActive)
	a.pollUnsettled.Store(true)

	a.reconcileState(1)
	if a.gameState.Load() != stateActive {
		t.Fatal("an unsettled poll's idle reading cleared an active game")
	}
	a.reconcileState(6)
	if a.gameState.Load() != stateActive {
		t.Fatal("an unsettled poll's active reading must still mark the game active")
	}

	a.gameState.Store(stateIdle)
	if a.safeForPrintServer(1) {
		t.Fatal("an unsettled poll opened the print-server gate")
	}
	if err := a.printServerGate(); !errors.Is(err, errPollUnsettled) {
		t.Fatalf("gate = %v, want errPollUnsettled", err)
	}
	if !deferrable(errPollUnsettled) {
		t.Fatal("errPollUnsettled must defer the work, not fail it")
	}

	// The next clean poll settles it.
	a.pollUnsettled.Store(false)
	a.gameState.Store(stateActive)
	a.reconcileState(1)
	if a.gameState.Load() != stateIdle {
		t.Fatal("a clean poll's idle reading should clear a stale active game")
	}
	if err := a.printServerGate(); err != nil {
		t.Fatalf("gate = %v after a clean idle poll, want nil", err)
	}
}

// scriptedOZone serves a fake O-Zone: script returns the frames to send for
// the n-th request (counting from 0), or hangUp to drop the connection.
func scriptedOZone(t *testing.T, script func(cmd string, n int) (frames []map[string]any, hangUp bool)) *ozone.Client {
	t.Helper()
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for n := 0; ; n++ {
			var req map[string]string
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			frames, hangUp := script(req["CMD"], n)
			if hangUp {
				return
			}
			for _, f := range frames {
				if err := conn.WriteJSON(f); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	client, err := ozone.Dial(host, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func packsReply() map[string]any {
	return map[string]any{"CMD": "GETACTIVEPACKS", "PACKS": []any{}}
}

// End to end through collect(): after the request, an old GETSERVERSTATE
// ("between games") arrives ahead of the real one (a game in progress). The
// old one is accepted, since it came after the request, but the real one then
// turns up as an extra frame that GETACTIVEPACKS drops. Its reading wins: the
// game the message bus reported stays active and the gate stays shut.
func TestCollectTreatsALateFrameAsUnsettled(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{
				{"CMD": "GETSERVERSTATE", "SERVERMODE": 2, "GAMENUM": 41},
				{"CMD": "GETSERVERSTATE", "SERVERMODE": 6, "GAMENUM": 42},
			}, false
		}
		return []map[string]any{packsReply()}, false
	})

	a := &App{}
	a.gameState.Store(stateActive) // the message bus saw the game start

	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if !a.pollUnsettled.Load() {
		t.Fatal("a poll that dropped its real reply as an extra frame was not marked unsettled")
	}
	if got := a.serverMode.Load(); got != 6 {
		t.Fatalf("serverMode = %d, want 6 (the extra frame's game in progress)", got)
	}
	if a.gameState.Load() != stateActive {
		t.Fatal("the late 'between games' reading cleared the game the bus reported")
	}
	if a.safeForPrintServer(int(a.serverMode.Load())) {
		t.Fatal("the print-server gate opened on an unsettled poll")
	}
}

// A game starting closes the gate the moment GETSERVERSTATE says so. It used
// to wait for GETACTIVEPACKS, and a slow or failed second reply left the gate
// open on the last poll's idle reading.
func TestActiveReadingClosesTheGateEvenIfThePollFails(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 4}}, false
		}
		return nil, true
	})

	a := &App{}
	a.noteServerMode(1)
	a.gameState.Store(stateIdle)
	if err := a.printServerGate(); err != nil {
		t.Fatalf("gate = %v before the poll, want open", err)
	}

	if _, _, err := a.collect(client, false); err == nil {
		t.Fatal("collect() succeeded although GETACTIVEPACKS hung up")
	}
	if got := a.serverMode.Load(); got != 4 {
		t.Fatalf("serverMode = %d, want 4", got)
	}
	if a.gameState.Load() != stateActive {
		t.Fatal("a starting game was not marked active")
	}
	if a.printServerGate() == nil {
		t.Fatal("the print-server gate stayed open after a game started")
	}
}

// A poll that fails at either call leaves the gate shut, even after an idle
// reading, though the last poll left it open.
func TestFailedPollLeavesTheGateShut(t *testing.T) {
	for _, failing := range []string{"GETSERVERSTATE", "GETACTIVEPACKS"} {
		t.Run(failing, func(t *testing.T) {
			client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
				if cmd == failing {
					return nil, true
				}
				return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
			})

			a := &App{}
			a.noteServerMode(1)
			a.gameState.Store(stateIdle)
			if err := a.printServerGate(); err != nil {
				t.Fatalf("gate = %v before the poll, want open", err)
			}

			if _, _, err := a.collect(client, false); err == nil {
				t.Fatalf("collect() succeeded although %s hung up", failing)
			}
			if err := a.printServerGate(); !errors.Is(err, errPollUnsettled) {
				t.Fatalf("gate = %v after a failed poll, want errPollUnsettled", err)
			}
		})
	}
}

// An O-Zone that sends every reply twice must not keep polls unsettled for
// ever: the duplicate server state reads idle too, so the verdict holds. With
// the bus off, a game the last active reading left marked active is cleared
// and the gate opens, so finished games are still fetched.
func TestDuplicatedRepliesStillSettle(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		var reply map[string]any
		if cmd == "GETSERVERSTATE" {
			reply = map[string]any{"CMD": cmd, "SERVERMODE": 2, "GAMENUM": 41}
		} else {
			reply = packsReply()
		}
		return []map[string]any{reply, reply}, false
	})

	a := &App{}
	a.gameState.Store(stateActive)
	for i := 0; i < 5; i++ {
		if _, _, err := a.collect(client, false); err != nil {
			t.Fatal(err)
		}
	}
	if a.pollUnsettled.Load() {
		t.Fatal("duplicated idle replies kept the poll unsettled")
	}
	if a.gameState.Load() != stateIdle {
		t.Fatal("duplicated idle replies never cleared the finished game")
	}
	if err := a.printServerGate(); err != nil {
		t.Fatalf("gate = %v, want open", err)
	}
}

// Replies to other commands turning up mid-poll (other clients' traffic) cannot
// be the server state, so they do not unsettle the poll.
func TestOtherCommandsRepliesDoNotUnsettleAPoll(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{
				{"CMD": cmd, "SERVERMODE": 2},
				{"CMD": "GETTEAMINFO", "TEAMDETAILS": []any{}},
			}, false
		}
		return []map[string]any{{"CMD": "GETGAMELIST"}, packsReply()}, false
	})

	a := &App{}
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if a.pollUnsettled.Load() {
		t.Fatal("replies to other commands unsettled the poll")
	}
}

// A game the bus reports starting while a poll is out cannot be undone by
// that poll: its idle reading may predate the start. It used to be, and the
// gate opened in the first seconds of the game.
func TestBusGameStartDuringAPollIsNotUndoneByIt(t *testing.T) {
	a := &App{}
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		}
		a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameStart})
		return []map[string]any{packsReply()}, false
	})
	a.noteServerMode(2)
	a.gameState.Store(stateIdle)

	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if a.gameState.Load() != stateActive {
		t.Fatal("the poll's idle reading cleared the game the bus reported starting")
	}
	if err := a.printServerGate(); err == nil {
		t.Fatal("the print-server gate opened on the game's first poll")
	}
	if !a.pollUnsettled.Load() {
		t.Fatal("a poll the bus overtook was not marked unsettled")
	}
}

// A clean poll leaves the gate as the last one did while it runs. It used to
// close it for the round trip, and a batch of fetches checking the gate
// between games gave up part-way through on every poll.
func TestACleanPollKeepsTheGateOpenWhileItRuns(t *testing.T) {
	a := &App{}
	var midPoll atomic.Value
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		}
		midPoll.Store(fmt.Sprint(a.printServerGate()))
		return []map[string]any{packsReply()}, false
	})
	a.noteServerMode(2)
	a.gameState.Store(stateIdle)

	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if got := midPoll.Load(); got != fmt.Sprint(nil) {
		t.Fatalf("gate = %v mid-poll, want open", got)
	}
	if err := a.printServerGate(); err != nil {
		t.Fatalf("gate = %v after a clean idle poll, want open", err)
	}
}

// The real server state arrives between the poll's two calls, behind another
// client's old idle reading and other traffic. The client keeps it for the
// verdict, and its reading wins: the game is marked active and the gate stays
// shut, every time.
func TestCollectNeverLosesTheRealServerStateBetweenCalls(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd != "GETSERVERSTATE" {
			return []map[string]any{packsReply()}, false
		}
		frames := []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}
		for i := 0; i < 8; i++ {
			frames = append(frames, map[string]any{"CMD": "GETTEAMINFO", "I": i})
		}
		return append(frames, map[string]any{"CMD": cmd, "SERVERMODE": 6}), false
	})

	a := &App{}
	for i := 0; i < 300; i++ {
		a.noteServerMode(2)
		a.gameState.Store(stateIdle)
		a.pollUnsettled.Store(false)

		if _, _, err := a.collect(client, false); err != nil {
			t.Fatal(err)
		}
		if got := a.serverMode.Load(); got != 6 || a.gameState.Load() != stateActive {
			t.Fatalf("poll %d: serverMode = %d, game state %d; want 6 and active (the real reply)", i, got, a.gameState.Load())
		}
		if a.printServerGate() == nil {
			t.Fatalf("poll %d: the print-server gate opened on another client's old reading", i)
		}
	}
}

// A server state without an echo is accepted by any command, so the real one
// can be taken as GETACTIVEPACKS's reply. It is judged like any other frame
// that could be the server state: here it says a game is on, and that wins
// over the idle reading GETSERVERSTATE took.
func TestAServerStateTakenAsThePacksReplyIsJudged(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"SERVERMODE": 2}}, false
		}
		return []map[string]any{{"SERVERMODE": 6}, {"PACKS": []any{}}}, false
	})

	a := &App{}
	a.gameState.Store(stateActive)
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if !a.pollUnsettled.Load() {
		t.Fatal("a poll whose packs reply was a server state settled")
	}
	if got := a.serverMode.Load(); got != 6 {
		t.Fatalf("serverMode = %d, want 6", got)
	}
	if a.gameState.Load() != stateActive {
		t.Fatal("the idle reading cleared the game the packs slot said was on")
	}
	if a.printServerGate() == nil {
		t.Fatal("the print-server gate opened during the game")
	}
}

// A frame lost unseen during a poll could have been the server state, so the
// poll does not settle. Here more frames come in than the client keeps.
func TestAPollThatLostAFrameDoesNotSettle(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		}
		var frames []map[string]any
		for i := 0; i < 40; i++ {
			frames = append(frames, map[string]any{"CMD": "GETTEAMINFO", "I": i})
		}
		return append(frames, packsReply()), false
	})

	a := &App{}
	a.gameState.Store(stateActive)
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if !a.pollUnsettled.Load() {
		t.Fatal("a poll that lost frames settled")
	}
	if a.gameState.Load() != stateActive {
		t.Fatal("a poll that lost frames cleared the game")
	}
}

// A game in progress is reported as one. Every in-game poll is unsettled, and
// the gate used to give that reason instead, so operators were told the
// replies were out of step on every game.
func TestAGameInProgressIsReportedAsOne(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 4}}, false
		}
		return []map[string]any{packsReply()}, false
	})

	a := &App{}
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if err := a.printServerGate(); !errors.Is(err, errGameActive) {
		t.Fatalf("gate = %v during a game, want errGameActive", err)
	}
}

// The slow poll's commands stop at the first that fails. Each can wait out the
// reply timeout while nothing reads the server state, and three in a row held
// the gate on the poll's verdict three times as long as one.
func TestASlowPollStopsAtTheFirstFailedCommand(t *testing.T) {
	var mu sync.Mutex
	var asked []string
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		mu.Lock()
		asked = append(asked, cmd)
		mu.Unlock()
		switch cmd {
		case "GETSERVERSTATE":
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		case "GETACTIVEPACKS":
			return []map[string]any{packsReply()}, false
		case "GETTEAMINFO":
			// Never answered: enough other traffic that the call gives up.
			frames := make([]map[string]any, 64)
			for i := range frames {
				frames[i] = map[string]any{"CMD": "GETGAMELIST", "I": i}
			}
			return frames, false
		}
		return []map[string]any{{"CMD": cmd}}, false
	})

	a := &App{buf: buffer.New(1)}
	if _, _, err := a.collect(client, true); err != nil {
		t.Fatal(err)
	}
	// One more request: its reply means O-Zone has read every request before it.
	if _, err := client.Command("GETSERVERSTATE"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, cmd := range asked {
		if cmd == "GETGAMELIST" || cmd == "GETFEATURELICINFO" {
			t.Fatalf("asked %v: the slow poll went on after GETTEAMINFO failed", asked)
		}
	}
}

// What a poll lost is its own: frames lost before it asked (here, more than
// the client keeps, between polls) do not leave it unsettled.
func TestLossesBeforeAPollDoNotUnsettleIt(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		switch cmd {
		case "GETSERVERSTATE":
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		case "GETACTIVEPACKS":
			return []map[string]any{packsReply()}, false
		}
		frames := make([]map[string]any, 40)
		for i := range frames {
			frames[i] = map[string]any{"CMD": "GETGAMELIST", "I": i}
		}
		return append(frames, map[string]any{"CMD": cmd}), false
	})

	a := &App{}
	// A call between polls that loses frames: more come in than are kept.
	if _, err := client.Command("GETTEAMINFO"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if a.pollUnsettled.Load() {
		t.Fatal("frames lost before the poll left it unsettled")
	}
}

// Another client's server state from before a game started can be the frame a
// poll takes, and that client's packs reply the one that ends the poll, ahead
// of this poll's own server state. The first poll after the bus reports the
// game may then hold nothing but the old idle reading; it may not clear the
// game. The next clean poll can.
func TestAPollRightAfterAGameStartCannotClearIt(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		switch {
		case n == 3:
			return []map[string]any{{"CMD": cmd, "PACKS": []any{}, "OTHERCLIENT": true}}, false
		case cmd == "GETSERVERSTATE":
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		}
		return []map[string]any{packsReply()}, false
	})

	a := &App{}
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if err := a.printServerGate(); err != nil {
		t.Fatalf("gate = %v after a clean idle poll, want open", err)
	}

	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameStart})
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if a.gameState.Load() != stateActive {
		t.Fatal("the first poll after the game started cleared it on an old idle reading")
	}
	if a.printServerGate() == nil {
		t.Fatal("the print-server gate opened on the first poll after the game started")
	}

	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if a.gameState.Load() != stateIdle {
		t.Fatal("a clean idle poll after that did not clear the game")
	}
}

// A server state saying a game is on closes the gate however it turns up. Here
// it comes after the poll's verdict, behind the packs reply, where no call
// takes it.
func TestAServerStateArrivingAfterThePollClosesTheGate(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		}
		return []map[string]any{packsReply(), {"CMD": "GETSERVERSTATE", "SERVERMODE": 4}}, false
	})

	a := &App{}
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for a.printServerGate() == nil {
		if time.Now().After(deadline) {
			t.Fatal("the gate stayed open after a server state said a game was on")
		}
		time.Sleep(time.Millisecond)
	}
	// Arriving before the verdict, the frame unsettles the poll and the
	// verdict records the poll's own reading; after it, the frame's. The game
	// is active either way.
	if a.gameState.Load() != stateActive {
		t.Fatal("a server state saying a game was on did not mark it active")
	}
}

// An active reading dropped as an extra frame closes the gate when it arrives,
// not when GETACTIVEPACKS returns; and it is kept if that call fails.
func TestAnActiveReadingClosesTheGateWhileThePollWaits(t *testing.T) {
	a := &App{}
	var closedMidPoll atomic.Bool
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{
				{"CMD": cmd, "SERVERMODE": 2},
				{"CMD": cmd, "SERVERMODE": 4},
			}, false
		}
		deadline := time.Now().Add(2 * time.Second)
		for a.printServerGate() == nil && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		closedMidPoll.Store(a.printServerGate() != nil)
		return nil, true
	})
	a.noteServerMode(2)
	a.gameState.Store(stateIdle)

	if _, _, err := a.collect(client, false); err == nil {
		t.Fatal("collect() succeeded although GETACTIVEPACKS hung up")
	}
	if !closedMidPoll.Load() {
		t.Fatal("the gate stayed open while GETACTIVEPACKS waited, after an active reading arrived")
	}
	if got := a.serverMode.Load(); got != 4 || a.gameState.Load() != stateActive {
		t.Fatalf("serverMode = %d, game state %d; want 4 and active", got, a.gameState.Load())
	}
}

// After a game, the gate opens on the second clean idle poll: the last active
// reading is a sign of a game during the poll that read it, and the next poll
// may not clear the game on its own.
func TestAGameEndsOnTheSecondCleanIdlePoll(t *testing.T) {
	modes := []int{4, 2, 2}
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": modes[n/2]}}, false
		}
		return []map[string]any{packsReply()}, false
	})

	a := &App{}
	for i, wantActive := range []bool{true, true, false} {
		if _, _, err := a.collect(client, false); err != nil {
			t.Fatal(err)
		}
		if active := a.gameState.Load() == stateActive; active != wantActive {
			t.Fatalf("after poll %d (mode %d): game active = %v, want %v", i, modes[i], active, wantActive)
		}
		if open := a.printServerGate() == nil; open == wantActive {
			t.Fatalf("after poll %d (mode %d): gate open = %v", i, modes[i], open)
		}
	}
}

// While the last poll is unsettled the next comes at the fast rate, so the
// clean poll that settles it does not wait out the idle interval.
func TestAnUnsettledPollIsFollowedByAFastOne(t *testing.T) {
	a := New(config.Config{PollInterval: 5 * time.Second, IdlePollInterval: 15 * time.Second})
	a.noteServerMode(2)
	a.gameState.Store(stateIdle)
	if got := a.nextPollInterval(); got != 15*time.Second {
		t.Fatalf("nextPollInterval() = %s idle and settled, want 15s", got)
	}
	a.pollUnsettled.Store(true)
	if got := a.nextPollInterval(); got != 5*time.Second {
		t.Fatalf("nextPollInterval() = %s while unsettled, want 5s", got)
	}

	// O-Zone starting up or shutting down: unsettled, but no sooner poll can
	// settle it, so the idle rate stands.
	a.noteServerMode(11)
	if got := a.nextPollInterval(); got != 15*time.Second {
		t.Fatalf("nextPollInterval() = %s unsettled on mode 11, want 15s", got)
	}
}

// A finished game's follow-up work that finds the print server off-limits is
// not dropped: the next poll that finds it open runs it. It used to wait for
// the next game to end, and with results polling off nothing else drained the
// fetches the bus had queued.
func TestAFinishedGamesWorkWaitsForTheGateToOpen(t *testing.T) {
	a, ps := newSimApp(t)
	a.queueFetch(9)
	a.pollUnsettled.Store(true) // the poll confirming the game's end has not come

	a.finishWork()
	if !a.finishPending.Load() {
		t.Fatal("work refused at the gate was not left for a later poll")
	}
	if n := ps.Connections(); n != 0 {
		t.Fatalf("print server contacted %d time(s) with the gate shut", n)
	}

	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		}
		return []map[string]any{packsReply()}, false
	})
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for ps.Connections() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the poll that opened the gate did not run the finished game's fetch")
		}
		time.Sleep(time.Millisecond)
	}
	if a.finishPending.Load() {
		t.Fatal("the work was run but is still marked pending")
	}
}

// A finished game's fetch that the drain has to put off part-way (here the
// results lock is busy, as during a cache refresh) is tried again at the next
// poll that finds the print server open. It used to be marked done before the
// drain ran, and waited for another game to end.
func TestAFinishedGamesFetchPutOffMidDrainIsRetried(t *testing.T) {
	a, ps := newSimApp(t)
	a.queueFetch(9)

	a.resultsMu.Lock()
	a.finishWork()
	a.resultsMu.Unlock()
	if !a.finishPending.Load() {
		t.Fatal("a fetch put off mid-drain was not left for a later poll")
	}
	if n := ps.Connections(); n != 0 {
		t.Fatalf("print server contacted %d time(s) while the results lock was held", n)
	}

	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		}
		return []map[string]any{packsReply()}, false
	})
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for ps.Connections() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the next open poll did not retry the fetch put off mid-drain")
		}
		time.Sleep(time.Millisecond)
	}
}

// A game the bus reports finished is not fetched before GAME_FINISH_DELAY,
// whichever drain reaches it: here one run early, as finish work left pending
// from an earlier game would once the gate opened inside the delay.
func TestABusFinishedGameIsNotFetchedBeforeTheDelay(t *testing.T) {
	a, ps := newSimApp(t)
	a.cfg.GameFinishDelay = 300 * time.Millisecond

	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameStart, Args: []string{"42"}})
	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameFinish})
	a.handleBusEvent(msgbus.Event{Code: msgbus.EventIdle}) // the gate opens inside the delay

	if deferred := a.drainPendingFetches(); deferred {
		t.Fatal("a game not yet due was reported put off")
	}
	if n := ps.Connections(); n != 0 {
		t.Fatalf("print server contacted %d time(s) inside the finish delay", n)
	}

	deadline := time.Now().Add(3 * time.Second)
	for ps.Connections() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the game was not fetched once the finish delay was over")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A drain reports a due game it put off for want of a safe moment, so finish
// work can be tried again: the gate shut under it, or another drain running.
func TestADrainReportsAGameItPutOff(t *testing.T) {
	a, ps := newSimApp(t)
	a.queueFetch(9)

	a.pollUnsettled.Store(true)
	if !a.drainPendingFetches() {
		t.Fatal("a game refused at the gate was not reported put off")
	}
	a.pollUnsettled.Store(false)

	a.fetchBusy.Store(true)
	if !a.drainPendingFetches() {
		t.Fatal("a drain that found another running did not report the work put off")
	}
	a.fetchBusy.Store(false)

	if n := ps.Connections(); n != 0 {
		t.Fatalf("print server contacted %d time(s)", n)
	}
}

// Nor is it cached before the delay: the finish work's cache refresh skips it
// while the delay runs, and the refresh after the delay caches it.
func TestABusFinishedGameIsNotCachedBeforeTheDelay(t *testing.T) {
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
		CacheEnabled:     true,
		CacheDir:         t.TempDir(),
		CentralURL:       "http://central.invalid",
		Token:            "test",
		GameFinishDelay:  300 * time.Millisecond,
	})
	a.noteServerMode(1)
	a.gameState.Store(stateIdle)

	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameStart, Args: []string{"9"}})
	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameFinish})
	a.handleBusEvent(msgbus.Event{Code: msgbus.EventIdle}) // the gate opens inside the delay

	a.refreshCache()
	if n := ps.Requests("all"); n != 0 {
		t.Fatalf("game fetched %d time(s) for the cache inside the finish delay", n)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, ok := a.cache.GameRaw(9); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the game was not cached once the finish delay was over")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
