package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/legacy/lasertag"
	"overwatch/agent/internal/legacy/translate"
)

// --- fakes ---

type fakeNexus struct {
	roster  map[int]string
	games   []translate.GameMeta
	results map[int][]translate.PlayerAgg
}

func (f *fakeNexus) Ping(context.Context) error                     { return nil }
func (f *fakeNexus) Roster(context.Context) (map[int]string, error) { return f.roster, nil }
func (f *fakeNexus) FinishedGamesSince(_ context.Context, after, _ int) ([]translate.GameMeta, error) {
	var out []translate.GameMeta
	for _, g := range f.games {
		if g.GameNumber > after {
			out = append(out, g)
		}
	}
	return out, nil
}
func (f *fakeNexus) GameResults(_ context.Context, id int) ([]translate.PlayerAgg, error) {
	return f.results[id], nil
}

type fakeLasertag struct {
	packs  []lasertag.PackLive
	status lasertag.ServerStatus
}

func (f *fakeLasertag) PacksLive(context.Context) ([]lasertag.PackLive, error) { return f.packs, nil }
func (f *fakeLasertag) Status(context.Context) (lasertag.ServerStatus, error)  { return f.status, nil }

type capturePusher struct {
	mu        sync.Mutex
	telemetry [][]byte
	games     map[int]map[string]any
}

func (c *capturePusher) Push(payload []byte, _ string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.telemetry = append(c.telemetry, payload)
	return nil
}
func (c *capturePusher) PushGameResults(n int, data map[string]any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.games == nil {
		c.games = map[int]map[string]any{}
	}
	c.games[n] = data
	return nil
}

// --- tests ---

func TestPollTelemetryPushesTranslatedBatch(t *testing.T) {
	r := 4
	lt := &fakeLasertag{
		packs: []lasertag.PackLive{
			{PackID: 1, Online: true, Playing: true, State: 6, Scraped: lasertag.ScrapedPack{Resets: &r}},
		},
		status: lasertag.ServerStatus{ServerMode: 3, GameID: 42, Elapsed: 12, Duration: 600},
	}
	push := &capturePusher{}
	a := newWithDeps(config.Config{}, &fakeNexus{}, lt, push)
	a.roster = map[int]string{1: "Astro"}

	a.pollTelemetry(context.Background())

	if len(push.telemetry) != 1 {
		t.Fatalf("expected 1 telemetry push, got %d", len(push.telemetry))
	}
	var payload map[string]any
	if err := json.Unmarshal(push.telemetry[0], &payload); err != nil {
		t.Fatal(err)
	}
	packs := payload["packs"].([]any)
	pack := packs[0].(map[string]any)
	if pack["ALIAS"] != "Astro" || pack["RESETS"] != float64(4) {
		t.Errorf("pack payload wrong: %v", pack)
	}
	if _, hasBattery := pack["BATT_MV"]; hasBattery {
		t.Error("legacy pack must not carry battery voltage")
	}
}

func TestSyncGamesPushesNewFinishedGamesOnce(t *testing.T) {
	nx := &fakeNexus{
		games: []translate.GameMeta{
			{GameNumber: 100, GameName: "TDM", Duration: 600, Date: "2026-03-20 04:42:32"},
			{GameNumber: 101, GameName: "Solo", Duration: 720, Date: "2026-03-20 05:00:00"},
		},
		results: map[int][]translate.PlayerAgg{
			100: {{PackID: 1, PackName: "Blade", MemberID: 749, Shots: 5, DealtByZone: [7]int{1}}},
			101: {{PackID: 2, PackName: "Vader", MemberID: -1, Shots: 3}},
		},
	}
	push := &capturePusher{}
	a := newWithDeps(config.Config{}, nx, &fakeLasertag{}, push)

	a.syncGames(context.Background())
	if len(push.games) != 2 {
		t.Fatalf("expected 2 games pushed, got %d", len(push.games))
	}
	if a.lastGameID != 101 {
		t.Fatalf("lastGameID = %d, want 101", a.lastGameID)
	}

	// A second sync with no newer games pushes nothing.
	before := len(push.games)
	a.syncGames(context.Background())
	if len(push.games) != before {
		t.Errorf("re-sync pushed already-synced games")
	}

	// The pushed game carries the omid + zone shape central expects.
	g100 := push.games[100]
	players := g100["players"].(map[string]any)
	p1 := players["1"].(map[string]any)
	if p1["omid"] != "749" {
		t.Errorf("omid = %v, want 749", p1["omid"])
	}
}
