package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// This file reconstructs Torn5's O-Zone parser (Torn5/OZone.cs) in Go and asserts
// the proxy's responses satisfy every field TORN reads. TORN is read-only and is
// NOT modified; this is the executable form of the §8 contract in
// docs/OZONE_PRINT_SERVER_API.md. Line references are to OZone.cs.

// --- TORN's data model (subset OZone.cs populates) -------------------------

type tornGame struct {
	GameID    int       // gamenum
	StartTime time.Time // starttime (invariant culture)
	EndTime   time.Time // endtime, falls back to starttime
	OnServer  bool      // valid > 0
}

type tornPlayer struct {
	Alias      string
	PlayerID   string // omid, or alias when omid == "-1"
	HitsOn     int    // <- tagsby  (deliberately swapped, OZone.cs:322)
	HitsBy     int    // <- tagson  (deliberately swapped)
	Rank       uint
	Eliminated bool // elim > 0
	Colour     int  // tid+1 for tid in 0..7, else 0 (Colour.None)
	TeamID     int  // tid
}

type tornEvent struct {
	EventType int // evtyp
	TimeSecs  int // time
	IDFor     int // idf
	IDAgainst int // ida
	TeamFor   int // tidf
	TeamAgst  int // tida
	Score     int // score
}

// --- TORN's parse logic (mirrors OZone.cs) ---------------------------------

// tornParseList mirrors OZone.cs GetGames (lines 59-107): requires a "gamelist"
// key; surfaces only games with valid > 0; parses starttime/endtime with the
// invariant culture and falls endtime back to starttime on failure.
func tornParseList(t *testing.T, payload []byte) []tornGame {
	t.Helper()
	if len(payload) < 6 { // OZone.cs:59 (result.Length < 6)
		return nil
	}
	var root struct {
		GameList []map[string]any `json:"gamelist"`
	}
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatalf("torn: gamelist parse failed (OZone.cs would reconnect): %v", err)
	}
	if root.GameList == nil {
		t.Fatal("torn: missing 'gamelist' key — OZone.cs throws NullReferenceException here")
	}

	var games []tornGame
	for _, g := range root.GameList {
		valid, hasValid := asInt(g["valid"])
		if !hasValid || valid <= 0 { // OZone.cs:96 — only valid>0 surfaces
			continue
		}
		gamenum, ok := asInt(g["gamenum"])
		if !ok {
			t.Error("torn: valid game missing parseable gamenum (needed for the 'all' request)")
			continue
		}
		start, err := tornParseTime(g["starttime"])
		if err != nil {
			t.Errorf("torn: starttime not invariant-parseable: %v", err)
		}
		end, err := tornParseTime(g["endtime"])
		if err != nil {
			end = start // OZone.cs:88-93 fallback
		}
		games = append(games, tornGame{GameID: gamenum, StartTime: start, EndTime: end, OnServer: true})
	}
	return games
}

// tornParseGame mirrors OZone.cs PopulateGame (lines 197-364).
func tornParseGame(t *testing.T, payload []byte) (map[string]tornPlayer, map[string]tornEvent) {
	t.Helper()
	if len(payload) < 6 {
		return nil, nil
	}
	var root struct {
		Players map[string]map[string]any `json:"players"`
		Events  map[string]map[string]any `json:"events"`
	}
	if err := json.Unmarshal(payload, &root); err != nil {
		// OZone.cs:365-371 catches and leaves the game unpopulated to retry.
		t.Fatalf("torn: game parse failed: %v", err)
	}

	players := map[string]tornPlayer{}
	for id, p := range root.Players {
		tp := tornPlayer{}
		tp.Alias, _ = p["alias"].(string)

		tagsby, _ := asInt(p["tagsby"])
		tagson, _ := asInt(p["tagson"])
		tp.HitsOn = tagsby // swap, OZone.cs:322
		tp.HitsBy = tagson // swap

		if r, ok := asInt(p["rank"]); ok && r >= 0 {
			tp.Rank = uint(r)
		}
		if e, ok := asInt(p["elim"]); ok {
			tp.Eliminated = e > 0
		}

		omid := asString(p["omid"])
		if omid == "-1" || omid == "" { // OZone.cs:329-340
			tp.PlayerID = tp.Alias
		} else {
			tp.PlayerID = omid
		}

		tid, _ := asInt(p["tid"])
		tp.TeamID = tid
		if tid >= 0 && tid <= 7 { // OZone.cs:342-350
			tp.Colour = tid + 1
		}
		players[id] = tp
	}

	events := map[string]tornEvent{}
	for id, e := range root.Events {
		te := tornEvent{}
		te.EventType, _ = asInt(e["evtyp"])
		te.TimeSecs, _ = asInt(e["time"])
		te.IDFor, _ = asInt(e["idf"])
		te.IDAgainst, _ = asInt(e["ida"])
		te.TeamFor, _ = asInt(e["tidf"])
		te.TeamAgst, _ = asInt(e["tida"])
		te.Score, _ = asInt(e["score"])
		events[id] = te
	}
	return players, events
}

func tornParseTime(v any) (time.Time, error) {
	s, ok := v.(string)
	if !ok || s == "" {
		return time.Time{}, fmt.Errorf("not a string: %v", v)
	}
	if s == "None" {
		return time.Time{}, fmt.Errorf("None")
	}
	// O-Zone uses SQL NOW() format "2006-01-02 15:04:05" (invariant).
	return time.Parse("2006-01-02 15:04:05", strings.TrimSpace(s))
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func asString(v any) string {
	switch s := v.(type) {
	case string:
		return s
	case float64:
		return fmt.Sprintf("%d", int(s))
	default:
		return ""
	}
}

// --- The contract assertions -----------------------------------------------

func TestTornCanConsumeProxyList(t *testing.T) {
	p, _ := startProxy(t)
	conn := dial(t, p.Addr())
	defer conn.Close()

	games := tornParseList(t, ask(t, conn, map[string]any{"command": "list"}))
	if len(games) != 1 {
		t.Fatalf("TORN should see 1 valid game, saw %d", len(games))
	}
	g := games[0]
	if g.GameID != 9 {
		t.Errorf("GameID = %d, want 9", g.GameID)
	}
	if g.StartTime.IsZero() {
		t.Error("TORN could not parse starttime")
	}
	if !g.EndTime.After(g.StartTime) {
		t.Errorf("end time %v should be after start %v", g.EndTime, g.StartTime)
	}
}

func TestTornCanConsumeProxyGame(t *testing.T) {
	p, _ := startProxy(t)
	conn := dial(t, p.Addr())
	defer conn.Close()

	players, events := tornParseGame(t, ask(t, conn, map[string]any{"command": "all", "gamenumber": 9}))
	if len(players) != 2 {
		t.Fatalf("want 2 players, got %d", len(players))
	}
	if len(events) != 4 {
		t.Fatalf("want 4 events, got %d", len(events))
	}

	// Player 10 "Krieger": raw tagsby=1, tagson=0 -> HitsOn=1, HitsBy=0 (swapped).
	krieger := players["10"]
	if krieger.Alias != "Krieger" {
		t.Errorf("player 10 alias = %q", krieger.Alias)
	}
	if krieger.HitsOn != 1 || krieger.HitsBy != 0 {
		t.Errorf("tagsby/tagson swap wrong: HitsOn=%d HitsBy=%d, want 1/0", krieger.HitsOn, krieger.HitsBy)
	}
	// omid == -1 -> PlayerID falls back to alias.
	if krieger.PlayerID != "Krieger" {
		t.Errorf("omid==-1 should fall back to alias, got PlayerID=%q", krieger.PlayerID)
	}
	// tid 2 -> Colour 3.
	if krieger.Colour != 3 {
		t.Errorf("tid 2 -> Colour = %d, want 3", krieger.Colour)
	}
	if krieger.Rank != 2 {
		t.Errorf("rank = %d, want 2", krieger.Rank)
	}

	blade := players["1"]
	if blade.HitsOn != 0 || blade.HitsBy != 1 {
		t.Errorf("Blade swap wrong: HitsOn=%d HitsBy=%d, want 0/1", blade.HitsOn, blade.HitsBy)
	}
	if blade.Colour != 1 { // tid 0 -> Colour 1
		t.Errorf("Blade colour = %d, want 1", blade.Colour)
	}

	// Event "1": a foe tag (evtyp 0) from player 1 against player 10 at t=24.
	ev1 := events["1"]
	if ev1.EventType != 0 || ev1.IDFor != 1 || ev1.IDAgainst != 10 || ev1.TimeSecs != 24 {
		t.Errorf("event 1 = %+v, want {evtyp0 idf1 ida10 t24}", ev1)
	}
	// Event "2": tagged-by-foe-on-weapon (evtyp 14).
	if events["2"].EventType != 14 {
		t.Errorf("event 2 evtyp = %d, want 14", events["2"].EventType)
	}
}

// A game TORN requests but the cache hasn't fetched yet must not crash TORN's
// parser: it parses, finds no players/events, and (per OZone.cs) retries later.
func TestTornToleratesMissingGame(t *testing.T) {
	p, _ := startProxy(t)
	conn := dial(t, p.Addr())
	defer conn.Close()

	players, events := tornParseGame(t, ask(t, conn, map[string]any{"command": "all", "gamenumber": 4242}))
	if len(players) != 0 || len(events) != 0 {
		t.Errorf("missing game should yield no players/events, got %d/%d", len(players), len(events))
	}
}
