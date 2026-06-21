package app

import (
	"bytes"
	"net"
	"testing"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/msgbus"
	"overwatch/agent/internal/ozonefix"
	"overwatch/agent/internal/ozonesim"
)

func TestSafeForPrintServerRequiresBothSignals(t *testing.T) {
	a := &App{}

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
	a := &App{pendingFetch: map[int]bool{}, fetchedGames: map[int]bool{}}
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
	active.serverMode.Store(6)
	active.gameState.Store(stateActive)
	active.refreshCache()
	if ps.Connections() != 0 {
		t.Fatalf("print server must not be contacted during a game (connections=%d)", ps.Connections())
	}

	// When idle, refreshCache pulls the list and caches the game verbatim.
	idle := New(cfg)
	idle.serverMode.Store(1)
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
