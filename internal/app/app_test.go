package app

import "testing"

func TestPrintServerSafe(t *testing.T) {
	// Safe: idle(1), running(2), finished(7), aborted(8), history(9), post-game(18).
	for _, mode := range []int{1, 2, 7, 8, 9, 18} {
		if !printServerSafe(mode) {
			t.Errorf("mode %d should be print-server safe", mode)
		}
	}
	// Unsafe: any active/transitioning game state.
	for _, mode := range []int{3, 4, 5, 6, 10, 12, 13, 14, 15, 16, 17, 0, -1, 11} {
		if printServerSafe(mode) {
			t.Errorf("mode %d must NOT be print-server safe (game active)", mode)
		}
	}
}

func TestGameActive(t *testing.T) {
	for _, mode := range []int{3, 4, 5, 6, 10, 12, 13, 14, 15, 16, 17} {
		if !gameActive(mode) {
			t.Errorf("mode %d should count as game-active (fast poll)", mode)
		}
	}
	for _, mode := range []int{1, 2, 7, 8, 9, 18} {
		if gameActive(mode) {
			t.Errorf("mode %d should NOT be game-active (slow poll)", mode)
		}
	}
}

func TestGameInProgressNeverSafe(t *testing.T) {
	// The single most important invariant: mode 6 (Game in progress) must be
	// both fast-poll and print-server-unsafe.
	if printServerSafe(6) {
		t.Fatal("mode 6 (game in progress) must never be print-server safe")
	}
	if !gameActive(6) {
		t.Fatal("mode 6 (game in progress) must be game-active")
	}
}

func TestIsResultsCommand(t *testing.T) {
	// cache_resync hits the print server, so it must be idle-gated like the
	// other results commands.
	for _, typ := range []string{"refetch_game", "backfill_all", "resync_all", "cache_resync"} {
		if !isResultsCommand(typ) {
			t.Errorf("%q should be a results command", typ)
		}
	}
	// agent_overview and cache_purge are local-only (no print-server access),
	// so they must stay safe to run during a live game.
	for _, typ := range []string{"reboot_agent", "agent_overview", "cache_purge"} {
		if isResultsCommand(typ) {
			t.Errorf("%q is not a results command", typ)
		}
	}
}
