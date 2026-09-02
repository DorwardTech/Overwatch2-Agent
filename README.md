# Overwatch Site Agent

Unattended per-venue daemon for [Overwatch](https://ow2.lasertag.net.au). It
connects to the local O-Zone server's WebSocket API (TCP `12113`, read-only),
batches telemetry, and pushes it to the central Overwatch server over HTTPS with
a per-site token. It buffers in memory when central is unreachable and replays on
reconnect.

One agent runs per venue. It needs outbound HTTPS to the central server and LAN
access to the O-Zone server — no inbound ports.

> **Running a legacy (Nexus) venue?** Set `AGENT_MODE=legacy` and see
> [LEGACY.md](LEGACY.md) — the same agent reads games from the Nexus MySQL
> database and live pack state from the on-box lasertag app instead of O-Zone.

## Quick start (Docker)

```bash
cp .env.example .env     # set CENTRAL_API_URL, AGENT_TOKEN, OZONE_WS_HOST
docker compose up -d
docker compose logs -f   # watch it connect + push
```

Or run the prebuilt image directly:

```bash
docker run -d --restart unless-stopped --name overwatch-agent \
  --add-host host.docker.internal:host-gateway \
  -e CENTRAL_API_URL=https://ow2.lasertag.net.au/api/agent/ingest \
  -e AGENT_TOKEN=OW2_xxx \
  -e OZONE_WS_HOST=192.168.1.50 \
  ghcr.io/OWNER/overwatch-agent:latest
```

## Windows (no Docker)

On Windows the agent runs as a native **Windows service** from one executable —
no Docker. Download the `windows_amd64` archive from the
[releases](https://github.com/DorwardTech/Overwatch2-Agent/releases), extract it
to `C:\Program Files\Overwatch Agent\`, then from an elevated PowerShell:

```powershell
.\overwatch-agent.exe install                          # registers the service, writes agent.env
notepad "C:\ProgramData\Overwatch Agent\agent.env"     # set the URL, token and game-server host
.\overwatch-agent.exe start
```

[WINDOWS.md](WINDOWS.md) is the full guide: where files live, the firewall,
running on the game server PC itself, upgrading and troubleshooting.

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `CENTRAL_API_URL` | ✅ | — | Full ingest endpoint, e.g. `https://ow2.lasertag.net.au/api/agent/ingest` |
| `AGENT_TOKEN` | ✅ | — | This venue's token (`OW2_<id>_<secret>`), issued from the Sites screen |
| `OZONE_WS_HOST` | | `127.0.0.1` | O-Zone server host. Use `host.docker.internal` if it runs on the Docker host |
| `OZONE_WS_PORT` | | `12113` | O-Zone WebSocket port |
| `POLL_INTERVAL` | | `5` | Seconds between fast polls (server state + active packs) |
| `SLOW_POLL_INTERVAL` | | `60` | Seconds between slow polls (teams, games, licences) |
| `BUFFER_MAX` | | `2000` | Max telemetry batches buffered while central is unreachable |
| `HEALTH_ADDR` | | `:8088` | Bind address for the health endpoint |
| `LOG_FILE` | | — | Also write the log to this file, rotated at 10 MB with five kept. Windows default: `<data>\logs\agent.log` |
| `AGENT_DATA_DIR` | | — | Data directory the cache, spill files, log and `agent.env` default to. Windows default: `%ProgramData%\Overwatch Agent` |
| `AGENT_ENV_FILE` | | — | `KEY=VALUE` file applied at startup (or `--config PATH`); the environment takes precedence over it |

The agent exits immediately if `CENTRAL_API_URL` or `AGENT_TOKEN` is missing.
Settings can also come from a `KEY=VALUE` file — the same lines as
`.env.example` — given as `--config PATH`, as `AGENT_ENV_FILE`, or found as
`agent.env` in the data directory; anything already set in the environment wins.

## Verify the token + endpoint

```bash
curl -i -X POST "$CENTRAL_API_URL" \
  -H "Content-Type: application/json" -H "X-Agent-Token: $AGENT_TOKEN" \
  -d '{"push_seq":1,"server_state":{"GAMENUM":1},"packs":[{"ID":1,"STATE":6,"CONNECTED":true}]}'
# expect: {"status":"ok","accepted":1}
```

## Control panel

When the cache/proxy is enabled, set `ADMIN_API_ADDR` (e.g. `0.0.0.0:8097`) and
`ADMIN_API_TOKEN` to expose a small browser control panel plus a JSON API. Open
`http://<agent-host>:8097/` in a browser, enter the admin token, and you get
buttons to view the agent's status, list cached games (and their raw payloads),
trigger an idle-gated **resync**, or **purge** the cache — no curl needed.

The page itself holds no secret; the token you type is verified against the API
and sent as a bearer header on each action. Keep the admin port on the **venue
LAN only** — never expose it publicly. The JSON API is also available directly:

| Method | Path | |
|---|---|---|
| `GET` | `/api/overview` | status snapshot |
| `GET` | `/api/games` | cached game metadata |
| `GET` | `/api/games/{n}` | verbatim O-Zone payload for game `n` |
| `POST` | `/api/resync` | idle-gated cache refresh |
| `POST` | `/api/purge` | drop all cached games |
| `POST` | `/api/collect` | pull games from central into the cache (`?from=&to=`, optional) |

All `/api/*` calls require `Authorization: Bearer <ADMIN_API_TOKEN>`.

## Health

The agent serves a health endpoint on `HEALTH_ADDR` and the binary supports a
`healthcheck` subcommand (used by the compose healthcheck):

```bash
docker compose exec agent /agent healthcheck
```

## Build from source

```bash
go build -o overwatch-agent ./cmd/agent                               # needs Go 1.25+
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o overwatch-agent.exe ./cmd/agent   # Windows
```

## Layout

```
cmd/agent/main.go        entrypoint, healthcheck + Windows service commands
internal/
  config/   env parsing + validation, KEY=VALUE config file
  platform/ per-OS data directory + path defaults
  logfile/  size-rotated log file
  ozone/    WebSocket client (GETSERVERSTATE, GETACTIVEPACKS, GETTEAMINFO, …)
  buffer/   bounded FIFO for offline batches
  push/     HTTPS client (token auth, retries)
  health/   health endpoint
  app/      main loop, signals, graceful shutdown
```
