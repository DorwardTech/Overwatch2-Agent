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

The agent exits immediately if `CENTRAL_API_URL` or `AGENT_TOKEN` is missing.

## Verify the token + endpoint

```bash
curl -i -X POST "$CENTRAL_API_URL" \
  -H "Content-Type: application/json" -H "X-Agent-Token: $AGENT_TOKEN" \
  -d '{"push_seq":1,"server_state":{"GAMENUM":1},"packs":[{"ID":1,"STATE":6,"CONNECTED":true}]}'
# expect: {"status":"ok","accepted":1}
```

## Health

The agent serves a health endpoint on `HEALTH_ADDR` and the binary supports a
`healthcheck` subcommand (used by the compose healthcheck):

```bash
docker compose exec agent /agent healthcheck
```

## Build from source

```bash
go build -o overwatch-agent ./cmd/agent     # needs Go 1.24+
```

## Layout

```
cmd/agent/main.go        entrypoint + healthcheck subcommand
internal/
  config/   env parsing + validation
  ozone/    WebSocket client (GETSERVERSTATE, GETACTIVEPACKS, GETTEAMINFO, …)
  buffer/   bounded FIFO for offline batches
  push/     HTTPS client (token auth, retries)
  health/   health endpoint
  app/      main loop, signals, graceful shutdown
```
