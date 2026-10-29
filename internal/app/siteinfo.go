package app

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// siteInfoEvery is how often the agent asks the game server what system it
// is (GETSITEINFO). The answer belongs to the installation and does not
// change between games, so once an hour is plenty; a new connection asks at
// its first chance, since the address may now be another game server.
const siteInfoEvery = time.Hour

// siteInfo is what the agent keeps of O-Zone's GETSITEINFO reply: only its
// system type, SYSFLAG. The rest of the reply describes the installation and
// is not forwarded. Only the poll loop's goroutine touches it.
type siteInfo struct {
	sysflag int
	known   bool
	next    time.Time // when to ask again; the zero time asks at once
}

// due reports whether it is time to ask.
func (s *siteInfo) due(now time.Time) bool { return !now.Before(s.next) }

// note takes one GETSITEINFO call's outcome (resp is nil when the call
// failed) and puts the next ask an hour away, answered or not: a core that
// does not know the command costs one reply timeout an hour, never one a
// poll. A reply without a usable SYSFLAG leaves the last reading standing.
func (s *siteInfo) note(resp map[string]any, now time.Time) {
	s.next = now.Add(siteInfoEvery)
	if v, ok := parseSysflag(resp["SYSFLAG"]); ok {
		s.sysflag, s.known = v, true
	}
}

// report is the push's site_info section, or nil until O-Zone has answered.
func (s *siteInfo) report() map[string]any {
	if !s.known {
		return nil
	}
	return map[string]any{"sysflag": s.sysflag}
}

// parseSysflag reads SYSFLAG as O-Zone may send it: a JSON number, or text
// in decimal or 0x hex. The field is one byte of system-type flags, so a
// value that is not a whole number from 0 to 255 is no reading.
func parseSysflag(v any) (int, bool) {
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case string:
		t := strings.TrimSpace(x)
		base := 10
		if len(t) > 2 && (t[:2] == "0x" || t[:2] == "0X") {
			t, base = t[2:], 16
		}
		i, err := strconv.ParseUint(t, base, 8)
		if err != nil {
			return 0, false
		}
		return int(i), true
	default:
		return 0, false
	}
	if n != math.Trunc(n) || n < 0 || n > 255 {
		return 0, false
	}
	return int(n), true
}
