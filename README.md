# Overwatch Site Agent

Unattended per-venue daemon for [Overwatch](https://ow2.lasertag.net.au). It
connects to the local O-Zone server's WebSocket API (TCP `12113`, read-only),
batches telemetry, and pushes it to the central Overwatch server over HTTPS with
a per-site token. It buffers in memory when central is unreachable and replays on
reconnect.

One agent runs per venue. It needs outbound HTTPS to the central server and LAN
access to the O-Zone server — no inbound ports, except UDP `12106` for auto
discovery and the report-only listener (see
[Finding O-Zone automatically](#finding-o-zone-automatically)).

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

Or let it find O-Zone by itself. That needs host networking, because the
announcement it listens for is a LAN broadcast that never reaches a bridge
network. Every listener then binds the host directly, so keep the health
endpoint on loopback:

```bash
docker run -d --restart unless-stopped --name overwatch-agent \
  --network host \
  -e CENTRAL_API_URL=https://ow2.lasertag.net.au/api/agent/ingest \
  -e AGENT_TOKEN=OW2_xxx \
  -e OZONE_WS_HOST=auto -e OZONE_SID=4242 -e OZONE_ARENA=1 \
  -e HEALTH_ADDR=127.0.0.1:8088 \
  ghcr.io/OWNER/overwatch-agent:latest
```

With the print-server proxy or the control panel on, bind them to the venue LAN
address too (`PROXY_LISTEN_ADDR=<LAN-IP>:12123`, `ADMIN_API_ADDR=<LAN-IP>:8097`);
ports `12123`, `8088` and `8097` must be free on the host.

## Windows (no Docker)

On Windows the agent runs as a native **Windows service** from one executable —
no Docker. Download `overwatch-agent_<version>_windows_setup.exe` from the
[releases](https://github.com/DorwardTech/Overwatch2-Agent/releases) and run it.
One installer covers x64 and Arm64; it puts the agent in *Program Files*,
registers the service, and offers to open the setup page when it finishes.

That page is a browser form for the venue's settings, with a **Test this
connection** button for Overwatch and for the game server, and a **Save and
start** button. No file editing, and no reading a log to find out whether the
token was right. (It is on the Start Menu as **Overwatch Agent Setup**, and
`overwatch-agent setup` opens the same page from an elevated prompt.)

The per-architecture `.zip` archives are still published for scripted rollouts;
[WINDOWS.md](WINDOWS.md) covers installing from one, and the installer's silent
switches.

[WINDOWS.md](WINDOWS.md) is the full guide: where files live, the firewall,
running on the game server PC itself, upgrading and troubleshooting.

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `CENTRAL_API_URL` | ✅ | — | Full ingest endpoint, e.g. `https://ow2.lasertag.net.au/api/agent/ingest` |
| `AGENT_TOKEN` | ✅ | — | This venue's token (`OW2_<id>_<secret>`), issued from the Sites screen |
| `AGENT_MODE` | | `ozone` | Which system this venue runs: `ozone`, or `legacy` for a Nexus venue |
| `OZONE_WS_HOST` | | `127.0.0.1` | O-Zone server host (ozone mode). Use `host.docker.internal` if it runs on the Docker host, or `auto` to find it from its LAN announcement (see [Finding O-Zone automatically](#finding-o-zone-automatically)) |
| `OZONE_WS_PORT` | | `12113` | O-Zone WebSocket port (ozone mode) |
| `OZONE_SID` | | — | Auto mode only: use only the server announcing this site id (1–10 digits, compared as a number). Ignored, with a log line, with a fixed host |
| `OZONE_ARENA` | | — | Auto mode only: use only this arena (0–255), compared exactly as the server announces it. Ignored, with a log line, with a fixed host |
| `OZONE_LIB_PORT` | | `12106` | UDP port the announcement arrives on (advanced) |
| `ENABLE_LIB_LISTENER` | | `true` | With a fixed `OZONE_WS_HOST`: listen for the announcement and report what is heard, without ever changing the host. `false` opens no socket. Auto mode always listens |
| `NEXUS_DSN` | legacy | — | Nexus database, `user:password@tcp(host:3306)/ng_system`. Read-only account |
| `LASERTAG_URL` | legacy | — | On-box management app, at the folder it lives in — no trailing slash |
| `GAME_SYNC_INTERVAL` | | `30` | Seconds between collections of finished Nexus games (legacy mode) |
| `POLL_INTERVAL` | | `5` | Seconds between fast polls (server state + active packs) |
| `SLOW_POLL_INTERVAL` | | `60` | Seconds between slow polls (teams, games, licences) |
| `BUFFER_MAX` | | `2000` | Max telemetry batches buffered while central is unreachable |
| `HEALTH_ADDR` | | `:8088` | Bind address for the health endpoint |
| `LOG_FILE` | | — | Also write the log to this file, rotated at 10 MB with five kept. Windows default: `<data>\logs\agent.log` |
| `AGENT_DATA_DIR` | | — | Data directory the cache, spill files, log and `agent.env` default to. Windows default: `%ProgramData%\Overwatch Agent` |
| `AGENT_ENV_FILE` | | — | `KEY=VALUE` file applied at startup (or `--config PATH`); the environment takes precedence over it |

The agent exits immediately if `CENTRAL_API_URL` or `AGENT_TOKEN` is missing,
or, in auto mode, if `OZONE_SID` or `OZONE_ARENA` is malformed.
Settings can also come from a `KEY=VALUE` file — the same lines as
`.env.example` — given as `--config PATH`, as `AGENT_ENV_FILE`, or found as
`agent.env` in the data directory; anything already set in the environment wins.

## Finding O-Zone automatically

O-Zone announces itself on the venue LAN every five seconds: a small UDP
broadcast on port `12106` carrying the server's address, its site id, its arena
number and its clock. Listening is passive; it puts no load on the server.

**Auto mode.** With `OZONE_WS_HOST=auto` the agent connects to the address the
server announces instead of a configured one:

- It listens for at least 11 seconds (two announcements and a margin) before it
  chooses, so a second server on the LAN is always heard first, and the server
  it chooses must have been the only one heard for 11 seconds in a row.
- It refuses to guess. With more than one site or arena on the LAN, or one site
  and arena announced from two addresses, it waits and logs what it hears. Set
  `OZONE_SID` and `OZONE_ARENA` to narrow the choice to one.
- With neither set and exactly one server heard, it uses that server, pinned
  for this run only (the log says so). Set both to make the choice permanent.
- If nothing is heard it waits for as long as it takes. It never falls back to a
  guessed address; the log says what to check every five minutes, and
  `/readyz` says why it is not ready.
- It chooses once per run. If the server later moves to a new address, the
  agent restarts itself to follow it, but only after its current address has
  been silent for 60 seconds, the new one has been heard at least twice, and the
  new one answers a read-only state request. If the machine at its address
  starts announcing a different site or arena, it restarts and chooses again.
  Every poll checks this first: the poll that notices is dropped, and nothing
  more is sent to central or asked of that machine's print server.
  A restart is the same graceful exit as a **Reboot agent** command, so it
  needs a restart policy (`unless-stopped`, the Windows service's recovery).

**Which announcements are trusted.** One is used only when it was sent from the
address it announces, and that sender is on a subnet this machine is directly
connected to. One from another address is listed but never used; one from beyond
those subnets is only counted.

**Report-only listener.** With a fixed `OZONE_WS_HOST` the agent still listens
by default (`ENABLE_LIB_LISTENER=true`) and includes what it hears in its
self-report: the servers heard, their site id and arena, and the configured
server's clock. It never changes the host. If the port cannot be bound it logs
that and carries on. `ENABLE_LIB_LISTENER=false` opens no socket at all. Two
things to know:

- The agent binds the port for shared use (`SO_REUSEADDR`), on Linux and on
  Windows alike, so another program on the same machine that binds UDP `12106`
  later *without* sharing fails. If something there needs that port, set
  `ENABLE_LIB_LISTENER=false`.
- In a container on a bridge network it hears nothing: it reports listening, with
  no packets. That is harmless.

**Seeing what a machine hears.** `overwatch-agent discover [--seconds N]`
listens for N seconds (default 11, 1–60) and lists every server heard, with its
site id, arena, address, sender, packet count and clock, and whether it would be
used. It only listens: it connects to nothing. It exits 0 when a usable server
was heard, and otherwise 1 with advice. It shares the port with a running agent,
so in a container run it as `docker exec <agent> /agent discover`.

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

`/healthz` answers while the process runs; that is what `healthcheck` probes.
`/readyz` answers `200` once the agent has pushed to central in the last 30
seconds; until then its body says why when there is a specific reason (auto
mode still waiting for an announcement, for example).

**Other programs on the game server.** O-Zone sends every client's replies to
every connection, so while another program uses its WebSocket API the agent
discards messages on every poll: replies to other clients' requests, and
messages that arrived between its own requests. That is normal. The log
explains it once, the first time it happens, and then sums it up at most once
an hour. The counts since the agent started are in every self-report as
`agent.frames`, and in `/api/overview` as `frames`: `foreign` (answered another
request), `stale` (arrived between requests), `overflow` (dropped because too
many were waiting) and `unsettled_lost` (polls that lost a message mid-poll).
Only the last one costs anything: such a poll cannot confirm the server state,
so the print server stays off-limits until a clean poll. When it grows the log
says so in a warning, at most every five minutes.

## Build from source

```bash
go build -o overwatch-agent ./cmd/agent                               # needs Go 1.26+
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -o overwatch-agent.exe ./cmd/agent   # Windows
```

## Layout

```
cmd/agent/main.go        entrypoint, healthcheck + Windows service commands
internal/
  config/   env parsing + validation, KEY=VALUE config file
  platform/ per-OS data directory + path defaults
  logfile/  size-rotated log file
  setupui/  loopback-only configuration page (`agent setup`)
  ozone/    WebSocket client (GETSERVERSTATE, GETACTIVEPACKS, GETTEAMINFO, …)
  ozonelib/ LAN announcement listener (auto discovery, report-only listener)
  buffer/   bounded FIFO for offline batches
  push/     HTTPS client (token auth, retries)
  health/   health endpoint
  app/      main loop, signals, graceful shutdown
```
