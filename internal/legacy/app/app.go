// Package app runs the agent in legacy (Nexus) mode: it polls the on-box
// management app for live pack state and the Nexus MySQL database for finished
// games, translates both into the central ingest + game-results payloads, and
// pushes them with the same client the O-Zone agent uses. Everything
// downstream (dashboards, alerts, analytics) is unaware the site is legacy.
package app

import (
	"context"
	"log"
	"time"

	"overwatch/agent/internal/config"
	"overwatch/agent/internal/health"
	"overwatch/agent/internal/legacy/lasertag"
	"overwatch/agent/internal/legacy/nexus"
	"overwatch/agent/internal/legacy/translate"
	"overwatch/agent/internal/packir"
	"overwatch/agent/internal/push"
)

// nexusDB is the read-only Nexus query surface the runner needs (an interface
// so tests can substitute a fake).
type nexusDB interface {
	Ping(ctx context.Context) error
	Roster(ctx context.Context) (map[int]string, error)
	FinishedGamesSince(ctx context.Context, afterGameID, limit int) ([]translate.GameMeta, error)
	GameResults(ctx context.Context, gameID int) ([]translate.PlayerAgg, error)
}

// lasertagAPI is the live-state surface the runner needs.
type lasertagAPI interface {
	PacksLive(ctx context.Context) ([]lasertag.PackLive, error)
	Status(ctx context.Context) (lasertag.ServerStatus, error)
}

// pusher delivers payloads to central.
type pusher interface {
	Push(payload []byte, idempotencyKey string) error
	PushGameResults(gameNumber int, data map[string]any) error
}

// App is the legacy-mode runner.
type App struct {
	cfg    config.Config
	db     nexusDB
	lt     lasertagAPI
	push   pusher
	health *health.Health

	seq        int64
	roster     map[int]string
	lastGameID int
}

// New builds a legacy runner from config, opening the Nexus DB and lasertag
// client. Returns an error if the DB can't be opened.
func New(cfg config.Config) (*App, error) {
	db, err := nexus.Open(cfg.NexusDSN)
	if err != nil {
		return nil, err
	}
	return &App{
		cfg:    cfg,
		db:     db,
		lt:     lasertag.New(cfg.LasertagURL),
		push:   push.New(cfg.CentralURL, cfg.Token),
		health: health.New(),
		seq:    time.Now().Unix(),
		roster: map[int]string{},
	}, nil
}

// newWithDeps is the test constructor — inject fakes for the DB, lasertag API
// and pusher.
func newWithDeps(cfg config.Config, db nexusDB, lt lasertagAPI, p pusher) *App {
	return &App{
		cfg: cfg, db: db, lt: lt, push: p,
		health: health.New(),
		seq:    1,
		roster: map[int]string{},
	}
}

// Run drives the poll loops until the context is cancelled.
func (a *App) Run(ctx context.Context) {
	go a.health.Serve(a.cfg.HealthAddr)

	// Optional LAN pack-IR bench listener (independent of the Nexus poll loop).
	if a.cfg.PackIRAddr != "" {
		pir := packir.New(a.cfg.PackIRAddr, a.cfg.Version, push.New(a.cfg.CentralURL, a.cfg.Token), a.cfg.BufferMax, a.cfg.PackIRBufferFile)
		if err := pir.Start(ctx); err != nil {
			log.Printf("[legacy] pack-IR listener failed to start: %v", err)
		} else {
			defer pir.Close()
		}
	}

	if roster, err := a.db.Roster(ctx); err != nil {
		log.Printf("[legacy] roster load failed (aliases will be blank until it recovers): %v", err)
	} else {
		a.roster = roster
	}

	// Game-results sync runs on its own slower cadence alongside live telemetry.
	go a.gameSyncLoop(ctx)

	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			a.pollTelemetry(ctx)
			timer.Reset(a.cfg.PollInterval)
		}
	}
}

// pollTelemetry fetches live pack state + server status and pushes one ingest batch.
func (a *App) pollTelemetry(ctx context.Context) {
	packs, err := a.lt.PacksLive(ctx)
	if err != nil {
		log.Printf("[legacy] packs_live failed: %v", err)
		return
	}
	status, err := a.lt.Status(ctx)
	if err != nil {
		log.Printf("[legacy] status failed: %v", err)
		return
	}

	a.seq++
	payload, key := translate.TelemetryBytes(a.seq, time.Now(), packs, status, a.roster)
	if err := a.push.Push(payload, key); err != nil {
		log.Printf("[legacy] telemetry push failed: %v", err)
		return
	}
	a.health.MarkPoll()
	a.health.MarkPush()
}

// gameSyncLoop periodically pulls finished Nexus games and pushes their results.
func (a *App) gameSyncLoop(ctx context.Context) {
	t := time.NewTicker(a.cfg.GameSyncInterval)
	defer t.Stop()
	a.syncGames(ctx) // once at startup
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			a.syncGames(ctx)
		}
	}
}

// syncGames pushes every finished game newer than the last one synced. Central
// dedupes by game number, so re-pushing is harmless; lastGameID just bounds the
// query.
func (a *App) syncGames(ctx context.Context) {
	games, err := a.db.FinishedGamesSince(ctx, a.lastGameID, 100)
	if err != nil {
		log.Printf("[legacy] game list failed: %v", err)
		return
	}
	for _, g := range games {
		players, err := a.db.GameResults(ctx, g.GameNumber)
		if err != nil {
			log.Printf("[legacy] game #%d results failed: %v", g.GameNumber, err)
			return // stop; retry from here next tick (don't advance past a failure)
		}
		payload := translate.GameResultsPayload(g, players)
		data, _ := payload["data"].(map[string]any)
		if err := a.push.PushGameResults(g.GameNumber, data); err != nil {
			log.Printf("[legacy] game #%d push failed: %v", g.GameNumber, err)
			return
		}
		a.lastGameID = g.GameNumber
		log.Printf("[legacy] synced game #%d (%d players)", g.GameNumber, len(players))
	}
}
