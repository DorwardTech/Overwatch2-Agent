package app

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"overwatch/agent/internal/config"
)

// The poll loop used to call deliver() inline, so the sampling period was
// `interval + O-Zone round trips + central's HTTPS latency` and never the
// interval alone. A venue set to a 1s in-game rate measured ~2.4s in production
// and no configuration could fix it, because the push sat on the critical path.
//
// These tests pin the two properties that make the configured rate real: the
// push no longer blocks the poll, and a fast poll taken for SAFETY reasons no
// longer drags the telemetry rate along with it.

// newCadenceApp wires an agent to a central that takes `latency` to answer, so
// a test can make the push arbitrarily slower than the poll interval.
func newCadenceApp(t *testing.T, latency time.Duration) (*App, *atomic.Int64) {
	t.Helper()

	var received atomic.Int64
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		time.Sleep(latency)
		received.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(central.Close)

	return New(config.Config{
		CentralURL: central.URL + "/api/agent/ingest",
		Token:      "test",
		BufferMax:  100,
	}), &received
}

func TestEnqueueDoesNotWaitForThePush(t *testing.T) {
	// Central takes far longer than any poll interval a venue would set. If
	// enqueue waited on it the poll loop would inherit that latency, which is
	// exactly the defect.
	a, received := newCadenceApp(t, 300*time.Millisecond)

	start := time.Now()
	for i := 0; i < 5; i++ {
		a.enqueue("k", []byte(`{"push_seq":1}`))
	}
	elapsed := time.Since(start)

	if elapsed > 50*time.Millisecond {
		t.Fatalf("five enqueues took %s — the push is still on the poll's critical path", elapsed)
	}
	if got := received.Load(); got != 0 {
		t.Fatalf("central saw %d pushes with no delivery goroutine running, want 0", got)
	}
	if n := a.buf.Len(); n != 5 {
		t.Fatalf("buffered = %d, want all 5 held for the drain", n)
	}
}

func TestDeliverLoopDrainsWhatEnqueueLeaves(t *testing.T) {
	a, received := newCadenceApp(t, 0)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.deliverLoop(ctx)

	for i := 0; i < 3; i++ {
		a.enqueue("k", []byte(`{"push_seq":1}`))
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && a.buf.Len() > 0 {
		time.Sleep(5 * time.Millisecond)
	}

	if n := a.buf.Len(); n != 0 {
		t.Fatalf("buffered = %d after the drain, want 0", n)
	}
	if got := received.Load(); got != 3 {
		t.Fatalf("central saw %d pushes, want 3", got)
	}
}

// Print-server work forces the FAST poll rate so the agent notices a game
// starting — a safety property that stays. What must not follow is the
// telemetry rate: one cache refresh a minute was enough to make a nominal 15s
// idle cadence average ~7.9s in production, because every safety poll also
// wrote a row.
func TestIdleTelemetryIgnoresAFastPollTakenForSafety(t *testing.T) {
	a, _ := newCadenceApp(t, 0)
	a.cfg.PollInterval = time.Second
	a.cfg.IdlePollInterval = 30 * time.Second
	a.serverMode.Store(2) // between games — not a play mode

	a.printServerBusy.Add(1)
	defer a.printServerBusy.Add(-1)

	if got := a.nextPollInterval(); got != time.Second {
		t.Fatalf("poll interval = %s while the print server is busy, want the fast 1s (safety)", got)
	}
	if got := a.telemetryInterval(); got != 30*time.Second {
		t.Fatalf("telemetry interval = %s, want the configured idle 30s", got)
	}

	now := time.Now()
	if !a.pushDue(now, false) {
		t.Fatal("the first sample should always be recorded")
	}
	if a.pushDue(now.Add(time.Second), false) {
		t.Fatal("a 1s safety poll recorded a sample — idle telemetry is being dragged to the fast rate")
	}
	if !a.pushDue(now.Add(31*time.Second), false) {
		t.Fatal("no sample recorded after the idle interval elapsed")
	}
}

func TestInGameEveryPollIsRecorded(t *testing.T) {
	// The whole point of the 1s setting: during play nothing is throttled.
	a, _ := newCadenceApp(t, 0)
	a.cfg.PollInterval = time.Second
	a.cfg.IdlePollInterval = 30 * time.Second
	a.serverMode.Store(6) // PACK/SERVER mode 6 is live play

	now := time.Now()
	if !a.pushDue(now, false) {
		t.Fatal("first in-game sample not recorded")
	}
	for i := 1; i <= 5; i++ {
		if !a.pushDue(now.Add(time.Duration(i)*time.Second), false) {
			t.Fatalf("in-game sample %d at +%ds was throttled — the 1s rate is not being honoured", i, i)
		}
	}
}

// The slow poll is the only payload carrying team info, the game list, licences
// and the agent's own self-report. Throttling one would blind central's agent
// health view for a minute at a time.
func TestASlowPollIsNeverThrottled(t *testing.T) {
	a, _ := newCadenceApp(t, 0)
	a.cfg.PollInterval = time.Second
	a.cfg.IdlePollInterval = 30 * time.Second
	a.serverMode.Store(2)

	now := time.Now()
	if !a.pushDue(now, false) {
		t.Fatal("first sample not recorded")
	}
	if a.pushDue(now.Add(time.Second), false) {
		t.Fatal("ordinary poll inside the idle window was recorded")
	}
	if !a.pushDue(now.Add(time.Second), true) {
		t.Fatal("a SLOW poll was throttled — central would lose the agent self-report")
	}
}

// The end of a game is the one sample the throttle must never eat.
//
// inGame() goes false the moment the mode leaves play, so telemetryInterval()
// widens from the in-game rate to the idle one — and the poll that DISCOVERS
// the game has ended is then measured against the new, wider interval. At the
// venue's requested 1s/30s that rejects the transition sample for being 1s
// after the previous one, and central goes on showing a game in progress for up
// to another 30 seconds. Nothing else carries it: a mode change enqueues
// nothing of its own.
func TestTheSampleThatEndsAGameIsNeverThrottled(t *testing.T) {
	a, _ := newCadenceApp(t, 0)
	a.cfg.PollInterval = time.Second
	a.cfg.IdlePollInterval = 30 * time.Second
	a.serverMode.Store(6) // live play

	now := time.Now()
	if !a.pushDue(now, false) {
		t.Fatal("first in-game sample not recorded")
	}

	// The game finishes. This poll is 1s after the last one, and the interval
	// that now applies is the idle 30s.
	a.serverMode.Store(7) // GAME_FINISH
	if got := a.telemetryInterval(); got != 30*time.Second {
		t.Fatalf("telemetry interval = %s after the game ended, want the idle 30s", got)
	}
	if !a.pushDue(now.Add(time.Second), false) {
		t.Fatal("the sample reporting the game had ENDED was throttled — central keeps showing a finished game as running")
	}

	// Having reported the new mode, the idle rate applies again as normal.
	if a.pushDue(now.Add(2*time.Second), false) {
		t.Fatal("a steady-state idle sample was recorded 1s later — the mode exemption is not one-shot")
	}
	if !a.pushDue(now.Add(32*time.Second), false) {
		t.Fatal("no sample recorded after the idle interval elapsed")
	}
}

// The converse, and the reason this is keyed on the mode rather than on
// inGame(): a game STARTING must reach central at once too, and so must a mode
// change that happens entirely between games.
func TestAModeChangeBetweenGamesIsRecordedImmediately(t *testing.T) {
	a, _ := newCadenceApp(t, 0)
	a.cfg.PollInterval = time.Second
	a.cfg.IdlePollInterval = 30 * time.Second
	a.serverMode.Store(1) // idle on the rack

	now := time.Now()
	if !a.pushDue(now, false) {
		t.Fatal("first sample not recorded")
	}
	if a.pushDue(now.Add(time.Second), false) {
		t.Fatal("an unchanged idle sample was recorded inside the idle window")
	}

	a.serverMode.Store(2) // still not a play mode, but a different state
	if !a.pushDue(now.Add(2*time.Second), false) {
		t.Fatal("a change of server mode was throttled — central holds the previous state for a whole idle interval")
	}
}
