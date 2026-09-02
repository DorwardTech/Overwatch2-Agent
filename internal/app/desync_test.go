package app

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/ozonefix"
	"overwatch/agent/internal/ozoneproto"
)

// faultyPrintServer is a print server that can be told to misbehave on a given
// fetch — answer with a DIFFERENT game's payload, or hang up mid-frame.
// ozonesim.PrintServer deliberately always behaves correctly, so the faults that
// desynchronise a connection get their own fake rather than being built into the
// simulator the demos ship with.
type faultyPrintServer struct {
	ln       net.Listener
	listJSON []byte

	mu    sync.Mutex
	conns int
	alls  int

	// onAll answers the nth (1-based) "all" request of the server's life. A nil
	// body with hangUp=true truncates the frame and drops the connection.
	onAll func(gameNumber, nth int) (body []byte, hangUp bool)
}

func startFaultyPrintServer(t *testing.T, listJSON string, onAll func(gameNumber, nth int) ([]byte, bool)) *faultyPrintServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ps := &faultyPrintServer{ln: ln, listJSON: []byte(listJSON), onAll: onAll}
	t.Cleanup(func() { _ = ln.Close() })
	go ps.acceptLoop()
	return ps
}

func (p *faultyPrintServer) connections() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.conns
}

func (p *faultyPrintServer) acceptLoop() {
	for {
		conn, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		p.conns++
		p.mu.Unlock()
		go p.handle(conn)
	}
}

func (p *faultyPrintServer) handle(conn net.Conn) {
	defer conn.Close()

	// The 2-frame connect banner real O-Zone pushes, which the agent drains.
	for _, frame := range [][]byte{
		ozonefix.Compact(ozonefix.EventTypesBannerJSON()),
		ozonefix.Compact(ozonefix.TextsBannerJSON()),
	} {
		if _, err := conn.Write(ozoneproto.Frame(frame)); err != nil {
			return
		}
	}

	for {
		payload, err := ozoneproto.ReadFrame(conn)
		if err != nil {
			return
		}
		var req map[string]any
		if err := json.Unmarshal(payload, &req); err != nil {
			continue
		}
		if _, ok := req["success"]; ok {
			continue // data-acknowledged: no reply
		}
		switch req["command"] {
		case "list":
			if _, err := conn.Write(ozoneproto.Frame(p.listJSON)); err != nil {
				return
			}
		case "all":
			num := int(req["gamenumber"].(float64))
			p.mu.Lock()
			p.alls++
			nth := p.alls
			p.mu.Unlock()

			body, hangUp := p.onAll(num, nth)
			if hangUp {
				// A header promising more bytes than follow: the client's read
				// fails part-way through the frame, exactly as a timeout or a
				// reset mid-reply does.
				_, _ = conn.Write([]byte{0x64, 0x00, 0x00, 0x00, ozoneproto.TokenByte})
				_, _ = conn.Write([]byte(`{"game":`))
				return
			}
			if _, err := conn.Write(ozoneproto.Frame(body)); err != nil {
				return
			}
		default:
			_, _ = conn.Write(ozoneproto.Frame([]byte(`{"error":"Unknown command"}`)))
		}
	}
}

// gamePayload is a minimal "all" response identifying itself as a given game.
func gamePayload(gameNumber int) []byte {
	return []byte(`{"game":{"gamenum":` + itoa(gameNumber) + `,"gamename":"g"},"players":{}}`)
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

// pushedGames records the game numbers central was asked to store.
type pushedGames struct {
	mu   sync.Mutex
	seen []map[string]any
}

func (g *pushedGames) add(m map[string]any) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.seen = append(g.seen, m)
}

func (g *pushedGames) all() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]any(nil), g.seen...)
}

// newFaultApp wires an idle agent to a faulty print server and to a fake central
// that records every game-results push.
func newFaultApp(t *testing.T, ps *faultyPrintServer) (*App, *pushedGames) {
	t.Helper()
	pushed := &pushedGames{}
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			pushed.add(payload)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(central.Close)

	host, port, _ := net.SplitHostPort(ps.ln.Addr().String())
	a := New(config.Config{
		OzoneHost:        host,
		OzoneResultsPort: port,
		ResultsHandshake: 2,
		CentralURL:       central.URL + "/api/agent/ingest",
		Token:            "test",
		CacheEnabled:     true,
		CacheDir:         t.TempDir(),
	})
	a.noteServerMode(1) // idle
	a.gameState.Store(stateIdle)
	return a, pushed
}

const twoGameList = `{"gamelist":[` +
	`{"gamenum":9,"gamename":"g9","duration":601,"starttime":"s","endtime":"e","playercount":2,"valid":1},` +
	`{"gamenum":10,"gamename":"g10","duration":601,"starttime":"s","endtime":"e","playercount":2,"valid":1}]}`

// The results protocol has no request/response correlation, so a desynchronised
// connection answers the NEXT request with the PREVIOUS game's frame. Pushing
// that would file one game's data under another game's number at central — and
// markFetched would then stop the agent ever fetching the real thing. The reply
// must be checked against the game it claims to be.
func TestSyncGamesRejectsAReplyForAnotherGame(t *testing.T) {
	ps := startFaultyPrintServer(t, twoGameList, func(gameNumber, nth int) ([]byte, bool) {
		if nth == 1 {
			return gamePayload(gameNumber), false // game 9, answered correctly
		}
		return gamePayload(9), false // game 10 asked for; game 9's frame returned
	})
	a, pushed := newFaultApp(t, ps)

	status, result := a.syncGames(map[string]any{}, true)

	if status != "acked" {
		t.Fatalf("status = %q, want acked (one good game, one rejected)", status)
	}
	if result["synced"] != 1 || result["failed"] != 1 {
		t.Fatalf("result = %v, want synced=1 failed=1", result)
	}
	for _, p := range pushed.all() {
		if n, _ := p["game_number"].(float64); int(n) == 10 {
			t.Fatal("central was sent game 10 carrying another game's data")
		}
	}
	if len(pushed.all()) != 1 {
		t.Fatalf("central received %d pushes, want only the one good game", len(pushed.all()))
	}
	// The rejected game must stay unfetched so a later run can still get it.
	if a.isFetched(10) {
		t.Error("game 10 was marked fetched despite never being received")
	}
}

// A read error is a property of the connection, not of the game it happened on:
// the bytes left behind desynchronise everything that follows. The batch must
// reconnect rather than keep issuing requests down a poisoned socket.
func TestSyncGamesReconnectsAfterAReadError(t *testing.T) {
	ps := startFaultyPrintServer(t, twoGameList, func(gameNumber, nth int) ([]byte, bool) {
		if nth == 1 {
			return nil, true // game 9: hang up part-way through the frame
		}
		return gamePayload(gameNumber), false
	})
	a, pushed := newFaultApp(t, ps)

	status, result := a.syncGames(map[string]any{}, true)

	if status != "acked" {
		t.Fatalf("status = %q, want acked", status)
	}
	if result["synced"] != 1 {
		t.Fatalf("result = %v, want synced=1 (game 10 fetched over a fresh connection)", result)
	}
	if got := ps.connections(); got != 2 {
		t.Fatalf("print server saw %d connections, want 2 (the poisoned one must be replaced)", got)
	}
	got := pushed.all()
	if len(got) != 1 {
		t.Fatalf("central received %d pushes, want 1", len(got))
	}
	if n, _ := got[0]["game_number"].(float64); int(n) != 10 {
		t.Fatalf("central received game %v, want 10", got[0]["game_number"])
	}
}

// A print server failing every fetch must not hold the results lock while it
// grinds through the whole list — each failure now also costs a reconnect.
func TestSyncGamesStopsAfterRepeatedFailures(t *testing.T) {
	list := `{"gamelist":[`
	for n := 1; n <= 10; n++ {
		if n > 1 {
			list += ","
		}
		list += `{"gamenum":` + itoa(n) + `,"gamename":"g","duration":1,"starttime":"s","endtime":"e","playercount":1,"valid":1}`
	}
	list += `]}`

	ps := startFaultyPrintServer(t, list, func(gameNumber, nth int) ([]byte, bool) {
		return nil, true // every fetch hangs up
	})
	a, _ := newFaultApp(t, ps)

	status, result := a.syncGames(map[string]any{}, true)

	if status != "failed" {
		t.Fatalf("status = %q, want failed", status)
	}
	if result["failed"] != maxBatchFetchErrors {
		t.Fatalf("result = %v, want it to stop after %d failures", result, maxBatchFetchErrors)
	}
}

// A payload with a "game" section but no number of its own is not a mismatch —
// only a reply that positively identifies as a different game is rejected.
// A reply with no "game" section at all is not game data.
func TestPayloadGameNumber(t *testing.T) {
	cases := map[string]struct {
		number int
		isGame bool
	}{
		`{"game":{"gamenum":9}}`:     {9, true},
		`{"game":{"gamenum":"9"}}`:   {9, true},
		`{"game":{}}`:                {0, true},
		`{"error":"Game not found"}`: {0, false},
		`{"players":{}}`:             {0, false},
		`not json`:                   {0, false},
	}
	for raw, want := range cases {
		number, isGame := payloadGameNumber([]byte(raw))
		if number != want.number || isGame != want.isGame {
			t.Errorf("payloadGameNumber(%s) = (%d, %t), want (%d, %t)", raw, number, isGame, want.number, want.isGame)
		}
	}
}

// O-Zone answers a game it no longer holds with an error reply. That must not
// be cached as the game's verbatim payload: HasRaw would then advertise it to
// TORN, the proxy would serve the error blob in place of the game, and the
// agent would never fetch the real one. The connection is fine, though — the
// rest of the batch carries on over it.
func TestSyncGamesDoesNotCacheAnErrorReply(t *testing.T) {
	ps := startFaultyPrintServer(t, twoGameList, func(gameNumber, nth int) ([]byte, bool) {
		if gameNumber == 9 {
			return []byte(`{"error":"Game not found"}`), false
		}
		return gamePayload(gameNumber), false
	})
	a, pushed := newFaultApp(t, ps)

	status, result := a.syncGames(map[string]any{}, true)

	if status != "acked" {
		t.Fatalf("status = %q, want acked", status)
	}
	if result["synced"] != 1 || result["failed"] != 1 {
		t.Fatalf("result = %v, want synced=1 failed=1", result)
	}
	if a.store.HasRaw(9) {
		t.Error("an error reply was cached as game 9's payload")
	}
	if got := ps.connections(); got != 1 {
		t.Fatalf("print server saw %d connections, want 1 (a refused game does not poison the stream)", got)
	}
	got := pushed.all()
	if len(got) != 1 {
		t.Fatalf("central received %d pushes, want 1", len(got))
	}
	if n, _ := got[0]["game_number"].(float64); int(n) != 10 {
		t.Fatalf("central received game %v, want 10", got[0]["game_number"])
	}
}
