package proxy

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"testing"
	"time"

	"overwatch/agent/internal/cache"
	"overwatch/agent/internal/ozonefix"
	"overwatch/agent/internal/ozoneproto"
	"overwatch/agent/internal/store"
)

// startProxy seeds a cache with golden game #9 and starts a proxy on an
// ephemeral port. Returns the proxy and the verbatim "all" bytes that were cached.
func startProxy(t *testing.T) (*Server, []byte) {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allRaw := ozonefix.Compact(ozonefix.AllResponseJSON())
	if err := s.StoreGame(store.GameMeta{
		GameNumber: 9, GameName: "Competition Team Elimination", GameType: 1,
		StartTime: "2020-02-18 16:06:05", PlayerCount: 2,
	}, allRaw); err != nil {
		t.Fatal(err)
	}
	_ = s.UpsertListEntry(store.GameMeta{
		GameNumber: 9, GameName: "Competition Team Elimination", Duration: 601,
		StartTime: "2020-02-18 16:06:05", EndTime: "2020-02-18 16:16:06", PlayerCount: 2, Valid: 1,
	})

	p := New(cache.New(s), "127.0.0.1:0")
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p, allRaw
}

// dial connects and drains the 2-frame connect banner, like a real client.
func dial(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := ozoneproto.ReadFrame(conn); err != nil {
			t.Fatalf("banner frame %d: %v", i, err)
		}
	}
	return conn
}

// ask sends one command and returns the raw reply payload bytes (post-header).
func ask(t *testing.T, conn net.Conn, cmd map[string]any) []byte {
	t.Helper()
	body, _ := json.Marshal(cmd)
	if _, err := conn.Write(ozoneproto.Frame(body)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	payload, err := ozoneproto.ReadFrame(conn)
	if err != nil {
		t.Fatalf("read reply: %v", err)
	}
	return payload
}

// The headline fidelity guarantee: "all" is byte-verbatim with what O-Zone sent.
func TestProxyAllIsByteVerbatim(t *testing.T) {
	p, allRaw := startProxy(t)
	conn := dial(t, p.Addr())
	defer conn.Close()

	got := ask(t, conn, map[string]any{"command": "all", "gamenumber": 9})
	if !bytes.Equal(got, allRaw) {
		t.Fatalf("all reply not byte-verbatim:\n got %s\nwant %s", got, allRaw)
	}
}

// Every response must end in '}' so TORN's trailing-} read terminator fires.
func TestAllResponsesEndWithBrace(t *testing.T) {
	p, _ := startProxy(t)
	conn := dial(t, p.Addr())
	defer conn.Close()

	for _, cmd := range []map[string]any{
		{"command": "list"},
		{"command": "all", "gamenumber": 9},
		{"command": "minimal", "gamenumber": 9},
		{"command": "team", "gamenumber": 9, "ids": []string{"0"}},
		{"command": "player", "gamenumber": 9, "ids": []string{"1"}},
		{"command": "all", "gamenumber": 999}, // not found
		{"command": "bogus"},                  // unknown
		{"autoprint": 1},
	} {
		reply := ask(t, conn, cmd)
		if len(reply) == 0 || reply[len(reply)-1] != '}' {
			t.Errorf("reply to %v does not end with '}': %s", cmd, reply)
		}
	}
}

func TestProxyMinimalDropsEvents(t *testing.T) {
	p, _ := startProxy(t)
	conn := dial(t, p.Addr())
	defer conn.Close()

	var m map[string]json.RawMessage
	if err := json.Unmarshal(ask(t, conn, map[string]any{"command": "minimal", "gamenumber": 9}), &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["events"]; ok {
		t.Error("minimal must not include events")
	}
	if _, ok := m["players"]; !ok {
		t.Error("minimal must still include players")
	}
}

func TestProxyListShapeAndErrors(t *testing.T) {
	p, _ := startProxy(t)
	conn := dial(t, p.Addr())
	defer conn.Close()

	var list struct {
		GameList []map[string]any `json:"gamelist"`
	}
	if err := json.Unmarshal(ask(t, conn, map[string]any{"command": "list"}), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.GameList) != 1 || list.GameList[0]["gamenum"].(float64) != 9 {
		t.Fatalf("unexpected gamelist: %v", list.GameList)
	}

	// Missing game -> error object (TORN tolerates and retries).
	var e map[string]any
	_ = json.Unmarshal(ask(t, conn, map[string]any{"command": "all", "gamenumber": 123}), &e)
	if _, ok := e["error"]; !ok {
		t.Error("missing game should yield an error object")
	}

	// Auto-print is acknowledged.
	var ack map[string]any
	_ = json.Unmarshal(ask(t, conn, map[string]any{"autoprint": 1}), &ack)
	if ack["success"] != true {
		t.Errorf("autoprint ack = %v", ack)
	}
}

// The data-ack message must elicit no reply (so the next read is the next command).
func TestProxyAckIsSilent(t *testing.T) {
	p, _ := startProxy(t)
	conn := dial(t, p.Addr())
	defer conn.Close()

	body, _ := json.Marshal(map[string]any{"success": true})
	if _, err := conn.Write(ozoneproto.Frame(body)); err != nil {
		t.Fatal(err)
	}
	// The reply we read next must be the list, not an ack echo.
	var list struct {
		GameList []map[string]any `json:"gamelist"`
	}
	if err := json.Unmarshal(ask(t, conn, map[string]any{"command": "list"}), &list); err != nil {
		t.Fatalf("expected list after silent ack: %v", err)
	}
}

// The listener is unauthenticated and reachable from the whole venue LAN. A
// client that declares a huge frame and then stalls would, without a tighter
// inbound cap, have the proxy allocate the declared buffer up front and hold
// it — about a dozen such connections is the agent's whole container.
func TestOversizedRequestFrameIsRefused(t *testing.T) {
	p, _ := startProxy(t)
	conn := dial(t, p.Addr())
	defer conn.Close()

	// Declare 8 MiB — inside the protocol maximum, far past any real request —
	// and send nothing after the header.
	header := make([]byte, 5)
	binary.LittleEndian.PutUint32(header[:4], 8<<20)
	header[4] = ozoneproto.TokenByte
	if _, err := conn.Write(header); err != nil {
		t.Fatal(err)
	}

	// The proxy must drop the connection rather than wait on the body.
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("proxy answered an oversized request instead of closing")
	}
}

// A request that stops part-way through its body must not hold the connection
// open forever — but a client that has sent nothing at all is simply idle
// between games and must be left alone.
func TestPartialRequestBodyTimesOut(t *testing.T) {
	p, _ := startProxy(t)
	conn := dial(t, p.Addr())
	defer conn.Close()

	body := []byte(`{"command":"list"}`)
	header := make([]byte, 5)
	binary.LittleEndian.PutUint32(header[:4], uint32(len(body)))
	header[4] = ozoneproto.TokenByte
	if _, err := conn.Write(append(header, body[:4]...)); err != nil { // header + a fragment
		t.Fatal(err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(bodyTimeout + 5*time.Second))
	if _, err := io.ReadFull(conn, make([]byte, 1)); err == nil {
		t.Fatal("proxy answered a truncated request instead of closing")
	}
}

// Past the concurrency cap the proxy refuses rather than accepting and holding:
// an accepted connection costs a goroutine and a buffer, and being refused is
// recoverable (clients reconnect) where an OOM kill is not.
func TestConnectionsAreCapped(t *testing.T) {
	p, _ := startProxy(t)

	open := make([]net.Conn, 0, maxConns)
	defer func() {
		for _, c := range open {
			_ = c.Close()
		}
	}()
	for i := 0; i < maxConns; i++ {
		c, err := net.Dial("tcp", p.Addr())
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		open = append(open, c)
	}

	// Wait for the accept loop to have registered them all.
	deadline := time.Now().Add(5 * time.Second)
	for p.Connections() < maxConns && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := p.Connections(); got != maxConns {
		t.Fatalf("open connections = %d, want %d", got, maxConns)
	}

	extra, err := net.Dial("tcp", p.Addr())
	if err != nil {
		return // refused at the TCP level is also a refusal
	}
	defer extra.Close()
	_ = extra.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(extra, make([]byte, 1)); err == nil {
		t.Fatal("proxy served a connection past the cap instead of closing it")
	}
	if p.Refused() == 0 {
		t.Error("the refusal was not counted")
	}
}
