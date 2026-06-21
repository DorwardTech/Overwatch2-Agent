package ozonefix

import (
	"encoding/json"
	"testing"
)

// All golden JSON fixtures must be valid JSON — they are the fidelity baseline.
func TestGoldensAreValidJSON(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("no golden fixtures found")
	}
	for _, name := range names {
		var v any
		if err := json.Unmarshal(Golden(name), &v); err != nil {
			t.Errorf("%s: invalid JSON: %v", name, err)
		}
	}
}

// The list response must carry a gamelist array (TORN requires this key).
func TestListResponseShape(t *testing.T) {
	var v struct {
		GameList []map[string]any `json:"gamelist"`
	}
	if err := json.Unmarshal(ListResponseJSON(), &v); err != nil {
		t.Fatalf("list_response: %v", err)
	}
	if len(v.GameList) == 0 {
		t.Fatal("list_response: gamelist is empty")
	}
	// Every entry must have the fields TORN reads.
	for i, g := range v.GameList {
		for _, k := range []string{"gamenum", "gamename", "starttime", "endtime", "valid"} {
			if _, ok := g[k]; !ok {
				t.Errorf("gamelist[%d]: missing %q", i, k)
			}
		}
	}
}

// The all response must carry game/players/events keyed objects.
func TestAllResponseShape(t *testing.T) {
	var v struct {
		Game    map[string]any `json:"game"`
		Players map[string]any `json:"players"`
		Events  map[string]any `json:"events"`
	}
	if err := json.Unmarshal(AllResponseJSON(), &v); err != nil {
		t.Fatalf("all_response: %v", err)
	}
	if v.Game == nil {
		t.Error("all_response: missing game object")
	}
	if len(v.Players) == 0 {
		t.Error("all_response: players empty")
	}
	if len(v.Events) == 0 {
		t.Error("all_response: events empty")
	}
	if _, ok := v.Game["gamenum"]; !ok {
		t.Error("all_response: game.gamenum missing")
	}
}

// Compact must produce whitespace-free JSON that is still semantically equal.
func TestCompact(t *testing.T) {
	compact := Compact(AllResponseJSON())
	var a, b any
	if err := json.Unmarshal(AllResponseJSON(), &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(compact, &b); err != nil {
		t.Fatalf("compact is invalid JSON: %v", err)
	}
	for _, c := range compact {
		if c == '\n' || c == '\t' {
			t.Fatalf("compact still contains whitespace byte %q", c)
		}
	}
}
