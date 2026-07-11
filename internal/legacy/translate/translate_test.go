package translate

import (
	"encoding/json"
	"testing"
	"time"

	"overwatch/agent/internal/legacy/lasertag"
)

func TestServerModeMapping(t *testing.T) {
	cases := []struct {
		nexusMode, gameID, want int
	}{
		{3, 100, ozoneModeInProgress}, // running
		{7, 100, ozoneModeFinished},   // finished
		{5, 100, ozoneModeInProgress}, // abort → still treated active (has game)
		{0, 0, ozoneModeIdle},         // no game
		{1, 0, ozoneModeIdle},         // idle mode, no game
	}
	for _, c := range cases {
		if got := ServerMode(c.nexusMode, c.gameID); got != c.want {
			t.Errorf("ServerMode(%d,%d) = %d, want %d", c.nexusMode, c.gameID, got, c.want)
		}
	}
}

func TestTelemetryPayloadOmitsUnavailableMetrics(t *testing.T) {
	r := 3
	packs := []lasertag.PackLive{
		{PackID: 1, TeamID: 0, Online: true, Playing: true, State: 6, Allocated: true,
			Scraped: lasertag.ScrapedPack{Resets: &r}},
	}
	status := lasertag.ServerStatus{ServerMode: 3, GameID: 42, Elapsed: 10, Duration: 600}

	out := TelemetryPayload(7, time.Unix(1700000000, 0), packs, status, map[int]string{1: "Batman"})

	packsOut := out["packs"].([]map[string]any)
	if len(packsOut) != 1 {
		t.Fatalf("want 1 pack, got %d", len(packsOut))
	}
	p := packsOut[0]
	if p["ALIAS"] != "Batman" {
		t.Errorf("alias = %v, want Batman (from roster)", p["ALIAS"])
	}
	if p["CONNECTED"] != true || p["PLAYING"] != true {
		t.Errorf("connected/playing flags wrong: %v", p)
	}
	if p["RESETS"] != 3 {
		t.Errorf("resets = %v, want 3", p["RESETS"])
	}
	// The unavailable telemetry must be ABSENT (→ null in central), not zero.
	for _, k := range []string{"BATT_MV", "WIFI_SIGNAL", "SYS_TEMP"} {
		if _, present := p[k]; present {
			t.Errorf("%s should be omitted for legacy packs, got %v", k, p[k])
		}
	}
	srv := out["server_state"].(map[string]any)
	if srv["SERVERMODE"] != ozoneModeInProgress || srv["GAMENUM"] != 42 {
		t.Errorf("server_state wrong: %v", srv)
	}
}

func TestGameResultsPayloadShapeMatchesAnalyzer(t *testing.T) {
	// Player 1: dealt weapon+front tags, received a back tag, logged-in member.
	players := []PlayerAgg{
		{
			PackID: 1, PackName: "Blade", TeamID: 0, MemberID: 749, Shots: 10,
			DealtByZone:    [7]int{2, 3, 0, 0, 0, 0, 0}, // weapon=2, front=3 → total 5
			ReceivedByZone: [7]int{0, 0, 0, 0, 0, 0, 1}, // back=1 → total 1
		},
		{PackID: 2, PackName: "Vader", TeamID: 1, MemberID: -1, Shots: 4}, // walk-in
	}
	meta := GameMeta{GameNumber: 23331, GameName: "TDM", GameType: 1, Duration: 720, Date: "2026-03-20 04:42:32"}

	out := GameResultsPayload(meta, players)

	if out["game_number"] != 23331 {
		t.Fatalf("game_number = %v", out["game_number"])
	}
	data := out["data"].(map[string]any)
	playerMap := data["players"].(map[string]any)
	p1 := playerMap["1"].(map[string]any)

	if p1["omid"] != "749" {
		t.Errorf("logged-in member omid = %v, want 749", p1["omid"])
	}
	if playerMap["2"].(map[string]any)["omid"] != "-1" {
		t.Errorf("walk-in omid should be -1")
	}
	// accuracy = dealt(5)/shots(10) = 50
	if p1["acc"] != 50 {
		t.Errorf("acc = %v, want 50", p1["acc"])
	}
	// Fitness zeroed (unavailable on legacy).
	if p1["steps"] != 0 || p1["distance"] != 0 || p1["calories"] != 0 {
		t.Errorf("fitness should be zero on legacy: %v", p1)
	}

	// The zone arrays must round-trip through sumDetailedZones' expected shape:
	// tagsbyl = dealt, entry keys "0".."6".
	tagsbyl := p1["tagsbyl"].([]map[string]int)
	if len(tagsbyl) != 1 || tagsbyl[0]["0"] != 2 || tagsbyl[0]["1"] != 3 {
		t.Errorf("tagsbyl zone map wrong: %v", tagsbyl)
	}
	tagsonl := p1["tagsonl"].([]map[string]int)
	if tagsonl[0]["6"] != 1 {
		t.Errorf("tagsonl back zone = %v, want 1", tagsonl[0]["6"])
	}

	// Whole payload must be JSON-serialisable (it's posted as-is).
	if _, err := json.Marshal(out); err != nil {
		t.Fatalf("payload not serialisable: %v", err)
	}
}

func TestAccuracyGuardsZeroShots(t *testing.T) {
	if accuracy(5, 0) != 0 {
		t.Error("accuracy with zero shots must be 0, not a divide-by-zero")
	}
	if accuracy(3, 6) != 50 {
		t.Errorf("accuracy(3,6) = %d, want 50", accuracy(3, 6))
	}
}
