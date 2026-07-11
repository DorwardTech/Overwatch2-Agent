package nexus

import (
	"context"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestFinishedGamesSince(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("SELECT g.Game_ID").
		WithArgs(23330, 100).
		WillReturnRows(sqlmock.NewRows([]string{"Game_ID", "name", "teams", "Duration", "date"}).
			AddRow(23331, "Standard Team", 3, 720, "2026-03-20 04:42:32").
			AddRow(23340, "Solo Elimination", 1, 600, "2026-03-20 10:21:45"))

	games, err := FromDB(db).FinishedGamesSince(context.Background(), 23330, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(games) != 2 {
		t.Fatalf("got %d games, want 2", len(games))
	}
	// teams > 1 → team game (gametype 1); teams == 1 → solo (0).
	if games[0].GameType != 1 || games[1].GameType != 0 {
		t.Errorf("gametype mapping wrong: %d, %d", games[0].GameType, games[1].GameType)
	}
	if games[0].GameNumber != 23331 || games[0].Duration != 720 {
		t.Errorf("game 0 wrong: %+v", games[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestGameResultsAggregatesEventLog(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	// Roster: two packs, one logged-in member, one walk-in.
	mock.ExpectQuery("FROM ng_player_stats").
		WithArgs(23331).
		WillReturnRows(sqlmock.NewRows([]string{"Pack_ID", "Team_ID", "Pack_Name", "Member_ID", "Shots_Fired"}).
			AddRow(1, 0, "Blade", 749, 0).
			AddRow(2, 1, "Vader", -1, 4))

	// Event log grouped by (Player_ID, Event_Type):
	//   pack 1: dealt weapon(0)×2, dealt front(1)×3, received back(20)×1, 35 triggers×10
	//   pack 2: dealt front(1)×1
	mock.ExpectQuery("FROM ng_player_event_log").
		WithArgs(23331).
		WillReturnRows(sqlmock.NewRows([]string{"Player_ID", "Event_Type", "count"}).
			AddRow(1, 0, 2).
			AddRow(1, 1, 3).
			AddRow(1, 20, 1).
			AddRow(1, 35, 10).
			AddRow(2, 1, 1))

	players, err := FromDB(db).GameResults(context.Background(), 23331)
	if err != nil {
		t.Fatal(err)
	}
	if len(players) != 2 {
		t.Fatalf("got %d players, want 2", len(players))
	}

	byID := map[int]int{}
	for i, p := range players {
		byID[p.PackID] = i
	}
	p1 := players[byID[1]]
	if p1.DealtByZone[0] != 2 || p1.DealtByZone[1] != 3 {
		t.Errorf("pack 1 dealt zones wrong: %v", p1.DealtByZone)
	}
	if p1.ReceivedByZone[6] != 1 { // event 20 → received zone 6 (back)
		t.Errorf("pack 1 received back = %d, want 1", p1.ReceivedByZone[6])
	}
	// Shots_Fired was 0 → overridden by the 10 trigger events.
	if p1.Shots != 10 {
		t.Errorf("pack 1 shots = %d, want 10 (from trigger events)", p1.Shots)
	}
	if p1.MemberID != 749 {
		t.Errorf("pack 1 member = %d, want 749", p1.MemberID)
	}
	// Pack 2 kept its non-zero Shots_Fired (4), not overridden.
	p2 := players[byID[2]]
	if p2.Shots != 4 {
		t.Errorf("pack 2 shots = %d, want 4 (kept from stats)", p2.Shots)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Error(err)
	}
}

func TestEventZoneMap(t *testing.T) {
	cases := []struct {
		event, zone int
		recv, ok    bool
	}{
		{0, 0, false, true},   // dealt weapon
		{6, 6, false, true},   // dealt back
		{14, 0, true, true},   // received weapon
		{20, 6, true, true},   // received back
		{35, 0, false, false}, // trigger — not a zone tag
		{30, 0, false, false}, // base hit
	}
	for _, c := range cases {
		z, r, ok := eventZone(c.event)
		if z != c.zone || r != c.recv || ok != c.ok {
			t.Errorf("eventZone(%d) = (%d,%v,%v), want (%d,%v,%v)", c.event, z, r, ok, c.zone, c.recv, c.ok)
		}
	}
}

func TestRoster(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	mock.ExpectQuery("FROM ng_packs").
		WillReturnRows(sqlmock.NewRows([]string{"Pack_ID", "Description"}).
			AddRow(1, "Astro").AddRow(2, "Blade"))

	roster, err := FromDB(db).Roster(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if roster[1] != "Astro" || roster[2] != "Blade" {
		t.Errorf("roster wrong: %v", roster)
	}
}
