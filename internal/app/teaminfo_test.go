package app

import (
	"reflect"
	"testing"
)

// O-Zone sends GETTEAMINFO's list as TEAMDETAILS; until 1.9.1 the agent read
// TEAMS, found nothing, and central never had a team list.
func TestTeamListReadsTeamDetails(t *testing.T) {
	teams := []any{map[string]any{"name": "Red", "colour_index": float64(3), "colour_rgb": "#F0426A"}}

	for _, tc := range []struct {
		name  string
		reply map[string]any
		want  any
	}{
		{"teamdetails", map[string]any{"CMD": "GETTEAMINFO", "STATUS": "OK", "TEAMDETAILS": teams}, teams},
		{"teams, as the simulator sent it", map[string]any{"CMD": "GETTEAMINFO", "TEAMS": teams}, teams},
		{"teamdetails wins", map[string]any{"TEAMDETAILS": teams, "TEAMS": []any{}}, teams},
		{"neither", map[string]any{"CMD": "GETTEAMINFO", "STATUS": "OK"}, nil},
	} {
		if got := teamList(tc.reply); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: teamList = %#v, want %#v", tc.name, got, tc.want)
		}
	}
}
