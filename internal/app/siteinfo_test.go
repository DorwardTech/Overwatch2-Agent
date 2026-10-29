package app

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"overwatch/agent/internal/buffer"
)

func TestParseSysflag(t *testing.T) {
	for _, tc := range []struct {
		in   any
		want int
		ok   bool
	}{
		{float64(0), 0, true},
		{float64(0x80), 128, true},
		{float64(16), 16, true},
		{"64", 64, true},
		{" 0x10 ", 16, true},
		{"0X80", 128, true},
		{float64(256), 0, false},
		{float64(-1), 0, false},
		{float64(1.5), 0, false},
		{"0x100", 0, false},
		{"Helios", 0, false},
		{"", 0, false},
		{nil, 0, false},
		{true, 0, false},
		{map[string]any{}, 0, false},
	} {
		got, ok := parseSysflag(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("parseSysflag(%#v) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// siteInfoOZone is a fake O-Zone in the given mode that answers GETSITEINFO
// with reply (nil: a core that does not know the command, answering with an
// error echo) and records every command asked.
func siteInfoOZone(t *testing.T, mode int, reply map[string]any) (*App, func() []string, func(slow bool) map[string]any) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		mu.Lock()
		asked = append(asked, cmd)
		mu.Unlock()
		switch cmd {
		case "GETSERVERSTATE":
			return []map[string]any{{"CMD": cmd, "SERVERMODE": mode}}, false
		case "GETACTIVEPACKS":
			return []map[string]any{packsReply()}, false
		case "GETSITEINFO":
			if reply == nil {
				return []map[string]any{{"CMD": cmd, "STATUS": "UNKNOWN_COMMAND"}}, false
			}
			return []map[string]any{reply}, false
		}
		return []map[string]any{{"CMD": cmd, "STATUS": "OK"}}, false
	})
	a := &App{buf: buffer.New(1)}
	a.gameState.Store(stateIdle)
	count := func() []string {
		mu.Lock()
		defer mu.Unlock()
		var got []string
		for _, c := range asked {
			if c == "GETSITEINFO" {
				got = append(got, c)
			}
		}
		return got
	}
	collect := func(slow bool) map[string]any {
		data, _, err := a.collect(client, slow)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}
	return a, count, collect
}

// Between games, a slow poll asks once and every slow push then carries the
// reading; the next ask waits an hour.
func TestSlowPollReportsTheSystemType(t *testing.T) {
	_, asked, collect := siteInfoOZone(t, 2, map[string]any{"CMD": "GETSITEINFO", "STATUS": "OK", "SYSFLAG": float64(0x80), "SITENAME": "Somewhere"})

	if p := collect(false); p["site_info"] != nil {
		t.Fatalf("a fast poll carried site_info %v", p["site_info"])
	}
	if n := len(asked()); n != 0 {
		t.Fatalf("a fast poll asked GETSITEINFO %d times", n)
	}

	p := collect(true)
	info, _ := p["site_info"].(map[string]any)
	if info == nil || info["sysflag"] != float64(128) || len(info) != 1 {
		t.Fatalf("site_info = %v, want only sysflag 128", p["site_info"])
	}

	p = collect(true)
	if info, _ := p["site_info"].(map[string]any); info == nil || info["sysflag"] != float64(128) {
		t.Fatalf("the next slow push dropped the reading: %v", p["site_info"])
	}
	if n := len(asked()); n != 1 {
		t.Fatalf("asked GETSITEINFO %d times in two slow polls, want once an hour", n)
	}
}

// While a game is on, the agent never asks: an unknown command's reply
// timeout would hold up the poll.
func TestSystemTypeIsNotAskedDuringAGame(t *testing.T) {
	a, asked, collect := siteInfoOZone(t, 6, map[string]any{"CMD": "GETSITEINFO", "SYSFLAG": float64(1)})
	a.gameState.Store(stateActive)

	if p := collect(true); p["site_info"] != nil {
		t.Fatalf("site_info = %v during a game", p["site_info"])
	}
	if n := len(asked()); n != 0 {
		t.Fatalf("asked GETSITEINFO %d times during a game", n)
	}
	if !a.site.due(time.Now()) {
		t.Fatal("a skipped ask was put off as if it had been made")
	}
}

// A core that does not know the command is asked again in an hour, not on
// every slow poll, and nothing is reported.
func TestUnknownSiteInfoWaitsAnHour(t *testing.T) {
	a, asked, collect := siteInfoOZone(t, 1, nil)

	for range 3 {
		if p := collect(true); p["site_info"] != nil {
			t.Fatalf("site_info = %v without a SYSFLAG", p["site_info"])
		}
	}
	if n := len(asked()); n != 1 {
		t.Fatalf("asked GETSITEINFO %d times, want once", n)
	}
	if a.site.due(time.Now().Add(siteInfoEvery - time.Minute)) {
		t.Fatal("asked again within the hour")
	}
	if !a.site.due(time.Now().Add(siteInfoEvery + time.Second)) {
		t.Fatal("not asked again after the hour")
	}
}

// SYSFLAG 0 (no system) is a reading, and a later reply without the field
// keeps the last one.
func TestSiteInfoKeepsTheLastReading(t *testing.T) {
	var s siteInfo
	now := time.Now()
	s.note(map[string]any{"SYSFLAG": float64(0)}, now)
	if r := s.report(); r == nil || r["sysflag"] != 0 {
		t.Fatalf("report = %v, want sysflag 0", r)
	}
	s.note(nil, now.Add(siteInfoEvery))
	if r := s.report(); r == nil || r["sysflag"] != 0 {
		t.Fatalf("a failed call dropped the reading: %v", r)
	}
}
