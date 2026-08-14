package config

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"time"

	"overwatch/agent/internal/version"
)

// Config is the agent's runtime configuration, loaded from the environment.
type Config struct {
	OzoneHost        string
	OzonePort        string
	OzoneResultsPort string
	ResultsEnabled   bool
	ResultsHandshake int
	CommandsEnabled  bool
	Version          string
	CentralURL       string
	Token            string
	PollInterval     time.Duration
	IdlePollInterval time.Duration
	SlowPollInterval time.Duration
	HealthAddr       string
	BufferMax        int
	BufferFile       string // spill unsent telemetry here across restarts ("" disables)

	// Pack IR bench listener: an on-prem calibration node POSTs emitter-strength
	// readings here over the LAN and the agent forwards them to central. Bind
	// LAN-only and never port-forward it (unauthenticated, like the proxy).
	PackIRAddr       string // pack-IR listener bind ("" disables)
	PackIRBufferFile string // spill unsent IR readings here across restarts ("" disables)

	// Mode selects the data source. "ozone" (default) polls the O-Zone WS/print
	// server; "legacy" polls a P&C Micros Nexus site via its MySQL DB + on-box
	// management app.
	Mode             string
	NexusDSN         string        // go-sql-driver DSN for the Nexus ng_system DB (legacy mode)
	LasertagURL      string        // base URL of the on-box management app (legacy mode)
	GameSyncInterval time.Duration // how often to pull finished Nexus games

	// Print-server cache + idle-gating (ported from the Lattice ozone-agent).
	MsgBusEnabled   bool          // consume the External Message Bus for game state
	OzoneMsgBusPort string        // O-Zone External Message Bus port
	CacheEnabled    bool          // maintain the local verbatim game cache
	CacheDir        string        // on-disk cache location
	CacheRetention  time.Duration // prune cached games older than this
	GameFinishDelay time.Duration // wait after game-finish before touching the print server

	// Transparent print-server proxy (what TORN connects to instead of O-Zone)
	// and the token-protected admin/control HTTP API.
	ProxyEnabled    bool   // serve the cached games over the O-Zone TCP protocol
	ProxyListenAddr string // proxy bind address (LAN-bound; never expose publicly)
	AdminAddr       string // admin API bind address ("" disables it)
	AdminToken      string // bearer token required by the admin API

	// PublicDocsBase is the base URL of the Overwatch-hosted public docs. The
	// control panel links to <base>/agent/changelog and <base>/agent/api.
	PublicDocsBase string

	FailoverEnabled bool // back up cached games to central, and restore on cold start
}

// Load reads and validates configuration. CentralURL and Token are required.
func Load() (Config, error) {
	c := Config{
		OzoneHost:        env("OZONE_WS_HOST", "127.0.0.1"),
		OzonePort:        env("OZONE_WS_PORT", "12113"),
		OzoneResultsPort: env("OZONE_RESULTS_PORT", "12123"),
		ResultsEnabled:   envBool("ENABLE_GAME_RESULTS", false),
		ResultsHandshake: envInt("OZONE_RESULTS_HANDSHAKE", 2),
		CommandsEnabled:  envBool("ENABLE_COMMANDS", true),
		Version:          env("AGENT_VERSION", version.Value),
		CentralURL:       os.Getenv("CENTRAL_API_URL"),
		Token:            os.Getenv("AGENT_TOKEN"),
		PollInterval:     time.Duration(envInt("POLL_INTERVAL", 5)) * time.Second,
		IdlePollInterval: time.Duration(envInt("IDLE_POLL_INTERVAL", 15)) * time.Second,
		SlowPollInterval: time.Duration(envInt("SLOW_POLL_INTERVAL", 60)) * time.Second,
		HealthAddr:       env("HEALTH_ADDR", ":8088"),
		BufferMax:        envInt("BUFFER_MAX", 2000),
		BufferFile:       env("BUFFER_FILE", ""),

		PackIRAddr:       env("PACK_IR_ADDR", ""),
		PackIRBufferFile: env("PACK_IR_BUFFER_FILE", ""),

		Mode:             env("AGENT_MODE", "ozone"),
		NexusDSN:         os.Getenv("NEXUS_DSN"),
		LasertagURL:      env("LASERTAG_URL", ""),
		GameSyncInterval: time.Duration(envInt("GAME_SYNC_INTERVAL", 30)) * time.Second,

		// Opt-in per venue: enabling these makes the agent connect to O-Zone's
		// message bus / print server. Off by default so existing fleet agents
		// keep their current behaviour until a venue is rolled onto the cache.
		MsgBusEnabled:   envBool("ENABLE_MSG_BUS", false),
		OzoneMsgBusPort: env("OZONE_MSG_BUS_PORT", "12111"),
		CacheEnabled:    envBool("ENABLE_CACHE", false),
		CacheDir:        env("CACHE_DIR", "./cache"),
		CacheRetention:  time.Duration(envInt("CACHE_RETENTION_HOURS", 24)) * time.Hour,
		GameFinishDelay: time.Duration(envInt("GAME_FINISH_DELAY", 15)) * time.Second,

		ProxyEnabled:    envBool("ENABLE_PROXY", false),
		ProxyListenAddr: env("PROXY_LISTEN_ADDR", "0.0.0.0:12123"),
		AdminAddr:       env("ADMIN_API_ADDR", ""),
		AdminToken:      os.Getenv("ADMIN_API_TOKEN"),
		PublicDocsBase:  env("PUBLIC_DOCS_BASE", "https://ow2.lasertag.net.au"),

		FailoverEnabled: envBool("ENABLE_CENTRAL_FAILOVER", true),
	}

	if c.CentralURL == "" {
		return c, fmt.Errorf("CENTRAL_API_URL is required")
	}
	if c.Token == "" {
		return c, fmt.Errorf("AGENT_TOKEN is required")
	}
	if c.Mode == "legacy" {
		if c.NexusDSN == "" {
			return c, fmt.Errorf("NEXUS_DSN is required in legacy mode")
		}
		if c.LasertagURL == "" {
			return c, fmt.Errorf("LASERTAG_URL is required in legacy mode")
		}
	}
	// Central marks an agent offline after ~30s without a push; an idle poll at
	// or above that flaps the site "offline" between pushes.
	if c.IdlePollInterval >= 30*time.Second {
		log.Printf("[agent] config: IDLE_POLL_INTERVAL=%s is at/above central's 30s offline window — the site will flap offline", c.IdlePollInterval)
	}
	if c.PollInterval < time.Second {
		log.Printf("[agent] config: POLL_INTERVAL below 1s clamped to 1s")
		c.PollInterval = time.Second
	}
	return c, nil
}

// envBool parses k as a bool, warning (rather than silently defaulting) on a
// malformed value so a typo'd flag doesn't go unnoticed for weeks.
func envBool(k string, def bool) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		log.Printf("[agent] config: %s=%q is not a boolean — using default %v", k, v, def)
		return def
	}
	return b
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// envInt parses k as an integer, warning on a malformed value (e.g. "5s"
// where plain seconds are expected).
func envInt(k string, def int) int {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		log.Printf("[agent] config: %s=%q is not an integer — using default %d", k, v, def)
		return def
	}
	return n
}
