package translate

// Nexus event-log → O-Zone hit-zone reconstruction.
//
// The Nexus per-player event log (ng_player_event_log) records one row per
// in-game event, typed by NG_Event_Types. Overwatch's game-results pipeline
// (GameResultAnalyzer) expects an O-Zone "all"-shaped player entry. We build
// that shape from aggregated event counts so central stores identical
// PackGameStat rows for legacy and O-Zone games.
//
// Event-type → zone (from NG_Event_Types, verbatim order
// "Weapon/Front/L-F/R-F/L-B/R-B/Back"):
//   dealt (tagged a foe):   0..6
//   received (tagged by foe): 14..20
//   35 = trigger pull (a shot fired)
//
// Fitness (steps/distance/calories) has no Nexus source and is left at zero —
// those columns are gated to an upgrade notice on legacy sites.

// zoneMap turns a 7-element zone-count array into the O-Zone detailed entry
// keyed "0".."6" that GameResultAnalyzer::sumDetailedZones reads.
func zoneMap(zones [7]int) map[string]int {
	m := make(map[string]int, 7)
	for i, v := range zones {
		m[itoa(i)] = v
	}
	return m
}

// PlayerAgg is the aggregated event counts for one pack in one game, as read
// from the Nexus DB (package nexus produces these).
type PlayerAgg struct {
	PackID   int
	PackName string
	TeamID   int
	MemberID int // Nexus Member_ID; -1 (or 0) means no member logged in
	Shots    int // count of trigger events (type 35), or ng_player_stats.Shots_Fired

	// DealtByZone[i] / ReceivedByZone[i] correspond to physical zone i (0..6).
	DealtByZone    [7]int
	ReceivedByZone [7]int
}

// GameMeta describes one finished Nexus game (from ng_game_stats + ng_profiles).
type GameMeta struct {
	GameNumber int
	GameName   string // profile description
	GameType   int    // 0 = solo, 1 = team
	Duration   int    // seconds
	Date       string // "YYYY-MM-DD HH:MM:SS"
}

// GameResultsPayload builds the /api/agent/game-results body for one game. The
// player entries carry the summed hit-zone fields O-Zone sends, so
// GameResultAnalyzer stores them unchanged. Members map to omid so the
// leaderboard's logged-in-only filter works identically to O-Zone.
func GameResultsPayload(meta GameMeta, players []PlayerAgg) map[string]any {
	playerMap := make(map[string]any, len(players))
	for _, p := range players {
		entry := map[string]any{
			"alias":  p.PackName,
			"omid":   memberOmid(p.MemberID),
			"colour": p.TeamID,
			"shots":  p.Shots,
			"acc":    accuracy(dealtTotal(p), p.Shots),
			// Fitness is unavailable on Nexus.
			"steps":    0,
			"distance": 0,
			"calories": 0,
			// O-Zone detailed hit-zone format: an array of per-attacker/target
			// zone maps (keys "0".."6") that GameResultAnalyzer::sumDetailedZones
			// sums. We collapse each side to a single pre-summed entry.
			//   tagsonl = tags landed ON this pack (received / hits)
			//   tagsbyl = tags dealt BY this pack (dealt)
			"tagsonl": []map[string]int{zoneMap(p.ReceivedByZone)},
			"tagsbyl": []map[string]int{zoneMap(p.DealtByZone)},
		}
		playerMap[itoa(p.PackID)] = entry
	}

	return map[string]any{
		"game_number": meta.GameNumber,
		"data": map[string]any{
			"game": map[string]any{
				"gamename": meta.GameName,
				"gametype": meta.GameType,
				"gamedur":  meta.Duration,
				"date":     meta.Date,
			},
			"players": playerMap,
		},
	}
}

func dealtTotal(p PlayerAgg) int {
	t := 0
	for _, v := range p.DealtByZone {
		t += v
	}
	return t
}

// accuracy is hits-dealt over shots, as a 0–100 percentage.
func accuracy(dealt, shots int) int {
	if shots <= 0 {
		return 0
	}
	return int(float64(dealt) / float64(shots) * 100.0)
}

// memberOmid returns the O-Zone member id string, or "-1" (central's
// not-logged-in sentinel) when no Nexus member was on the pack.
func memberOmid(memberID int) string {
	if memberID <= 0 {
		return "-1"
	}
	return itoa(memberID)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
