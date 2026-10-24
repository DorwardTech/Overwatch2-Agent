package ozone

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeOZone answers each command by sending the frames reply returns for it,
// in order. Extra frames model what a multi-client O-Zone was seen to deliver:
// replies to commands this connection did not just send.
func fakeOZone(t *testing.T, reply func(cmd string, n int) []map[string]any) *Client {
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
			for _, frame := range reply(req["CMD"], n) {
				if err := conn.WriteJSON(frame); err != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(srv.Close)

	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	c, err := Dial(host, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

func TestCommandSkipsFramesThatAnswerOtherCommands(t *testing.T) {
	c := fakeOZone(t, func(cmd string, n int) []map[string]any {
		if n == 0 {
			// A packs reply arrives ahead of the server state, the shape
			// production sent in the server state's place.
			return []map[string]any{
				{"CMD": "GETACTIVEPACKS", "STATUS": "OK", "PACKS": []any{}},
				{"CMD": "GETGAMELIST", "STATUS": "OK"},
				{"CMD": cmd, "STATUS": "OK", "SERVERMODE": 2},
			}
		}
		return []map[string]any{{"CMD": cmd, "STATUS": "OK", "PACKS": []any{map[string]any{"ID": 1}}}}
	})

	server, err := c.Command("GETSERVERSTATE")
	if err != nil {
		t.Fatal(err)
	}
	if server["CMD"] != "GETSERVERSTATE" || server["SERVERMODE"] != float64(2) {
		t.Fatalf("GETSERVERSTATE got %v", server)
	}

	// The next command is answered by its own reply, not by a frame left over
	// from the first: nothing shifted.
	packs, err := c.Command("GETACTIVEPACKS")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := packs["PACKS"]; !ok || packs["CMD"] != "GETACTIVEPACKS" {
		t.Fatalf("GETACTIVEPACKS got %v", packs)
	}
	if got := c.ForeignFrames(); got != 2 {
		t.Fatalf("ForeignFrames() = %d, want 2", got)
	}
}

// A duplicate of an earlier reply left queued is discarded by the next command.
func TestCommandSkipsALeftoverReplyFromTheCommandBefore(t *testing.T) {
	c := fakeOZone(t, func(cmd string, n int) []map[string]any {
		frame := map[string]any{"CMD": cmd, "STATUS": "OK", "N": n}
		if n == 0 {
			return []map[string]any{frame, frame}
		}
		return []map[string]any{frame}
	})

	if _, err := c.Command("GETSERVERSTATE"); err != nil {
		t.Fatal(err)
	}
	packs, err := c.Command("GETACTIVEPACKS")
	if err != nil {
		t.Fatal(err)
	}
	if packs["CMD"] != "GETACTIVEPACKS" || packs["N"] != float64(1) {
		t.Fatalf("GETACTIVEPACKS got %v", packs)
	}
}

func TestCommandAcceptsAnEchoInAnyCaseOrNoEcho(t *testing.T) {
	c := fakeOZone(t, func(cmd string, n int) []map[string]any {
		if n == 0 {
			return []map[string]any{{"CMD": " getServerState ", "SERVERMODE": 1}}
		}
		return []map[string]any{{"SERVERMODE": 6}}
	})

	for _, want := range []float64{1, 6} {
		resp, err := c.Command("GETSERVERSTATE")
		if err != nil {
			t.Fatal(err)
		}
		if resp["SERVERMODE"] != want {
			t.Fatalf("SERVERMODE = %v, want %v", resp["SERVERMODE"], want)
		}
	}
	if got := c.ForeignFrames(); got != 0 {
		t.Fatalf("ForeignFrames() = %d, want 0", got)
	}
}

func TestCommandGivesUpOnAFloodOfOtherReplies(t *testing.T) {
	c := fakeOZone(t, func(cmd string, n int) []map[string]any {
		frames := make([]map[string]any, maxForeignFrames+1)
		for i := range frames {
			frames[i] = map[string]any{"CMD": "GETTEAMINFO"}
		}
		return frames
	})

	if _, err := c.Command("GETSERVERSTATE"); err == nil || !strings.Contains(err.Error(), "GETSERVERSTATE") {
		t.Fatalf("Command() error = %v, want a GETSERVERSTATE error", err)
	}
}

// waitQueued waits until the reader has queued n frames that no call asked for.
func waitQueued(t *testing.T, c *Client, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for len(c.frames) < n {
		if time.Now().After(deadline) {
			t.Fatalf("reader queued %d frame(s), want %d", len(c.frames), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// The case echo matching alone left open: a GETSERVERSTATE frame that arrived
// between polls matches the next GETSERVERSTATE by name. Taken as the reply,
// its stale "idle" could clear an active game and open the print-server gate,
// while the real reply was thrown away as foreign by the next command.
func TestCommandIgnoresAReplyLeftWaitingSinceTheLastCall(t *testing.T) {
	c := fakeOZone(t, func(cmd string, n int) []map[string]any {
		switch n {
		case 0:
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 1}}
		case 1:
			return []map[string]any{
				{"CMD": cmd, "PACKS": []any{}},
				{"CMD": "GETSERVERSTATE", "SERVERMODE": 2}, // arrives after the call has its reply
			}
		default:
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 6}}
		}
	})

	if _, err := c.Command("GETSERVERSTATE"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Command("GETACTIVEPACKS"); err != nil {
		t.Fatal(err)
	}
	waitQueued(t, c, 1)

	server, err := c.Command("GETSERVERSTATE")
	if err != nil {
		t.Fatal(err)
	}
	if server["SERVERMODE"] != float64(6) {
		t.Fatalf("SERVERMODE = %v, want 6 (the reply, not the frame left waiting)", server["SERVERMODE"])
	}
	if got := c.StaleFrames(); got != 1 {
		t.Fatalf("StaleFrames() = %d, want 1", got)
	}
}

// A burst of other replies arrives between a request and its reply, as many as
// the queue holds. None is dropped, so the reply gets through.
func TestCommandKeepsTheReplyBehindABurst(t *testing.T) {
	burst := frameQueue - 1
	c := fakeOZone(t, func(cmd string, n int) []map[string]any {
		frames := make([]map[string]any, 0, burst+1)
		for i := 0; i < burst; i++ {
			frames = append(frames, map[string]any{"CMD": "GETTEAMINFO", "I": i})
		}
		return append(frames, map[string]any{"CMD": cmd, "SERVERMODE": 2})
	})

	resp, err := c.Command("GETSERVERSTATE")
	if err != nil {
		t.Fatal(err)
	}
	if resp["SERVERMODE"] != float64(2) {
		t.Fatalf("got %v, want the GETSERVERSTATE reply", resp)
	}
	if got := c.ForeignFrames(); got != int64(burst) {
		t.Fatalf("ForeignFrames() = %d, want %d", got, burst)
	}
}

// Frames nobody takes are dropped once the queue is full rather than held
// without bound. The reader goes on reading past them, so the next call still
// gets its reply.
func TestFramesBetweenCallsAreDroppedOnceTheQueueIsFull(t *testing.T) {
	const flood = frameQueue + 20
	c := fakeOZone(t, func(cmd string, n int) []map[string]any {
		frames := []map[string]any{{"CMD": cmd, "N": n}}
		if n == 0 {
			for i := 0; i < flood; i++ {
				frames = append(frames, map[string]any{"CMD": "GETGAMELIST", "I": i})
			}
		}
		return frames
	})

	if _, err := c.Command("GETSERVERSTATE"); err != nil {
		t.Fatal(err)
	}
	// The reply can be taken before the flood arrives or queued with it, so
	// the queue fills with frameQueue frames or one fewer.
	deadline := time.Now().Add(2 * time.Second)
	for c.OverflowFrames()+int64(len(c.frames)) < flood {
		if time.Now().After(deadline) {
			t.Fatalf("reader accounted for %d of %d frames", c.OverflowFrames()+int64(len(c.frames)), flood)
		}
		time.Sleep(time.Millisecond)
	}
	q := len(c.frames)
	if q != frameQueue && q != frameQueue-1 {
		t.Fatalf("queue holds %d frames, want it full at %d (or %d with the reply taken late)", q, frameQueue, frameQueue-1)
	}
	if got := c.OverflowFrames(); got != int64(flood-q) {
		t.Fatalf("OverflowFrames() = %d, want %d", got, flood-q)
	}

	resp, err := c.Command("GETACTIVEPACKS")
	if err != nil {
		t.Fatal(err)
	}
	if resp["N"] != float64(1) {
		t.Fatalf("got %v, want the GETACTIVEPACKS reply", resp)
	}
	if got := c.StaleFrames(); got != int64(q) {
		t.Fatalf("StaleFrames() = %d, want %d", got, q)
	}
	if _, lost := c.TakeDiscarded(); lost < int64(flood-q) {
		t.Fatalf("TakeDiscarded() lost = %d, want at least the %d dropped", lost, flood-q)
	}
	if got := c.queued.Load(); got != 0 {
		t.Fatalf("queued bytes = %d, want 0 once every frame is taken", got)
	}
}

// A connection O-Zone closes is reported to the waiting call at once, so the
// poll loop reconnects without sitting out the reply timeout.
func TestCommandReportsAClosedConnectionPromptly(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		var req map[string]string
		_ = conn.ReadJSON(&req)
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	c, err := Dial(host, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	start := time.Now()
	if _, err := c.Command("GETSERVERSTATE"); err == nil {
		t.Fatal("Command() on a closed connection returned no error")
	}
	if took := time.Since(start); took > replyTimeout/2 {
		t.Fatalf("Command() took %s to notice the connection closed", took)
	}
}

func TestCommandTimesOutWithoutAReply(t *testing.T) {
	old := replyTimeout
	replyTimeout = 50 * time.Millisecond
	t.Cleanup(func() { replyTimeout = old })

	c := fakeOZone(t, func(string, int) []map[string]any { return nil })
	if _, err := c.Command("GETSERVERSTATE"); err == nil || !strings.Contains(err.Error(), "no reply within") {
		t.Fatalf("Command() error = %v, want a timeout", err)
	}
}

// Every call after the connection closes fails at once, not just the first:
// the reader's error is kept, as a websocket's read error is. The far end
// vanishing without a close frame is the hard case: the next write still
// succeeds, and without the kept error that call would wait out the timeout.
func TestEveryCallFailsFastAfterTheConnectionCloses(t *testing.T) {
	old := replyTimeout
	replyTimeout = 3 * time.Second
	t.Cleanup(func() { replyTimeout = old })

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		var req map[string]string
		if err := conn.ReadJSON(&req); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"CMD": req["CMD"]})
		_ = conn.UnderlyingConn().Close() // gone, no close frame
	}))
	t.Cleanup(srv.Close)
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")
	c, err := Dial(host, port)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)

	if _, err := c.Command("GETSERVERSTATE"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.dead:
	case <-time.After(2 * time.Second):
		t.Fatal("the reader never saw the connection close")
	}

	for _, cmd := range []string{"GETTEAMINFO", "GETGAMELIST", "GETFEATURELICINFO"} {
		start := time.Now()
		if _, err := c.Command(cmd); err == nil {
			t.Fatalf("%s on a closed connection returned no error", cmd)
		}
		if took := time.Since(start); took > replyTimeout/3 {
			t.Fatalf("%s took %s to fail on a closed connection", cmd, took)
		}
	}
}

// What calls drop is kept for the poll loop to inspect.
func TestTakeDiscardedReturnsWhatCallsDropped(t *testing.T) {
	c := fakeOZone(t, func(cmd string, n int) []map[string]any {
		if n == 0 {
			return []map[string]any{{"CMD": cmd, "N": 0}, {"CMD": "GETSERVERSTATE", "SERVERMODE": 2}}
		}
		return []map[string]any{{"CMD": "GETTEAMINFO"}, {"CMD": cmd, "N": n}}
	})
	if _, err := c.Command("GETSERVERSTATE"); err != nil {
		t.Fatal(err)
	}
	waitQueued(t, c, 1)
	if _, err := c.Command("GETACTIVEPACKS"); err != nil {
		t.Fatal(err)
	}

	frames, lost := c.TakeDiscarded()
	if lost != 0 || len(frames) != 2 {
		t.Fatalf("TakeDiscarded() = %v, %d; want the stale GETSERVERSTATE and the GETTEAMINFO", frames, lost)
	}
	if frames[0]["CMD"] != "GETSERVERSTATE" || frames[1]["CMD"] != "GETTEAMINFO" {
		t.Fatalf("TakeDiscarded() = %v", frames)
	}
	if again, _ := c.TakeDiscarded(); len(again) != 0 {
		t.Fatalf("TakeDiscarded() did not start afresh: %v", again)
	}
}

// lateReplyBurst answers a GETSERVERSTATE request the way an out-of-step
// O-Zone can: another client's old server state first, taken as the reply
// since it came after the request, then other traffic, then the real reply,
// after the call has returned.
func lateReplyBurst(others int) func(cmd string, n int) []map[string]any {
	return func(cmd string, n int) []map[string]any {
		if cmd != "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "N": n}}
		}
		frames := []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}
		for i := 0; i < others; i++ {
			frames = append(frames, map[string]any{"CMD": "GETTEAMINFO", "I": i})
		}
		return append(frames, map[string]any{"CMD": cmd, "SERVERMODE": 6, "N": n})
	}
}

// hasLateReply reports whether frames holds the real reply to request n.
func hasLateReply(frames []map[string]any, n int) bool {
	for _, f := range frames {
		if f["CMD"] == "GETSERVERSTATE" && f["SERVERMODE"] == float64(6) && f["N"] == float64(n) {
			return true
		}
	}
	return false
}

// However the frames interleave with the calls, the real reply arriving
// between them is never lost: the next call drops it where TakeDiscarded sees
// it, whether it came in before that call's request or after.
func TestALateReplyIsNeverLost(t *testing.T) {
	c := fakeOZone(t, lateReplyBurst(20))
	for i := 0; i < 1000; i++ {
		c.Flush()
		if _, err := c.Command("GETSERVERSTATE"); err != nil {
			t.Fatal(err)
		}
		c.DropDiscarded()
		if _, err := c.Command("GETACTIVEPACKS"); err != nil {
			t.Fatal(err)
		}
		frames, lost := c.TakeDiscarded()
		if lost != 0 || !hasLateReply(frames, 2*i) {
			t.Fatalf("poll %d: TakeDiscarded() = %v, %d; want the real reply kept and nothing lost", i, frames, lost)
		}
	}
}

// With more traffic between the calls than the queue holds, the real reply is
// dropped, but not unseen: the loss is counted, so a poll judging it cannot
// settle.
func TestALateReplyDroppedWithTheQueueFullIsCounted(t *testing.T) {
	const others = frameQueue + 10
	c := fakeOZone(t, lateReplyBurst(others))
	c.Flush()
	if _, err := c.Command("GETSERVERSTATE"); err != nil {
		t.Fatal(err)
	}
	// The lookalike was taken; the rest fill the queue, and the reader drops
	// the 10 others and the real reply that do not fit.
	deadline := time.Now().Add(2 * time.Second)
	for c.OverflowFrames() < others+1-frameQueue {
		if time.Now().After(deadline) {
			t.Fatalf("OverflowFrames() = %d, want %d", c.OverflowFrames(), others+1-frameQueue)
		}
		time.Sleep(time.Millisecond)
	}
	c.DropDiscarded()
	resp, err := c.Command("GETACTIVEPACKS")
	if err != nil {
		t.Fatal(err)
	}
	if resp["N"] != float64(1) {
		t.Fatalf("GETACTIVEPACKS got %v, want its own reply", resp)
	}
	frames, lost := c.TakeDiscarded()
	if hasLateReply(frames, 0) {
		t.Fatal("the real reply was kept although the queue had no room for it")
	}
	if lost < others+1-frameQueue {
		t.Fatalf("TakeDiscarded() lost = %d, want at least the %d dropped", lost, others+1-frameQueue)
	}
}

// A frame sent before a request is never taken for its reply, however much
// traffic came with it: the reader stamps each frame as it arrives, never late.
// A reader that stopped for room stamped what piled up in the socket only when
// it got to it, after the next request, and an old GETACTIVEPACKS reply behind
// a burst then passed for the new one, while the real server state behind it
// was still unread when the call returned.
func TestAnOldReplyBehindABurstIsNotTakenForTheNextOne(t *testing.T) {
	const burst = 20
	c := fakeOZone(t, func(cmd string, n int) []map[string]any {
		if n > 0 {
			return []map[string]any{{"CMD": cmd, "N": n}}
		}
		frames := []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}
		for i := 0; i < burst; i++ {
			frames = append(frames, map[string]any{"CMD": "GETGAMELIST", "I": i})
		}
		return append(frames,
			map[string]any{"CMD": "GETACTIVEPACKS", "OLD": true},
			map[string]any{"CMD": "GETSERVERSTATE", "SERVERMODE": 6},
		)
	})

	if _, err := c.Command("GETSERVERSTATE"); err != nil {
		t.Fatal(err)
	}
	waitQueued(t, c, burst+2)
	c.DropDiscarded()

	resp, err := c.Command("GETACTIVEPACKS")
	if err != nil {
		t.Fatal(err)
	}
	if resp["OLD"] != nil || resp["N"] != float64(1) {
		t.Fatalf("GETACTIVEPACKS got %v, want its own reply, not the old one", resp)
	}
	frames, lost := c.TakeDiscarded()
	found := false
	for _, f := range frames {
		found = found || (f["CMD"] == "GETSERVERSTATE" && f["SERVERMODE"] == float64(6))
	}
	if lost != 0 || !found {
		t.Fatalf("TakeDiscarded() = %v, %d; want the real server state kept and nothing lost", frames, lost)
	}
}

// A frame that arrived before the request is not the reply even if it reaches
// the call after the request went out: a call judges by when a frame came in,
// not when it gets to it.
func TestCommandRejectsAFrameThatArrivedBeforeItsRequest(t *testing.T) {
	var c *Client
	c = fakeOZone(t, func(cmd string, n int) []map[string]any {
		// The request has been written. A frame stamped before it reaches
		// the call now, ahead of the reply, as one the reader read just
		// before the request but queued after the call's drain would.
		old := []byte(`{"CMD":"GETSERVERSTATE","SERVERMODE":2}`)
		c.queued.Add(int64(len(old)))
		c.frames <- frame{data: old, at: time.Now().Add(-time.Second)}
		return []map[string]any{{"CMD": cmd, "SERVERMODE": 6}}
	})

	resp, err := c.Command("GETSERVERSTATE")
	if err != nil {
		t.Fatal(err)
	}
	if resp["SERVERMODE"] != float64(6) {
		t.Fatalf("SERVERMODE = %v, want 6 (the reply, not the frame from before the request)", resp["SERVERMODE"])
	}
	if got := c.StaleFrames(); got != 1 {
		t.Fatalf("StaleFrames() = %d, want 1", got)
	}
}

// Flush forgets what came in before it, frames and losses alike.
func TestFlushForgetsWhatCameBefore(t *testing.T) {
	c := fakeOZone(t, func(cmd string, n int) []map[string]any {
		return []map[string]any{{"CMD": cmd}, {"CMD": "GETTEAMINFO"}, {"CMD": "GETGAMELIST"}}
	})
	if _, err := c.Command("GETSERVERSTATE"); err != nil {
		t.Fatal(err)
	}
	waitQueued(t, c, 2)
	c.discarded = []map[string]any{{"CMD": "GETTEAMINFO"}}
	c.lost.Store(3)

	c.Flush()
	if frames, lost := c.TakeDiscarded(); len(frames) != 0 || lost != 0 {
		t.Fatalf("TakeDiscarded() = %v, %d after Flush; want nothing", frames, lost)
	}
	if n := len(c.frames); n != 0 {
		t.Fatalf("%d frame(s) still queued after Flush", n)
	}
	if got := c.StaleFrames(); got != 2 {
		t.Fatalf("StaleFrames() = %d, want the 2 flushed", got)
	}
	if got := c.queued.Load(); got != 0 {
		t.Fatalf("queued bytes = %d after Flush, want 0", got)
	}
}

// DropDiscarded forgets the frames kept but not the count of those lost.
func TestDropDiscardedKeepsTheLossCount(t *testing.T) {
	c := &Client{}
	c.discarded = []map[string]any{{"CMD": "GETTEAMINFO"}}
	c.lost.Store(2)

	c.DropDiscarded()
	frames, lost := c.TakeDiscarded()
	if len(frames) != 0 || lost != 2 {
		t.Fatalf("TakeDiscarded() = %v, %d after DropDiscarded; want no frames and 2 lost", frames, lost)
	}
}

// The observer sees every frame as it arrives, those dropped with the queue
// full included.
func TestObserveSeesEveryFrame(t *testing.T) {
	const flood = frameQueue + 10
	var seen atomic.Int64
	var c *Client
	c = fakeOZone(t, func(cmd string, n int) []map[string]any {
		frames := []map[string]any{{"CMD": cmd}}
		for i := 0; i < flood; i++ {
			frames = append(frames, map[string]any{"CMD": "GETGAMELIST", "I": i})
		}
		return frames
	})
	c.Observe(func(resp map[string]any) {
		if resp["CMD"] == "GETGAMELIST" {
			seen.Add(1)
		}
	})

	if _, err := c.Command("GETSERVERSTATE"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for seen.Load() < flood {
		if time.Now().After(deadline) {
			t.Fatalf("observer saw %d of %d frames", seen.Load(), flood)
		}
		time.Sleep(time.Millisecond)
	}
	if c.OverflowFrames() == 0 {
		t.Fatal("no frame was dropped, so the test does not show dropped frames are observed")
	}
}

// An address not chosen yet is never dialled: empty means this computer.
func TestDialAndProbeRefuseAnEmptyHost(t *testing.T) {
	if _, err := Dial(" ", "12113"); !errors.Is(err, ErrNoHost) {
		t.Fatalf("Dial(\"\") error = %v, want ErrNoHost", err)
	}
	if _, err := Probe(context.Background(), "", "12113", time.Second); !errors.Is(err, ErrNoHost) {
		t.Fatalf("Probe(\"\") error = %v, want ErrNoHost", err)
	}
}

func TestProbeReturnsTheServerMode(t *testing.T) {
	var asked []string
	var mu sync.Mutex
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var req map[string]string
		if conn.ReadJSON(&req) != nil {
			return
		}
		mu.Lock()
		asked = append(asked, req["CMD"])
		mu.Unlock()
		_ = conn.WriteJSON(map[string]any{"CMD": req["CMD"], "SERVERMODE": 2})
	}))
	defer srv.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")

	mode, err := Probe(context.Background(), host, port, 2*time.Second)
	if err != nil || mode != 2 {
		t.Fatalf("Probe() = %d, %v; want 2", mode, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(asked) != 1 || asked[0] != "GETSERVERSTATE" {
		t.Fatalf("Probe sent %v, want only GETSERVERSTATE", asked)
	}
}

func TestProbeGivesUpAtItsTimeout(t *testing.T) {
	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var req map[string]string
		_ = conn.ReadJSON(&req)
		time.Sleep(2 * time.Second) // never answers in time
	}))
	defer srv.Close()
	host, port, _ := strings.Cut(strings.TrimPrefix(srv.URL, "http://"), ":")

	start := time.Now()
	if _, err := Probe(context.Background(), host, port, 200*time.Millisecond); err == nil {
		t.Fatal("Probe() succeeded without a reply")
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Probe took %s with a 200ms timeout", took)
	}
}
