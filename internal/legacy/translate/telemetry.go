// Package translate maps legacy Nexus data into the exact payload shapes the
// Overwatch central ingest + game-results endpoints already accept, so every
// downstream feature (dashboards, alerts, analytics) works unchanged. All
// functions here are pure — no I/O — so the mapping is unit-tested against
// fixtures derived from real Nexus data.
package translate

import (
	"encoding/json"
	"strconv"
	"time"

	"overwatch/agent/internal/legacy/lasertag"
)

// TelemetryBytes marshals a TelemetryPayload and returns it with the
// idempotency key (the push sequence) central dedupes on.
func TelemetryBytes(
	seq int64,
	now time.Time,
	packs []lasertag.PackLive,
	status lasertag.ServerStatus,
	aliases map[int]string,
) ([]byte, string) {
	data, _ := json.Marshal(TelemetryPayload(seq, now, packs, status, aliases))
	return data, strconv.FormatInt(seq, 10)
}

// O-Zone SERVERMODE values the frontend understands. Legacy sites only need the
// idle-vs-active distinction; we map the Nexus server mode onto these so the
// existing live UI renders correctly.
const (
	ozoneModeIdle       = 1
	ozoneModeInProgress = 6
	ozoneModeFinished   = 7
)

// TelemetryPayload builds the /api/agent/ingest body from a live pack snapshot
// and server status. Battery/wifi/temperature are omitted (null in central) —
// the Nexus system has no such feed — so the live UI shows them as unavailable
// rather than as misleading zeros. aliases maps pack id → roster alias (from
// the Nexus DB), used because the live COM snapshot has no alias.
func TelemetryPayload(
	seq int64,
	now time.Time,
	packs []lasertag.PackLive,
	status lasertag.ServerStatus,
	aliases map[int]string,
) map[string]any {
	packOut := make([]map[string]any, 0, len(packs))
	for _, p := range packs {
		pack := map[string]any{
			"ID":        p.PackID,
			"ALIAS":     aliases[p.PackID],
			"TEAM_ID":   p.TeamID,
			"STATE":     p.State,
			"CONNECTED": p.Online,
			"PLAYING":   p.Playing,
			"ACTIVE":    p.Allocated,
			"BLOCKED":   boolToInt(p.Blocked),
		}
		// Scraped counters are genuine telemetry; include when present.
		if p.Scraped.Resets != nil {
			pack["RESETS"] = *p.Scraped.Resets
		}
		if p.Scraped.Timeouts != nil {
			pack["TIME_OUTS"] = *p.Scraped.Timeouts
		}
		if p.Scraped.Charge != nil {
			pack["BATT_CHARGE"] = *p.Scraped.Charge
		}
		// Deliberately no BATT_MV / WIFI_SIGNAL / SYS_TEMP → central stores null.
		packOut = append(packOut, pack)
	}

	return map[string]any{
		"push_seq":  seq,
		"client_ts": now.UTC().Format(time.RFC3339),
		"server_state": map[string]any{
			"SERVERMODE":   ServerMode(status.ServerMode, status.GameID),
			"GAMENUM":      status.GameID,
			"GAMETIME":     status.Elapsed,
			"GAMEDURATION": status.Duration,
			"DESCRIPTION":  status.GameName,
			"TELEONLINE":   true,
		},
		"packs": packOut,
		"agent": map[string]any{
			"mode":         "legacy",
			"packs_online": status.PacksOnline,
		},
	}
}

// ServerMode maps a Nexus server mode to the O-Zone SERVERMODE the frontend
// expects. Nexus modes: 3 = game start/running, 5 = abort, 7 = finish,
// 12 = reload; anything else (or no current game) is idle.
func ServerMode(nexusMode, gameID int) int {
	switch nexusMode {
	case 3:
		return ozoneModeInProgress
	case 7:
		return ozoneModeFinished
	default:
		if gameID > 0 && nexusMode != 0 {
			return ozoneModeInProgress
		}
		return ozoneModeIdle
	}
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
