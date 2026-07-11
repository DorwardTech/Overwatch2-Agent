# Legacy (Nexus) mode

The agent normally connects to an **O-Zone** server. For venues still running
the older **P&C Micros "Nexus"** platform (NGServerPro), it can run in **legacy
mode** instead: it reads the same telemetry and game results from Nexus's own
data sources and translates them into the exact payloads the central server
already expects, so dashboards, alerts and analytics are unaware the site is
legacy.

Enable it with `AGENT_MODE=legacy` (see `.env.example`).

## Data sources

Nexus has no O-Zone WebSocket/print-server APIs, so the agent uses two
read-only sources:

| Source | Provides |
|---|---|
| **Nexus MySQL** (`ng_system`) | finished games + per-player event log |
| **on-box "lasertag" app** (HTTP) | live pack state: online/playing, resets, timeouts, charge, status |

```
  lasertag app  ──▶ live pack state ──▶ ingest payload  ──▶ /api/agent/ingest
  Nexus MySQL   ──▶ finished games  ──▶ game-results     ──▶ /api/agent/game-results
```

Grant the MySQL account `SELECT` only — the agent never writes to Nexus.

## What works, what doesn't

Anything Nexus records is forwarded. Features that only exist on O-Zone render
an "upgrade to O-Zone" notice in the central UI rather than empty data:

- **Not available on legacy:** per-pack battery / wifi / temperature, fitness
  (steps / distance / calories), the print-server game cache, and O-Zone licence
  expiry. Battery/wifi/temp alerts are suppressed for legacy sites.
- **Works on legacy:** live online/playing state, resets/timeouts, game results
  (per-zone hits dealt/taken, shots, accuracy), the player leaderboard, members
  (logged-in players), and disconnect/offline alerts.

### Game-results detail

Per-zone hit counts are reconstructed from `ng_player_event_log` (event types
0–6 = hits dealt by zone, 14–20 = hits taken by zone). Shots default to
`ng_player_stats.Shots_Fired`, but when the firmware left that 0 the agent
reconstructs them from the trigger-event count (event type 35). Logged-in
members map to the central `omid` (`-1` when no member was signed in).

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `AGENT_MODE` | ✅ | `ozone` | Set to `legacy` to enable Nexus mode |
| `NEXUS_DSN` | ✅ (legacy) | — | Read-only Nexus MySQL DSN, e.g. `ro_user:pass@tcp(host:3306)/ng_system?parseTime=true` |
| `LASERTAG_URL` | ✅ (legacy) | — | Base URL of the on-box lasertag management app |
| `GAME_SYNC_INTERVAL` | | `30` | Seconds between finished-game pulls (central dedupes, so re-pulls are harmless) |
| `POLL_INTERVAL` | | `5` | Seconds between live pack-state polls |

## Running

```bash
cp .env.example .env      # fill in the legacy block
docker compose -f docker-compose.legacy.yml up -d
```

## Tests

`go test ./internal/legacy/...` covers the query layer (via sqlmock) and payload
translation. The Nexus integration tests (`go test -tags=integration
./internal/legacy/nexus/...`) run the real SQL against a database seeded from
production dumps and self-skip unless `NEXUS_TEST_DSN` points at one; those dumps
are not part of this repository.
