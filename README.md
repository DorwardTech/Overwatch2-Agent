# Overwatch Site Agent

Unattended per-venue daemon for [Overwatch](https://ow2.lasertag.net.au). One
agent runs per venue. It has two jobs:

1. **Telemetry** — connect to the local O-Zone server's WebSocket API (TCP
   `12113`, read-only), batch server/pack telemetry, and push it to the central
   Overwatch server over HTTPS with a per-site token. It buffers in memory when
   central is unreachable and replays on reconnect.
2. **Print-server cache & proxy** *(opt-in)* — pull finished games from O-Zone's
   print server **only while the system is idle**, cache them byte-for-byte, and
   serve them to TORN/printers from a local proxy on TCP `12123`. This means
   scoring/printing software can keep reading game data **without ever hitting
   O-Zone's print server during a live game** (which lags the game).

The agent needs outbound HTTPS to the central server and LAN access to the
O-Zone server. The only inbound port is the optional print-server proxy
(`12123`), and only when you enable it.

---

## Quick start (Docker)

```bash
cp .env.example .env     # set CENTRAL_API_URL, AGENT_TOKEN, OZONE_WS_HOST
docker compose up -d
docker compose logs -f   # watch it connect + push
```

Or run the prebuilt image directly (telemetry only):

```bash
docker run -d --restart unless-stopped --name overwatch-agent \
  --pull always \
  --add-host host.docker.internal:host-gateway \
  -e CENTRAL_API_URL=https://ow2.lasertag.net.au/api/agent/ingest \
  -e AGENT_TOKEN=OW2_xxx \
  -e OZONE_WS_HOST=192.168.1.50 \
  ghcr.io/dorwardtech/overwatch2-agent:latest
```

The agent exits immediately if `CENTRAL_API_URL` or `AGENT_TOKEN` is missing.
Everything else has a safe default; the cache/proxy/message-bus features are
**off** unless you turn them on.

**Architectures.** The published image is multi-arch — `linux/amd64`,
`linux/arm64`, and `linux/arm/v7` — so the same `:latest` tag runs on x86 hosts,
64-bit ARM boards (Raspberry Pi 3/4/5, ARM servers/VMs, Apple Silicon), and
older 32-bit ARM. Docker pulls the right variant automatically; no per-arch tag
needed.

> **`exec /agent: exec format error`?** You have an old `:latest` cached from
> before the image was multi-arch (an amd64 binary on an ARM host). Docker won't
> re-pull a tag it already has, so force a fresh pull and recreate the container:
> `docker pull ghcr.io/dorwardtech/overwatch2-agent:latest` (or run with
> `--pull always`; with Compose, `docker compose pull`). Verify with
> `docker image inspect … --format '{{.Architecture}}'` — it should match
> `uname -m` (`aarch64`→`arm64`, `armv7l`→`arm/v7`).

---

## Configuration

All configuration is via environment variables. "Default" is the built-in value
used when the variable is unset (the shipped `.env.example` overrides some of
these — noted where relevant).

### Required

| Variable | Default | Description |
|---|---|---|
| `CENTRAL_API_URL` | — | Full ingest endpoint, e.g. `https://ow2.lasertag.net.au/api/agent/ingest`. Other central endpoints are derived from it. |
| `AGENT_TOKEN` | — | This venue's token (`OW2_<id>_<secret>`). Contact Tr1cky. |

### O-Zone connection

| Variable | Default | Description |
|---|---|---|
| `OZONE_WS_HOST` | `127.0.0.1` | O-Zone server host (LAN IP, or `host.docker.internal` if O-Zone runs on the Docker host). Shared by all O-Zone ports below. |
| `OZONE_WS_PORT` | `12113` | O-Zone WebSocket read API port (telemetry). |
| `OZONE_RESULTS_PORT` | `12123` | O-Zone print-server (results) port. |
| `OZONE_MSG_BUS_PORT` | `12111` | O-Zone External Message Bus port (game lifecycle events). |

### Polling & tuning

| Variable | Default | Description |
|---|---|---|
| `POLL_INTERVAL` | `5` | Seconds between fast polls **while a game is active** (server state + active packs). |
| `IDLE_POLL_INTERVAL` | `15` | Seconds between polls **when idle**. Keep it **under central's agent-offline window (30s)** or the agent will flap offline between idle pushes. |
| `SLOW_POLL_INTERVAL` | `60` | Cadence (s) for metadata (teams, games, licences), command checks, and the agent self-report. |
| `BUFFER_MAX` | `2000` | Max telemetry batches buffered in memory while central is unreachable (oldest dropped past this). |
| `AGENT_VERSION` | build value | Overrides the reported agent version string (normally leave unset). |

### Post-game results → central (optional)

| Variable | Default | Description |
|---|---|---|
| `ENABLE_GAME_RESULTS` | `false` | Fetch each finished game's hit-zone + fitness data from the print server and POST it to central `/api/agent/game-results`. Requires the O-Zone scoresheet-printing licence. *(The shipped `.env.example` sets this `true`.)* |
| `OZONE_RESULTS_HANDSHAKE` | `2` | Number of handshake frames (texts + event_types) O-Zone pushes on connect that the agent drains before requesting a game. Only change if your O-Zone firmware differs. |

### Two-way command queue (optional)

| Variable | Default | Description |
|---|---|---|
| `ENABLE_COMMANDS` | `true` | Agent polls central for queued commands (e.g. re-fetch a game, resync) and acknowledges outcomes. Commands that touch the print server are **deferred** while a game is active. |

### Print-server cache & transparent proxy (opt-in)

See [How the cache works](#how-the-print-server-cache--proxy-works) below.

| Variable | Default | Description |
|---|---|---|
| `ENABLE_CACHE` | `false` | Maintain the local, byte-for-byte cache of print-server games. |
| `ENABLE_PROXY` | `false` | Serve cached games to TORN/printers over the O-Zone print-server TCP protocol. |
| `PROXY_LISTEN_ADDR` | `0.0.0.0:12123` | Bind address for the proxy. **LAN-bound only — never port-forward it to the public internet** (the TCP protocol is unauthenticated, exactly like O-Zone). |
| `CACHE_DIR` | `./cache` | On-disk cache location. In Docker this is `/data/cache` on a writable volume. |
| `CACHE_RETENTION_HOURS` | `24` | Prune cached games older than this (checked hourly). |

### Game-state detection / idle-gating

| Variable | Default | Description |
|---|---|---|
| `ENABLE_MSG_BUS` | `false` | Consume O-Zone's External Message Bus (`:12111`) for precise, event-driven game start/finish. The WS `SERVERMODE` allowlist is always used as a backstop. |
| `GAME_FINISH_DELAY` | `15` | Seconds to wait after a game finishes before fetching its data (lets O-Zone finish writing the game). |

### Central failover (optional)

| Variable | Default | Description |
|---|---|---|
| `ENABLE_CENTRAL_FAILOVER` | `true` | Back each cached game up to central (verbatim) and restore the cache from central on a cold start with an empty local cache. Has effect only when the cache is enabled. |

### Admin / control API (optional)

| Variable | Default | Description |
|---|---|---|
| `ADMIN_API_ADDR` | — | Bind address for the token-protected HTTP admin API (e.g. `:8090`). Empty disables it. |
| `ADMIN_API_TOKEN` | — | Bearer token required by the admin API. The API only starts when **both** address and token are set. |

### Health

| Variable | Default | Description |
|---|---|---|
| `HEALTH_ADDR` | `:8088` | Bind address for `/healthz` (liveness) and `/readyz` (readiness). |

---

## How the print-server cache & proxy works

O-Zone's print server has a hard limitation: a large print-server request while a
competition game is in progress makes the **game lag**. The agent works around it
by **only talking to the print server when the system is idle**, caching the
results, and serving scoring/printing software (TORN) from that cache instead.

```
                          ┌────────────── venue LAN ──────────────┐
   TORN / printers ──TCP 12123──▶  Overwatch Agent  ──TCP 12123──▶  O-Zone print server
                                   (serves from cache)   (only when idle)
                                        │  ▲
                          WS 12113 ─────┘  └───── Message Bus 12111 (game start/finish)
                                        │
                                   HTTPS ▼  (telemetry + verbatim game backup)
                                   Overwatch central
```

- **Transparent.** The proxy reproduces O-Zone's exact wire protocol (5-byte
  `0x28` header + JSON, the connect banner, and the `list` / `all` / `minimal` /
  `team` / `player` / `autoprint` commands). `all` is replayed **byte-for-byte**,
  so a consumer cannot tell it is talking to the cache instead of O-Zone.
- **Idle-gated.** The agent connects to the print server only when **both** the
  Message Bus game state **and** the WS `SERVERMODE` read idle. There is no code
  path that contacts the print server while a game is active.
- **Persistent + pruned.** Games are stored under `CACHE_DIR` and pruned after
  `CACHE_RETENTION_HOURS`. With failover on, they are also backed up to central
  and restored automatically after a rebuild.

### Enabling it

```bash
# In .env (and point TORN's game server at this agent's LAN IP:12123)
ENABLE_CACHE=true
ENABLE_PROXY=true
ENABLE_MSG_BUS=true
PROXY_LISTEN_ADDR=0.0.0.0:12123
```

> **Cache volume permissions.** The container runs as a non-root user (UID
> `65532`), so `CACHE_DIR` must be writable by that user — otherwise the agent
> logs `cache disabled: … permission denied` (and, since the proxy serves from
> the cache, the proxy won't start either). A volume created from a recent image
> inherits the right ownership automatically. For an **existing** volume or a
> **bind mount**, chown it once:
>
> ```bash
> # named volume (replace with your volume name from `docker volume ls`)
> docker run --rm -v overwatch-agent_agent-cache:/data alpine chown -R 65532:65532 /data
> # bind mount
> sudo chown -R 65532:65532 /path/to/cache
> ```

---

## Admin API

When `ADMIN_API_ADDR` + `ADMIN_API_TOKEN` are set, every request needs
`Authorization: Bearer <token>`:

| Method | Path | Description |
|---|---|---|
| `GET` | `/api/overview` | Cache + game-state summary (`games`, `state`, `server_mode`, `msg_bus_connected`). |
| `GET` | `/api/games` | Cached game metadata. |
| `GET` | `/api/games/{n}` | One game's verbatim payload. |
| `POST` | `/api/resync` | Trigger an idle-gated cache refresh. |
| `POST` | `/api/purge` | Drop all cached games. |

```bash
curl -s -H "Authorization: Bearer $ADMIN_API_TOKEN" http://localhost:8090/api/overview
```

---

## Network & ports

| Direction | Port | Purpose | When |
|---|---|---|---|
| Outbound | `443` | HTTPS to central (telemetry, results, commands, failover) | always |
| Outbound | `12113/tcp` | O-Zone WebSocket read API | always |
| Outbound | `12123/tcp` | O-Zone print server | when cache/results enabled (idle only) |
| Outbound | `12111/tcp` | O-Zone External Message Bus | when `ENABLE_MSG_BUS=true` |
| Inbound | `12123/tcp` | Print-server proxy for TORN | when `ENABLE_PROXY=true` (LAN only) |
| Inbound | `8088/tcp` | Health (`/healthz`, `/readyz`) | always |
| Inbound | `8090/tcp` | Admin API | when `ADMIN_API_ADDR` set |

---

## Verify the token + endpoint

```bash
curl -i -X POST "$CENTRAL_API_URL" \
  -H "Content-Type: application/json" -H "X-Agent-Token: $AGENT_TOKEN" \
  -d '{"push_seq":1,"server_state":{"GAMENUM":1},"packs":[{"ID":1,"STATE":6,"CONNECTED":true}]}'
# expect: {"status":"ok","accepted":1}
```

## Health

```bash
docker compose exec agent /agent healthcheck   # used by the compose healthcheck
curl -s localhost:8088/healthz                 # liveness
curl -s localhost:8088/readyz                  # readiness (O-Zone reachable + buffer ok)
```

## Build from source

```bash
go build -o overwatch-agent ./cmd/agent      # needs Go 1.24+
```

## Layout

```
cmd/
  agent/       entrypoint + healthcheck subcommand
internal/
  config/      env parsing + validation
  ozone/       O-Zone WebSocket client (GETSERVERSTATE, GETACTIVEPACKS, …)
  msgbus/      O-Zone External Message Bus client (game lifecycle)
  results/     O-Zone print-server client (verbatim game fetch)
  ozoneproto/  print-server binary framing (0x28 protocol)
  store/       on-disk verbatim game cache + retention
  cache/       O-Zone-shaped views over the store (list/all/minimal/team/player)
  proxy/       transparent print-server TCP proxy (what TORN connects to)
  adminapi/    token-protected control/observability API
  buffer/      bounded FIFO for offline telemetry batches
  push/        HTTPS client to central (telemetry, results, commands, failover)
  health/      health endpoint
  app/         main loop, game-state machine, idle-gating, graceful shutdown
```

## License

This source is **source-available, not open-source**: published for reference and
transparency, with all rights reserved. See [LICENSE](LICENSE). Authorised
operators may deploy and run the agent to connect to the Overwatch service under
their agreement; for any other use, contact the maintainer.


