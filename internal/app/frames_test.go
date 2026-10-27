package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"overwatch/agent/internal/config"
)

var framesT0 = time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)

func countWith(lines []string, part string) int {
	n := 0
	for _, l := range lines {
		if strings.Contains(l, part) {
			n++
		}
	}
	return n
}

// The first discard is explained once; after that the log sums up at most
// once an hour, covering the hour since the last line, and never explains
// again.
func TestFramesExplainOnceThenSummariseHourly(t *testing.T) {
	var tl frameTally
	tl.connected()

	tl.sample(frameCounts{Stale: 5})
	if got := tl.lines(framesT0, time.Hour); len(got) != 1 || got[0] != frameExplanation {
		t.Fatalf("first discard logged %q, want only the explanation", got)
	}

	tl.sample(frameCounts{Stale: 20})
	if got := tl.lines(framesT0.Add(59*time.Minute), time.Hour); len(got) != 0 {
		t.Fatalf("logged %q inside the hour", got)
	}
	got := tl.lines(framesT0.Add(time.Hour), time.Hour)
	if len(got) != 1 {
		t.Fatalf("at the hour logged %q, want one summary", got)
	}
	if !strings.Contains(got[0], "in the last hour discarded 0 answering another command and 15 that arrived between requests; dropped 0 with the queue full") ||
		!strings.Contains(got[0], "since start: 0 answering another command, 20 between requests, 0 dropped") {
		t.Fatalf("summary = %q, want the 15 since the explanation and 20 since start", got[0])
	}

	// More of every kind: a summary, never a second explanation.
	tl.sample(frameCounts{Foreign: 2, Stale: 30, Overflow: 1})
	got = tl.lines(framesT0.Add(3*time.Hour), time.Hour)
	if len(got) != 1 || got[0] == frameExplanation || countWith(got, "in the last 2h0m discarded 2 answering another command and 10 that arrived between requests; dropped 1") != 1 {
		t.Fatalf("logged %q, want one summary of the new discards", got)
	}
	if got := tl.lines(framesT0.Add(3*time.Hour+time.Minute), time.Hour); len(got) != 0 {
		t.Fatalf("logged %q right after a summary", got)
	}
}

// With nothing new, nothing is logged, however long it has been.
func TestFramesQuietWithoutGrowth(t *testing.T) {
	var tl frameTally
	if got := tl.lines(framesT0, time.Hour); len(got) != 0 {
		t.Fatalf("an empty tally logged %q", got)
	}
	tl.sample(frameCounts{Foreign: 4})
	if got := tl.lines(framesT0, time.Hour); len(got) != 1 {
		t.Fatalf("logged %q, want the explanation", got)
	}
	for _, after := range []time.Duration{time.Hour, 2 * time.Hour, 5 * time.Hour} {
		tl.sample(frameCounts{Foreign: 4}) // the same counters again
		if got := tl.lines(framesT0.Add(after), time.Hour); len(got) != 0 {
			t.Fatalf("after %s with no new discards logged %q", after, got)
		}
	}
	if r := tl.report(); r["foreign"] != int64(4) {
		t.Fatalf("report = %v, want 4 foreign", r)
	}
}

// Each connection counts from zero; the tally keeps the total across them. A
// counter that goes backwards without a reconnect being announced is taken for
// a new connection's, never as a negative or a repeat.
func TestFramesSurviveAReconnect(t *testing.T) {
	var tl frameTally
	tl.connected()
	tl.sample(frameCounts{Stale: 10})
	tl.connected()
	tl.sample(frameCounts{Stale: 3})
	if r := tl.report(); r["stale"] != int64(13) {
		t.Fatalf("stale = %v across a reconnect, want 13", r["stale"])
	}

	tl.sample(frameCounts{Stale: 1}) // went backwards: a new connection's 1
	tl.sample(frameCounts{Stale: 1}) // the same reading again: nothing new
	tl.sample(frameCounts{Stale: 4})
	r := tl.report()
	if r["stale"] != int64(17) {
		t.Fatalf("stale = %v, want 17 (13 + 1 + 3)", r["stale"])
	}
	for k, v := range r {
		if v.(int64) < 0 {
			t.Fatalf("%s = %d, negative", k, v)
		}
	}
}

// Only polls left unsettled by a frame lost mid-poll warn, at most every five
// minutes. Frames dropped with the queue full are summarised, never warned
// about on their own.
func TestFramesUnsettledWarningIsRateLimited(t *testing.T) {
	var tl frameTally
	tl.sample(frameCounts{Overflow: 100})
	got := tl.lines(framesT0, time.Hour)
	if countWith(got, "WARNING") != 0 || countWith(got, "dropped 100 with the queue full") != 1 {
		t.Fatalf("queue-full drops alone logged %q, want a summary and no warning", got)
	}
	tl.sample(frameCounts{Overflow: 500})
	if got := tl.lines(framesT0.Add(10*time.Minute), time.Hour); countWith(got, "WARNING") != 0 {
		t.Fatalf("more queue-full drops warned: %q", got)
	}

	tl.lostPoll()
	got = tl.lines(framesT0.Add(11*time.Minute), time.Hour)
	if countWith(got, "WARNING: 1 poll(s) lost a message unread mid-poll (1 since start)") != 1 {
		t.Fatalf("a poll left unsettled by a lost frame logged %q, want one warning", got)
	}
	tl.lostPoll()
	tl.lostPoll()
	for _, after := range []time.Duration{12 * time.Minute, 15*time.Minute + 59*time.Second} {
		if got := tl.lines(framesT0.Add(after), time.Hour); countWith(got, "WARNING") != 0 {
			t.Fatalf("warned again %s in: %q", after-11*time.Minute, got)
		}
	}
	got = tl.lines(framesT0.Add(16*time.Minute), time.Hour)
	if countWith(got, "WARNING: 2 poll(s) lost a message unread mid-poll (3 since start)") != 1 {
		t.Fatalf("five minutes on logged %q, want a warning for the 2 new polls", got)
	}
	if got := tl.lines(framesT0.Add(30*time.Minute), time.Hour); countWith(got, "WARNING") != 0 {
		t.Fatalf("warned with no new lost polls: %q", got)
	}
	if r := tl.report(); r["unsettled_lost"] != int64(3) {
		t.Fatalf("unsettled_lost = %v, want 3", r["unsettled_lost"])
	}
}

// report may be read from another goroutine (the admin overview) while the
// poll loop samples and logs. Run with -race.
func TestFramesReportIsSafeWhileSampling(t *testing.T) {
	var tl frameTally
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := int64(1); i <= 2000; i++ {
			if i%500 == 0 {
				tl.connected()
			}
			tl.sample(frameCounts{Foreign: i % 500, Stale: i % 500})
			tl.lostPoll()
			_ = tl.lines(framesT0.Add(time.Duration(i)*time.Minute), time.Hour)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 2000; i++ {
			r := tl.report()
			if r["foreign"].(int64) < 0 || r["unsettled_lost"].(int64) < 0 {
				t.Errorf("report = %v", r)
				return
			}
		}
	}()
	wg.Wait()
	if r := tl.report(); r["unsettled_lost"] != int64(2000) {
		t.Fatalf("unsettled_lost = %v, want 2000", r["unsettled_lost"])
	}
}

// The slow poll's self-report carries the counts (agent.frames), the same as
// the admin overview. Here another client's reply follows every one of ours,
// and the next request finds it waiting.
func TestSelfReportAndOverviewCarryTheFrameCounts(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		var reply map[string]any
		switch cmd {
		case "GETSERVERSTATE":
			reply = map[string]any{"CMD": cmd, "SERVERMODE": 2}
		case "GETACTIVEPACKS":
			reply = packsReply()
		default:
			reply = map[string]any{"CMD": cmd}
		}
		return []map[string]any{reply, {"CMD": "GETOTHER", "LEFTOVER": n}}, false
	})
	a := New(config.Config{Version: "test"})

	var frames map[string]any
	deadline := time.Now().Add(3 * time.Second)
	for {
		data, _, err := a.collect(client, true)
		if err != nil {
			t.Fatal(err)
		}
		var payload map[string]any
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		agent, _ := payload["agent"].(map[string]any)
		frames, _ = agent["frames"].(map[string]any)
		if frames == nil {
			t.Fatalf("self-report %v carries no frames", agent)
		}
		if frames["stale"].(float64) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("frames = %v: no leftover ever counted as arriving between requests", frames)
		}
		time.Sleep(5 * time.Millisecond) // the leftover arrives before the next poll
	}

	ov, ok := a.Overview()["frames"].(map[string]any)
	if !ok {
		t.Fatalf("overview has no frames: %v", a.Overview())
	}
	for _, k := range []string{"foreign", "stale", "overflow", "unsettled_lost"} {
		got, ok := ov[k].(int64)
		if !ok || float64(got) != frames[k] {
			t.Fatalf("overview frames[%s] = %v, self-report %v", k, ov[k], frames[k])
		}
	}
}

// A frame lost unread during a poll leaves it unsettled, exactly as before,
// and counts as such; losses between polls do not.
func TestAFrameLostMidPollIsCountedAndTheGateStaysShut(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		}
		var frames []map[string]any
		for i := 0; i < 40; i++ { // more than the client keeps
			frames = append(frames, map[string]any{"CMD": "GETTEAMINFO", "I": i})
		}
		return append(frames, packsReply()), false
	})

	a := &App{}
	a.noteServerMode(2)
	a.gameState.Store(stateIdle)
	if err := a.printServerGate(); err != nil {
		t.Fatalf("gate = %v before the poll, want open", err)
	}
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if !a.pollUnsettled.Load() {
		t.Fatal("a poll that lost a frame settled")
	}
	if err := a.printServerGate(); !errors.Is(err, errPollUnsettled) {
		t.Fatalf("gate = %v after a poll that lost a frame, want errPollUnsettled", err)
	}
	r := a.frames.report()
	if r["unsettled_lost"] != int64(1) || r["foreign"] != int64(40) {
		t.Fatalf("frames = %v, want unsettled_lost 1 and the 40 foreign", r)
	}
	if got := a.frames.lines(time.Now(), time.Hour); countWith(got, "WARNING") != 1 {
		t.Fatalf("logged %q, want a warning", got)
	}
}

func TestFramesLostBetweenPollsDoNotWarn(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		switch cmd {
		case "GETSERVERSTATE":
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		case "GETACTIVEPACKS":
			return []map[string]any{packsReply()}, false
		}
		frames := make([]map[string]any, 40)
		for i := range frames {
			frames[i] = map[string]any{"CMD": "GETGAMELIST", "I": i}
		}
		return append(frames, map[string]any{"CMD": cmd}), false
	})

	a := &App{}
	if _, err := client.Command("GETTEAMINFO"); err != nil { // loses frames between polls
		t.Fatal(err)
	}
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	if a.pollUnsettled.Load() {
		t.Fatal("frames lost before the poll left it unsettled")
	}
	if r := a.frames.report(); r["unsettled_lost"] != int64(0) {
		t.Fatalf("frames = %v, want unsettled_lost 0", r)
	}
	if got := a.frames.lines(time.Now(), time.Hour); countWith(got, "WARNING") != 0 {
		t.Fatalf("losses between polls warned: %q", got)
	}
}

// pollConfig polls as fast as it can, every poll a slow one.
func pollConfig() config.Config {
	return config.Config{Version: "test", PollInterval: time.Millisecond, IdlePollInterval: time.Millisecond}
}

// foreignThenReply serves a fake O-Zone that sends another client's reply
// ahead of each server state, and hangs up at request limit.
func foreignThenReply(limit int) func(cmd string, n int) ([]map[string]any, bool) {
	return foreignNThenReply(1, limit)
}

// foreignNThenReply sends nForeign replies to other clients ahead of each
// server state.
func foreignNThenReply(nForeign, limit int) func(cmd string, n int) ([]map[string]any, bool) {
	return func(cmd string, n int) ([]map[string]any, bool) {
		if n >= limit {
			return nil, true
		}
		switch cmd {
		case "GETSERVERSTATE":
			var out []map[string]any
			for i := 0; i < nForeign; i++ {
				out = append(out, map[string]any{"CMD": "GETOTHER"})
			}
			return append(out, map[string]any{"CMD": cmd, "SERVERMODE": 2}), false
		case "GETACTIVEPACKS":
			return []map[string]any{packsReply()}, false
		}
		return []map[string]any{{"CMD": cmd}}, false
	}
}

// A new connection's counters start from zero even when its first reading is
// higher than the old connection's last one: pollLoop must call connected().
func TestAReconnectCountsTheNewConnectionFromZero(t *testing.T) {
	a := New(pollConfig())
	a.pollLoop(context.Background(), scriptedOZone(t, foreignNThenReply(1, 5))) // one slow poll, 1 discarded
	a.pollLoop(context.Background(), scriptedOZone(t, foreignNThenReply(3, 5))) // one slow poll, 3 discarded
	if r := a.frames.report(); r["foreign"] != int64(4) {
		t.Fatalf("frames = %v, want 4 foreign over the two connections", r)
	}
}

// A reconnect neither restarts the summary window nor forgets what the next
// summary covers.
func TestFramesSummaryWindowSpansAReconnect(t *testing.T) {
	var tl frameTally
	tl.connected()
	tl.sample(frameCounts{Stale: 5})
	if got := tl.lines(framesT0, time.Hour); len(got) != 1 || got[0] != frameExplanation {
		t.Fatalf("first discard logged %q, want only the explanation", got)
	}
	tl.sample(frameCounts{Stale: 7}) // 2 more on the first connection
	tl.connected()
	tl.sample(frameCounts{Stale: 4})
	if got := tl.lines(framesT0.Add(time.Minute), time.Hour); len(got) != 0 {
		t.Fatalf("logged %q just after a reconnect", got)
	}
	if got := tl.lines(framesT0.Add(59*time.Minute), time.Hour); len(got) != 0 {
		t.Fatalf("logged %q inside the hour", got)
	}
	got := tl.lines(framesT0.Add(time.Hour), time.Hour)
	if len(got) != 1 || !strings.Contains(got[0], "in the last hour discarded 0 answering another command and 6 that arrived between requests") ||
		!strings.Contains(got[0], "since start: 0 answering another command, 11 between requests") {
		t.Fatalf("at the hour logged %q, want one summary of the 6 since the explanation, 11 since start", got)
	}
}

// A summary after a long gap without slow polls (the game server off
// overnight) says how long its window really is.
func TestFramesSummaryNamesALongWindow(t *testing.T) {
	var tl frameTally
	tl.connected()
	tl.sample(frameCounts{Stale: 5})
	_ = tl.lines(framesT0, time.Hour) // the explanation opens the window
	tl.sample(frameCounts{Stale: 9})
	tl.connected() // the game server goes away; it is back ten and a half hours later
	got := tl.lines(framesT0.Add(10*time.Hour+30*time.Minute), time.Hour)
	if len(got) != 1 || !strings.Contains(got[0], "in the last 10h30m discarded 0 answering another command and 4 that arrived between requests") {
		t.Fatalf("logged %q, want a summary naming its 10h30m window", got)
	}
}

// Across reconnects the counts add up, the explanation is logged once, and
// there is no summary inside the hour. The 1.6.5 to 1.7.1 line came on every
// slow poll and started over on every connection.
func TestTheDiscardLogSurvivesAReconnect(t *testing.T) {
	logs := captureLog(t)
	a := New(pollConfig())

	// Five commands a slow poll: three polls, then the fourth's first request
	// is hung up on. Each poll discards one frame answering another command.
	for run := 0; run < 3; run++ {
		a.pollLoop(context.Background(), scriptedOZone(t, foreignThenReply(15)))
	}

	if r := a.frames.report(); r["foreign"] != int64(9) {
		t.Fatalf("frames = %v, want 9 foreign over the three connections", r)
	}
	out := logs.String()
	if n := strings.Count(out, frameExplanation); n != 1 {
		t.Fatalf("explanation logged %d times, want once:\n%s", n, out)
	}
	if n := strings.Count(out, "] O-Zone: in "); n != 0 {
		t.Fatalf("%d summaries inside the hour after the explanation, want none:\n%s", n, out)
	}
	if strings.Contains(out, "frame(s) that answered another command") {
		t.Fatalf("the per-poll line is still logged:\n%s", out)
	}
}

// What a poll counted before it failed is kept: the connection's last sample
// is taken as the poll loop ends.
func TestFramesCountedInAFailedPollAreKept(t *testing.T) {
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": "GETOTHER"}, {"CMD": "GETOTHER"}, {"CMD": "GETOTHER"}, {"CMD": cmd, "SERVERMODE": 2}}, false
		}
		return nil, true // GETACTIVEPACKS: the poll fails
	})
	a := New(pollConfig())
	a.pollLoop(context.Background(), client)
	if r := a.frames.report(); r["foreign"] != int64(3) {
		t.Fatalf("frames = %v, want the 3 foreign counted before the poll failed", r)
	}
}

// The admin overview reads the counts while the poll loop updates them. Run
// with -race.
func TestOverviewIsSafeWhilePolling(t *testing.T) {
	a := New(pollConfig())
	client := scriptedOZone(t, foreignThenReply(1<<30))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.pollLoop(ctx, client)
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		f := a.Overview()["frames"].(map[string]any)
		if f["foreign"].(int64) >= 5 {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("frames = %v after 3s of polling", f)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
}
