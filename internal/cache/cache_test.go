package cache

import (
	"bytes"
	"encoding/json"
	"testing"

	"overwatch/agent/internal/ozonefix"
	"overwatch/agent/internal/store"
)

func seeded(t *testing.T) *Cache {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	raw := ozonefix.Compact(ozonefix.AllResponseJSON())
	if err := s.StoreGame(store.GameMeta{
		GameNumber: 9, GameName: "Competition Team Elimination", GameType: 1,
		StartTime: "2020-02-18 16:06:05", PlayerCount: 2, Duration: 601,
	}, raw); err != nil {
		t.Fatal(err)
	}
	_ = s.UpsertListEntry(store.GameMeta{
		GameNumber: 9, GameName: "Competition Team Elimination", Duration: 601,
		StartTime: "2020-02-18 16:06:05", EndTime: "2020-02-18 16:16:06", PlayerCount: 2, Valid: 1,
	})
	return New(s)
}

func TestGameRawIsVerbatim(t *testing.T) {
	c := seeded(t)
	want := ozonefix.Compact(ozonefix.AllResponseJSON())
	got, ok := c.GameRaw(9)
	if !ok {
		t.Fatal("game 9 not found")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("all payload not byte-verbatim")
	}
}

func TestMinimalDropsEvents(t *testing.T) {
	c := seeded(t)
	min, ok := c.MinimalRaw(9)
	if !ok {
		t.Fatal("game 9 not found")
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(min, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["events"]; ok {
		t.Error("minimal must not contain events")
	}
	for _, k := range []string{"game", "players", "teams", "scores"} {
		if _, ok := m[k]; !ok {
			t.Errorf("minimal missing %q", k)
		}
	}
}

func TestBuildListResponseShape(t *testing.T) {
	c := seeded(t)
	var v struct {
		GameList []map[string]any `json:"gamelist"`
	}
	if err := json.Unmarshal(c.BuildListResponse(), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.GameList) != 1 {
		t.Fatalf("want 1 game, got %d", len(v.GameList))
	}
	g := v.GameList[0]
	if g["gamenum"].(float64) != 9 {
		t.Errorf("gamenum = %v", g["gamenum"])
	}
	if g["endtime"] != "2020-02-18 16:16:06" {
		t.Errorf("endtime = %v", g["endtime"])
	}
	if g["valid"].(float64) != 1 {
		t.Errorf("valid = %v", g["valid"])
	}
}

func TestListRendersNoneForMissingEndTime(t *testing.T) {
	s, _ := store.Open(t.TempDir())
	_ = s.UpsertListEntry(store.GameMeta{GameNumber: 5, GameName: "Solo", Valid: 0})
	c := New(s)
	var v struct {
		GameList []map[string]any `json:"gamelist"`
	}
	_ = json.Unmarshal(c.BuildListResponse(), &v)
	if v.GameList[0]["endtime"] != "None" {
		t.Errorf("missing end time should render \"None\", got %v", v.GameList[0]["endtime"])
	}
}

func TestSubsetPicksIDs(t *testing.T) {
	c := seeded(t)
	out, ok := c.Subset(9, "players", []string{"1"})
	if !ok {
		t.Fatal("subset failed")
	}
	var v struct {
		Game    map[string]any `json:"game"`
		Players map[string]any `json:"players"`
	}
	if err := json.Unmarshal(out, &v); err != nil {
		t.Fatal(err)
	}
	if v.Game == nil {
		t.Error("subset should include game")
	}
	if len(v.Players) != 1 {
		t.Fatalf("want 1 player, got %d", len(v.Players))
	}
	if _, ok := v.Players["1"]; !ok {
		t.Error("missing requested player 1")
	}
}
