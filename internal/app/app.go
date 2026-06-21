// Package app wires the agent together: poll O-Zone, assemble a batch, deliver to
// central (buffering on failure), and expose health. Reconnects with backoff.
package app

import (
	"context"
	"encoding/json"
	"log"
	"math"
	"os"
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
	gameState    atomic.Int32 // idle/active/finishing (Message Bus driven)
	fetchedGames map[int]bool
	pendingFetch map[int]bool // finished games awaiting a safe (non-game) window
	mu           sync.Mutex   // guards fetchedGames + pendingFetch + lastBusGame
	resultsMu    sync.Mutex   // serialises O-Zone results-API access
	cmdBusy      atomic.Bool  // single-flight for command processing

	store *store.Store   // local verbatim game cache (nil if disabled/unavailable)
	cache *cache.Cache   // O-Zone-shaped view over the store
	bus   *msgbus.Client // External Message Bus consumer
	proxy *proxy.Server  // transparent print-server proxy (TORN connects here)
}

func New(cfg config.Config) *App {
	a := &App{
		cfg:          cfg,
		pusher:       push.New(cfg.CentralURL, cfg.Token),
		buf:          buffer.New(cfg.BufferMax),
		health:       health.New(),
		startedAt:    time.Now(),
		fetchedGames: map[int]bool{},
		pendingFetch: map[int]bool{},
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
	return map[string]any{
		"games":             games,
		"state":             stateName(a.gameState.Load()),
		"server_mode":       int(a.serverMode.Load()),
		"msg_bus_connected": a.bus != nil && a.bus.Connected(),
		"version":           a.cfg.Version,
		"uptime_seconds":    int(time.Since(a.startedAt).Seconds()),
	}
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
		admin := adminapi.New(a, a.cfg.AdminAddr, a.cfg.AdminToken)
		_ = admin.Start()
		defer admin.Close()
	} else if a.cfg.AdminAddr != "" {
		log.Print("[agent] admin API not started: ADMIN_API_TOKEN is required")
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
	// SERVERMODE is a small enum, but toInt yields a platform-width int; bound it
	// against the int32 range before the atomic store so the conversion can't
	// overflow (CodeQL go/incorrect-integer-conversion).
	if mode < math.MinInt32 || mode > math.MaxInt32 {
		mode = 0
	}
	a.serverMode.Store(int32(mode))
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
		payload["agent"] = map[string]any{
			"version":        a.cfg.Version,
			"buffered":       a.buf.Len(),
			"uptime_seconds": int(time.Since(a.startedAt).Seconds()),
			"poll_interval":  int(a.cfg.PollInterval.Seconds()),
			"results":        a.cfg.ResultsEnabled,
			"commands":       a.cfg.CommandsEnabled,
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
		if err := a.pusher.Push(entry.Data, entry.Key); err != nil {
			log.Printf("[agent] push failed: %v (buffered=%d)", err, a.buf.Len())
			return
		}
		a.buf.PopFront()
		a.health.MarkPush()
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

	// Only ever touch the print server when it's safe to do so.
	if a.safeForPrintServer(mode) {
		a.drainPendingFetches()
	}
}

// drainPendingFetches fetches every queued finished game. Called only from a
// safe (non-game) server mode.
func (a *App) drainPendingFetches() {
	a.mu.Lock()
	pending := make([]int, 0, len(a.pendingFetch))
	for n := range a.pendingFetch {
		pending = append(pending, n)
	}
	a.mu.Unlock()

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
// results API — retrying on a later poll.
func (a *App) fetchGameResults(gameNumber int) {
	if !a.safeForPrintServer(int(a.serverMode.Load())) {
		return // a game is active; never touch the print server now
	}
	if !a.resultsMu.TryLock() {
		return // a command is using the results API; retry next poll
	}
	defer a.resultsMu.Unlock()

	if err := a.pullGameResults(gameNumber); err != nil {
		log.Printf("[agent] results: game #%d failed: %v", gameNumber, err)
		return
	}
	a.markFetched(gameNumber)
	a.clearPending(gameNumber)
	log.Printf("[agent] results: game #%d synced to central", gameNumber)
}

// pullGameResults fetches one game from O-Zone's results API and pushes it to
// central. Shared by the automatic finish detector and the command queue.
func (a *App) pullGameResults(gameNumber int) error {
	rc, err := results.Dial(a.cfg.OzoneHost, a.cfg.OzoneResultsPort, 5*time.Second)
	if err != nil {
		return err
	}
	defer rc.Close()

	// O-Zone pushes a fixed handshake (texts + event_types) on connect; consume
	// it before issuing a command, or the reply would be the handshake instead.
	rc.Drain(a.cfg.ResultsHandshake, 5*time.Second)

	raw, err := rc.GameDataRaw(gameNumber, 20*time.Second)
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
	_ = rc.Acknowledge(5 * time.Second)
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
		if isResultsCommand(c.Type) && !a.safeForPrintServer(int(a.serverMode.Load())) {
			_ = a.pusher.AckCommand(c.ID, "deferred", map[string]any{"reason": "game in progress"})
			log.Printf("[agent] commands: deferred %s #%d — game in progress", c.Type, c.ID)
			continue
		}

		status, result := a.runCommand(c)
		if err := a.pusher.AckCommand(c.ID, status, result); err != nil {
			log.Printf("[agent] commands: ack #%d failed: %v", c.ID, err)
			continue
		}
		log.Printf("[agent] commands: #%d (%s) -> %s", c.ID, c.Type, status)

		if c.Type == "reboot_agent" && status == "acked" {
			log.Printf("[agent] reboot requested — exiting for container restart")
			os.Exit(0)
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

// safeForPrintServer combines the two idle signals: the Message Bus driven game
// state (primary) AND the WS SERVERMODE allowlist (backstop). Both must say idle
// before the agent touches the O-Zone print server — never during a live game.
func (a *App) safeForPrintServer(mode int) bool {
	return printServerSafe(mode) && a.gameState.Load() == stateIdle
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
	if !a.safeForPrintServer(int(a.serverMode.Load())) {
		return
	}
	if !a.resultsMu.TryLock() {
		return
	}
	defer a.resultsMu.Unlock()

	rc, err := results.Dial(a.cfg.OzoneHost, a.cfg.OzoneResultsPort, 5*time.Second)
	if err != nil {
		return
	}
	defer rc.Close()
	rc.Drain(a.cfg.ResultsHandshake, 5*time.Second)

	listRaw, err := rc.GameListRaw(20 * time.Second)
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
	for _, gm := range list.GameList {
		num := toInt(gm["gamenum"])
		if num <= 0 || toInt(gm["valid"]) != 1 || a.store.HasRaw(num) {
			continue
		}
		// Stop immediately if a game starts mid-refresh.
		if !a.safeForPrintServer(int(a.serverMode.Load())) {
			return
		}
		raw, err := rc.GameDataRaw(num, 20*time.Second)
		if err != nil {
			continue
		}
		a.cacheGame(num, raw)
	}
	_ = rc.Acknowledge(5 * time.Second)
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
		merged, _ := a.store.Meta(meta.GameNumber)
		go a.failoverPush(merged, append([]byte(nil), raw...))
	}
}

// failoverPush backs one game up to central's failover store (best-effort).
func (a *App) failoverPush(m store.GameMeta, raw []byte) {
	err := a.pusher.PushOzoneGame(push.OzoneGameMeta{
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

	rc, err := results.Dial(a.cfg.OzoneHost, a.cfg.OzoneResultsPort, 5*time.Second)
	if err != nil {
		return "failed", map[string]any{"error": err.Error()}
	}
	defer rc.Close()
	rc.Drain(a.cfg.ResultsHandshake, 5*time.Second)

	list, err := rc.GameList(20 * time.Second)
	if err != nil {
		return "failed", map[string]any{"error": err.Error()}
	}
	games, _ := list["gamelist"].([]any)

	synced, failed, skipped := 0, 0, 0
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
		raw, err := rc.GameDataRaw(num, 20*time.Second)
		if err != nil {
			failed++
			continue
		}
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
	if len(a.fetchedGames) > 500 {
		a.fetchedGames = map[int]bool{n: true} // bound memory; re-pushes are idempotent
	}
}

// queueFetch records a finished game awaiting a safe window to be fetched.
func (a *App) queueFetch(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.pendingFetch[n] = true
	if len(a.pendingFetch) > 500 {
		a.pendingFetch = map[int]bool{n: true} // bound memory
	}
}

func (a *App) clearPending(n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.pendingFetch, n)
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
