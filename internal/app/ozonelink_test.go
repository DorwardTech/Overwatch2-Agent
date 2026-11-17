package app

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"overwatch/agent/internal/buffer"
	"overwatch/agent/internal/config"
)

// fakeCentral records what the agent sends central: ingest pushes (with their
// Idempotency-Key and arrival time), command fetches and command acks.
type fakeCentral struct {
	srv         *httptest.Server
	commandGets atomic.Int32

	mu       sync.Mutex
	pushes   []centralPush
	acks     map[int]string
	fetchAts []time.Time // when each command fetch came in
	// commands answers the nth GET of the commands endpoint (from 1).
	commands func(n int) string
}

type centralPush struct {
	key  string
	at   time.Time
	body map[string]any
}

func newFakeCentral(t *testing.T) *fakeCentral {
	t.Helper()
	c := &fakeCentral{acks: map[int]string{}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/api/agent/commands"):
			n := int(c.commandGets.Add(1))
			body := `{"commands":[]}`
			c.mu.Lock()
			c.fetchAts = append(c.fetchAts, time.Now())
			if c.commands != nil {
				body = c.commands(n)
			}
			c.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(body))
			return
		case strings.HasSuffix(r.URL.Path, "/ack"):
			parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/ack"), "/")
			id, _ := strconv.Atoi(parts[len(parts)-1])
			var ack struct {
				Status string `json:"status"`
			}
			_ = json.Unmarshal(data, &ack)
			c.mu.Lock()
			c.acks[id] = ack.Status
			c.mu.Unlock()
		case strings.HasSuffix(r.URL.Path, "/api/agent/ingest"):
			var body map[string]any
			_ = json.Unmarshal(data, &body)
			c.mu.Lock()
			c.pushes = append(c.pushes, centralPush{key: r.Header.Get("Idempotency-Key"), at: time.Now(), body: body})
			c.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// firstTelemetry is the oldest buffered entry that is not a heartbeat; the
// heartbeats ahead of it are dropped.
func firstTelemetry(a *App) (buffer.Entry, bool) {
	for {
		e, ok := a.buf.Peek()
		if !ok || !isHeartbeat(e.Key) {
			return e, ok
		}
		a.buf.PopSent(e)
	}
}

func (c *fakeCentral) ingestURL() string { return c.srv.URL + "/api/agent/ingest" }

// heartbeats returns the pushes that came in as heartbeats.
func (c *fakeCentral) heartbeats() []centralPush {
	c.mu.Lock()
	defer c.mu.Unlock()
	var hb []centralPush
	for _, p := range c.pushes {
		if strings.HasPrefix(p.key, heartbeatKeyPrefix) {
			hb = append(hb, p)
		}
	}
	return hb
}

func (c *fakeCentral) all() []centralPush {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]centralPush(nil), c.pushes...)
}

func (c *fakeCentral) ack(id int) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.acks[id]
}

// closedPort is a local port nothing listens on: a dial is refused at once.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	_ = ln.Close()
	return port
}

// linkConfig is a fixed-host agent for the game server at 127.0.0.1:port,
// reporting to central, with short timings.
func linkConfig(central *fakeCentral, port string, idle time.Duration) config.Config {
	return config.Config{
		OzoneHost:        "127.0.0.1",
		OzonePort:        port,
		OzoneResultsPort: "1", // nothing listens; and nothing may dial it
		CentralURL:       central.ingestURL(),
		Token:            "test",
		Version:          "1.12.0-test",
		PollInterval:     idle,
		IdlePollInterval: idle,
		SlowPollInterval: 4 * idle,
		HealthAddr:       "127.0.0.1:0",
		BufferMax:        100,
		CommandsEnabled:  true,
	}
}

// linkOf is a push's agent.ozone_link block (nil if it has none).
func linkOf(p centralPush) map[string]any {
	agent, _ := p.body["agent"].(map[string]any)
	link, _ := agent["ozone_link"].(map[string]any)
	return link
}

func TestOzoneLinkTransitionsAndReport(t *testing.T) {
	t0 := time.Date(2026, 11, 16, 1, 0, 0, 0, time.UTC)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	stamp := func(s int) string { return at(s).Format(time.RFC3339) }

	var l ozoneLink
	l.start(false, 30*time.Second, at(0))
	r := l.report()
	if r["state"] != "unreachable" || r["since"] != stamp(0) || r["last_ok_at"] != nil ||
		r["last_error"] != "not connected yet" || r["attempts"] != 0 {
		t.Fatalf("before any attempt: %v", r)
	}

	l.failed(errors.New("dial tcp 10.0.0.5:9090: connect: connection refused"), at(1))
	l.failed(errors.New("dial tcp 10.0.0.5:9090: i/o timeout"), at(3))
	r = l.report()
	if r["state"] != "unreachable" || r["since"] != stamp(0) || r["attempts"] != 2 ||
		r["last_error"] != "dial tcp 10.0.0.5:9090: i/o timeout" {
		t.Fatalf("after two failures: %v", r)
	}

	l.ok(at(5))
	r = l.report()
	if _, has := r["last_error"]; has {
		t.Fatalf("connected, with a last_error: %v", r)
	}
	if r["state"] != "connected" || r["since"] != stamp(5) || r["last_ok_at"] != stamp(5) || r["attempts"] != 0 {
		t.Fatalf("connected: %v", r)
	}
	l.ok(at(20)) // still connected: since stays
	if r = l.report(); r["since"] != stamp(5) || r["last_ok_at"] != stamp(20) {
		t.Fatalf("connected again: %v", r)
	}

	// Errors are kept on one line and to 200 characters.
	l.failed(errors.New("GETSERVERSTATE: no reply\nwithin 10s "+strings.Repeat("é", 300)), at(30))
	r = l.report()
	text, _ := r["last_error"].(string)
	if r["state"] != "unreachable" || r["since"] != stamp(30) || r["last_ok_at"] != stamp(20) || r["attempts"] != 1 ||
		!strings.HasPrefix(text, "GETSERVERSTATE: no reply within 10s éé") || len([]rune(text)) != 200 || strings.Contains(text, "\n") {
		t.Fatalf("after the link dropped: %v (%d characters)", r, len([]rune(text)))
	}

	// Auto mode: discovering, with what /readyz says, until a server is
	// chosen; then not connected yet until the first attempt has an outcome.
	var d ozoneLink
	d.start(true, 30*time.Second, at(0))
	if r = d.report(); r["state"] != "discovering" || r["since"] != stamp(0) || r["last_error"] != "" {
		t.Fatalf("auto mode at start: %v", r)
	}
	d.discovering("waiting for the game server's announcement on UDP 12106", at(1))
	if r = d.report(); r["state"] != "discovering" || r["since"] != stamp(0) ||
		r["last_error"] != "waiting for the game server's announcement on UDP 12106" || r["attempts"] != 0 {
		t.Fatalf("discovering: %v", r)
	}
	d.chosen(at(12))
	if r = d.report(); r["state"] != "unreachable" || r["since"] != stamp(12) || r["last_error"] != "not connected yet" {
		t.Fatalf("chosen: %v", r)
	}
	d.ok(at(13))
	d.chosen(at(14)) // only ever leaves discovering
	if r = d.report(); r["state"] != "connected" {
		t.Fatalf("chosen moved a connected link: %v", r)
	}
}

// No heartbeat at start until an attempt has failed or two heartbeat
// intervals have passed; none while the link is connected.
func TestHeartbeatWaitsOutTheStartGrace(t *testing.T) {
	cfg := config.Config{OzoneHost: "127.0.0.1", OzonePort: "1", CentralURL: "http://central.invalid",
		Token: "test", Version: "1.12.0-test", IdlePollInterval: 15 * time.Second, BufferMax: 10}

	a := New(cfg)
	// Two intervals would be 30 s, the Agent offline alert's own limit: the
	// grace stops at 20.
	if a.heartbeat(a.startedAt.Add(19*time.Second)) || a.buf.Len() != 0 {
		t.Fatal("a heartbeat went inside the start grace with no attempt failed")
	}
	if !a.heartbeat(a.startedAt.Add(20*time.Second)) || a.buf.Len() != 1 {
		t.Fatal("no heartbeat once the start grace had passed")
	}
	e, _ := a.buf.Peek()
	var body map[string]any
	if err := json.Unmarshal(e.Data, &body); err != nil {
		t.Fatal(err)
	}
	if e.Key != "hb-"+strconv.FormatInt(int64(body["push_seq"].(float64)), 10) {
		t.Fatalf("buffer key %q for push_seq %v", e.Key, body["push_seq"])
	}
	if link := body["agent"].(map[string]any)["ozone_link"].(map[string]any); link["last_error"] != "not connected yet" {
		t.Fatalf("ozone_link = %v", link)
	}

	b := New(cfg)
	b.ozLink.failed(errors.New("connection refused"), b.startedAt.Add(time.Second))
	if !b.heartbeat(b.startedAt.Add(2 * time.Second)) {
		t.Fatal("no heartbeat after a failed attempt inside the start grace")
	}
	b.ozLink.ok(b.startedAt.Add(3 * time.Second))
	if b.heartbeat(b.startedAt.Add(time.Hour)) || b.buf.Len() != 1 {
		t.Fatal("a heartbeat went while the link was connected")
	}
}

// The game server refuses every dial: the agent still reports to central at
// the idle rate, with its version and why, and with no server_state or packs
// keys at all; it touches none of the readings the print-server gate goes by,
// and a delivered heartbeat does not make /readyz say ready.
func TestHeartbeatWhileTheGameServerIsUnreachable(t *testing.T) {
	central := newFakeCentral(t)
	a := New(linkConfig(central, closedPort(t), 50*time.Millisecond))
	runApp(t, a)

	waitUntil(t, "three heartbeats", 5*time.Second, func() bool { return len(central.heartbeats()) >= 3 })

	for _, p := range central.all() {
		if !strings.HasPrefix(p.key, heartbeatKeyPrefix) {
			t.Fatalf("a push other than a heartbeat (key %q): %v", p.key, p.body)
		}
		for _, k := range []string{"server_state", "packs", "team_info", "game_list", "licence_info", "site_info"} {
			if _, has := p.body[k]; has {
				t.Fatalf("heartbeat carries %q: %v", k, p.body)
			}
		}
		if p.key != "hb-"+strconv.FormatInt(int64(p.body["push_seq"].(float64)), 10) {
			t.Fatalf("Idempotency-Key %q for push_seq %v", p.key, p.body["push_seq"])
		}
		if _, ok := p.body["client_ts"].(string); !ok {
			t.Fatalf("no client_ts: %v", p.body)
		}
		agent, _ := p.body["agent"].(map[string]any)
		if agent["version"] != "1.12.0-test" || agent["print_server"] == nil || agent["frames"] == nil {
			t.Fatalf("self-report = %v", agent)
		}
		link := linkOf(p)
		text, _ := link["last_error"].(string)
		if link["state"] != "unreachable" || link["since"] == nil || link["last_ok_at"] != nil ||
			link["attempts"].(float64) < 1 || !strings.Contains(text, "127.0.0.1") {
			t.Fatalf("ozone_link = %v", link)
		}
	}

	if ov := a.Overview()["ozone_link"].(map[string]any); ov["state"] != "unreachable" {
		t.Fatalf("overview ozone_link = %v", ov)
	}
	// The gate stays shut, on the reading it never had, and nothing the
	// gate, discovery or the push throttle go by was written.
	if err := a.printServerGate(); !errors.Is(err, errLinkStale) {
		t.Fatalf("gate = %v after heartbeats, want errLinkStale", err)
	}
	if a.serverModeAt.Load() != 0 || a.serverModeWall.Load() != 0 || a.serverMode.Load() != 0 ||
		a.pollUnsettled.Load() || a.gameState.Load() != stateIdle || a.lastPushAt.Load() != 0 {
		t.Fatal("a heartbeat touched the server-state readings or the push throttle")
	}
	if a.health.Ready() {
		t.Fatal("/readyz says ready on heartbeats alone")
	}
}

// Commands are fetched while the game server is unreachable: one that needs
// only the agent runs, one that needs the print server is deferred.
func TestCommandsAreFetchedWhileTheGameServerIsUnreachable(t *testing.T) {
	central := newFakeCentral(t)
	central.commands = func(n int) string {
		if n == 1 {
			return `{"commands":[{"id":1,"type":"agent_overview"},{"id":2,"type":"refetch_game","payload":{"game_number":9}}]}`
		}
		return `{"commands":[]}`
	}
	cfg := linkConfig(central, closedPort(t), 50*time.Millisecond)
	cfg.ResultsEnabled = true
	a := New(cfg)
	runApp(t, a)

	waitUntil(t, "two command fetches", 5*time.Second, func() bool { return central.commandGets.Load() >= 2 })
	waitUntil(t, "both acks", 5*time.Second, func() bool { return central.ack(1) != "" && central.ack(2) != "" })
	if central.ack(1) != "acked" || central.ack(2) != "deferred" {
		t.Fatalf("acks: agent_overview %q, refetch_game %q", central.ack(1), central.ack(2))
	}
	// At the slow rate (200ms, four heartbeats), not on every heartbeat.
	central.mu.Lock()
	defer central.mu.Unlock()
	for i := 1; i < len(central.fetchAts); i++ {
		if gap := central.fetchAts[i].Sub(central.fetchAts[i-1]); gap < 100*time.Millisecond {
			t.Fatalf("command fetches %s apart, want the slow poll interval", gap)
		}
	}
}

func TestNoHeartbeatWhileTheGameServerAnswers(t *testing.T) {
	ws := newFakeOzoneWS(t, 1)
	central := newFakeCentral(t)
	a := New(linkConfig(central, ws.port, 100*time.Millisecond))
	runApp(t, a)

	waitUntil(t, "five pushes", 5*time.Second, func() bool { return len(central.all()) >= 5 })
	pushes := central.all()
	for _, p := range pushes {
		if strings.HasPrefix(p.key, heartbeatKeyPrefix) || p.body["server_state"] == nil {
			t.Fatalf("a heartbeat while connected: key %q, %v", p.key, p.body)
		}
	}
	// The first poll is slow: its own self-report already says connected.
	if link := linkOf(pushes[0]); link["state"] != "connected" || link["last_ok_at"] == nil {
		t.Fatalf("first push's ozone_link = %v", link)
	}
	if !a.health.Ready() {
		t.Fatal("/readyz not ready with telemetry delivered")
	}
}

// The game server goes away: heartbeats start at once (an attempt has
// failed), say when the link last worked, and stop when it is back.
func TestHeartbeatAfterTheGameServerGoesAway(t *testing.T) {
	ws := newFakeOzoneWS(t, 1)
	central := newFakeCentral(t)
	a := New(linkConfig(central, ws.port, 50*time.Millisecond))
	runApp(t, a)

	waitUntil(t, "telemetry", 5*time.Second, func() bool { return len(central.all()) >= 2 })
	ws.kill()
	waitUntil(t, "a heartbeat", 5*time.Second, func() bool { return len(central.heartbeats()) >= 2 })
	hb := central.heartbeats()
	link := linkOf(hb[len(hb)-1])
	if link["state"] != "unreachable" || link["last_ok_at"] == nil || link["last_error"] == "" || link["attempts"].(float64) < 1 {
		t.Fatalf("ozone_link after the drop = %v", link)
	}
}

// A dial that hangs fails nothing: the first heartbeat waits out the start
// grace, and says the agent is not connected yet.
func TestHeartbeatAfterTheGraceWhenTheFirstDialHangs(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	central := newFakeCentral(t)
	a := New(linkConfig(central, port, 100*time.Millisecond))
	runApp(t, a)
	// Registered after runApp, so it runs first: the dial returns and Run
	// can end.
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range held {
			_ = c.Close()
		}
	})

	waitUntil(t, "a heartbeat", 5*time.Second, func() bool { return len(central.heartbeats()) >= 1 })
	first := central.heartbeats()[0]
	if grace := 200 * time.Millisecond; first.at.Sub(a.startedAt) < grace {
		t.Fatalf("first heartbeat %s after start, inside the %s grace", first.at.Sub(a.startedAt), grace)
	}
	if link := linkOf(first); link["state"] != "unreachable" || link["last_error"] != "not connected yet" ||
		link["attempts"].(float64) != 0 || link["last_ok_at"] != nil {
		t.Fatalf("ozone_link = %v", link)
	}
}

// In auto mode, with no game server announced, the heartbeat says the agent
// is still looking, and what it has heard.
func TestHeartbeatWhileDiscovering(t *testing.T) {
	ws := newFakeOzoneWS(t, 1)
	central := newFakeCentral(t)
	cfg := autoConfig(ws.port)
	cfg.CentralURL, cfg.HealthAddr, cfg.Version = central.ingestURL(), "127.0.0.1:0", "1.12.0-test"
	a := discoveryApp(t, cfg, 0)
	runApp(t, a)

	waitUntil(t, "a heartbeat after the window", 5*time.Second, func() bool {
		hb := central.heartbeats()
		return len(hb) > 0 && strings.Contains(linkOf(hb[len(hb)-1])["last_error"].(string), "no game server announcement heard")
	})
	for _, p := range central.heartbeats() {
		link := linkOf(p)
		text, _ := link["last_error"].(string)
		if link["state"] != "discovering" || link["attempts"].(float64) != 0 || !strings.Contains(text, "announcement") {
			t.Fatalf("ozone_link = %v", link)
		}
		if agent := p.body["agent"].(map[string]any); agent["version"] != "1.12.0-test" || agent["lib"] == nil {
			t.Fatalf("self-report = %v", agent)
		}
	}
	if ws.conns.Load() != 0 {
		t.Fatal("dialled a game server nobody announced")
	}
}

// A heartbeat delivered to central says the agent is up, not that telemetry
// is flowing: /readyz stays not ready until telemetry is delivered.
func TestADeliveredHeartbeatDoesNotMakeTheAgentReady(t *testing.T) {
	a, received := newDeliverApp(t, func(string) int { return http.StatusOK })

	a.deliver("hb-7", []byte(`{"push_seq":7,"agent":{"version":"1.12.0"}}`))
	if received.Load() != 1 || a.buf.Len() != 0 {
		t.Fatal("the heartbeat was not delivered")
	}
	if a.health.Ready() {
		t.Fatal("ready after a heartbeat")
	}
	a.deliver("8", []byte(`{"push_seq":8,"server_state":{"SERVERMODE":1},"packs":[]}`))
	if !a.health.Ready() {
		t.Fatal("not ready after telemetry")
	}
}

// collect and the heartbeat take push_seqs from different goroutines: every
// one is distinct (and the race detector sees no race on the counter).
func TestPushSeqIsUniqueAcrossCollectAndHeartbeat(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		switch cmd {
		case "GETSERVERSTATE":
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 1}}, false
		case "GETACTIVEPACKS":
			return []map[string]any{packsReply()}, false
		}
		return []map[string]any{{"CMD": cmd}}, false
	})
	a := &App{buf: buffer.New(1000)}
	a.seq.Store(1000)

	const n = 50
	var polled, beat []int64
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for range n {
			_, key, err := a.collect(client, false)
			if err != nil {
				t.Error(err)
				return
			}
			seq, _ := strconv.ParseInt(key, 10, 64)
			polled = append(polled, seq)
		}
	}()
	go func() {
		defer wg.Done()
		for range n {
			beat = append(beat, a.sendHeartbeat(time.Now(), a.ozLink.report()))
		}
	}()
	wg.Wait()

	seen := map[int64]bool{}
	for _, s := range append(polled, beat...) {
		if seen[s] {
			t.Fatalf("push_seq %d taken twice", s)
		}
		seen[s] = true
	}
	if len(seen) != 2*n {
		t.Fatalf("%d distinct push_seqs, want %d", len(seen), 2*n)
	}
}

// The link goes down: the first heartbeat goes lossDelay later, not at the
// loop's next tick. Waiting for the tick could leave central without word for
// the failed poll's timeout plus a whole interval: past the Agent offline
// alert's 30 seconds at the default timings.
func TestFirstHeartbeatGoesAsSoonAsTheLinkIsLost(t *testing.T) {
	central := newFakeCentral(t)
	a := New(linkConfig(central, closedPort(t), time.Hour))
	a.lossDelay = 50 * time.Millisecond
	a.ozLink.ok(time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.heartbeatLoop(ctx)

	time.Sleep(50 * time.Millisecond)
	if a.buf.Len() != 0 {
		t.Fatalf("%d entries queued while the link was up", a.buf.Len())
	}

	a.ozLink.failed(errors.New("read tcp: i/o timeout"), time.Now())
	waitUntil(t, "a heartbeat right after the link went down", 2*time.Second, func() bool {
		e, ok := a.buf.Peek()
		return ok && isHeartbeat(e.Key)
	})

	// Only the first goes at once; the next waits its interval.
	a.ozLink.failed(errors.New("read tcp: i/o timeout"), time.Now())
	time.Sleep(100 * time.Millisecond)
	if n := a.buf.Len(); n != 1 {
		t.Fatalf("%d entries queued, want the one heartbeat", n)
	}
}

// While central is unreachable too, heartbeats replace each other in the
// buffer: one stays queued, the latest, behind the telemetry queued before
// it, which they no longer push out.
func TestHeartbeatsReplaceEachOtherInTheBuffer(t *testing.T) {
	central := newFakeCentral(t)
	cfg := linkConfig(central, closedPort(t), time.Hour)
	cfg.BufferMax = 5
	a := New(cfg)
	a.enqueue("telemetry-1", []byte(`{"push_seq":1}`))
	a.ozLink.failed(errors.New("connection refused"), time.Now())

	var last int64
	for i := 0; i < 20; i++ {
		if !a.ozLink.whileDown(time.Now(), func(link map[string]any) { last = a.sendHeartbeat(time.Now(), link) }) {
			t.Fatal("no heartbeat while the link is down")
		}
	}

	if n := a.buf.Len(); n != 2 {
		t.Fatalf("%d entries queued, want the telemetry and one heartbeat", n)
	}
	head, _ := a.buf.Peek()
	if head.Key != "telemetry-1" {
		t.Fatalf("head = %q, want the telemetry kept", head.Key)
	}
	a.buf.PopSent(head)
	hb, _ := a.buf.Peek()
	var body map[string]any
	_ = json.Unmarshal(hb.Data, &body)
	if hb.Key != heartbeatKeyPrefix+strconv.FormatInt(last, 10) || int64(body["push_seq"].(float64)) != last {
		t.Fatalf("queued heartbeat %q (push_seq %v), want the latest, %d", hb.Key, body["push_seq"], last)
	}
}

// A command the print-server gate defers comes back on every fetch, every
// minute for as long as the game server is off: each is acked deferred, but
// logged once, until the reason changes.
func TestADeferredCommandIsLoggedOncePerReason(t *testing.T) {
	logs := captureLog(t)
	central := newFakeCentral(t)
	central.commands = func(int) string {
		return `{"commands":[{"id":7,"type":"refetch_game","payload":{"game_number":9}}]}`
	}
	cfg := linkConfig(central, closedPort(t), time.Hour)
	cfg.ResultsEnabled = true
	a := New(cfg)

	for i := 0; i < 3; i++ {
		a.processCommands()
	}
	if central.ack(7) != "deferred" {
		t.Fatalf("ack = %q, want deferred", central.ack(7))
	}
	if n := strings.Count(logs.String(), "deferred refetch_game #7"); n != 1 {
		t.Fatalf("logged the deferral %d times over three fetches, want once:\n%s", n, logs.String())
	}
}

// The start grace: two intervals, never past 20 s, and in auto mode always
// longer than the discovery window, so a normal discovery is not reported
// however short the interval.
func TestTheStartGrace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		idle     time.Duration
		discover bool
		want     time.Duration
	}{
		{"fixed, short interval", 2 * time.Second, false, 4 * time.Second},
		{"fixed, default interval", 15 * time.Second, false, 20 * time.Second},
		{"fixed, long interval", 25 * time.Second, false, 20 * time.Second},
		{"auto, default interval", 15 * time.Second, true, 24 * time.Second},
		{"auto, short interval", 5 * time.Second, true, 24 * time.Second},
	} {
		a := New(config.Config{OzoneHost: "127.0.0.1", OzoneDiscover: tc.discover, IdlePollInterval: tc.idle, BufferMax: 10})
		if got := a.startGrace(); got != tc.want {
			t.Errorf("%s: start grace %s, want %s", tc.name, got, tc.want)
		}
	}
}

// A heartbeat waiting on the start grace goes when it ends, not at the
// loop's next tick (which, at the defaults, would be at 30 s).
func TestTheFirstHeartbeatGoesWhenTheStartGraceEnds(t *testing.T) {
	central := newFakeCentral(t)
	a := New(linkConfig(central, closedPort(t), time.Hour))
	a.ozLink.start(false, 150*time.Millisecond, time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.heartbeatLoop(ctx)

	waitUntil(t, "a heartbeat when the grace ended", 2*time.Second, func() bool {
		e, ok := a.buf.Peek()
		return ok && isHeartbeat(e.Key)
	})
}

// Auto mode's discovery window passes with nothing chosen: the first
// heartbeat goes then, inside the start grace.
func TestAStalledDiscoveryIsReportedAtOnce(t *testing.T) {
	var l ozoneLink
	now := time.Now()
	l.start(true, time.Hour, now)
	l.discovering("no game server announcement heard", now)
	if l.whileDown(now.Add(time.Second), func(map[string]any) {}) {
		t.Fatal("a heartbeat inside the grace while discovery is still running")
	}
	l.stalled()
	select {
	case <-l.wakeC():
	default:
		t.Fatal("a stalled discovery did not wake the heartbeat loop")
	}
	if !l.whileDown(now.Add(time.Second), func(map[string]any) {}) {
		t.Fatal("no heartbeat after discovery stalled")
	}
}

// A link that comes back within lossDelay (one missed reply, a reset socket,
// and the run redials at once) sends no heartbeat: the venue doesn't flash
// red for a reconnect.
func TestALinkBackWithinTheLossDelaySendsNoHeartbeat(t *testing.T) {
	central := newFakeCentral(t)
	a := New(linkConfig(central, closedPort(t), time.Hour))
	a.lossDelay = 300 * time.Millisecond
	a.ozLink.ok(time.Now())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.heartbeatLoop(ctx)

	a.ozLink.failed(errors.New("read tcp: connection reset by peer"), time.Now())
	time.Sleep(50 * time.Millisecond)
	a.ozLink.ok(time.Now())
	time.Sleep(500 * time.Millisecond)
	if n := a.buf.Len(); n != 0 {
		t.Fatalf("%d entries queued for a link that was back within the loss delay", n)
	}
}
