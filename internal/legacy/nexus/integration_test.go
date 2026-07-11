//go:build integration

// Integration tests that run the real queries against a MySQL/MariaDB seeded
// from the production Nexus dumps (resources/*.sql). Enabled with the
// `integration` build tag and a NEXUS_TEST_DSN pointing at the seeded DB; the
// CI "legacy-nexus" job wires both up. Assertions are against real game 23331
// (10 players, all Shots_Fired=0, 1841 events).
package nexus

import (
	"context"
	"database/sql"
	"os"
	"testing"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dsn := os.Getenv("NEXUS_TEST_DSN")
	if dsn == "" {
		t.Skip("NEXUS_TEST_DSN not set")
	}
	raw, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.Ping(); err != nil {
		t.Fatalf("ping: %v", err)
	}
	return FromDB(raw)
}

func TestIntegrationFinishedGames(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	games, err := db.FinishedGamesSince(context.Background(), 23330, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(games) == 0 {
		t.Fatal("expected finished games after 23330")
	}
	if games[0].GameNumber != 23331 {
		t.Errorf("first game = %d, want 23331", games[0].GameNumber)
	}
	if games[0].Duration != 720 {
		t.Errorf("game 23331 duration = %d, want 720", games[0].Duration)
	}
	if games[0].Date == "" {
		t.Error("game 23331 date should be populated")
	}
}

func TestIntegrationGameResults(t *testing.T) {
	db := openTestDB(t)
	defer db.Close()

	players, err := db.GameResults(context.Background(), 23331)
	if err != nil {
		t.Fatal(err)
	}
	if len(players) != 10 {
		t.Fatalf("game 23331 players = %d, want 10", len(players))
	}

	totalShots, totalTags, withMember := 0, 0, 0
	for _, p := range players {
		totalShots += p.Shots
		for i := 0; i < 7; i++ {
			totalTags += p.DealtByZone[i] + p.ReceivedByZone[i]
		}
		if p.MemberID > 0 {
			withMember++
		}
		if p.PackName == "" {
			t.Errorf("pack %d has no name", p.PackID)
		}
	}
	// ng_player_stats.Shots_Fired is 0 for this game, so shots must have been
	// reconstructed from trigger (event 35) rows.
	if totalShots == 0 {
		t.Error("expected shots reconstructed from the event log, got 0")
	}
	// A real game logged plenty of tag events across zones.
	if totalTags == 0 {
		t.Error("expected per-zone tag counts from the event log, got 0")
	}
	// Every pack in this game had a logged-in member (2936..2952).
	if withMember != 10 {
		t.Errorf("players with a member = %d, want 10", withMember)
	}
}
