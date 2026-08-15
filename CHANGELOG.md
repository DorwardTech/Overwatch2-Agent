# Changelog — Overwatch Site Agent

All notable changes to the Overwatch Site Agent are recorded here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The Site Agent and central Overwatch are versioned independently.

## [1.1.1] — 2026-08-15

### Fixed

- **The standalone deployment kept no persistent storage**, so every restart
  was a cold start. `docker-compose.yml` declared no volume at all while the
  container filesystem is read-only, which meant the game cache and the
  unsent-telemetry spill had nowhere to live. Two consequences at venues: any
  telemetry buffered during an outage was silently discarded on the next
  restart or upgrade, and the agent re-downloaded its **entire** game cache
  from Overwatch on every redeploy — hundreds of requests that exhaust the
  per-site rate limit and stall live telemetry for as long as it takes to
  finish. The compose file now mounts a persistent volume and pins the cache
  and buffer paths to it, matching the deployment the fleet has been using.
- The reported version is correct again. Bumping it in the source was not
  enough: every image build stamps the version over the top, and the build
  paths still carried the previous release, so agents kept reporting 1.0.2
  after the 1.1.0 release. All build paths now agree, and CI fails if they
  ever drift from the source again.

## [1.1.0] — 2026-07-14

### Added

- **Pack IR bench listener + forwarder.** A new LAN-only, unauthenticated
  endpoint `POST /local/pack-ir` accepts IR emitter-strength readings from an
  on-prem calibration node and forwards them to central, buffering across
  outages (with disk spill across restarts) exactly like telemetry. Malformed
  readings are rejected at the edge so a bad reading can't poison the forward
  queue; pack identity is carried explicitly in the payload. Enabled with
  `PACK_IR_ADDR`; runs in both agent modes as an independent local telemetry
  source, separate from the print-server path, and its status appears in the
  admin overview.

## [1.0.2] — 2026-07-12

### Fixed

- Defensively bound an outgoing print-server frame's payload to `MaxPayload`, so
  the length always fits the 32-bit header field (parity with central).

## [1.0.1] — 2026-07-12

### Fixed

- Restore the central-dispatched cache commands (status refresh, cache resync,
  cache purge) that had been dropped in an earlier refactor, so operating an
  agent from Overwatch works again.
- The agent no longer reports its version as `dev`. Every build path now stamps
  the real release version — the standalone and monorepo Docker builds default
  to it, both publish workflows stamp a clean SemVer, and an `AGENT_VERSION`
  environment variable overrides the reported version on an already-built image
  without a rebuild.

## [1.0.0] — 2026-07-11

Initial release.

### Added

- Live telemetry streaming to central, with offline buffering and replay so a
  connectivity outage never loses data or disrupts the venue.
- Idle-gated collection of completed games — the agent reads finished games only
  when the game server is idle, governed by a dual-signal safety gate, so it
  never queries the game server during a live game.
- A local game cache that serves completed game data to scoring software
  instantly, so scoring reads from the agent instead of the live game server.
- Central failover backup of every cached game, and automatic restore on a cold
  start.
- **Collect from Overwatch** — on-demand, timeframe-scoped pull of games from
  central into the local cache (skips games already cached; safe during a live
  game).
- A token-protected admin API and a browser **control panel** (status, cached
  games, cache resync, cache purge, collect), with links to this changelog and
  the public API reference.
- Multi-architecture container image — `linux/amd64`, `linux/arm64`,
  `linux/arm/v7` — so it runs on standard PCs and ARM boards from one image tag.
