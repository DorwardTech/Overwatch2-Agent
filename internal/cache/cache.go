// Package cache presents the local store as O-Zone Print Server responses. It is
// the read side the proxy serves from.
//
// Fidelity rules (see docs/OZONE_PRINT_SERVER_API.md):
//   - "all" is returned BYTE-VERBATIM — exactly the bytes O-Zone sent. This is
//     the response TORN consumes, and only verbatim replay satisfies every
//     consumer's differing field subset.
//   - "list" is rebuilt from per-game metadata into the exact O-Zone shape (it is
//     simple, well-specified, and must reflect what the cache can actually serve).
//   - "minimal" / "team" / "player" are derived views over the verbatim payload;
//     they are not byte-verbatim and are not consumed by TORN.
package cache

import (
	"encoding/json"

	"overwatch/agent/internal/store"
)

// Cache adapts a store into O-Zone-shaped responses.
type Cache struct {
	store *store.Store
}

// New wraps a store.
func New(s *store.Store) *Cache { return &Cache{store: s} }

// listEntry mirrors the O-Zone game-list element exactly (PrintServerAPI.MD).
type listEntry struct {
	GameNum     int    `json:"gamenum"`
	GameName    string `json:"gamename"`
	Duration    int    `json:"duration"`
	StartTime   string `json:"starttime"`
	EndTime     string `json:"endtime"`
	PlayerCount int    `json:"playercount"`
	Valid       int    `json:"valid"`
}

// BuildListResponse renders {"gamelist":[...]} from every known game's metadata.
// A missing end time is rendered as O-Zone's literal "None".
func (c *Cache) BuildListResponse() []byte {
	metas := c.store.AllMeta()
	list := make([]listEntry, 0, len(metas))
	for _, m := range metas {
		end := m.EndTime
		if end == "" {
			end = "None"
		}
		list = append(list, listEntry{
			GameNum:     m.GameNumber,
			GameName:    m.GameName,
			Duration:    m.Duration,
			StartTime:   m.StartTime,
			EndTime:     end,
			PlayerCount: m.PlayerCount,
			Valid:       m.Valid,
		})
	}
	out, err := json.Marshal(map[string]any{"gamelist": list})
	if err != nil {
		return []byte(`{"gamelist":[]}`)
	}
	return out
}

// GameRaw returns the verbatim "all" payload for a game.
func (c *Cache) GameRaw(gameNumber int) ([]byte, bool) {
	raw, ok, err := c.store.GameRaw(gameNumber)
	if err != nil || !ok {
		return nil, false
	}
	return raw, true
}

// MinimalRaw returns the "all" payload with the events section removed.
func (c *Cache) MinimalRaw(gameNumber int) ([]byte, bool) {
	raw, ok := c.GameRaw(gameNumber)
	if !ok {
		return nil, false
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(raw, &data); err != nil {
		return raw, true // fall back to verbatim rather than fail
	}
	delete(data, "events")
	out, err := json.Marshal(data)
	if err != nil {
		return raw, true
	}
	return out, true
}

// Subset returns {"game":..., "<section>":{ only the requested ids }} for a
// "team" or "player" command. Empty ids returns the whole section.
func (c *Cache) Subset(gameNumber int, section string, ids []string) ([]byte, bool) {
	raw, ok := c.GameRaw(gameNumber)
	if !ok {
		return nil, false
	}
	var full map[string]json.RawMessage
	if err := json.Unmarshal(raw, &full); err != nil {
		return nil, false
	}

	out := map[string]json.RawMessage{}
	if g, ok := full["game"]; ok {
		out["game"] = g
	}
	if sectionRaw, ok := full[section]; ok {
		var all map[string]json.RawMessage
		if err := json.Unmarshal(sectionRaw, &all); err == nil {
			picked := all
			if len(ids) > 0 {
				picked = map[string]json.RawMessage{}
				for _, id := range ids {
					if v, ok := all[id]; ok {
						picked[id] = v
					}
				}
			}
			if b, err := json.Marshal(picked); err == nil {
				out[section] = b
			}
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, false
	}
	return b, true
}
