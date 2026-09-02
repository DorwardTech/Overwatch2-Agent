package app

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net"
	"testing"
	"time"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/msgbus"
	"overwatch/agent/internal/ozonefix"
	"overwatch/agent/internal/ozonesim"
)

func TestSafeForPrintServerRequiresBothSignals(t *testing.T) {
	a := &App{}
	a.noteServerMode(1) // a current reading, so only the two signals are in play

	// Idle state + safe mode => safe.
	a.gameState.Store(stateIdle)
	if !a.safeForPrintServer(1) {
		t.Error("idle state + safe mode should be safe")
	}
	// Safe mode but bus says active => NOT safe (bus is the stronger signal).
	a.gameState.Store(stateActive)
	if a.safeForPrintServer(1) {
		t.Error("active game state must block the print server even in a safe mode")
	}
	// Finishing grace period => NOT safe.
	a.gameState.Store(stateFinishing)
	if a.safeForPrintServer(7) {
		t.Error("finishing state must block the print server")
	}
	// Idle state but active mode => NOT safe (mode backstop).
	a.gameState.Store(stateIdle)
	if a.safeForPrintServer(6) {
		t.Error("active mode must block the print server even when state is idle")
	}
}

// Both idle signals are only refreshed by the poll loop, so an agent that has
// lost its O-Zone link holds a reading that says whatever was true when the
// link dropped. Not knowing the server state is not the same as knowing it is
// idle, so an aged reading must not open the gate.
func TestSafeForPrintServerRequiresACurrentReading(t *testing.T) {
	a := &App{}
	a.gameState.Store(stateIdle)

	// Never polled: the gate has nothing to go on.
	if a.safeForPrintServer(1) {
		t.Error("an agent that has never read SERVERMODE must not touch the print server")
	}
	if !errors.Is(a.printServerGate(), errLinkStale) {
		t.Errorf("gate = %v, want errLinkStale before the first poll", a.printServerGate())
	}

	// A current reading opens it.
	a.noteServerMode(1)
	if !a.safeForPrintServer(1) {
		t.Error("a current idle reading should be safe")
	}
	if err := a.printServerGate(); err != nil {
		t.Errorf("gate = %v, want nil on a current idle reading", err)
	}

	// The same reading, now older than the bound, closes it again.
	a.serverModeAt.Store(time.Now().Add(-maxServerModeAge - time.Second).UnixNano())
	if a.safeForPrintServer(1) {
		t.Error("a stale idle reading must not open the gate")
	}
	if !errors.Is(a.printServerGate(), errLinkStale) {
		t.Errorf("gate = %v, want errLinkStale on a stale reading", a.printServerGate())
	}

	// An active game still reports as such rather than as a stale link.
	a.noteServerMode(6)
	a.gameState.Store(stateActive)
	if !errors.Is(a.printServerGate(), errGameActive) {
		t.Errorf("gate = %v, want errGameActive during a game", a.printServerGate())
	}
}

// SERVERMODE arrives as untrusted JSON and is stored narrowed to int32, so a
// value outside that range truncates — and 2^32+1 truncates to 1, which is in
// printServerSafe's allowlist. A nonsense reading could therefore open the
// print-server gate during a live game.
func TestOutOfRangeServerModeClosesTheGate(t *testing.T) {
	if math.MaxInt <= math.MaxInt32 {
		// linux/arm/v7 is one of the published images: there int IS int32, so
		// the conversion cannot truncate and there is nothing to close.
		t.Skip("int is 32 bits here; SERVERMODE cannot truncate into int32")
	}

	a := &App{}
	a.gameState.Store(stateIdle)

	// 2^32 + 1. Narrowed to int32 that is exactly 1 — "idle", and inside
	// printServerSafe's allowlist. Built from a variable shift so the constant
	// cannot overflow int when this file is compiled for 32-bit.
	bits := 32
	truncatesToIdle := int(int64(1)<<bits | 1)

	a.noteServerMode(truncatesToIdle)

	if got := a.serverMode.Load(); got == 1 {
		t.Fatal("an out-of-range mode truncated into the safe allowlist")
	}
	if printServerSafe(int(a.serverMode.Load())) {
		t.Error("an unreadable server mode must not be treated as safe")
	}
	if gameActive(int(a.serverMode.Load())) {
		t.Error("an unreadable server mode must not be claimed as an active game either")
	}
	if a.printServerGate() == nil {
		t.Error("the print-server gate must stay closed on an unreadable mode")
	}

	// A mode that fits is still recorded exactly.
	a.noteServerMode(6)
	if got := a.serverMode.Load(); got != 6 {
		t.Fatalf("serverMode = %d, want 6", got)
	}
}

// deferrable separates "not now" from "this failed": neither an active game nor
// a stale link may burn a per-game retry attempt or a batch's failure budget.
func TestDeferrableCoversBothIdleRefusals(t *testing.T) {
	for _, err := range []error{errGameActive, errLinkStale} {
		if !deferrable(err) {
			t.Errorf("deferrable(%v) = false, want true", err)
		}
		if !deferrable(fmt.Errorf("wrapped: %w", err)) {
			t.Errorf("deferrable(wrapped %v) = false, want true", err)
		}
	}
	if deferrable(errFrameMismatch) {
		t.Error("a desync is a real failure, not a deferral")
	}
	if deferrable(nil) {
		t.Error("deferrable(nil) = true, want false")
	}
}

func TestReconcileStateBackstop(t *testing.T) {
	a := &App{}

	// An active mode forces active even if the bus said idle.
	a.gameState.Store(stateIdle)
	a.reconcileState(6)
	if a.gameState.Load() != stateActive {
		t.Error("active mode should promote state to active")
	}
	// A clearly-safe mode clears a stale active (bus missed the finish).
	a.reconcileState(1)
	if a.gameState.Load() != stateIdle {
		t.Error("safe mode should clear a stale active state")
	}
	// But a safe mode must NOT shortcut the finishing grace period.
	a.gameState.Store(stateFinishing)
	a.reconcileState(7)
	if a.gameState.Load() != stateFinishing {
		t.Error("safe mode must not clear the finishing grace period")
	}
}

func TestHandleBusEventTransitions(t *testing.T) {
	a := &App{pendingFetch: map[int]*pendingGame{}, fetchedGames: map[int]bool{}, givenUpFetch: map[int]bool{}}
	a.cfg.GameFinishDelay = 0 // afterFinish returns quickly

	start, _ := msgbus.Parse("[1001, 42, -1]")
	a.handleBusEvent(start)
	if a.gameState.Load() != stateActive {
		t.Fatal("GAME_START should set active")
	}
	a.mu.Lock()
	g := a.lastBusGame
	a.mu.Unlock()
	if g != 42 {
		t.Fatalf("lastBusGame = %d, want 42", g)
	}

	idle, _ := msgbus.Parse("[1000]")
	a.handleBusEvent(idle)
	if a.gameState.Load() != stateIdle {
		t.Fatal("IDLE should set idle")
	}
}

func TestMetaFromRaw(t *testing.T) {
	raw := ozonefix.Compact(ozonefix.AllResponseJSON())
	m := metaFromRaw(0, raw)
	if m.GameNumber != 9 {
		t.Errorf("GameNumber = %d, want 9", m.GameNumber)
	}
	if m.GameName != "Competition Team Elimination" {
		t.Errorf("GameName = %q", m.GameName)
	}
	if m.GameType != 1 {
		t.Errorf("GameType = %d, want 1", m.GameType)
	}
	if m.StartTime != "2020-02-18 16:06:05" {
		t.Errorf("StartTime = %q", m.StartTime)
	}
	if m.PlayerCount != 2 {
		t.Errorf("PlayerCount = %d, want 2", m.PlayerCount)
	}
	if m.Duration != 0 {
		t.Errorf("Duration should be left 0 (preserved from list), got %d", m.Duration)
	}
}

// A cache refresh reaches the print server from paths that do not depend on the
// poll loop — an operator pressing Resync on the control panel, or the Message
// Bus finish handler — while the reading it is gated on is written only BY the
// poll loop. So an agent that has lost its O-Zone link holds an "idle" that is
// just the last thing it saw, and the heaviest print-server operation there is
// (the full list, plus every uncached game) must not run on it.
func TestRefreshCacheRefusesOnAStaleLink(t *testing.T) {
	ps := ozonesim.NewPrintServer()
	if err := ps.Start(0); err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	host, port, _ := net.SplitHostPort(ps.Addr())

	a := New(config.Config{
		OzoneHost:        host,
		OzoneResultsPort: port,
		ResultsHandshake: 2,
		CacheEnabled:     true,
		CacheDir:         t.TempDir(),
		CentralURL:       "http://central.invalid",
		Token:            "test",
	})
	a.noteServerMode(1) // idle...
	a.gameState.Store(stateIdle)
	a.serverModeAt.Store(time.Now().Add(-maxServerModeAge - time.Second).UnixNano()) // ...as of a while ago

	a.refreshCache()

	if ps.Connections() != 0 {
		t.Fatalf("print server was contacted on a stale reading (connections=%d)", ps.Connections())
	}
}

// End-to-end: against a fake O-Zone print server, refreshCache fills the cache
// verbatim when idle, and refuses entirely while a game is active.
func TestRefreshCacheIdleGated(t *testing.T) {
	ps := ozonesim.NewPrintServer()
	if err := ps.Start(0); err != nil {
		t.Fatal(err)
	}
	defer ps.Close()
	host, port, _ := net.SplitHostPort(ps.Addr())

	cfg := config.Config{
		OzoneHost:        host,
		OzoneResultsPort: port,
		ResultsHandshake: 2,
		CacheEnabled:     true,
		CacheDir:         t.TempDir(),
		CentralURL:       "http://central.invalid",
		Token:            "test",
	}

	// While a game is active, refreshCache must not even connect.
	active := New(cfg)
	active.noteServerMode(6)
	active.gameState.Store(stateActive)
	active.refreshCache()
	if ps.Connections() != 0 {
		t.Fatalf("print server must not be contacted during a game (connections=%d)", ps.Connections())
	}

	// When idle, refreshCache pulls the list and caches the game verbatim.
	idle := New(cfg)
	idle.noteServerMode(1)
	idle.gameState.Store(stateIdle)
	idle.refreshCache()

	if ps.Connections() == 0 {
		t.Fatal("print server should have been contacted when idle")
	}
	raw, ok := idle.cache.GameRaw(9)
	if !ok {
		t.Fatal("game 9 should be cached after an idle refresh")
	}
	want := ozonefix.Compact(ozonefix.AllResponseJSON())
	if !bytes.Equal(raw, want) {
		t.Fatal("cached payload is not byte-verbatim with what O-Zone served")
	}
}
