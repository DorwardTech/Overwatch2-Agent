package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/msgbus"
	"overwatch/agent/internal/ozonelib"
	"overwatch/agent/internal/ozonesim"
)

// fakeOzoneWS is a game server's WebSocket API on 127.0.0.1 that answers
// every server-state request with mode and counts connections.
type fakeOzoneWS struct {
	srv   *httptest.Server
	port  string
	conns atomic.Int32
	mode  atomic.Int32

	mu   sync.Mutex
	open []*websocket.Conn
}

// kill closes every connection and stops answering: the server is gone.
// (httptest does not track hijacked connections, so Close alone leaves them.)
func (f *fakeOzoneWS) kill() {
	f.srv.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.open {
		_ = c.Close()
	}
}

func newFakeOzoneWS(t *testing.T, mode int) *fakeOzoneWS {
	t.Helper()
	f := &fakeOzoneWS{}
	f.mode.Store(int32(mode))
	upgrader := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.conns.Add(1)
		f.mu.Lock()
		f.open = append(f.open, conn)
		f.mu.Unlock()
		defer conn.Close()
		for {
			var req map[string]string
			if err := conn.ReadJSON(&req); err != nil {
				return
			}
			reply := map[string]any{"CMD": req["CMD"]}
			switch req["CMD"] {
			case "GETSERVERSTATE":
				reply["SERVERMODE"] = f.mode.Load()
			case "GETACTIVEPACKS":
				reply["PACKS"] = []any{}
			}
			if err := conn.WriteJSON(reply); err != nil {
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	_, f.port, _ = strings.Cut(strings.TrimPrefix(f.srv.URL, "http://"), ":")
	return f
}

func autoConfig(wsPort string) config.Config {
	return config.Config{
		OzoneDiscover:    true,
		OzonePort:        wsPort,
		OzoneResultsPort: "1", // nothing listens; and nothing may dial it
		CentralURL:       "http://central.invalid",
		Token:            "test",
		PollInterval:     50 * time.Millisecond,
		IdlePollInterval: 50 * time.Millisecond,
		SlowPollInterval: 100 * time.Millisecond,
		BufferMax:        100,
	}
}

// discoveryApp builds an auto-mode app with short timings and a listener on a
// free port, whose entries expire after ttl.
func discoveryApp(t *testing.T, cfg config.Config, ttl time.Duration) *App {
	t.Helper()
	a := New(cfg)
	a.lib = ozonelib.New(ozonelib.Config{Auto: true, TTL: ttl, Selector: ozonelib.Selector{SID: cfg.LIB.SID, Arena: cfg.LIB.Arena}})
	a.health.SetReason(a.lib.NotReadyReason)
	a.libWindow = 150 * time.Millisecond
	a.followAfter = 200 * time.Millisecond
	return a
}

// runApp runs a in the background; the returned func cancels it and waits.
func runApp(t *testing.T, a *App) (done <-chan struct{}, stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	d := make(chan struct{})
	go func() {
		defer close(d)
		a.Run(ctx)
	}()
	stop = func() {
		cancel()
		select {
		case <-d:
		case <-time.After(5 * time.Second):
			t.Error("Run did not return after cancel")
		}
	}
	t.Cleanup(stop)
	return d, stop
}

func waitUntil(t *testing.T, what string, within time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// announcer sends a's announcement every 30ms from source to the app's
// listener, once it is listening.
func announcer(t *testing.T, a *App, source string, ann ozonesim.LIBAnnouncement) *ozonesim.LIBSender {
	t.Helper()
	waitUntil(t, "the listener", 3*time.Second, func() bool { return a.lib.Report()["listening"] == true })
	s := &ozonesim.LIBSender{Target: "127.0.0.1:" + strconv.Itoa(a.lib.Port()), Source: source}
	if err := s.Send(ann); err != nil {
		t.Skipf("cannot send from %s here: %v", source, err)
	}
	s.Start(ann, 30*time.Millisecond)
	t.Cleanup(s.Stop)
	return s
}

func libState(a *App) any { return a.Overview()["lib"].(map[string]any)["state"] }

func TestAutoModeConnectsToTheAnnouncedServer(t *testing.T) {
	ws := newFakeOzoneWS(t, 1)
	bus := ozonesim.NewMessageBus()
	if err := bus.Start(0); err != nil {
		t.Fatal(err)
	}
	defer bus.Close()
	_, busPort, _ := net.SplitHostPort(bus.Addr())

	cfg := autoConfig(ws.port)
	cfg.MsgBusEnabled, cfg.OzoneMsgBusPort = true, busPort
	a := discoveryApp(t, cfg, 0)
	_, stop := runApp(t, a)

	waitUntil(t, "the listener", 3*time.Second, func() bool { return a.lib.Report()["listening"] == true })
	time.Sleep(200 * time.Millisecond)
	if ws.conns.Load() != 0 || a.bus.Connected() || a.ozoneAddr() != "" {
		t.Fatal("something connected before any server was announced")
	}
	if libState(a) != "waiting" {
		t.Fatalf("state = %v, want waiting", libState(a))
	}

	announcer(t, a, "127.0.0.1", ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"})
	waitUntil(t, "a connection to the announced server", 3*time.Second, func() bool { return ws.conns.Load() > 0 })
	if a.ozoneAddr() != "127.0.0.1" || a.Overview()["ozone_host"] != "127.0.0.1" || libState(a) != "selected" {
		t.Fatalf("host %q, overview %v", a.ozoneAddr(), a.Overview())
	}
	waitUntil(t, "the message bus", 3*time.Second, a.bus.Connected)

	// The first poll is a slow one: its self-report carries what was heard.
	waitUntil(t, "a buffered push", 3*time.Second, func() bool { _, ok := a.buf.Peek(); return ok })
	e, _ := a.buf.Peek()
	var payload struct {
		Agent struct {
			LIB map[string]any `json:"lib"`
		} `json:"agent"`
	}
	if err := json.Unmarshal(e.Data, &payload); err != nil {
		t.Fatal(err)
	}
	sel, _ := payload.Agent.LIB["selected"].(map[string]any)
	last, _ := payload.Agent.LIB["last"].(map[string]any)
	if sel["ip"] != "127.0.0.1" || last == nil || last["received_at"] == "" {
		t.Fatalf("self-report lib = %v", payload.Agent.LIB)
	}
	stop()
}

// Two arenas heard: the agent waits and says so, and dials nothing.
func TestAutoModeWaitsWhenItCannotChoose(t *testing.T) {
	ws := newFakeOzoneWS(t, 1)
	a := discoveryApp(t, autoConfig(ws.port), 0)
	runApp(t, a)

	announcer(t, a, "127.0.0.1", ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 0, SID: "4242"})
	announcer(t, a, "127.0.0.1", ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"})
	waitUntil(t, "the ambiguity", 3*time.Second, func() bool { return libState(a) == "ambiguous" })
	time.Sleep(a.libWindow + 300*time.Millisecond)
	if ws.conns.Load() != 0 || a.ozoneAddr() != "" {
		t.Fatalf("connected (%d) to %q with two arenas announced", ws.conns.Load(), a.ozoneAddr())
	}
	if !strings.Contains(a.lib.NotReadyReason(), "OZONE_SID") {
		t.Fatalf("NotReadyReason() = %q", a.lib.NotReadyReason())
	}
}

// Before a server is chosen there is no address, and nothing can reach the
// print server, even with a reading that would otherwise open the gate.
func TestNothingReachesThePrintServerBeforeAChoice(t *testing.T) {
	ps := ozonesim.NewPrintServer()
	if err := ps.Start(0); err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	_, port, _ := net.SplitHostPort(ps.Addr())

	cfg := autoConfig("1")
	cfg.OzoneResultsPort = port
	a := New(cfg)
	if err := a.printServerGate(); !errors.Is(err, errLinkStale) {
		t.Fatalf("gate = %v before any reading, want errLinkStale", err)
	}
	a.noteServerMode(1)
	a.gameState.Store(stateIdle)
	if _, err := a.newResultsSession().connect(); !errors.Is(err, errLinkStale) {
		t.Fatalf("connect() = %v with no address, want errLinkStale", err)
	}
	a.queueFetch(9)
	if !a.drainPendingFetches() {
		t.Fatal("a fetch with no address was not reported put off")
	}
	if n := ps.Connections(); n != 0 {
		t.Fatalf("print server contacted %d time(s) with no address chosen", n)
	}
}

func TestAutoModeFollowsAServerThatMoved(t *testing.T) {
	ws := newFakeOzoneWS(t, 1)
	a := discoveryApp(t, autoConfig(ws.port), 300*time.Millisecond)
	var probed sync.Map
	a.probe = func(_ context.Context, host, _ string, _ time.Duration) (int, error) {
		probed.Store(host, true)
		return 1, nil
	}
	done, _ := runApp(t, a)

	old := announcer(t, a, "127.0.0.1", ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"})
	waitUntil(t, "the first connection", 3*time.Second, func() bool { return ws.conns.Load() > 0 })

	// The server goes quiet at its old address and announces a new one.
	old.Stop()
	ws.kill()
	announcer(t, a, "127.0.0.2", ozonesim.LIBAnnouncement{IP: "127.0.0.2", Arena: 1, SID: "4242"})

	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("the agent did not restart after its server moved")
	}
	if _, ok := probed.Load("127.0.0.2"); !ok {
		t.Fatal("the new address was not probed before restarting")
	}
	if r := a.restartReason; !strings.Contains(r, "moved 127.0.0.1 -> 127.0.0.2") {
		t.Fatalf("restart reason = %q", r)
	}
}

// A server that still answers is never left, whatever is announced; nor is
// one announced from two addresses, or at an address that does not answer.
func TestMaybeFollowStaysPut(t *testing.T) {
	setup := func(t *testing.T) (*App, context.Context, *atomic.Bool) {
		a := discoveryApp(t, autoConfig("1"), 300*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		if err := a.lib.Start(ctx); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(a.lib.Close)
		a.setOzoneHost("127.0.0.1")
		a.pin = ozonelib.Candidate{Key: ozonelib.Key{SID: 4242, Arena: 1}, SIDRaw: "4242"}
		a.pinnedAt = time.Now().Add(-time.Minute)
		stopped := &atomic.Bool{}
		a.stopRun = func() { stopped.Store(true) }
		a.probe = func(context.Context, string, string, time.Duration) (int, error) { return 1, nil }
		return a, ctx, stopped
	}
	moved := ozonesim.LIBAnnouncement{IP: "127.0.0.2", Arena: 1, SID: "4242"}

	t.Run("follows when everything holds", func(t *testing.T) {
		a, ctx, stopped := setup(t)
		announcer(t, a, "127.0.0.2", moved)
		waitUntil(t, "a move", 3*time.Second, func() bool { return a.maybeFollow(ctx) })
		if !stopped.Load() {
			t.Fatal("followed without stopping the run")
		}
	})
	t.Run("not while the old address still answers", func(t *testing.T) {
		a, ctx, stopped := setup(t)
		announcer(t, a, "127.0.0.2", moved)
		time.Sleep(200 * time.Millisecond)
		a.noteServerMode(1) // our address just answered
		if a.maybeFollow(ctx) || stopped.Load() {
			t.Fatal("followed away from a server that still answers")
		}
	})
	t.Run("not on a conflict", func(t *testing.T) {
		a, ctx, stopped := setup(t)
		announcer(t, a, "127.0.0.2", moved)
		announcer(t, a, "127.0.0.1", ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"})
		time.Sleep(200 * time.Millisecond)
		if a.maybeFollow(ctx) || stopped.Load() {
			t.Fatal("followed a server announced from two addresses")
		}
	})
	t.Run("not when the new address does not answer", func(t *testing.T) {
		a, ctx, stopped := setup(t)
		a.probe = func(context.Context, string, string, time.Duration) (int, error) { return 0, errors.New("refused") }
		announcer(t, a, "127.0.0.2", moved)
		time.Sleep(200 * time.Millisecond)
		if a.maybeFollow(ctx) || stopped.Load() {
			t.Fatal("followed to an address that does not answer")
		}
	})
}

// The machine at our address becomes a different server: the agent restarts
// to choose again, although that machine still answers.
func TestAutoModeRestartsWhenItsServerChangesIdentity(t *testing.T) {
	ws := newFakeOzoneWS(t, 1)
	a := discoveryApp(t, autoConfig(ws.port), 300*time.Millisecond)
	done, _ := runApp(t, a)

	pinned := announcer(t, a, "127.0.0.1", ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 1, SID: "4242"})
	waitUntil(t, "the first connection", 3*time.Second, func() bool { return ws.conns.Load() > 0 })
	pinned.Stop()
	announcer(t, a, "127.0.0.1", ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 0, SID: "5555"})

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent did not restart when its address became another server")
	}
	if r := a.restartReason; !strings.Contains(r, "no longer site 4242 arena 1") {
		t.Fatalf("restart reason = %q", r)
	}
}

// With the listener off, a fixed host behaves exactly as before: no socket,
// no report, and the configured address.
func TestAFixedHostWithTheListenerOff(t *testing.T) {
	a := New(config.Config{OzoneHost: "192.0.2.10", LIBListener: false})
	if a.lib != nil {
		t.Fatal("a listener was created with ENABLE_LIB_LISTENER=false")
	}
	if a.ozoneAddr() != "192.0.2.10" {
		t.Fatalf("ozoneAddr() = %q", a.ozoneAddr())
	}
	ov := a.Overview()
	if _, ok := ov["lib"]; ok {
		t.Fatal("the overview reports a listener that does not exist")
	}
	if ov["ozone_host"] != "192.0.2.10" {
		t.Fatalf("overview ozone_host = %v", ov["ozone_host"])
	}

	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		return []map[string]any{{"CMD": cmd, "SERVERMODE": 1, "PACKS": []any{}}}, false
	})
	data, _, err := a.collect(client, true)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Agent map[string]any `json:"agent"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if _, ok := payload.Agent["lib"]; ok {
		t.Fatal("the self-report carries a lib block with the listener off")
	}
}

// With the listener on (the default), a fixed host only gains a report: what
// is announced, even a lone other server, never changes the address.
func TestAFixedHostListenerOnlyReports(t *testing.T) {
	ws := newFakeOzoneWS(t, 1)
	cfg := autoConfig(ws.port)
	cfg.OzoneDiscover, cfg.OzoneHost, cfg.LIBListener = false, "127.0.0.1", true
	a := New(cfg)
	runApp(t, a)

	waitUntil(t, "the configured server", 3*time.Second, func() bool { return ws.conns.Load() > 0 })
	announcer(t, a, "127.0.0.2", ozonesim.LIBAnnouncement{IP: "127.0.0.2", Arena: 0, SID: "5555"})
	waitUntil(t, "the announcement", 3*time.Second, func() bool { return a.lib.Report()["packets"].(int64) > 1 })
	r := a.lib.Report()
	if r["state"] != "report_only" || r["mode"] != "static" || r["selected"] != nil {
		t.Fatalf("Report() = %v", r)
	}
	if _, ok := a.Overview()["lib"]; !ok {
		t.Fatal("the overview does not report the listener")
	}
	before := ws.conns.Load()
	time.Sleep(300 * time.Millisecond)
	if a.ozoneAddr() != "127.0.0.1" || ws.conns.Load() != before {
		t.Fatalf("the fixed host changed to %q (connections %d -> %d)", a.ozoneAddr(), before, ws.conns.Load())
	}
}

func TestTheAddressIsSetOnce(t *testing.T) {
	a := &App{}
	if !a.setOzoneHost("192.0.2.10") {
		t.Fatal("the first set was refused")
	}
	if a.setOzoneHost("192.0.2.11") || a.ozoneAddr() != "192.0.2.10" {
		t.Fatalf("a second set was taken: %q", a.ozoneAddr())
	}
}

// A fixed-host agent whose listener cannot bind carries on regardless.
func TestAFixedHostRunsWhenTheListenerCannotBind(t *testing.T) {
	held, err := net.ListenPacket("udp4", ":0") // without reuse: a reuse bind cannot share it
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	port := held.LocalAddr().(*net.UDPAddr).Port

	ws := newFakeOzoneWS(t, 1)
	cfg := autoConfig(ws.port)
	cfg.OzoneDiscover, cfg.OzoneHost, cfg.LIBListener = false, "127.0.0.1", true
	cfg.LIB.Port = port
	a := New(cfg)
	runApp(t, a)

	waitUntil(t, "the configured server", 3*time.Second, func() bool { return ws.conns.Load() > 0 })
	if r := a.lib.Report(); r["state"] != "bind_failed" {
		t.Skipf("this platform shared a port held without reuse (state %v)", r["state"])
	}
}

// logCapture collects what the standard logger writes during a test.
type logCapture struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func captureLog(t *testing.T) *logCapture {
	t.Helper()
	l := &logCapture{}
	prev := log.Writer()
	log.SetOutput(l)
	t.Cleanup(func() { log.SetOutput(prev) })
	return l
}

// A selector that matches none of the servers heard: the agent says which
// servers it heard, in the words /readyz uses, once rather than on every look,
// and without the advice about host networking (the network plainly works).
// The self-report's state stays "waiting".
func TestAutoModeLogsWhatItHeardWhenNothingMatches(t *testing.T) {
	logs := captureLog(t)
	ws := newFakeOzoneWS(t, 1)
	cfg := autoConfig(ws.port)
	arena := 1
	cfg.LIB.Arena = &arena
	a := discoveryApp(t, cfg, 0)
	runApp(t, a)

	announcer(t, a, "127.0.0.1", ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 0, SID: "0564"})
	want := "heard site 0564 arena 0 at 127.0.0.1, but OZONE_ARENA=1 selects no server"
	waitUntil(t, "the log line", 3*time.Second, func() bool { return strings.Contains(logs.String(), want) })
	time.Sleep(3 * a.libWindow) // many more looks, and many more announcements
	out := logs.String()
	if n := strings.Count(out, want); n != 1 {
		t.Fatalf("logged %d times, want once:\n%s", n, out)
	}
	if strings.Contains(out, "host networking") {
		t.Fatalf("advised on networking although a server was heard:\n%s", out)
	}
	if got := a.lib.NotReadyReason(); got != want {
		t.Fatalf("NotReadyReason() = %q, want %q", got, want)
	}
	if ws.conns.Load() != 0 || a.ozoneAddr() != "" {
		t.Fatalf("connected (%d) to %q with no server matching", ws.conns.Load(), a.ozoneAddr())
	}
	r := a.lib.Report()
	if r["state"] != "waiting" {
		t.Fatalf("state = %v, want waiting", r["state"])
	}
	if _, ok := r["wait_reason"]; ok {
		t.Fatal("the self-report gained a field in a patch release")
	}
}

// otherServerAtOurAddress builds an auto-mode agent that chose site 4242
// arena 1 at 127.0.0.1, whose address now announces site 5555 arena 0, with
// everything that could reach the print server lined up and the gate open: a
// fresh, settled idle reading, a finished game's work left pending, a game
// queued, a cache to refresh, and a central with a results command waiting.
func otherServerAtOurAddress(t *testing.T) (a *App, ps *ozonesim.PrintServer, commandFetches *atomic.Int32, stopped *atomic.Bool) {
	t.Helper()
	ps = ozonesim.NewPrintServer()
	if err := ps.Start(0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	ps.SetList([]byte(`{"gamelist":[{"gamenum":9,"gamename":"g9","duration":601,` +
		`"starttime":"s","endtime":"e","playercount":2,"valid":1}]}`))
	_, psPort, _ := net.SplitHostPort(ps.Addr())

	commandFetches = &atomic.Int32{}
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/commands") {
			commandFetches.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"commands":[{"id":1,"type":"refetch_game","payload":{"game_number":9}}]}`))
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(central.Close)

	cfg := autoConfig("1")
	cfg.CentralURL = central.URL + "/api/agent/ingest"
	cfg.OzoneResultsPort, cfg.ResultsHandshake = psPort, 2
	cfg.ResultsEnabled, cfg.CommandsEnabled = true, true
	cfg.CacheEnabled, cfg.CacheDir = true, t.TempDir()
	a = discoveryApp(t, cfg, 300*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := a.lib.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.lib.Close)
	a.setOzoneHost("127.0.0.1")
	a.pin = ozonelib.Candidate{Key: ozonelib.Key{SID: 4242, Arena: 1}, IP: netip.MustParseAddr("127.0.0.1"), SIDRaw: "4242"}
	a.pinnedAt = time.Now()
	stopped = &atomic.Bool{}
	a.stopRun = func() { stopped.Store(true) }

	a.noteServerMode(1)
	a.gameState.Store(stateIdle)
	a.finishPending.Store(true)
	a.queueFetch(8)
	if a.printServerGate() != nil {
		t.Fatalf("gate = %v; the test needs it open", a.printServerGate())
	}

	announcer(t, a, "127.0.0.1", ozonesim.LIBAnnouncement{IP: "127.0.0.1", Arena: 0, SID: "5555"})
	waitUntil(t, "the other server to be heard twice", 3*time.Second, func() bool {
		return a.lib.IdentityLost(a.pin.Key, a.pin.IP)
	})
	return a, ps, commandFetches, stopped
}

// The machine at our address has become another game server. The very next
// poll notices, as soon as it has the server state, and before anything acts
// on it: nothing reaches that machine's print server (no results drain, no
// finish work, no command, no cache refresh), no payload is queued for
// central, the gate closes, and the run ends to choose again.
func TestAnotherServerAtOurAddressIsNotPushed(t *testing.T) {
	a, ps, commandFetches, stopped := otherServerAtOurAddress(t)
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		switch cmd {
		case "GETSERVERSTATE": // its game 9 has finished: a fetch, if it were ours
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 7, "GAMENUM": 9}}, false
		case "GETACTIVEPACKS":
			return []map[string]any{packsReply()}, false
		}
		return []map[string]any{{"CMD": cmd}}, false
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.pollLoop(ctx, client) // the first poll is a slow one: commands and a cache refresh too
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the poll loop carried on polling another game server")
	}

	time.Sleep(300 * time.Millisecond) // anything started would have dialled by now
	if n := ps.Connections(); n != 0 {
		t.Fatalf("the other server's print server was contacted %d time(s)", n)
	}
	if n := commandFetches.Load(); n != 0 {
		t.Fatalf("commands were fetched %d time(s) on the other server's reading", n)
	}
	if n := a.buf.Len(); n != 0 {
		t.Fatalf("%d payload(s) from the other server queued for central", n)
	}
	if !stopped.Load() || !strings.Contains(a.restartReason, "no longer site 4242 arena 1") {
		t.Fatalf("restart: stopped=%v reason=%q", stopped.Load(), a.restartReason)
	}
	if a.printServerGate() == nil {
		t.Fatal("the print-server gate stayed open")
	}
	a.mu.Lock()
	_, queued := a.pendingFetch[9]
	a.mu.Unlock()
	if queued || a.lastGameNum != 0 || a.serverMode.Load() != 1 {
		t.Fatalf("the other server's reading was used: game 9 queued=%v, lastGameNum=%d, serverMode=%d", queued, a.lastGameNum, a.serverMode.Load())
	}
}

// A message-bus GAME_FINISH from the machine at our address, once that has
// become another game server, is not our server's finish: it queues nothing
// and starts no finish work.
func TestABusFinishFromAnotherServerStartsNothing(t *testing.T) {
	a, ps, _, _ := otherServerAtOurAddress(t)
	a.mu.Lock()
	a.lastBusGame = 9
	a.mu.Unlock()

	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameFinish})

	time.Sleep(300 * time.Millisecond) // finish work, if started, would have dialled by now
	if n := ps.Connections(); n != 0 {
		t.Fatalf("the other server's print server was contacted %d time(s)", n)
	}
	a.mu.Lock()
	_, queued := a.pendingFetch[9]
	a.mu.Unlock()
	if queued {
		t.Fatal("the other server's finished game was queued")
	}
	if a.gameState.Load() != stateIdle {
		t.Fatalf("game state = %d: the other server's finish was acted on", a.gameState.Load())
	}
}

// Between polls the machine at our address becomes another game server. The
// gate closes the moment the announcements tell the two apart, not at the
// next poll: an admin Resync, a finish timer, or a drain already running
// reaches only for our own server's print server.
func TestTheGateClosesAsSoonAsAnotherServerIsHeard(t *testing.T) {
	a, ps, _, _ := otherServerAtOurAddress(t) // no poll since the switch

	if err := a.printServerGate(); !errors.Is(err, errPollUnsettled) {
		t.Fatalf("gate = %v, want errPollUnsettled once our address is another server", err)
	}
	if a.safeForPrintServer(int(a.serverMode.Load())) {
		t.Fatal("safeForPrintServer still says yes")
	}
	a.refreshCache() // what Resync runs
	a.finishWork()   // what a finish timer runs: a queued game and finish work are pending
	time.Sleep(300 * time.Millisecond)
	if n := ps.Connections(); n != 0 {
		t.Fatalf("the other server's print server was contacted %d time(s) between polls", n)
	}
}
