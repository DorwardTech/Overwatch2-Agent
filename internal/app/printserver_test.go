package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/msgbus"
	"overwatch/agent/internal/ozonesim"
	"overwatch/agent/internal/push"
	"overwatch/agent/internal/results"
)

// --- the tracker on its own -------------------------------------------------

var errDialTimeout = errors.New("dial tcp 192.0.2.10:12123: i/o timeout")

func newPresence(absentAfter int, minSpan time.Duration, now time.Time) *printServerPresence {
	p := &printServerPresence{}
	p.configure(absentAfter, 15*time.Minute, false, now)
	p.minSpan = minSpan
	return p
}

// Absent exactly at the Nth consecutive failure, once the failures span the
// minimum time, and not before either.
func TestPrintServerAbsentAtTheNthFailureOverTheMinimumSpan(t *testing.T) {
	t0 := time.Now()

	p := newPresence(3, 0, t0)
	for i := 1; i <= 2; i++ {
		if p.failed("ps", errDialTimeout, t0.Add(time.Duration(i)*time.Second), false) {
			t.Fatalf("absent after %d failure(s), want 3", i)
		}
	}
	if !p.failed("ps", errDialTimeout, t0.Add(3*time.Second), false) {
		t.Fatal("not absent after the 3rd failure")
	}
	if got := p.report(t0)["state"]; got != "absent" {
		t.Fatalf("state = %v, want absent", got)
	}

	// With the minimum span: three quick failures are not enough...
	p = newPresence(3, printServerAbsentSpan, t0)
	for _, at := range []time.Duration{0, time.Minute, 2 * time.Minute, 9 * time.Minute} {
		if p.failed("ps", errDialTimeout, t0.Add(at), false) {
			t.Fatalf("absent %s after the first failure, inside the %s span", at, printServerAbsentSpan)
		}
	}
	// ...until they span it.
	if !p.failed("ps", errDialTimeout, t0.Add(printServerAbsentSpan), false) {
		t.Fatal("not absent once the failures spanned the minimum time")
	}
}

// A venue power cut: the game server's WebSocket API is up before its print
// server. The failures in the first minutes must not make it absent, and the
// first answer makes it present.
func TestABootRaceDoesNotMakeThePrintServerAbsent(t *testing.T) {
	t0 := time.Now()
	p := newPresence(3, printServerAbsentSpan, t0)
	for i := 0; i < 6; i++ {
		if p.failed("ps", errDialTimeout, t0.Add(time.Duration(i)*time.Minute), false) {
			t.Fatalf("absent after %d failures in %d minutes", i+1, i)
		}
		if err := p.allow(t0.Add(time.Duration(i)*time.Minute), false); err != nil {
			t.Fatalf("dial held back during the boot race: %v", err)
		}
	}
	p.reached("ps", t0.Add(6*time.Minute), nil)
	if got := p.report(t0)["state"]; got != "present" {
		t.Fatalf("state = %v, want present", got)
	}
}

// A success between failures starts the count again.
func TestASuccessResetsTheFailureCount(t *testing.T) {
	t0 := time.Now()
	p := newPresence(3, 0, t0)
	p.failed("ps", errDialTimeout, t0, false)
	p.failed("ps", errDialTimeout, t0, false)
	p.reached("ps", t0, nil)
	p.failed("ps", errDialTimeout, t0, false)
	if p.failed("ps", errDialTimeout, t0, false) {
		t.Fatal("absent after 2 failures since a success")
	}
	if got := p.report(t0)["failures"]; got != 2 {
		t.Fatalf("failures = %v, want 2", got)
	}
}

// While absent, automatic work waits for the next check, which comes after 60s
// and then twice as long after each failed check, up to the cap. A check that
// is due is claimed by the first to ask.
func TestTheRecheckBacksOffToTheCap(t *testing.T) {
	t0 := time.Now()
	p := newPresence(3, 0, t0)
	for i := 0; i < 3; i++ {
		p.failed("ps", errDialTimeout, t0, false)
	}
	if !errors.Is(p.allow(t0.Add(59*time.Second), false), errPrintServerAbsent) {
		t.Fatal("dial allowed before the first check")
	}
	if err := p.allow(t0.Add(30*time.Second), true); err != nil {
		t.Fatalf("an operator's dial was held back: %v", err)
	}

	now := t0
	want := []time.Duration{60, 120, 240, 480, 900, 900, 900}
	for i, w := range want {
		if p.backoff != w*time.Second {
			t.Fatalf("check %d: backoff = %s, want %s", i, p.backoff, w*time.Second)
		}
		now = p.nextCheck
		if err := p.allow(now, false); err != nil {
			t.Fatalf("check %d: due check refused: %v", i, err)
		}
		if !errors.Is(p.allow(now, false), errPrintServerAbsent) {
			t.Fatalf("check %d: a due check was handed out twice", i)
		}
		if !p.failed("ps", errDialTimeout, now, false) {
			t.Fatalf("check %d: a failed check did not keep it absent", i)
		}
	}
	if got := p.report(now)["next_check_s"]; got != 900 {
		t.Fatalf("next_check_s = %v, want 900", got)
	}
}

// An operator's failed dial while absent is a check of its own: it moves
// nothing in the automatic schedule.
func TestAnOperatorsFailedDialMovesNoCheck(t *testing.T) {
	t0 := time.Now()
	p := newPresence(3, 0, t0)
	for i := 0; i < 3; i++ {
		p.failed("ps", errDialTimeout, t0, false)
	}
	next, backoff := p.nextCheck, p.backoff
	if !p.failed("ps", errDialTimeout, t0.Add(10*time.Second), true) {
		t.Fatal("still absent should be reported")
	}
	if p.nextCheck != next || p.backoff != backoff {
		t.Fatalf("an operator's dial moved the check: next %s -> %s, backoff %s -> %s", next, p.nextCheck, backoff, p.backoff)
	}
}

// reached reports a recovery once, and only from absent.
func TestReachedAfterAbsentReportsARecovery(t *testing.T) {
	t0 := time.Now()
	p := newPresence(3, 0, t0)
	if p.reached("ps", t0, nil) {
		t.Fatal("the first answer was reported as a recovery")
	}
	for i := 0; i < 3; i++ {
		p.failed("ps", errDialTimeout, t0, false)
	}
	waiting := 0
	if !p.reached("ps", t0.Add(time.Minute), func() int { waiting++; return 4 }) {
		t.Fatal("answering after absent was not reported as a recovery")
	}
	if waiting != 1 {
		t.Fatalf("waiting counted %d times, want once", waiting)
	}
	if p.reached("ps", t0.Add(2*time.Minute), nil) {
		t.Fatal("a second answer was reported as a recovery")
	}
	if err := p.allow(t0.Add(2*time.Minute), false); err != nil {
		t.Fatalf("dial held back once present: %v", err)
	}
}

// Each change of state logs one line; failed checks while absent log nothing;
// "reached" is said once per process.
func TestOneLogLinePerTransition(t *testing.T) {
	logs := captureLog(t)
	t0 := time.Now()
	p := newPresence(3, 0, t0)

	cycle := func(at time.Time) {
		p.reached("10.0.0.5:12123", at, nil)
		for i := 0; i < 3; i++ {
			p.failed("10.0.0.5:12123", errDialTimeout, at.Add(time.Duration(i)*time.Minute), false)
		}
		for i := 0; i < 4; i++ { // failed checks
			now := p.nextCheck
			_ = p.allow(now, false)
			p.failed("10.0.0.5:12123", errDialTimeout, now, false)
		}
		p.reached("10.0.0.5:12123", p.nextCheck, func() int { return 5 })
	}
	cycle(t0)
	cycle(t0.Add(24 * time.Hour))

	out := logs.String()
	for part, want := range map[string]int{
		"print server: reached at 10.0.0.5:12123":                          1,
		"10.0.0.5:12123 not answering after 3 attempts over 2m":            2,
		"treating it as absent: results and cache refresh paused":          2,
		"(next in 1m, at most every 15m)":                                  2,
		"10.0.0.5:12123 answering again after":                             2,
		"resuming (5 game(s) waiting)":                                     2,
		"i/o timeout":                                                      2,
		"[agent] print server:":                                            5,
		"10.0.0.5:12123 answering for the first time in this run":          0,
		"print server: 10.0.0.5:12123 not answering after 4 attempts over": 0,
	} {
		if got := strings.Count(out, part); got != want {
			t.Errorf("%q logged %d time(s), want %d\n%s", part, got, want, out)
		}
	}
}

// PRINT_SERVER_ABSENT_AFTER=0 is the old behaviour: nothing is ever held back.
func TestDetectionOffNeverHoldsADialBack(t *testing.T) {
	t0 := time.Now()
	p := newPresence(0, 0, t0)
	for i := 0; i < 50; i++ {
		now := t0.Add(time.Duration(i) * time.Hour)
		if p.failed("ps", errDialTimeout, now, false) {
			t.Fatalf("absent after %d failures with detection off", i+1)
		}
		if err := p.allow(now, false); err != nil {
			t.Fatalf("dial held back with detection off: %v", err)
		}
	}
	r := p.report(t0)
	if r["state"] != "unknown" || r["failures"] != 50 {
		t.Fatalf("report = %v, want unknown with 50 failures", r)
	}
	// The zero value (an App built without New) is the same.
	var zero printServerPresence
	for i := 0; i < 10; i++ {
		zero.failed("ps", errDialTimeout, t0, false)
	}
	if err := zero.allow(t0, false); err != nil {
		t.Fatalf("the zero value held a dial back: %v", err)
	}
}

// With nothing that uses the print server on, the state is "disabled" and an
// operator's dials, failing or not, never change it.
func TestADisabledStateIsNeverFlipped(t *testing.T) {
	t0 := time.Now()
	p := &printServerPresence{}
	p.configure(3, 15*time.Minute, true, t0)
	p.minSpan = 0
	for i := 0; i < 5; i++ {
		if p.failed("ps", errDialTimeout, t0, true) {
			t.Fatal("a disabled tracker went absent")
		}
	}
	if p.reached("ps", t0, nil) {
		t.Fatal("a disabled tracker reported a recovery")
	}
	r := p.report(t0)
	if r["state"] != "disabled" || r["failures"] != 0 || r["last_ok_at"] == nil {
		t.Fatalf("report = %v, want disabled, no failures, last_ok_at set", r)
	}
}

// A finished game makes the check due at once, but only for a print server
// that has answered in this run.
func TestAFinishedGameMakesTheCheckDueOnlyIfSeenBefore(t *testing.T) {
	t0 := time.Now()
	never := newPresence(3, 0, t0)
	for i := 0; i < 3; i++ {
		never.failed("ps", errDialTimeout, t0, false)
	}
	never.gameFinished(t0.Add(time.Second))
	if !errors.Is(never.allow(t0.Add(time.Second), false), errPrintServerAbsent) {
		t.Fatal("a finished game made a check due for a print server never seen")
	}

	seen := newPresence(3, 0, t0)
	seen.reached("ps", t0, nil)
	for i := 0; i < 3; i++ {
		seen.failed("ps", errDialTimeout, t0, false)
	}
	seen.gameFinished(t0.Add(time.Second))
	if err := seen.allow(t0.Add(time.Second), false); err != nil {
		t.Fatalf("a finished game did not make the check due: %v", err)
	}
}

// The report's fields, absent and present.
func TestThePrintServerReport(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	p := newPresence(3, 0, t0)
	r := p.report(t0)
	if r["state"] != "unknown" || r["since"] != "2026-10-25T01:00:00Z" || r["last_ok_at"] != nil {
		t.Fatalf("fresh report = %v", r)
	}
	for i := 0; i < 3; i++ {
		p.failed("ps", errDialTimeout, t0.Add(time.Minute), false)
	}
	r = p.report(t0.Add(time.Minute + 15*time.Second))
	want := map[string]any{
		"state": "absent", "since": "2026-10-25T01:01:00Z", "last_ok_at": nil, "failures": 3,
		"last_error": errDialTimeout.Error(), "next_check_s": 45, "dropped": 0,
	}
	if fmt.Sprint(r) != fmt.Sprint(want) {
		t.Fatalf("absent report = %v, want %v", r, want)
	}
	p.addDropped(2)
	p.reached("ps", t0.Add(time.Hour), nil)
	r = p.report(t0.Add(time.Hour))
	if _, ok := r["last_error"]; ok {
		t.Fatalf("last_error reported while present: %v", r)
	}
	if r["state"] != "present" || r["last_ok_at"] != "2026-10-25T02:00:00Z" || r["next_check_s"] != 0 || r["dropped"] != 2 {
		t.Fatalf("present report = %v", r)
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		1500 * time.Millisecond:         "2s",
		time.Minute:                     "1m",
		15 * time.Minute:                "15m",
		42*time.Minute + 20*time.Second: "42m",
		time.Hour:                       "1h",
		time.Hour + 5*time.Minute:       "1h5m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

// The overview runs on the admin API's goroutine while the results paths dial.
func TestThePrintServerReportIsSafeWhileDialling(t *testing.T) {
	p := newPresence(3, 0, time.Now())
	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				now := time.Now()
				switch (g + i) % 4 {
				case 0:
					p.failed("ps", errDialTimeout, now, false)
				case 1:
					p.reached("ps", now, func() int { return 0 })
				case 2:
					_ = p.allow(now, false)
					p.gameFinished(now)
				default:
					_ = p.report(now)
				}
			}
		}(g)
	}
	wg.Wait()
}

// --- in the agent -------------------------------------------------------------

// testDialer stands in for results.Dial: it counts dials, can fail them, and
// can run a hook inside a dial.
type testDialer struct {
	mu     sync.Mutex
	n      int
	fail   error
	during func()
}

func (d *testDialer) dial(host, port string, timeout time.Duration) (*results.Client, error) {
	d.mu.Lock()
	d.n++
	fail, during := d.fail, d.during
	d.mu.Unlock()
	if during != nil {
		during()
	}
	if fail != nil {
		return nil, fail
	}
	return results.Dial(host, port, timeout)
}

func (d *testDialer) dials() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.n
}

func (d *testDialer) failWith(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.fail = err
}

func (d *testDialer) setDuring(fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.during = fn
}

// pushedNumbers is the game numbers central was sent results for, in order.
func pushedNumbers(p *pushedGames) []int {
	var out []int
	for _, m := range p.all() {
		if n, ok := m["game_number"].(float64); ok {
			out = append(out, int(n))
		}
	}
	return out
}

// presenceApp is an idle agent with results on, wired to the simulated print
// server through a dialer the test controls, and to a fake central that
// records the results it is sent. Detection is on after 3 failures, without
// the minimum span, so three failed dials make the print server absent.
func presenceApp(t *testing.T, mutate func(*config.Config)) (*App, *ozonesim.PrintServer, *testDialer, *pushedGames) {
	t.Helper()
	ps := ozonesim.NewPrintServer()
	if err := ps.Start(0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ps.Close() })
	host, port, _ := net.SplitHostPort(ps.Addr())

	pushed := &pushedGames{}
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			pushed.add(payload)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	t.Cleanup(central.Close)

	cfg := config.Config{
		OzoneHost:              host,
		OzoneResultsPort:       port,
		ResultsHandshake:       2,
		ResultsEnabled:         true,
		CentralURL:             central.URL + "/api/agent/ingest",
		Token:                  "test",
		Version:                "test",
		PrintServerAbsentAfter: 3,
		PrintServerRecheckMax:  15 * time.Minute,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	a := New(cfg)
	a.ps.mu.Lock()
	a.ps.minSpan = 0
	a.ps.mu.Unlock()
	d := &testDialer{}
	a.dialResults = d.dial
	a.noteServerMode(1) // idle
	a.gameState.Store(stateIdle)
	return a, ps, d, pushed
}

func withCache(t *testing.T) func(*config.Config) {
	return func(c *config.Config) {
		c.CacheEnabled = true
		c.CacheDir = t.TempDir()
	}
}

// dialOnce makes one automatic (or operator's) dial through the single dial
// point, and closes what it opened.
func dialOnce(a *App, manual bool) error {
	s := a.newResultsSession(manual)
	defer s.retire()
	_, err := s.connect()
	return err
}

// makeAbsent fails three dials. seenFirst makes one succeed before them.
func makeAbsent(t *testing.T, a *App, d *testDialer, seenFirst bool) {
	t.Helper()
	if seenFirst {
		d.failWith(nil)
		if err := dialOnce(a, false); err != nil {
			t.Fatal(err)
		}
	}
	d.failWith(errDialTimeout)
	for i := 0; i < 3; i++ {
		_ = dialOnce(a, false)
	}
	if got := presenceState(a); got != "absent" {
		t.Fatalf("state = %q after three failed dials, want absent", got)
	}
}

func presenceState(a *App) string {
	s, _ := a.ps.report(time.Now())["state"].(string)
	return s
}

// recheckDue makes the next check due now.
func recheckDue(a *App) {
	a.ps.mu.Lock()
	defer a.ps.mu.Unlock()
	a.ps.nextCheck = time.Now().Add(-time.Second)
}

func nextCheck(a *App) time.Time {
	a.ps.mu.Lock()
	defer a.ps.mu.Unlock()
	return a.ps.nextCheck
}

func pending(a *App, n int) (pendingGame, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p, ok := a.pendingFetch[n]
	if !ok {
		return pendingGame{}, false
	}
	return *p, true
}

// Once absent, the results drain, the cache refresh and finish work make no
// dial until the next check: the queued game keeps its attempts (at most the
// ones before it went absent), is never abandoned, and logs nothing more.
func TestAnAbsentPrintServerIsLeftAloneUntilItsCheck(t *testing.T) {
	logs := captureLog(t)
	a, _, d, _ := presenceApp(t, withCache(t))
	d.failWith(errDialTimeout)
	a.queueFetch(9)

	for i := 0; i < 3; i++ {
		a.drainPendingFetches()
		a.mu.Lock()
		if p := a.pendingFetch[9]; p != nil {
			p.nextRetry = time.Time{} // due again at once
		}
		a.mu.Unlock()
	}
	if got := presenceState(a); got != "absent" {
		t.Fatalf("state = %q after three failed fetches, want absent", got)
	}
	if got := d.dials(); got != 3 {
		t.Fatalf("dials = %d, want 3", got)
	}
	p, _ := pending(a, 9)
	if p.attempts != 2 {
		t.Fatalf("attempts = %d, want 2: the failure that made it absent is not the game's", p.attempts)
	}
	failLines := strings.Count(logs.String(), "results: game #9 failed")

	for i := 0; i < 5; i++ {
		// Absent is not "deferred": finish work reported as put off would be
		// run again by every poll that finds the gate open, for as long as
		// the print server stays away.
		if a.drainPendingFetches() {
			t.Fatal("drain reported deferred while the print server is absent")
		}
		_ = a.refreshCache(false)
		a.finishWork()
		if a.finishPending.Load() {
			t.Fatal("finish work left pending while the print server is absent")
		}
	}
	if got := d.dials(); got != 3 {
		t.Fatalf("dials = %d while absent with no check due, want still 3", got)
	}
	if q, ok := pending(a, 9); !ok || q.attempts != 2 {
		t.Fatalf("game 9 pending=%v attempts=%d, want still queued with 2", ok, q.attempts)
	}
	if _, givenUp := a.resultsBacklog(); len(givenUp) != 0 {
		t.Fatalf("given up = %v, want none", givenUp)
	}
	if got := strings.Count(logs.String(), "results: game #9 failed"); got != failLines || failLines != 2 {
		t.Fatalf("per-game failure lines: %d before absent, %d after, want 2 and 2", failLines, got)
	}
	if got := strings.Count(logs.String(), "abandoned"); got != 0 {
		t.Fatalf("a game was abandoned: %s", logs.String())
	}
	if got := strings.Count(logs.String(), "treating it as absent"); got != 1 {
		t.Fatalf("absent logged %d times, want once", got)
	}

	// The check, when due, is one dial; failing, it costs the game nothing,
	// logs nothing, and puts the next check twice as far off.
	linesBefore := strings.Count(logs.String(), "[agent]")
	recheckDue(a)
	a.drainPendingFetches()
	a.drainPendingFetches()
	if got := d.dials(); got != 4 {
		t.Fatalf("dials = %d after a due check, want 4", got)
	}
	if q, _ := pending(a, 9); q.attempts != 2 {
		t.Fatalf("a failed check spent an attempt: %d", q.attempts)
	}
	if wait := time.Until(nextCheck(a)); wait < 110*time.Second || wait > 120*time.Second {
		t.Fatalf("next check in %s after a failed check, want about 2m", wait)
	}
	if got := strings.Count(logs.String(), "[agent]"); got != linesBefore {
		t.Fatalf("a failed check logged:\n%s", logs.String())
	}
}

// The check is a dial like any other: with the gate shut it never happens, by
// any path, even when it is due. An operator's command is refused too.
func TestAnAbsentPrintServerIsNeverCheckedDuringAGame(t *testing.T) {
	a, _, d, _ := presenceApp(t, withCache(t))
	makeAbsent(t, a, d, true)
	a.queueFetch(9)
	recheckDue(a)
	base := d.dials()

	type closer struct {
		name       string
		shut, open func()
	}
	closers := []closer{
		{"the bus says a game is on", func() { a.gameState.Store(stateActive) }, func() { a.gameState.Store(stateIdle) }},
		{"the last poll is unsettled", func() { a.pollUnsettled.Store(true) }, func() { a.pollUnsettled.Store(false) }},
		{"the reading is stale", func() { ageServerMode(a, maxServerModeAge+time.Second) }, func() { a.noteServerMode(1) }},
	}
	for _, m := range []int{3, 4, 5, 6, 10, 12, 13, 14, 15, 16, 17} {
		closers = append(closers, closer{fmt.Sprintf("SERVERMODE %d", m), func() { a.noteServerMode(m) }, func() { a.noteServerMode(1) }})
	}
	for _, c := range closers {
		c.shut()
		a.drainPendingFetches()
		_ = a.refreshCache(false)
		a.finishWork()
		a.finishPending.Store(false)
		// The dial point itself, as reached by work that passed its own
		// check of the gate just before the gate shut.
		if err := dialOnce(a, false); !deferrable(err) {
			t.Errorf("%s: connect = %v, want the gate's refusal", c.name, err)
		}
		for _, cmd := range []push.Command{
			{Type: "refetch_game", Payload: map[string]any{"game_number": 9}},
			{Type: "backfill_all", Payload: map[string]any{}},
			{Type: "cache_resync"},
		} {
			if status, _ := a.runCommand(cmd); status != "deferred" {
				t.Errorf("%s: %s = %q, want deferred", c.name, cmd.Type, status)
			}
		}
		time.Sleep(20 * time.Millisecond) // a drain a path started, if any, would have dialled
		c.open()
		if got := d.dials(); got != base {
			t.Fatalf("%s: %d dial(s) to the print server", c.name, got-base)
		}
	}

	// The check was never spent: with the gate open it happens.
	a.drainPendingFetches()
	if got := d.dials(); got != base+1 {
		t.Fatalf("dials = %d once the gate opened, want %d", got, base+1)
	}
}

// A dial that fails while the gate closes under it says nothing about the
// print server.
func TestADialThatFailsAsTheGateClosesDoesNotCount(t *testing.T) {
	a, _, d, _ := presenceApp(t, nil)
	d.failWith(errDialTimeout)
	d.setDuring(func() { a.pollUnsettled.Store(true) })
	for i := 0; i < 5; i++ {
		if err := dialOnce(a, false); !errors.Is(err, errDialTimeout) {
			t.Fatalf("dial %d: err = %v, want the dial error", i, err)
		}
		a.pollUnsettled.Store(false)
	}
	r := a.ps.report(time.Now())
	if r["state"] != "unknown" || r["failures"] != 0 {
		t.Fatalf("report = %v, want unknown with no failures counted", r)
	}
	if got := d.dials(); got != 5 {
		t.Fatalf("dials = %d, want 5", got)
	}
}

// PRINT_SERVER_ABSENT_AFTER=0: every dial still goes out.
func TestWithDetectionOffEveryDialGoesOut(t *testing.T) {
	a, _, d, _ := presenceApp(t, func(c *config.Config) { c.PrintServerAbsentAfter = 0 })
	d.failWith(errDialTimeout)
	for i := 0; i < 10; i++ {
		if err := dialOnce(a, false); errors.Is(err, errPrintServerAbsent) {
			t.Fatalf("dial %d held back with detection off", i)
		}
	}
	if got := d.dials(); got != 10 {
		t.Fatalf("dials = %d, want 10", got)
	}
}

// The print server answering again: the waiting games become due (attempts
// kept), the next open poll drains them oldest first, and the cache refresh
// after it fills in what the cache is missing.
func TestThePrintServerAnsweringAgainDrainsTheBacklog(t *testing.T) {
	logs := captureLog(t)
	a, ps, d, pushed := presenceApp(t, func(c *config.Config) {
		c.ResultsEnabled = false // results reach central through finish work here
		c.CacheEnabled = true
		c.CacheDir = t.TempDir()
	})
	ps.SetList([]byte(`{"gamelist":[]}`))
	if err := a.refreshCache(false); err != nil { // seen
		t.Fatal(err)
	}
	d.failWith(errDialTimeout)
	for _, n := range []int{12, 10, 11} {
		a.queueFetch(n)
	}
	a.noteFetchFailure(10, errTest) // backing off, one attempt spent
	for i := 0; i < 3; i++ {
		_ = a.refreshCache(false)
	}
	if got := presenceState(a); got != "absent" {
		t.Fatalf("state = %q, want absent", got)
	}

	for _, n := range []int{10, 11, 12, 13} {
		ps.AddGame(n, gamePayload(n))
	}
	recheckDue(a)
	d.failWith(nil)
	if err := a.refreshCache(false); err != nil { // the check: the list answers (still empty)
		t.Fatal(err)
	}
	if got := presenceState(a); got != "present" {
		t.Fatalf("state = %q after the print server answered, want present", got)
	}
	if !a.finishPending.Load() {
		t.Fatal("the waiting work was not handed to the next open poll")
	}
	if p, _ := pending(a, 10); !p.nextRetry.IsZero() || p.attempts != 1 {
		t.Fatalf("game 10 nextRetry=%s attempts=%d, want due now with its 1 attempt kept", p.nextRetry, p.attempts)
	}
	if got := strings.Count(logs.String(), "answering again after"); got != 1 {
		t.Fatalf("recovery logged %d times, want once:\n%s", got, logs.String())
	}
	if !strings.Contains(logs.String(), "resuming (3 game(s) waiting)") {
		t.Fatalf("recovery line does not count the waiting games:\n%s", logs.String())
	}

	ps.SetList([]byte(`{"gamelist":[` +
		`{"gamenum":10,"gamename":"g","duration":1,"starttime":"s","endtime":"e","playercount":0,"valid":1},` +
		`{"gamenum":11,"gamename":"g","duration":1,"starttime":"s","endtime":"e","playercount":0,"valid":1},` +
		`{"gamenum":12,"gamename":"g","duration":1,"starttime":"s","endtime":"e","playercount":0,"valid":1},` +
		`{"gamenum":13,"gamename":"g","duration":1,"starttime":"s","endtime":"e","playercount":0,"valid":1}]}`))
	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		if cmd == "GETSERVERSTATE" {
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
		}
		return []map[string]any{packsReply()}, false
	})
	if _, _, err := a.collect(client, false); err != nil {
		t.Fatal(err)
	}
	waitUntil(t, "the backlog drained and the cache refreshed", 3*time.Second, func() bool {
		_, cached := a.cache.GameRaw(13)
		return len(pushedNumbers(pushed)) == 3 && cached
	})
	if got := fmt.Sprint(pushedNumbers(pushed)); got != "[10 11 12]" {
		t.Fatalf("pushed %s, want [10 11 12] (oldest first)", got)
	}
	if n := a.pendingCount(); n != 0 {
		t.Fatalf("%d game(s) still pending", n)
	}
}

// Resuming never touches a game's finish delay: one that has only just
// finished is still not fetched before GAME_FINISH_DELAY is over.
func TestResumingKeepsTheFinishDelay(t *testing.T) {
	a, _, d, _ := presenceApp(t, nil)
	a.queueFetch(9)
	a.noteFetchFailure(9, errTest)
	until := time.Now().Add(time.Hour)
	a.notBefore(9, until)

	a.onPrintServerBack()

	p, _ := pending(a, 9)
	if !p.nextRetry.IsZero() || !p.notBefore.Equal(until) || p.attempts != 1 {
		t.Fatalf("after resuming: nextRetry=%s notBefore=%s attempts=%d; want due, the finish delay kept, 1 attempt", p.nextRetry, p.notBefore, p.attempts)
	}
	a.drainPendingFetches()
	if got := d.dials(); got != 0 {
		t.Fatalf("a game inside its finish delay was fetched (%d dial(s))", got)
	}
}

// A game that ends while a print server that answered earlier is absent is
// fetched once the finish delay is over, without waiting out the backoff.
func TestAFinishedGameChecksAPrintServerThatAnsweredBefore(t *testing.T) {
	a, ps, d, pushed := presenceApp(t, func(c *config.Config) { c.GameFinishDelay = 300 * time.Millisecond })
	ps.AddGame(42, gamePayload(42))
	makeAbsent(t, a, d, true)
	if wait := time.Until(nextCheck(a)); wait < 50*time.Second {
		t.Fatalf("next check in %s, want about a minute", wait)
	}
	d.failWith(nil) // back

	finished := time.Now()
	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameStart, Args: []string{"42"}})
	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameFinish})
	if n := ps.Requests("all"); n != 0 {
		t.Fatalf("game fetched %d time(s) before the finish delay", n)
	}
	waitUntil(t, "game 42 fetched after the finish delay", 3*time.Second, func() bool {
		return len(pushedNumbers(pushed)) == 1
	})
	if since := time.Since(finished); since < 300*time.Millisecond {
		t.Fatalf("fetched %s after the finish, inside the delay", since)
	}
	if got := presenceState(a); got != "present" {
		t.Fatalf("state = %q, want present", got)
	}
}

// A print server never reached in this run is not checked after every game
// (the finish work's cache refresh stays off it), and the finished game is
// not queued.
func TestAFinishedGameDoesNotCheckAPrintServerNeverSeen(t *testing.T) {
	a, _, d, _ := presenceApp(t, func(c *config.Config) {
		c.GameFinishDelay = 50 * time.Millisecond
		c.CacheEnabled = true
		c.CacheDir = t.TempDir()
	})
	makeAbsent(t, a, d, false)
	base := d.dials()
	d.failWith(nil)

	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameStart, Args: []string{"42"}})
	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameFinish})
	time.Sleep(300 * time.Millisecond) // finish work has run
	a.checkGameResults(map[string]any{"SERVERMODE": 7, "GAMENUM": 43})
	time.Sleep(100 * time.Millisecond)

	if got := d.dials(); got != base {
		t.Fatalf("%d dial(s) after a game at a venue whose print server never answered", got-base)
	}
	if n := a.pendingCount(); n != 0 {
		t.Fatalf("%d game(s) queued for a print server never seen", n)
	}
}

// An operator's command while the print server is absent dials once (still
// never in a game): failing, it is acked failed with the dial's error and
// moves no check; answering, the print server is present again.
func TestOperatorCommandsCheckAnAbsentPrintServer(t *testing.T) {
	type run func(a *App) (status string, result map[string]any)
	command := func(c push.Command) run {
		return func(a *App) (string, map[string]any) { return a.runCommand(c) }
	}
	runs := map[string]run{
		"refetch_game": command(push.Command{Type: "refetch_game", Payload: map[string]any{"game_number": 9}}),
		"backfill_all": command(push.Command{Type: "backfill_all", Payload: map[string]any{}}),
		"resync_all":   command(push.Command{Type: "resync_all", Payload: map[string]any{}}),
		"cache_resync": command(push.Command{Type: "cache_resync"}),
	}
	for name, do := range runs {
		t.Run(name, func(t *testing.T) {
			a, _, d, _ := presenceApp(t, withCache(t))
			makeAbsent(t, a, d, false)
			base, next := d.dials(), nextCheck(a)

			a.gameState.Store(stateActive)
			if status, _ := do(a); status != "deferred" {
				t.Fatalf("in a game: status %q, want deferred", status)
			}
			a.gameState.Store(stateIdle)
			if got := d.dials(); got != base {
				t.Fatalf("in a game: %d dial(s)", got-base)
			}

			status, result := do(a)
			if got := d.dials(); got != base+1 {
				t.Fatalf("absent: %d dial(s), want 1", got-base)
			}
			msg, _ := result["error"].(string)
			if status != "failed" || !strings.Contains(msg, "i/o timeout") || strings.Contains(msg, errPrintServerAbsent.Error()) {
				t.Fatalf("absent and failing: %q %v, want failed with the dial's error", status, result)
			}
			if presenceState(a) != "absent" || !nextCheck(a).Equal(next) {
				t.Fatal("an operator's failed dial changed the state or moved the check")
			}

			d.failWith(nil)
			if status, result := do(a); status != "acked" {
				t.Fatalf("answering: %q %v, want acked", status, result)
			}
			if got := presenceState(a); got != "present" {
				t.Fatalf("state = %q after the print server answered an operator, want present", got)
			}
		})
	}

	t.Run("admin Resync", func(t *testing.T) {
		logs := captureLog(t)
		a, _, d, _ := presenceApp(t, withCache(t))
		makeAbsent(t, a, d, false)
		base := d.dials()

		// In a game, and with the reading gone stale so the refusal is logged
		// and the test knows the background refresh has run.
		a.gameState.Store(stateActive)
		ageServerMode(a, maxServerModeAge+time.Second)
		a.Resync()
		waitUntil(t, "Resync to be refused", 2*time.Second, func() bool {
			return strings.Contains(logs.String(), "cache: refresh deferred")
		})
		a.noteServerMode(1)
		a.gameState.Store(stateIdle)
		if got := d.dials(); got != base {
			t.Fatalf("in a game: %d dial(s)", got-base)
		}

		a.Resync()
		waitUntil(t, "Resync to dial", 2*time.Second, func() bool { return d.dials() == base+1 })
		// The failed refresh holds resultsMu until its dial is recorded, so
		// taking it waits for that (a fixed sleep could lose on a slow
		// runner, and the next Resync would then wait behind this one).
		a.resultsMu.Lock()
		a.resultsMu.Unlock()
		if got := d.dials(); got != base+1 || presenceState(a) != "absent" {
			t.Fatalf("absent: %d dial(s), state %q; want 1 and still absent", got-base, presenceState(a))
		}

		d.failWith(nil)
		a.Resync()
		waitUntil(t, "the print server to be present", 2*time.Second, func() bool { return presenceState(a) == "present" })
		// The state flips inside connect, before the refresh lists and caches
		// game 9; the refresh holds resultsMu throughout, so taking it waits
		// for the last write before t.TempDir is removed.
		a.resultsMu.Lock()
		a.resultsMu.Unlock()
	})
}

// With nothing that uses the print server on, an operator's command still
// dials, but the state stays "disabled".
func TestOperatorCommandsNeverFlipADisabledState(t *testing.T) {
	a, _, d, _ := presenceApp(t, func(c *config.Config) { c.ResultsEnabled = false })
	if got := presenceState(a); got != "disabled" {
		t.Fatalf("state = %q with results, cache and bus off, want disabled", got)
	}
	refetch := push.Command{Type: "refetch_game", Payload: map[string]any{"game_number": 9}}
	d.failWith(errDialTimeout)
	for i := 0; i < 5; i++ {
		if status, _ := a.runCommand(refetch); status != "failed" {
			t.Fatalf("status %q, want failed", status)
		}
	}
	d.failWith(nil)
	if status, _ := a.runCommand(refetch); status != "acked" {
		t.Fatalf("status %q, want acked", status)
	}
	r := a.ps.report(time.Now())
	if r["state"] != "disabled" || r["failures"] != 0 || d.dials() != 6 {
		t.Fatalf("report %v after %d dials, want disabled with no failures after 6", r, d.dials())
	}
}

// A finished game is not queued for a print server never seen in this run.
func TestNothingQueuesForAPrintServerNeverSeen(t *testing.T) {
	a, _, d, _ := presenceApp(t, nil)
	makeAbsent(t, a, d, false)
	for n := 1; n <= 60; n++ {
		if a.queueFetch(n) {
			t.Fatalf("game %d queued", n)
		}
	}
	a.checkGameResults(map[string]any{"SERVERMODE": 7, "GAMENUM": 61})
	time.Sleep(50 * time.Millisecond)
	if n := a.pendingCount(); n != 0 {
		t.Fatalf("%d game(s) pending", n)
	}
	if got := a.ps.report(time.Now())["dropped"]; got != 0 {
		t.Fatalf("dropped = %v, want 0", got)
	}
	if got := d.dials(); got != 3 {
		t.Fatalf("dials = %d, want 3", got)
	}
}

// For a print server seen earlier, games keep queueing while it is absent:
// the newest 50 by queue order, whatever their numbers (O-Zone's can start
// again from 1); the rest are dropped and counted.
func TestQueueingWhileAbsentKeepsTheNewestByQueueOrder(t *testing.T) {
	a, _, d, _ := presenceApp(t, nil)
	makeAbsent(t, a, d, true)
	var order []int
	for n := 100; n < 130; n++ {
		order = append(order, n)
	}
	for n := 1; n <= 30; n++ { // the game numbers started again
		order = append(order, n)
	}
	for _, n := range order {
		if !a.queueFetch(n) {
			t.Fatalf("game %d not queued", n)
		}
	}
	if n := a.pendingCount(); n != maxPendingWhileAbsent {
		t.Fatalf("%d pending, want %d", n, maxPendingWhileAbsent)
	}
	for i, n := range order {
		_, kept := pending(a, n)
		if want := i >= 10; kept != want {
			t.Fatalf("game %d (queued %d of %d) kept=%v, want %v", n, i+1, len(order), kept, want)
		}
	}
	if got := a.ps.report(time.Now())["dropped"]; got != 10 {
		t.Fatalf("dropped = %v, want 10", got)
	}
}

// The slow poll's self-report and the overview carry the same print_server
// block.
func TestSelfReportAndOverviewCarryThePrintServer(t *testing.T) {
	idle := func(t *testing.T) func(cmd string, n int) ([]map[string]any, bool) {
		return func(cmd string, n int) ([]map[string]any, bool) {
			switch cmd {
			case "GETSERVERSTATE":
				return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false
			case "GETACTIVEPACKS":
				return []map[string]any{packsReply()}, false
			}
			return []map[string]any{{"CMD": cmd}}, false
		}
	}
	selfReport := func(t *testing.T, a *App) map[string]any {
		t.Helper()
		data, _, err := a.collect(scriptedOZone(t, idle(t)), true)
		if err != nil {
			t.Fatal(err)
		}
		var payload struct {
			Agent map[string]any `json:"agent"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			t.Fatal(err)
		}
		ps, ok := payload.Agent["print_server"].(map[string]any)
		if !ok {
			t.Fatalf("self-report %v carries no print_server", payload.Agent)
		}
		ov, ok := a.Overview()["print_server"].(map[string]any)
		if !ok {
			t.Fatalf("overview carries no print_server")
		}
		// Same keys and values, once both are JSON.
		ovJSON, _ := json.Marshal(ov)
		var ovBack map[string]any
		_ = json.Unmarshal(ovJSON, &ovBack)
		for _, k := range []string{"state", "since", "last_ok_at", "failures", "last_error", "dropped"} {
			if fmt.Sprint(ovBack[k]) != fmt.Sprint(ps[k]) {
				t.Fatalf("overview %s = %v, self-report %v", k, ovBack[k], ps[k])
			}
		}
		return ps
	}

	t.Run("absent, never seen", func(t *testing.T) {
		a, _, d, _ := presenceApp(t, nil)
		makeAbsent(t, a, d, false)
		ps := selfReport(t, a)
		if ps["state"] != "absent" || ps["last_ok_at"] != nil || ps["failures"] != float64(3) || ps["dropped"] != float64(0) {
			t.Fatalf("print_server = %v", ps)
		}
		if s, _ := ps["since"].(string); s == "" {
			t.Fatalf("since = %v, want a time", ps["since"])
		}
		if e, _ := ps["last_error"].(string); !strings.Contains(e, "i/o timeout") {
			t.Fatalf("last_error = %v", ps["last_error"])
		}
		if next, _ := ps["next_check_s"].(float64); next <= 0 || next > 60 {
			t.Fatalf("next_check_s = %v, want within the first minute", ps["next_check_s"])
		}
	})

	t.Run("present", func(t *testing.T) {
		a, _, d, _ := presenceApp(t, nil)
		if err := dialOnce(a, false); err != nil {
			t.Fatal(err)
		}
		_ = d
		ps := selfReport(t, a)
		if _, ok := ps["last_error"]; ok || ps["state"] != "present" || ps["next_check_s"] != float64(0) {
			t.Fatalf("print_server = %v", ps)
		}
		if s, _ := ps["last_ok_at"].(string); s == "" {
			t.Fatalf("last_ok_at = %v, want a time", ps["last_ok_at"])
		}
	})

	t.Run("disabled", func(t *testing.T) {
		a := New(config.Config{Version: "test", PrintServerAbsentAfter: 3})
		if ps := selfReport(t, a); ps["state"] != "disabled" {
			t.Fatalf("print_server = %v, want disabled", ps)
		}
	})
}

// --- review fixes (1.10.0) ----------------------------------------------------

// An operator's failed commands never make the print server absent. At a
// venue with the cache off, one never reached in this run would then queue
// nothing, and nothing automatic would ever check it again: results would
// stop for good, with Fleet's Re-fetch hidden behind "No print server".
func TestAnOperatorsFailedCommandsNeverMakeThePrintServerAbsent(t *testing.T) {
	refetch := push.Command{Type: "refetch_game", Payload: map[string]any{"game_number": 9}}

	t.Run("not reached yet", func(t *testing.T) {
		a, ps, d, pushed := presenceApp(t, nil) // results only: no cache, no bus
		ps.AddGame(42, gamePayload(42))
		d.failWith(errDialTimeout)
		for i := 0; i < 5; i++ {
			if status, _ := a.runCommand(refetch); status != "failed" {
				t.Fatalf("command %d: status %q, want failed", i, status)
			}
		}
		r := a.ps.report(time.Now())
		if r["state"] != "unknown" || r["failures"] != 0 {
			t.Fatalf("report after 5 failed commands = %v, want unknown with no failures counted", r)
		}
		// The next finished game is queued and fetched once it answers.
		d.failWith(nil)
		a.checkGameResults(map[string]any{"SERVERMODE": 7, "GAMENUM": 42})
		waitUntil(t, "game 42 fetched", 3*time.Second, func() bool { return len(pushedNumbers(pushed)) == 1 })
	})

	t.Run("present", func(t *testing.T) {
		a, _, d, _ := presenceApp(t, nil)
		d.failWith(nil)
		if err := dialOnce(a, false); err != nil {
			t.Fatal(err)
		}
		d.failWith(errDialTimeout)
		for i := 0; i < 5; i++ {
			_, _ = a.runCommand(refetch)
		}
		if got := presenceState(a); got != "present" {
			t.Fatalf("state = %q after an operator's failed commands, want still present", got)
		}
	})
}

// A game the bus reports finished keeps its GAME_FINISH_DELAY hold even when
// it is not queued (a print server never reached queues nothing). Should the
// print server answer inside the delay, neither the cache refresh nor a later
// poll's queueing fetches the game before O-Zone has finished writing it.
func TestTheFinishDelayHoldsAGameThatWasNotQueued(t *testing.T) {
	a, ps, d, pushed := presenceApp(t, func(c *config.Config) {
		withCache(t)(c)
		c.GameFinishDelay = time.Hour
	})
	ps.AddGame(42, gamePayload(42))
	ps.SetList([]byte(`{"gamelist":[{"gamenum":42,"gamename":"Solo","duration":600,"playercount":2,"valid":1}]}`))
	makeAbsent(t, a, d, false)

	finished := time.Now()
	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameStart, Args: []string{"42"}})
	a.handleBusEvent(msgbus.Event{Code: msgbus.EventGameFinish})
	if _, queued := pending(a, 42); queued {
		t.Fatal("game 42 queued for a print server never reached")
	}
	a.handleBusEvent(msgbus.Event{Code: msgbus.EventIdle}) // the gate opens inside the delay

	recheckDue(a)
	d.failWith(nil)
	_ = a.refreshCache(false)
	if got := presenceState(a); got != "present" {
		t.Fatalf("state = %q, want present: the check should have run", got)
	}
	if a.store.HasRaw(42) {
		t.Fatal("game 42 cached inside its finish delay")
	}

	a.checkGameResults(map[string]any{"SERVERMODE": 18, "GAMENUM": 42})
	p, queued := pending(a, 42)
	if !queued {
		t.Fatal("game 42 not queued once the print server answered")
	}
	if want := finished.Add(time.Hour); p.notBefore.Before(want.Add(-time.Second)) {
		t.Fatalf("notBefore = %s, want the finish delay kept (about %s)", p.notBefore, want)
	}
	time.Sleep(100 * time.Millisecond) // a drain the poll started, if any, has run
	if got := pushedNumbers(pushed); len(got) != 0 {
		t.Fatalf("pushed %v inside the finish delay", got)
	}
}

// While the print server is absent, a due check whose dial fails as a game
// starts under it is put off: it costs the queued game no attempt, logs no
// per-game failure, and so can never abandon it.
func TestACheckCutShortByAGameCostsTheGameNothing(t *testing.T) {
	logs := captureLog(t)
	a, _, d, _ := presenceApp(t, nil)
	makeAbsent(t, a, d, true)
	a.queueFetch(9)
	base := d.dials()
	d.setDuring(func() { a.gameState.Store(stateActive) })

	const checks = maxFetchAttempts + 2
	for i := 0; i < checks; i++ {
		recheckDue(a)
		a.mu.Lock()
		if p := a.pendingFetch[9]; p != nil {
			p.nextRetry = time.Time{} // due at once, as it would be after a deferral
		}
		a.mu.Unlock()
		a.drainPendingFetches()
		a.gameState.Store(stateIdle)
	}
	if got := d.dials() - base; got != checks {
		t.Fatalf("dials = %d, want %d: each due check should have dialled", got, checks)
	}
	if p, ok := pending(a, 9); !ok || p.attempts != 0 {
		t.Fatalf("game 9 pending=%v attempts=%d, want still queued with 0", ok, p.attempts)
	}
	if _, givenUp := a.resultsBacklog(); len(givenUp) != 0 {
		t.Fatalf("given up = %v, want none", givenUp)
	}
	if n := strings.Count(logs.String(), "results: game #9 failed"); n != 0 {
		t.Fatalf("%d per-game failure line(s) for checks cut short by a game", n)
	}
	if got := presenceState(a); got != "absent" {
		t.Fatalf("state = %q, want still absent", got)
	}
}

// Built through New(cfg) with the production minimum span (not overridden):
// a boot race of several quick failed dials does not make the print server
// absent, and a finished game is still queued.
func TestProductionMinSpanGuardsTheBootRace(t *testing.T) {
	a := New(config.Config{
		OzoneHost:              "192.0.2.10",
		OzoneResultsPort:       "12123",
		ResultsHandshake:       2,
		ResultsEnabled:         true,
		CentralURL:             "http://127.0.0.1:1/api/agent/ingest",
		Token:                  "test",
		Version:                "test",
		PrintServerAbsentAfter: 3,
		PrintServerRecheckMax:  15 * time.Minute,
	})
	if a.ps.minSpan != printServerAbsentSpan {
		t.Errorf("minSpan after New = %s, want %s", a.ps.minSpan, printServerAbsentSpan)
	}
	d := &testDialer{}
	a.dialResults = d.dial
	a.noteServerMode(1)
	a.gameState.Store(stateIdle)
	d.failWith(errDialTimeout)
	for i := 0; i < 5; i++ {
		_ = dialOnce(a, false)
	}
	if got := presenceState(a); got != "unknown" {
		t.Errorf("state after 5 quick failed dials = %q, want unknown (boot-race guard)", got)
	}
	if !a.queueFetch(42) {
		t.Error("queueFetch refused during a boot race: the game's results would never be fetched")
	}
}

// waitDrainIdle waits for a drain a poll started to finish.
func waitDrainIdle(t *testing.T, a *App) {
	t.Helper()
	time.Sleep(20 * time.Millisecond)
	waitUntil(t, "drain idle", 3*time.Second, func() bool { return !a.fetchBusy.Load() })
}

// Without the message bus, the poll that first sees a game finished checks a
// print server that answered earlier at once, and only that poll does: the
// game server sits in mode 7 or 18 for poll after poll.
func TestAPolledFinishChecksAPrintServerThatAnsweredBeforeOnce(t *testing.T) {
	t.Run("still absent: one dial", func(t *testing.T) {
		a, _, d, _ := presenceApp(t, nil)
		makeAbsent(t, a, d, true)
		base := d.dials()
		a.checkGameResults(map[string]any{"SERVERMODE": 7, "GAMENUM": 42})
		waitDrainIdle(t, a)
		for i := 0; i < 10; i++ {
			a.checkGameResults(map[string]any{"SERVERMODE": 18, "GAMENUM": 42})
			waitDrainIdle(t, a)
		}
		if got := d.dials() - base; got != 1 {
			t.Fatalf("dials after the finish = %d, want exactly 1", got)
		}
	})
	t.Run("back: fetched without waiting out the backoff", func(t *testing.T) {
		a, ps, d, pushed := presenceApp(t, nil)
		ps.AddGame(42, gamePayload(42))
		makeAbsent(t, a, d, true)
		if wait := time.Until(nextCheck(a)); wait < 50*time.Second {
			t.Fatalf("next check in %s, want about a minute", wait)
		}
		d.failWith(nil)
		a.checkGameResults(map[string]any{"SERVERMODE": 7, "GAMENUM": 42})
		waitUntil(t, "game 42 fetched", 3*time.Second, func() bool { return len(pushedNumbers(pushed)) == 1 })
		if got := presenceState(a); got != "present" {
			t.Fatalf("state = %q, want present", got)
		}
	})
}

// The slow poll's cache refresh is automatic work: with the print server
// treated as absent and its next check not due, polling between games dials
// nothing. An operator's (manual) refresh would skip that and wait out a dial
// timeout every SLOW_POLL_INTERVAL.
func TestTheSlowPollCacheRefreshLeavesAnAbsentPrintServerAlone(t *testing.T) {
	a, _, d, _ := presenceApp(t, func(c *config.Config) {
		withCache(t)(c)
		c.PollInterval = 5 * time.Millisecond
		c.IdlePollInterval = 5 * time.Millisecond
		c.SlowPollInterval = 0 // every poll is a slow one
	})
	if a.store == nil {
		t.Fatal("the test needs the cache on")
	}
	makeAbsent(t, a, d, false) // never answered: a venue without a print server
	if wait := time.Until(nextCheck(a)); wait < 50*time.Second {
		t.Fatalf("next check in %s; the test needs it well off", wait)
	}
	base := d.dials()

	client := scriptedOZone(t, func(cmd string, n int) ([]map[string]any, bool) {
		switch cmd {
		case "GETSERVERSTATE":
			return []map[string]any{{"CMD": cmd, "SERVERMODE": 2}}, false // idle
		case "GETACTIVEPACKS":
			return []map[string]any{packsReply()}, false
		}
		return []map[string]any{{"CMD": cmd}}, false
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		a.pollLoop(ctx, client)
	}()

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && d.dials() == base {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	time.Sleep(50 * time.Millisecond) // a refresh started by the last poll
	if err := a.printServerGate(); err != nil {
		t.Fatalf("gate = %v; the test needs it open", err)
	}
	if got := d.dials() - base; got != 0 {
		t.Fatalf("slow polls dialled the absent print server %d time(s) with no check due", got)
	}
}

// With results and the cache off but the message bus on, the bus GAME_FINISH
// path still dials the print server, so detection runs instead of reading
// "disabled".
func TestTheMessageBusAloneKeepsDetectionOn(t *testing.T) {
	a, _, d, _ := presenceApp(t, func(c *config.Config) {
		c.ResultsEnabled = false
		c.MsgBusEnabled = true
		c.OzoneMsgBusPort = "1" // built, never started
	})
	if a.bus == nil {
		t.Fatal("bus not built")
	}
	if got := presenceState(a); got != "unknown" {
		t.Fatalf("state = %q with results and cache off and the bus on, want unknown", got)
	}
	makeAbsent(t, a, d, false)
}

// The queue is capped only while the print server is absent: while it
// answers, every finished game stays queued and nothing counts as dropped.
func TestTheQueueIsNotCappedWhilePresent(t *testing.T) {
	a, _, d, _ := presenceApp(t, nil)
	d.failWith(nil)
	if err := dialOnce(a, false); err != nil {
		t.Fatal(err)
	}
	if got := presenceState(a); got != "present" {
		t.Fatalf("state = %q, want present", got)
	}
	for n := 1; n <= maxPendingWhileAbsent+10; n++ {
		if !a.queueFetch(n) {
			t.Fatalf("game %d not queued", n)
		}
	}
	if n := a.pendingCount(); n != maxPendingWhileAbsent+10 {
		t.Fatalf("%d pending, want %d", n, maxPendingWhileAbsent+10)
	}
	if got := a.ps.report(time.Now())["dropped"]; got != 0 {
		t.Fatalf("dropped = %v, want 0", got)
	}
}

// An operator's cache_resync arrives on a slow poll, beside the poll's own
// automatic refresh. It waits for that refresh instead of taking it for its
// own success, so it dials and says what it found.
func TestACacheResyncWaitsForTheSlowPollsRefresh(t *testing.T) {
	cases := map[string]func(*testing.T, *App, *testDialer){
		"not reached yet":       func(*testing.T, *App, *testDialer) {},
		"absent, check due now": func(t *testing.T, a *App, d *testDialer) { makeAbsent(t, a, d, false); recheckDue(a) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			a, _, d, _ := presenceApp(t, withCache(t))
			setup(t, a, d)
			d.failWith(errDialTimeout)
			d.setDuring(func() { time.Sleep(300 * time.Millisecond) }) // a dial waiting out its timeout
			base := d.dials()

			done := make(chan struct{})
			go func() { defer close(done); _ = a.refreshCache(false) }()
			waitUntil(t, "the automatic refresh to dial", 2*time.Second, func() bool { return d.dials() == base+1 })

			status, result := a.runCommand(push.Command{Type: "cache_resync"})
			<-done
			msg, _ := result["error"].(string)
			if status != "failed" || !strings.Contains(msg, "i/o timeout") {
				t.Fatalf("cache_resync = %q %v, want failed with the dial error", status, result)
			}
			if got := d.dials() - base; got != 2 {
				t.Fatalf("dials = %d, want 2: the automatic refresh's and the operator's", got)
			}
		})
	}
}

// Every results command whose dial a starting game cuts short, while the
// print server is absent, is put off for central to queue again, not
// failed: backfill and resync as well as a re-fetch or cache resync.
func TestOperatorCommandsCutShortByAGameAreDeferred(t *testing.T) {
	for _, cmd := range []push.Command{
		{Type: "refetch_game", Payload: map[string]any{"game_number": 9}},
		{Type: "backfill_all", Payload: map[string]any{}},
		{Type: "resync_all", Payload: map[string]any{}},
		{Type: "cache_resync"},
	} {
		t.Run(cmd.Type, func(t *testing.T) {
			a, _, d, _ := presenceApp(t, withCache(t))
			makeAbsent(t, a, d, false)
			base := d.dials()
			d.setDuring(func() { a.gameState.Store(stateActive) })

			status, result := a.runCommand(cmd)
			if got := d.dials() - base; got != 1 {
				t.Fatalf("dials = %d, want 1", got)
			}
			reason, _ := result["reason"].(string)
			if status != "deferred" || !strings.Contains(reason, errGameActive.Error()) {
				t.Fatalf("%s = %q %v, want deferred because a game started", cmd.Type, status, result)
			}
		})
	}
}
