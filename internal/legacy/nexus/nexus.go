// Package nexus reads game history and roster data from a legacy P&C Micros
// Nexus laser-tag MySQL database (schema `ng_system`). It is read-only — the
// game server owns the data and caches state, so the agent never writes here.
// Rows are assembled into the shapes package translate turns into central
// payloads.
//
// The database is the system of record for finished games; live pack state
// (online/playing) is not modelled here and comes from the on-box management
// app instead (package lasertag).
package nexus

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"overwatch/agent/internal/legacy/translate"

	_ "github.com/go-sql-driver/mysql"
)

// DB wraps a read-only connection to the Nexus MySQL database.
type DB struct {
	db *sql.DB
}

// Open connects using a go-sql-driver DSN, e.g.
// "ow2ro:secret@tcp(10.0.0.2:3306)/ng_system?parseTime=true&timeout=5s".
func Open(dsn string) (*DB, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	db.SetConnMaxLifetime(5 * time.Minute)
	return &DB{db: db}, nil
}

// FromDB wraps an existing *sql.DB (used by tests with a mock).
func FromDB(db *sql.DB) *DB { return &DB{db: db} }

func (d *DB) Close() error { return d.db.Close() }

// Ping verifies the connection is alive.
func (d *DB) Ping(ctx context.Context) error { return d.db.PingContext(ctx) }

// FinishedGamesSince lists finished games with an id greater than afterGameID
// (0 = all), oldest first, capped at limit. Only games that have actually
// finished (Finish_Time set) are returned, so an in-progress game isn't synced
// mid-play.
func (d *DB) FinishedGamesSince(ctx context.Context, afterGameID, limit int) ([]translate.GameMeta, error) {
	const q = `
		SELECT g.Game_ID, COALESCE(p.Profile_Description, p.Game, ''), COALESCE(p.Number_Of_Teams, 1),
		       g.Duration, DATE_FORMAT(g.Start_Time, '%Y-%m-%d %H:%i:%s')
		FROM ng_game_stats g
		LEFT JOIN ng_profiles p ON p.Profile_ID = g.Profile_ID
		WHERE g.Game_ID > ?
		  AND g.Finish_Time > '2000-01-01 00:00:00'
		ORDER BY g.Game_ID ASC
		LIMIT ?`

	rows, err := d.db.QueryContext(ctx, q, afterGameID, limit)
	if err != nil {
		return nil, fmt.Errorf("nexus: list games: %w", err)
	}
	defer rows.Close()

	var games []translate.GameMeta
	for rows.Next() {
		var g translate.GameMeta
		var teams int
		if err := rows.Scan(&g.GameNumber, &g.GameName, &teams, &g.Duration, &g.Date); err != nil {
			return nil, err
		}
		// gametype: O-Zone uses 0 = solo, 1 = team; Nexus stores team count.
		if teams > 1 {
			g.GameType = 1
		}
		games = append(games, g)
	}
	return games, rows.Err()
}

// GameResults reconstructs per-pack stats for one game: roster + shots from
// ng_player_stats, and per-zone tag counts aggregated from ng_player_event_log
// against the fixed event-type→zone map.
func (d *DB) GameResults(ctx context.Context, gameID int) ([]translate.PlayerAgg, error) {
	players, err := d.playerRoster(ctx, gameID)
	if err != nil {
		return nil, err
	}
	if len(players) == 0 {
		return nil, nil
	}
	if err := d.applyEventCounts(ctx, gameID, players); err != nil {
		return nil, err
	}

	out := make([]translate.PlayerAgg, 0, len(players))
	for _, p := range players {
		out = append(out, *p)
	}
	return out, nil
}

// playerRoster reads one row per pack in the game.
func (d *DB) playerRoster(ctx context.Context, gameID int) (map[int]*translate.PlayerAgg, error) {
	const q = `SELECT Pack_ID, Team_ID, Pack_Name, Member_ID, Shots_Fired
	           FROM ng_player_stats WHERE Game_ID = ?`
	rows, err := d.db.QueryContext(ctx, q, gameID)
	if err != nil {
		return nil, fmt.Errorf("nexus: player roster: %w", err)
	}
	defer rows.Close()

	players := map[int]*translate.PlayerAgg{}
	for rows.Next() {
		p := &translate.PlayerAgg{}
		var name sql.NullString
		if err := rows.Scan(&p.PackID, &p.TeamID, &name, &p.MemberID, &p.Shots); err != nil {
			return nil, err
		}
		p.PackName = name.String
		players[p.PackID] = p
	}
	return players, rows.Err()
}

// eventZone maps a Nexus Event_Type to a (physical zone 0..6, received?) pair.
// Dealt-to-foe events are 0..6; tagged-by-foe events are 14..20. Returns
// ok=false for events that aren't per-zone tags.
func eventZone(eventType int) (zone int, received, ok bool) {
	switch {
	case eventType >= 0 && eventType <= 6:
		return eventType, false, true // dealt
	case eventType >= 14 && eventType <= 20:
		return eventType - 14, true, true // received
	default:
		return 0, false, false
	}
}

// applyEventCounts sums per-zone tag counts and trigger pulls (event 35) from
// the event log into the roster. Shots default to the ng_player_stats value but
// are overridden by the trigger-event count when the firmware left it zero.
func (d *DB) applyEventCounts(ctx context.Context, gameID int, players map[int]*translate.PlayerAgg) error {
	const q = `SELECT Player_ID, Event_Type, COUNT(*)
	           FROM ng_player_event_log WHERE Game_ID = ?
	           GROUP BY Player_ID, Event_Type`
	rows, err := d.db.QueryContext(ctx, q, gameID)
	if err != nil {
		return fmt.Errorf("nexus: event counts: %w", err)
	}
	defer rows.Close()

	triggers := map[int]int{}
	for rows.Next() {
		var packID, eventType, count int
		if err := rows.Scan(&packID, &eventType, &count); err != nil {
			return err
		}
		p := players[packID]
		if p == nil {
			continue // event for a pack not in the roster; ignore
		}
		if eventType == 35 {
			triggers[packID] = count
			continue
		}
		if zone, received, ok := eventZone(eventType); ok {
			if received {
				p.ReceivedByZone[zone] += count
			} else {
				p.DealtByZone[zone] += count
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Prefer the trigger-event count for shots when it's available and the
	// stats column was left at zero (the firmware sometimes doesn't fill it).
	for id, p := range players {
		if p.Shots == 0 && triggers[id] > 0 {
			p.Shots = triggers[id]
		}
	}
	return nil
}

// Roster returns pack id → current alias from NG_Packs, used to label live packs
// (the live COM snapshot has no alias).
func (d *DB) Roster(ctx context.Context) (map[int]string, error) {
	const q = `SELECT Pack_ID, Description FROM ng_packs`
	rows, err := d.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("nexus: roster: %w", err)
	}
	defer rows.Close()

	roster := map[int]string{}
	for rows.Next() {
		var id int
		var desc sql.NullString
		if err := rows.Scan(&id, &desc); err != nil {
			return nil, err
		}
		roster[id] = desc.String
	}
	return roster, rows.Err()
}
