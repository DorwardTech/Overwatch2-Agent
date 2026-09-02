// Package app wires the agent together: poll O-Zone, assemble a batch, deliver to
// central (buffering on failure), and expose health. Reconnects with backoff.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"overwatch/agent/internal/adminapi"
	"overwatch/agent/internal/buffer"
	"overwatch/agent/internal/cache"
	"overwatch/agent/internal/config"
	"overwatch/agent/internal/health"
	"overwatch/agent/internal/msgbus"
	"overwatch/agent/internal/ozone"
	"overwatch/agent/internal/packir"
	"overwatch/agent/internal/proxy"
	"overwatch/agent/internal/push"
	"overwatch/agent/internal/results"
	"overwatch/agent/internal/store"
)

// Game-state machine values (primary idle-gating signal, fed by the Message Bus;
// reconciled with WS SERVERMODE as a backstop).
const (
	stateIdle      int32 = 0
	stateActive    int32 = 1
	stateFinishing int32 = 2
)

// errGameActive is returned when a print-server operation is refused because a
// game is (or became) active. It is not a fetch failure: the work is retried in
// the next safe window without burning a retry attempt.
var errGameActive = errors.New("game in progress: print server off-limits")

// errLinkStale is returned when a print-server operation is refused because the
// agent has no CURRENT reading of O-Zone's server state. Like errGameActive it
// defers rather than fails — the work is retried once the link is back — but it
// is a distinct error so an operator is never told "game in progress" when the
// truth is that the agent lost contact with O-Zone.
var errLinkStale = errors.New("O-Zone link stale: server state unknown")

// errFrameMismatch is returned when a results reply identifies itself as a
// different game than the one requested — the signature of a desynchronised
// connection. Caching or pushing such a reply would file one game's data under
// another game's number, so it is refused.
var errFrameMismatch = errors.New("results: reply is for a different game")

// errNotGameData is returned when a reply is well-formed and in sequence but
// isn't game data — O-Zone answers a game it no longer holds with
// {"error":"Game not found"}. Storing that as the verbatim payload would mark
// the game cached, serve the error blob to TORN in its place, and stop the
// agent ever fetching the real thing.
var errNotGameData = errors.New("results: reply is not game data")

// unknownServerMode stands for "the server state could not be read as a mode".
// It is deliberately outside both printServerSafe and gameActive, so it neither
// opens the print-server gate nor claims a game is running.
const unknownServerMode = -1

// maxFailoverPushes bounds how many game payloads the failover backup may hold
// in memory at once. A bulk resync caches hundreds of games back to back, and
// each backup took its own full copy of the payload the moment it was queued —
// hundreds of copies live at once, in an agent limited to 128 MB, competing
// with the very telemetry buffer that exists to survive an outage. Goroutines
// are cheap; payloads are not, so the payload is now read from the store only
// once a slot is free. Four at a time keeps the backup useful without letting
// it become the reason the agent is killed.
const maxFailoverPushes = 4

// maxServerModeAge bounds how old a SERVERMODE reading may be and still be
// trusted to gate print-server access. collect() is the sole writer, so the
// reading only refreshes while the O-Zone WebSocket poll loop is running; when
// that link drops, Run reconnects with a backoff capped at 30s and keeps trying
// for as long as the outage lasts. Without an age bound the last reading stands
// forever, and paths that do not depend on the poll loop — an operator pressing
// Resync, a Message Bus finish handler — would keep passing a gate that is
// really just an old memory of an idle server.
//
// Two minutes is comfortably above the worst honest gap between readings (a
// slow poll can spend ~10s on each of five WebSocket commands plus a ~15s push
// before the next idle tick) and far below any real outage.
const maxServerModeAge = 2 * time.Minute

// maxBatchFetchErrors bounds how many CONNECTION failures in a row a batch
// (backfill/resync or a cache refresh) will absorb before giving up: read
// errors and desyncs, each of which now costs a reconnect on top of its
// timeout. A print server failing that consistently will not serve the rest of
// the list either, and the batch holds the results lock while it tries — far
// better to release it and let the next run start fresh. A game the server
// simply refuses does NOT count: that is one bad game, not a bad server, and
// the rest of the list is still worth fetching.
const maxBatchFetchErrors = 3

// Per-game retry policy for results fetches. A game whose fetch keeps failing
// (e.g. the print server is stuck on it) backs off exponentially and is
// abandoned after maxFetchAttempts so it can't hammer the print server on every
// poll forever. Abandoned games are surfaced in the agent self-report and can
// still be recovered manually via a refetch_game command.
const (
	maxFetchAttempts = 10
	fetchBackoffBase = 30 * time.Second
	fetchBackoffMax  = 30 * time.Minute
)

// pendingGame tracks the retry state of one finished game awaiting fetch.
type pendingGame struct {
	attempts  int
	nextRetry time.Time // zero = due immediately
}

// fetchBackoff returns the wait before retry number attempts+1.
func fetchBackoff(attempts int) time.Duration {
	d := fetchBackoffBase
	for i := 1; i < attempts && d < fetchBackoffMax; i++ {
		d *= 2
	}
	if d > fetchBackoffMax {
		return fetchBackoffMax
	}
	return d
}

type App struct {
	cfg          config.Config
	pusher       *push.Pusher
	buf          *buffer.Buffer
	health       *health.Health
	seq          int64
	startedAt    time.Time
	lastGameNum  int
	lastBusGame  int          // game number from the most recent GAME_START bus event
	serverMode   atomic.Int32 // latest O-Zone SERVERMODE
	serverModeAt atomic.Int64 // unix nanos when serverMode was last read (0 = never)
	gameState    atomic.Int32 // idle/active/finishing (Message Bus driven)
	fetchedGames map[int]bool
	pendingFetch map[int]*pendingGame // finished games awaiting a safe (non-game) window
	givenUpFetch map[int]bool         // games abandoned after maxFetchAttempts
	mu           sync.Mutex           // guards fetchedGames + pendingFetch + givenUpFetch + lastBusGame
	resultsMu    sync.Mutex           // serialises O-Zone results-API access
	cmdBusy      atomic.Bool          // single-flight for command processing
	collectBusy  atomic.Bool          // single-flight for a "collect from central" run
	fetchBusy    atomic.Bool          // single-flight for a pending-results drain
	failoverSem  chan struct{}        // bounds concurrent failover payload copies
	stopRun      context.CancelFunc   // set by Run; requestShutdown cancels it (graceful reboot)

	store  *store.Store   // local verbatim game cache (nil if disabled/unavailable)
	cache  *cache.Cache   // O-Zone-shaped view over the store
	bus    *msgbus.Client // External Message Bus consumer
	proxy  *proxy.Server  // transparent print-server proxy (TORN connects here)
	packir *packir.Server // LAN pack-IR bench listener + forwarder (nil if disabled)
}

func New(cfg config.Config) *App {
	a := &App{
		cfg:    cfg,
		pusher: push.New(cfg.CentralURL, cfg.Token),
		buf:    buffer.New(cfg.BufferMax),
		health: health.New(),
		// Seed push_seq from the clock so idempotency keys never repeat across
		// restarts (pushes are >1s apart, so seconds always outrun the counter).
		seq:          time.Now().Unix(),
		startedAt:    time.Now(),
		fetchedGames: map[int]bool{},
		pendingFetch: map[int]*pendingGame{},
		givenUpFetch: map[int]bool{},
		failoverSem:  make(chan struct{}, maxFailoverPushes),
	}

	// Restore telemetry spilled by the previous process (outage + restart).
	if cfg.BufferFile != "" {
		if n, err := a.buf.Load(cfg.BufferFile); err != nil {
			log.Printf("[agent] buffer: restore failed: %v", err)
		} else if n > 0 {
			log.Printf("[agent] buffer: restored %d unsent batch(es) from %s", n, cfg.BufferFile)
		}
	}

	// Optional LAN pack-IR bench listener (independent of O-Zone). Created here
	// (not in Run) so Overview can read it without a data race.
	if cfg.PackIRAddr != "" {
		a.packir = packir.New(cfg.PackIRAddr, cfg.Version, a.pusher, cfg.BufferMax, cfg.PackIRBufferFile)
	}

	if cfg.CacheEnabled {
		if s, err := store.Open(cfg.CacheDir); err != nil {
			log.Printf("[agent] cache disabled: %v", err)
		} else {
			a.store = s
			a.cache = cache.New(s)
			log.Printf("[agent] cache ready at %s (%d games)", cfg.CacheDir, s.Count())
		}
	}
	if cfg.MsgBusEnabled {
		a.bus = msgbus.New(cfg.OzoneHost, cfg.OzoneMsgBusPort, a.handleBusEvent)
	}
	return a
}

// Cache exposes the O-Zone-shaped cache view for the proxy/admin API (may be nil).
func (a *App) Cache() *cache.Cache { return a.cache }

// --- adminapi.Backend implementation ---------------------------------------

// Games returns cached game metadata (nil if the cache is disabled).
func (a *App) Games() []store.GameMeta {
	if a.store == nil {
		return nil
	}
	return a.store.AllMeta()
}

// GameRaw returns the verbatim "all" payload for a game.
func (a *App) GameRaw(gameNumber int) ([]byte, bool) {
	if a.cache == nil {
		return nil, false
	}
	return a.cache.GameRaw(gameNumber)
}

// Overview summarises cache + game-state for the admin API.
func (a *App) Overview() map[string]any {
	games := 0
	if a.store != nil {
		games = a.store.Count()
	}
	pending, givenUp := a.resultsBacklog()
	overview := map[string]any{
		"games":             games,
		"state":             stateName(a.gameState.Load()),
		"server_mode":       int(a.serverMode.Load()),
		"msg_bus_connected": a.bus != nil && a.bus.Connected(),
		"version":           a.cfg.Version,
		"uptime_seconds":    int(time.Since(a.startedAt).Seconds()),
		"results_pending":   pending,
		"results_given_up":  givenUp,
		"buffered":          a.buf.Len(),
	}
	if a.proxy != nil {
		overview["proxy_connections"] = a.proxy.Connections()
		overview["proxy_served"] = a.proxy.Served()
		// Non-zero means something on the venue LAN opened more connections
		// than the listener will hold — worth an operator's attention.
		overview["proxy_refused"] = a.proxy.Refused()
	}
	if a.packir != nil {
		overview["pack_ir"] = a.packir.Status()
	}
	return overview
}

// Resync triggers an idle-gated cache refresh in the background.
func (a *App) Resync() { go a.refreshCache() }

// Purge drops every cached game.
func (a *App) Purge() (int, error) {
	if a.store == nil {
		return 0, nil
	}
	return a.store.PurgeAll()
}

func stateName(s int32) string {
	switch s {
	case stateActive:
		return "active"
	case stateFinishing:
		return "finishing"
	default:
		return "idle"
	}
}

func (a *App) Run(ctx context.Context) {
	// Wrap the signal context so a reboot_agent command can also stop the run
	// gracefully (deferred closers run, the buffer spills to disk).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.stopRun = cancel

	if a.cfg.BufferFile != "" {
		defer a.saveBuffer()
	}

	go a.health.Serve(a.cfg.HealthAddr)

	if a.bus != nil {
		go a.bus.Run(ctx)
	}
	if a.store != nil {
		go a.pruneLoop(ctx)
		// Cold start with an empty cache: restore from central's failover store.
		if a.cfg.FailoverEnabled && a.store.Count() == 0 {
			go a.restoreFromCentral(ctx)
		}
	}

	// The proxy serves TORN from the cache; never expose it beyond the venue LAN.
	if a.cfg.ProxyEnabled && a.cache != nil {
		p := proxy.New(a.cache, a.cfg.ProxyListenAddr)
		if err := p.Start(); err != nil {
			log.Printf("[agent] print-server proxy failed to start: %v", err)
		} else {
			a.proxy = p
			defer p.Close()
		}
	}
	// Token-protected admin/control API (only if both address and token are set).
	if a.cfg.AdminAddr != "" && a.cfg.AdminToken != "" {
		var links adminapi.Links
		if base := strings.TrimRight(a.cfg.PublicDocsBase, "/"); base != "" {
			links = adminapi.Links{Changelog: base + "/agent/changelog", APIDocs: base + "/agent/api"}
		}
		admin := adminapi.New(a, a.cfg.AdminAddr, a.cfg.AdminToken, links)
		_ = admin.Start()
		defer admin.Close()
	} else if a.cfg.AdminAddr != "" {
		log.Print("[agent] admin API not started: ADMIN_API_TOKEN is required")
	}

	// LAN pack-IR bench listener: forwards emitter-strength readings to central,
	// buffering on outage. Runs regardless of whether O-Zone connects.
	if a.packir != nil {
		_ = a.packir.Start(ctx)
		defer a.packir.Close()
	}

	backoff := time.Second
	for ctx.Err() == nil {
		client, err := ozone.Dial(a.cfg.OzoneHost, a.cfg.OzonePort)
		if err != nil {
			log.Printf("[agent] O-Zone connect failed: %v (retry in %s)", err, backoff)
			if !sleepCtx(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}

		log.Printf("[agent] connected to O-Zone ws://%s:%s", a.cfg.OzoneHost, a.cfg.OzonePort)
		backoff = time.Second
		a.pollLoop(ctx, client)
		client.Close()
	}
}

func (a *App) pollLoop(ctx context.Context, client *ozone.Client) {
	// Cadence is dynamic: poll fast while a game is active, slow when the server
	// is idle. The timer is reset after each poll based on the latest mode.
	timer := time.NewTimer(0)
	defer timer.Stop()

	var lastSlow time.Time
	poll := func() bool {
		slow := time.Since(lastSlow) >= a.cfg.SlowPollInterval
		payload, key, err := a.collect(client, slow)
		if err != nil {
			log.Printf("[agent] poll error: %v (reconnecting)", err)
			return false
		}
		if slow {
			lastSlow = time.Now()
			if a.cfg.CommandsEnabled {
				go a.processCommands()
			}
			if a.store != nil {
				go a.refreshCache()
			}
		}
		a.health.MarkPoll()
		a.deliver(key, payload)
		return true
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if !poll() {
				return
			}
			// Fast while a game is active; idle rate otherwise. The idle rate is
			// kept short enough to stay within central's agent-offline window so
			// the agent never flaps "offline" between idle pushes.
			next := a.cfg.IdlePollInterval
			if gameActive(int(a.serverMode.Load())) || a.gameState.Load() == stateActive {
				next = a.cfg.PollInterval
			}
			timer.Reset(next)
		}
	}
}

// collect polls O-Zone and assembles a push payload.
func (a *App) collect(client *ozone.Client, slow bool) ([]byte, string, error) {
	server, err := client.Command("GETSERVERSTATE")
	if err != nil {
		return nil, "", err
	}
	mode := toInt(server["SERVERMODE"])
	a.noteServerMode(mode)
	a.reconcileState(mode)
	if a.cfg.ResultsEnabled {
		a.checkGameResults(server)
	}
	packs, err := client.Command("GETACTIVEPACKS")
	if err != nil {
		return nil, "", err
	}

	a.seq++
	payload := map[string]any{
		"push_seq":     a.seq,
		"client_ts":    time.Now().UTC().Format(time.RFC3339),
		"server_state": server,
		"packs":        packs["PACKS"],
	}

	if slow {
		if t, err := client.Command("GETTEAMINFO"); err == nil {
			payload["team_info"] = t["TEAMS"]
		}
		if g, err := client.Command("GETGAMELIST"); err == nil {
			payload["game_list"] = g["GAMES"]
		}
		if l, err := client.Command("GETFEATURELICINFO"); err == nil {
			// Forward the full feature-licence response (feature flags +
			// TIMEDLICS), so the Agent Log shows everything and central can
			// extract the timed licences for expiry tracking.
			payload["licence_info"] = l
		}
		// Agent self-report: version + health, so central can surface it.
		pending, givenUp := a.resultsBacklog()
		payload["agent"] = map[string]any{
			"version":          a.cfg.Version,
			"buffered":         a.buf.Len(),
			"uptime_seconds":   int(time.Since(a.startedAt).Seconds()),
			"poll_interval":    int(a.cfg.PollInterval.Seconds()),
			"results":          a.cfg.ResultsEnabled,
			"commands":         a.cfg.CommandsEnabled,
			"results_pending":  pending,
			"results_given_up": givenUp,
		}
	}

	data, err := json.Marshal(payload)
	return data, strconv.FormatInt(a.seq, 10), err
}

// deliver queues the batch then drains the buffer oldest-first.
func (a *App) deliver(key string, payload []byte) {
	a.buf.Push(key, payload)

	for {
		entry, ok := a.buf.Peek()
		if !ok {
			return
		}
		err := a.pusher.Push(entry.Data, entry.Key)
		if err == nil {
			a.buf.PopFront()
			a.health.MarkPush()
			continue
		}
		if push.Unsendable(err) {
			// The batch itself is what central objects to, so replaying it can
			// only fail the same way — and the queue is strictly FIFO, so
			// leaving it at the head holds back every NEWER batch behind it
			// until capacity finally evicts it, hours later. Drop it here and
			// keep the fresher telemetry flowing.
			log.Printf("[agent] push rejected permanently — dropping batch %s: %v (buffered=%d)",
				entry.Key, err, a.buf.Len())
			a.buf.PopFront()
			continue
		}
		log.Printf("[agent] push failed: %v (buffered=%d)", err, a.buf.Len())
		return
	}
}

// checkGameResults watches live server state for a finished game. A finished
// game is queued, then fetched ONLY when the server is in a safe (non-game)
// mode — the O-Zone print server must never be hit during active play, as it
// degrades game performance. Back-to-back games therefore defer the previous
// game's fetch until the server next goes idle/finished.
func (a *App) checkGameResults(server map[string]any) {
	gameNum := toInt(server["GAMENUM"])
	mode := toInt(server["SERVERMODE"])

	finished := 0
	switch {
	case (mode == 7 || mode == 18) && gameNum > 0: // GAME_FINISH / POST_GAME
		finished = gameNum
	case a.lastGameNum > 0 && gameNum > 0 && gameNum != a.lastGameNum: // advanced to a new game
		finished = a.lastGameNum
	}
	if gameNum > 0 {
		a.lastGameNum = gameNum
	}
	if finished > 0 && !a.isFetched(finished) {
		a.queueFetch(finished)
	}

	// Only ever touch the print server when it's safe to do so, and run the
	// drain OFF this goroutine. collect() is the sole writer of serverMode (and,
	// without the Message Bus, of gameState), so blocking it inside a drain
	// would freeze the very idle signal every fetch re-checks: a game starting
	// mid-drain would go unnoticed and the next queued game would be pulled from
	// the print server during live play. Keeping the poll loop free also stops a
	// slow drain from stalling telemetry past central's offline window.
	if a.safeForPrintServer(mode) {
		go a.drainPendingFetches()
	}
}

// drainPendingFetches fetches every queued finished game that is due (its
// per-game backoff has elapsed). Called only from a safe (non-game) server mode,
// and never on the poll goroutine (see checkGameResults). Single-flighted, so
// repeated polls during a long drain can't pile up goroutines; a game queued
// after the run started is picked up by the next drain.
func (a *App) drainPendingFetches() {
	if !a.fetchBusy.CompareAndSwap(false, true) {
		return // a drain is already in progress
	}
	defer a.fetchBusy.Store(false)

	now := time.Now()
	a.mu.Lock()
	pending := make([]int, 0, len(a.pendingFetch))
	for n, p := range a.pendingFetch {
		if now.Before(p.nextRetry) {
			continue // backing off; not due yet
		}
		pending = append(pending, n)
	}
	a.mu.Unlock()

	sort.Ints(pending) // oldest game first
	for _, n := range pending {
		if a.isFetched(n) {
			a.clearPending(n)
			continue
		}
		a.fetchGameResults(n)
	}
}

// fetchGameResults handles the automatic finish path. It refuses to run during
// active play, and yields (TryLock) if a command-driven sync is using the
// results API — retrying on a later poll. Genuine fetch failures back off
// per-game and give up after maxFetchAttempts.
func (a *App) fetchGameResults(gameNumber int) {
	if a.printServerGate() != nil {
		return // a game is active, or the link is stale; never touch it now
	}
	if !a.resultsMu.TryLock() {
		return // a command is using the results API; retry next poll
	}
	defer a.resultsMu.Unlock()

	if err := a.pullGameResults(gameNumber); err != nil {
		if deferrable(err) {
			return // not now; retry in the next safe window, no attempt burnt
		}
		a.noteFetchFailure(gameNumber, err)
		return
	}
	a.markFetched(gameNumber)
	a.clearPending(gameNumber)
	log.Printf("[agent] results: game #%d synced to central", gameNumber)
}

// noteFetchFailure records a failed results fetch: exponential per-game backoff,
// abandoning the game after maxFetchAttempts so a stuck print-server game can't
// be hammered on every poll forever (the #347 failure mode).
func (a *App) noteFetchFailure(n int, err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	p := a.pendingFetch[n]
	if p == nil {
		p = &pendingGame{}
		a.pendingFetch[n] = p
	}
	p.attempts++
	if p.attempts >= maxFetchAttempts {
		delete(a.pendingFetch, n)
		a.givenUpFetch[n] = true
		if len(a.givenUpFetch) > 100 {
			a.givenUpFetch = map[int]bool{n: true} // bound memory
		}
		log.Printf("[agent] results: game #%d abandoned after %d attempts: %v (recover with a refetch_game command)",
			n, p.attempts, err)
		return
	}
	wait := fetchBackoff(p.attempts)
	p.nextRetry = time.Now().Add(wait)
	log.Printf("[agent] results: game #%d failed (attempt %d/%d, next retry in %s): %v",
		n, p.attempts, maxFetchAttempts, wait, err)
}

// resultsBacklog reports the pending-fetch depth and the abandoned games, for
// the agent self-report and the admin API.
func (a *App) resultsBacklog() (pending int, givenUp []int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	givenUp = make([]int, 0, len(a.givenUpFetch))
	for n := range a.givenUpFetch {
		givenUp = append(givenUp, n)
	}
	sort.Ints(givenUp)
	return len(a.pendingFetch), givenUp
}

// resultsSession owns the print-server connection for a run of fetches — one
// game or a whole backfill — and is the only place the agent dials the results
// API.
//
// It exists because the results protocol carries no request/response
// correlation: a reply says nothing about what was asked for, so the Nth reply
// is only ever "the Nth frame on this socket". That makes a read error a
// property of the CONNECTION, not of the game it happened on. A deadline
// expiring mid-frame leaves unread bytes in the socket; a reply the print
// server sends after we gave up waiting arrives later, unbidden. Either way the
// stream is off by one, and the next game's request reads the PREVIOUS game's
// frame — which central would store under the wrong game number and mark
// fetched, so nothing would ever correct it.
//
// So any read error retires the connection and the next fetch dials a fresh
// one. Because a fresh dial is a new touch of the print server, it re-checks
// the idle gate; and because a desync can in principle survive a clean read,
// every payload is matched against the game number it reports for itself.
type resultsSession struct {
	app  *App
	conn *results.Client
}

func (a *App) newResultsSession() *resultsSession { return &resultsSession{app: a} }

// connect returns a live connection, dialling one if there isn't a usable one
// already. Every dial is gated on the print server being idle.
func (s *resultsSession) connect() (*results.Client, error) {
	if s.conn != nil {
		return s.conn, nil
	}
	if err := s.app.printServerGate(); err != nil {
		return nil, err
	}
	rc, err := results.Dial(s.app.cfg.OzoneHost, s.app.cfg.OzoneResultsPort, 5*time.Second)
	if err != nil {
		return nil, err
	}
	// O-Zone pushes a fixed handshake (texts + event_types) on connect; consume
	// it before issuing a command, or the reply would be the handshake instead.
	rc.Drain(s.app.cfg.ResultsHandshake, 5*time.Second)
	s.conn = rc
	return rc, nil
}

// retire closes the current connection. The next command dials a clean one.
func (s *resultsSession) retire() {
	if s.conn == nil {
		return
	}
	s.conn.Close()
	s.conn = nil
}

// acknowledge tells O-Zone the data was received (best-effort, only if the
// connection is still live).
func (s *resultsSession) acknowledge() {
	if s.conn != nil {
		_ = s.conn.Acknowledge(5 * time.Second)
	}
}

// list fetches the verbatim game list.
func (s *resultsSession) list() ([]byte, error) {
	rc, err := s.connect()
	if err != nil {
		return nil, err
	}
	raw, err := rc.GameListRaw(20 * time.Second)
	if err != nil {
		s.retire()
		return nil, err
	}
	return raw, nil
}

// game fetches one game's verbatim payload and verifies the reply is for the
// game that was asked for.
func (s *resultsSession) game(gameNumber int) ([]byte, error) {
	rc, err := s.connect()
	if err != nil {
		return nil, err
	}
	raw, err := rc.GameDataRaw(gameNumber, 20*time.Second)
	if err != nil {
		s.retire()
		return nil, err
	}
	got, isGame := payloadGameNumber(raw)
	if !isGame {
		// Read cleanly and in sequence, so the connection is fine — this is
		// O-Zone declining the game, not a desync. Keep the connection.
		return nil, fmt.Errorf("%w (game #%d)", errNotGameData, gameNumber)
	}
	if got > 0 && got != gameNumber {
		s.retire()
		return nil, fmt.Errorf("%w: asked for #%d, got #%d", errFrameMismatch, gameNumber, got)
	}
	return raw, nil
}

// payloadGameNumber reads the game number a results payload reports for itself.
// isGame is false when the reply carries no "game" section at all — an error
// reply rather than game data. The number is 0 when the section is there but
// nameless, which is NOT treated as a mismatch: the check only ever rejects a
// reply that positively identifies as a DIFFERENT game.
func payloadGameNumber(raw []byte) (number int, isGame bool) {
	var payload struct {
		Game map[string]any `json:"game"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.Game == nil {
		return 0, false
	}
	return toInt(payload.Game["gamenum"]), true
}

// pullGameResults fetches one game from O-Zone's results API and pushes it to
// central. Shared by the automatic finish detector and the command queue. It is
// the single chokepoint for per-game print-server access: every caller gets the
// idle-gate re-checked here, closing any window between an earlier check and
// the actual connection.
func (a *App) pullGameResults(gameNumber int) error {
	s := a.newResultsSession()
	defer s.retire()

	raw, err := s.game(gameNumber)
	if err != nil {
		return err
	}
	// Cache verbatim first (the proxy's source of truth), then push the decoded
	// game to central for analytics.
	a.cacheGame(gameNumber, raw)

	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return err
	}
	if err := a.pusher.PushGameResults(gameNumber, data); err != nil {
		return err
	}
	s.acknowledge()
	return nil
}

// processCommands pulls queued commands from central, runs each, and reports
// the outcome. Runs in its own goroutine (so a long backfill never stalls
// telemetry) and is single-flighted so runs can't overlap. A successful
// reboot_agent ack exits the process for the container to restart it.
func (a *App) processCommands() {
	if !a.cmdBusy.CompareAndSwap(false, true) {
		return // a previous run is still in progress
	}
	defer a.cmdBusy.Store(false)

	cmds, err := a.pusher.FetchCommands()
	if err != nil {
		log.Printf("[agent] commands: fetch failed: %v", err)
		return
	}
	for _, c := range cmds {
		// Commands that hit the O-Zone print server must never run during active
		// play. Defer them (central re-queues) until the server is idle.
		if isResultsCommand(c.Type) {
			if gate := a.printServerGate(); gate != nil {
				_ = a.pusher.AckCommand(c.ID, "deferred", map[string]any{"reason": gate.Error()})
				log.Printf("[agent] commands: deferred %s #%d — %v", c.Type, c.ID, gate)
				continue
			}
		}

		status, result := a.runCommand(c)
		if err := a.pusher.AckCommand(c.ID, status, result); err != nil {
			log.Printf("[agent] commands: ack #%d failed: %v", c.ID, err)
			continue
		}
		log.Printf("[agent] commands: #%d (%s) -> %s", c.ID, c.Type, status)

		if c.Type == "reboot_agent" && status == "acked" {
			// Stop the run loop gracefully (defers run, the buffer spills to
			// disk) and let the container restart policy bring us back up.
			log.Printf("[agent] reboot requested — shutting down for container restart")
			if a.stopRun != nil {
				a.stopRun()
			}
			return
		}
	}
}

// isResultsCommand reports whether a command type connects to the O-Zone print
// server (and so must be blocked during active play).
func isResultsCommand(t string) bool {
	switch t {
	case "refetch_game", "backfill_all", "resync_all", "cache_resync":
		return true
	}
	return false
}

// gameActive reports whether a game is starting, running, paused or pre-game —
// the window where pack telemetry should be polled fast.
func gameActive(mode int) bool {
	switch mode {
	case 3, 4, 5, 6, 10, 12, 13, 14, 15, 16, 17:
		return true
	}
	return false
}

// printServerSafe reports whether it's safe to connect to the O-Zone print
// server. Only true when the server is clearly NOT in active play: idle,
// running-idle, finished, aborted, history, or post-game.
func printServerSafe(mode int) bool {
	switch mode {
	case 1, 2, 7, 8, 9, 18:
		return true
	}
	return false
}

// safeForPrintServer combines the two idle signals — the Message Bus driven game
// state (primary) AND the WS SERVERMODE allowlist (backstop) — with the age of
// the SERVERMODE reading itself. Both signals must say idle, and the reading
// they rest on must be current, before the agent touches the O-Zone print
// server. The age matters because collect() is the sole writer of both: once
// the WS link drops they stop moving, so a favourable pair is only as good as
// the moment it was read (see maxServerModeAge).
func (a *App) safeForPrintServer(mode int) bool {
	return printServerSafe(mode) && a.gameState.Load() == stateIdle && a.serverModeFresh()
}

// noteServerMode records a freshly read SERVERMODE. It is the ONLY writer of
// serverMode, so the value and the time it was read can never drift apart — the
// gate trusts the value only because this pairs it with its age.
func (a *App) noteServerMode(mode int) {
	// SERVERMODE arrives as untrusted JSON and is stored narrowed to int32.
	// A value outside that range TRUNCATES, and a truncated value can land
	// inside printServerSafe's allowlist — 2^32+1 becomes 1, "idle" — so a
	// nonsense reading could open the print-server gate during a live game.
	// Out-of-range is therefore recorded as a mode no allowlist contains, which
	// closes the gate: not being able to read the server state is not the same
	// as reading that it is idle.
	if mode < math.MinInt32 || mode > math.MaxInt32 {
		mode = unknownServerMode
	}
	a.serverMode.Store(int32(mode))
	a.serverModeAt.Store(time.Now().UnixNano())
}

// serverModeFresh reports whether the last SERVERMODE reading is recent enough
// to gate print-server access. A never-polled agent is never fresh: not knowing
// the server state is not the same as knowing it is idle.
func (a *App) serverModeFresh() bool {
	at := a.serverModeAt.Load()
	return at != 0 && time.Since(time.Unix(0, at)) <= maxServerModeAge
}

// printServerGate is safeForPrintServer with the reason attached, for the paths
// that report why they deferred. nil means the print server may be touched.
func (a *App) printServerGate() error {
	if !a.serverModeFresh() {
		return errLinkStale
	}
	if !a.safeForPrintServer(int(a.serverMode.Load())) {
		return errGameActive
	}
	return nil
}

// deferrable reports whether an error means "not now" rather than "this
// failed": the work is retried in the next safe window without burning an
// attempt or counting against a batch's failure budget.
func deferrable(err error) bool {
	return errors.Is(err, errGameActive) || errors.Is(err, errLinkStale)
}

// reconcileState lets WS SERVERMODE correct the bus-driven game state if the bus
// is absent or missed an event. It only promotes to active or clears a stale
// active; the finishing grace period is cleared by the finish timer alone.
func (a *App) reconcileState(mode int) {
	switch {
	case gameActive(mode):
		a.gameState.Store(stateActive)
	case printServerSafe(mode):
		a.gameState.CompareAndSwap(stateActive, stateIdle)
	}
}

// handleBusEvent maps External Message Bus lifecycle events onto the game state.
func (a *App) handleBusEvent(ev msgbus.Event) {
	switch ev.Code {
	case msgbus.EventGameStart:
		a.gameState.Store(stateActive)
		if n, ok := ev.GameNumber(); ok {
			a.mu.Lock()
			a.lastBusGame = n
			a.mu.Unlock()
		}
		log.Print("[agent] bus: GAME_START — game active, print server off-limits")
	case msgbus.EventGameAbort, msgbus.EventIdle:
		a.gameState.Store(stateIdle)
	case msgbus.EventGameFinish:
		a.gameState.Store(stateFinishing)
		a.mu.Lock()
		g := a.lastBusGame
		a.mu.Unlock()
		if g > 0 {
			a.queueFetch(g)
		}
		go a.afterFinish()
	}
}

// afterFinish waits out the finish delay (so O-Zone finishes writing the game),
// returns to idle, then drains any pending fetch and refreshes the cache.
func (a *App) afterFinish() {
	time.Sleep(a.cfg.GameFinishDelay)
	a.gameState.CompareAndSwap(stateFinishing, stateIdle)
	a.drainPendingFetches()
	a.refreshCache()
}

// refreshCache pulls the O-Zone game list and backfills the local cache with any
// missing valid games. Cache-only (no central push), idle-gated, and serialised
// behind the results mutex. Cheap to call repeatedly — it skips games it has.
func (a *App) refreshCache() {
	if a.store == nil {
		return
	}
	if err := a.printServerGate(); err != nil {
		if errors.Is(err, errLinkStale) {
			// The poll loop refreshes the reading, so it is never stale on the
			// polled path — this is a Resync or a bus finish arriving while the
			// O-Zone link is down. Say so, or the refresh looks like a no-op.
			log.Printf("[agent] cache: refresh deferred — %v", err)
		}
		return
	}
	if !a.resultsMu.TryLock() {
		return
	}
	defer a.resultsMu.Unlock()

	sess := a.newResultsSession()
	defer sess.retire()

	listRaw, err := sess.list()
	if err != nil {
		return
	}
	var list struct {
		GameList []map[string]any `json:"gamelist"`
	}
	if err := json.Unmarshal(listRaw, &list); err != nil {
		return
	}
	for _, gm := range list.GameList {
		a.upsertListEntry(gm)
	}
	consecutive := 0
	for _, gm := range list.GameList {
		num := toInt(gm["gamenum"])
		if num <= 0 || toInt(gm["valid"]) != 1 || a.store.HasRaw(num) {
			continue
		}
		// Stop immediately if a game starts mid-refresh.
		if !a.safeForPrintServer(int(a.serverMode.Load())) {
			return
		}
		raw, err := sess.game(num)
		if err != nil {
			if deferrable(err) {
				return // a game started, or the link went stale, before reconnect
			}
			log.Printf("[agent] cache: refresh of game #%d failed: %v", num, err)
			if errors.Is(err, errNotGameData) {
				continue // this game only; the connection is still good
			}
			// The session has retired the connection, so the next game dials
			// afresh. Stop once the server has failed repeatedly in a row —
			// this runs again in a minute, and it holds the results lock
			// meanwhile.
			consecutive++
			if consecutive >= maxBatchFetchErrors {
				log.Printf("[agent] cache: refresh stopped after %d consecutive failures", consecutive)
				return
			}
			continue
		}
		consecutive = 0
		a.cacheGame(num, raw)
	}
	sess.acknowledge()
}

// cacheGame stores a verbatim "all" payload in the local cache and, when failover
// is enabled, backs it up to central asynchronously (best-effort).
func (a *App) cacheGame(gameNumber int, raw []byte) {
	if a.store == nil {
		return
	}
	meta := metaFromRaw(gameNumber, raw)
	if err := a.store.StoreGame(meta, raw); err != nil {
		log.Printf("[agent] cache: store game #%d failed: %v", gameNumber, err)
		return
	}
	if a.cfg.FailoverEnabled {
		go a.failoverPush(meta.GameNumber)
	}
}

// failoverPush backs one game up to central's failover store (best-effort).
// The payload is read from the store rather than carried in, and only after a
// slot is acquired, so a backlog of pending backups costs goroutines rather
// than a copy of every game payload it is waiting to send.
func (a *App) failoverPush(gameNumber int) {
	a.failoverSem <- struct{}{}
	defer func() { <-a.failoverSem }()

	if a.store == nil {
		return
	}
	m, ok := a.store.Meta(gameNumber)
	if !ok {
		return
	}
	raw, ok, err := a.store.GameRaw(gameNumber)
	if err != nil || !ok {
		return // pruned or unreadable since it was queued; nothing to back up
	}

	err = a.pusher.PushOzoneGame(push.OzoneGameMeta{
		GameNumber:  m.GameNumber,
		GameName:    m.GameName,
		GameType:    m.GameType,
		Duration:    m.Duration,
		StartTime:   m.StartTime,
		EndTime:     m.EndTime,
		PlayerCount: m.PlayerCount,
		Valid:       m.Valid,
	}, raw)
	if err != nil {
		log.Printf("[agent] failover: game #%d backup failed: %v", m.GameNumber, err)
	}
}

// restoreFromCentral repopulates an empty local cache from central's failover
// store after an on-prem rebuild. Best-effort; runs once at startup.
func (a *App) restoreFromCentral(ctx context.Context) {
	metas, err := a.pusher.FetchOzoneGameMeta()
	if err != nil || len(metas) == 0 {
		return
	}
	restored := 0
	for _, m := range metas {
		if ctx.Err() != nil {
			return
		}
		raw, ok, err := a.pusher.FetchOzoneGameRaw(m.GameNumber)
		if err != nil || !ok {
			continue
		}
		sm := store.GameMeta{
			GameNumber:  m.GameNumber,
			GameName:    m.GameName,
			GameType:    m.GameType,
			Duration:    m.Duration,
			StartTime:   m.StartTime,
			EndTime:     m.EndTime,
			PlayerCount: m.PlayerCount,
			Valid:       m.Valid,
		}
		_ = a.store.UpsertListEntry(sm) // carries end_time/duration
		if err := a.store.StoreGame(sm, raw); err == nil {
			restored++
		}
	}
	if restored > 0 {
		log.Printf("[agent] cache: restored %d game(s) from central failover", restored)
	}
}

// ozoneStartLayout is O-Zone's start_time format (SQL NOW(), naive local time).
const ozoneStartLayout = "2006-01-02 15:04:05"

// parseOzoneStart parses an O-Zone start_time; ok is false for empty/"None"/
// unparseable values (which can't be placed on a timeframe).
func parseOzoneStart(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" || s == "None" {
		return time.Time{}, false
	}
	t, err := time.Parse(ozoneStartLayout, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// Collect pulls games from central's failover store into the local cache for the
// window [from, to] (zero bounds are open-ended), skipping games already cached.
// It only reads from central and writes to disk — it never touches the O-Zone
// print server, so it is safe to run during a live game. Returns a summary.
func (a *App) Collect(from, to time.Time) (map[string]any, error) {
	if a.store == nil {
		return nil, fmt.Errorf("cache is disabled on this agent")
	}
	if !a.collectBusy.CompareAndSwap(false, true) {
		return nil, fmt.Errorf("a collect is already running")
	}
	defer a.collectBusy.Store(false)

	metas, err := a.pusher.FetchOzoneGameMeta()
	if err != nil {
		return nil, fmt.Errorf("fetch game list from central: %w", err)
	}

	filtering := !from.IsZero() || !to.IsZero()
	var collected, already, outOfRange, failed int
	for _, m := range metas {
		if filtering {
			t, ok := parseOzoneStart(m.StartTime)
			if !ok || (!from.IsZero() && t.Before(from)) || (!to.IsZero() && t.After(to)) {
				outOfRange++
				continue
			}
		}
		if a.store.HasRaw(m.GameNumber) {
			already++
			continue
		}
		raw, ok, err := a.pusher.FetchOzoneGameRaw(m.GameNumber)
		if err != nil || !ok {
			failed++
			continue
		}
		sm := store.GameMeta{
			GameNumber:  m.GameNumber,
			GameName:    m.GameName,
			GameType:    m.GameType,
			Duration:    m.Duration,
			StartTime:   m.StartTime,
			EndTime:     m.EndTime,
			PlayerCount: m.PlayerCount,
			Valid:       m.Valid,
		}
		_ = a.store.UpsertListEntry(sm)
		if err := a.store.StoreGame(sm, raw); err != nil {
			failed++
			continue
		}
		collected++
	}

	log.Printf("[agent] collect: %d collected, %d already cached, %d out of range, %d failed (central had %d)",
		collected, already, outOfRange, failed, len(metas))
	return map[string]any{
		"collected":      collected,
		"already_cached": already,
		"out_of_range":   outOfRange,
		"failed":         failed,
		"central_total":  len(metas),
	}, nil
}

// upsertListEntry records the metadata from one O-Zone game-list element (the
// only place the end time / actual duration are available).
func (a *App) upsertListEntry(gm map[string]any) {
	if a.store == nil {
		return
	}
	num := toInt(gm["gamenum"])
	if num <= 0 {
		return
	}
	end, _ := gm["endtime"].(string)
	if end == "None" {
		end = ""
	}
	name, _ := gm["gamename"].(string)
	start, _ := gm["starttime"].(string)
	_ = a.store.UpsertListEntry(store.GameMeta{
		GameNumber:  num,
		GameName:    name,
		Duration:    toInt(gm["duration"]),
		StartTime:   start,
		EndTime:     end,
		PlayerCount: toInt(gm["playercount"]),
		Valid:       toInt(gm["valid"]),
	})
}

// metaFromRaw derives cache metadata from a verbatim "all" payload. It does NOT
// set Duration: the "all" payload only carries the configured game length, while
// the list response carries the actual run time, which StoreGame preserves.
func metaFromRaw(gameNumber int, raw []byte) store.GameMeta {
	meta := store.GameMeta{GameNumber: gameNumber}
	var payload struct {
		Game    map[string]any             `json:"game"`
		Players map[string]json.RawMessage `json:"players"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return meta
	}
	if g := payload.Game; g != nil {
		if n := toInt(g["gamenum"]); n > 0 {
			meta.GameNumber = n
		}
		if s, ok := g["gamename"].(string); ok {
			meta.GameName = s
		}
		meta.GameType = toInt(g["gametype"])
		if s, ok := g["date"].(string); ok {
			meta.StartTime = s
		}
	}
	meta.PlayerCount = len(payload.Players)
	return meta
}

// pruneLoop enforces the cache retention window once an hour.
func (a *App) pruneLoop(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := a.store.Prune(time.Now().Add(-a.cfg.CacheRetention)); err == nil && n > 0 {
				log.Printf("[agent] cache: pruned %d game(s) past retention", n)
			}
		}
	}
}

// runCommand executes a single command and returns its ack status + result.
func (a *App) runCommand(c push.Command) (string, map[string]any) {
	switch c.Type {
	case "refetch_game":
		n := toInt(c.Payload["game_number"])
		if n <= 0 {
			return "failed", map[string]any{"error": "missing or invalid game_number"}
		}
		a.resultsMu.Lock()
		err := a.pullGameResults(n)
		a.resultsMu.Unlock()
		if deferrable(err) {
			// A game started between the command-queue gate and now, or the
			// O-Zone link went stale; central re-queues deferred commands for
			// the next safe window.
			return "deferred", map[string]any{"reason": err.Error(), "game_number": n}
		}
		if err != nil {
			return "failed", map[string]any{"error": err.Error(), "game_number": n}
		}
		a.markFetched(n)
		return "acked", map[string]any{"game_number": n}

	case "backfill_all":
		return a.syncGames(c.Payload, false)
	case "resync_all":
		return a.syncGames(c.Payload, true)

	case "reboot_agent":
		// The actual restart is handled after the ack so it isn't repeated.
		return "acked", map[string]any{"rebooting": true}

	case "agent_overview":
		// Read-only status snapshot — identical to the admin API's /overview.
		// Lets an operator reach the agent's admin view through central without
		// any inbound path to the venue.
		return "acked", a.Overview()

	case "cache_resync":
		// Idle-gated cache refresh — the admin API's /resync. Listed in
		// isResultsCommand so central re-queues it until the server is idle
		// rather than running it during a live game.
		a.refreshCache()
		n := 0
		if a.store != nil {
			n = a.store.Count()
		}
		return "acked", map[string]any{"games": n}

	case "cache_purge":
		// Drop every cached game — the admin API's /purge. Local filesystem
		// only, so it never touches the print server and is safe any time.
		n, err := a.Purge()
		if err != nil {
			return "failed", map[string]any{"error": err.Error()}
		}
		return "acked", map[string]any{"purged": n}

	default:
		return "failed", map[string]any{"error": "unsupported command type: " + c.Type}
	}
}

// syncGames lists the games O-Zone holds and fetches each one central is
// missing (backfill) or every valid game (resync, force=true). The whole
// batch runs over a single results connection.
func (a *App) syncGames(payload map[string]any, force bool) (string, map[string]any) {
	have := map[int]bool{}
	if !force {
		if arr, ok := payload["have"].([]any); ok {
			for _, v := range arr {
				have[toInt(v)] = true
			}
		}
	}

	a.resultsMu.Lock()
	defer a.resultsMu.Unlock()

	// processCommands checked the idle gate BEFORE the (potentially long) wait on
	// resultsMu — a refreshCache holding the lock can run for tens of seconds, so
	// a game may have started while we blocked here. Re-check before dialling:
	// every print-server connection is made under a freshly read idle signal,
	// exactly as pullGameResults does for the per-game path.
	if err := a.printServerGate(); err != nil {
		return "deferred", map[string]any{"reason": err.Error()}
	}

	sess := a.newResultsSession()
	defer sess.retire()

	listRaw, err := sess.list()
	if err != nil {
		return "failed", map[string]any{"error": err.Error()}
	}
	var list map[string]any
	if err := json.Unmarshal(listRaw, &list); err != nil {
		return "failed", map[string]any{"error": err.Error()}
	}
	games, _ := list["gamelist"].([]any)

	synced, failed, skipped, consecutive := 0, 0, 0, 0
	for _, g := range games {
		gm, ok := g.(map[string]any)
		if !ok {
			continue
		}
		a.upsertListEntry(gm) // keep the cache's game list in sync with O-Zone
		num := toInt(gm["gamenum"])
		if num <= 0 || toInt(gm["valid"]) != 1 {
			skipped++
			continue
		}
		if !force && have[num] {
			skipped++
			continue
		}
		// Stop immediately if a game starts mid-batch — a long backfill must
		// never keep pulling from the print server during active play. The same
		// applies if the O-Zone link goes stale under us: the gate is then an
		// old reading, not a live all-clear. Central re-queues the deferred
		// command for the next safe window.
		if err := a.printServerGate(); err != nil {
			log.Printf("[agent] sync aborted mid-batch: %v (%d synced so far)", err, synced)
			return "deferred", map[string]any{
				"reason": err.Error(), "synced": synced, "skipped": skipped, "failed": failed,
			}
		}
		raw, err := sess.game(num)
		if err != nil {
			if deferrable(err) {
				log.Printf("[agent] sync aborted before reconnect: %v (%d synced so far)", err, synced)
				return "deferred", map[string]any{
					"reason": err.Error(), "synced": synced, "skipped": skipped, "failed": failed,
				}
			}
			log.Printf("[agent] sync: game #%d failed: %v", num, err)
			failed++
			if errors.Is(err, errNotGameData) {
				continue // this game only; the connection is still good
			}
			// The connection has been retired; the next game dials afresh. Stop
			// once the print server has failed repeatedly in a row rather than
			// grinding through the rest of the list holding the results lock —
			// central can re-issue the command.
			consecutive++
			if consecutive >= maxBatchFetchErrors {
				log.Printf("[agent] sync abandoned after %d consecutive failures (%d synced)", consecutive, synced)
				return "failed", map[string]any{
					"error": err.Error(), "synced": synced, "skipped": skipped, "failed": failed,
				}
			}
			continue
		}
		consecutive = 0
		a.cacheGame(num, raw)

		var data map[string]any
		if err := json.Unmarshal(raw, &data); err != nil {
			failed++
			continue
		}
		if err := a.pusher.PushGameResults(num, data); err != nil {
			failed++
			continue
		}
		a.markFetched(num)
		synced++
	}

	log.Printf("[agent] sync (%s): %d synced, %d skipped, %d failed",
		map[bool]string{true: "resync", false: "backfill"}[force], synced, skipped, failed)
	return "acked", map[string]any{"synced": synced, "skipped": skipped, "failed": failed}
}

func (a *App) isFetched(n int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.fetchedGames[n]
}

func (a *App) markFetched(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fetchedGames[n] = true
	delete(a.givenUpFetch, n) // a manual refetch recovers an abandoned game
	if len(a.fetchedGames) > 500 {
		a.fetchedGames = map[int]bool{n: true} // bound memory; re-pushes are idempotent
	}
}

// queueFetch records a finished game awaiting a safe window to be fetched.
// Re-queueing an already-pending game keeps its retry state (backoff must not
// reset just because the finish was noticed again on a later poll).
func (a *App) queueFetch(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.givenUpFetch[n] {
		return // abandoned after repeated failures; only refetch_game revives it
	}
	if _, ok := a.pendingFetch[n]; !ok {
		a.pendingFetch[n] = &pendingGame{}
	}
	if len(a.pendingFetch) > 500 {
		a.pendingFetch = map[int]*pendingGame{n: a.pendingFetch[n]} // bound memory
	}
}

func (a *App) clearPending(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pendingFetch, n)
}

// saveBuffer spills unsent telemetry to disk so it survives the restart.
func (a *App) saveBuffer() {
	if err := a.buf.Save(a.cfg.BufferFile); err != nil {
		log.Printf("[agent] buffer: spill failed: %v", err)
	} else if n := a.buf.Len(); n > 0 {
		log.Printf("[agent] buffer: spilled %d unsent batch(es) to %s", n, a.cfg.BufferFile)
	}
}

// toInt coerces O-Zone JSON values (float64, string, int) to an int.
func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(strings.TrimSpace(n))
		return i
	default:
		return 0
	}
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

func nextBackoff(d time.Duration) time.Duration {
	d *= 2
	if d > 30*time.Second {
		return 30 * time.Second
	}
	return d
}
