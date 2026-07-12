# Changelog — Overwatch Site Agent

All notable changes to the Overwatch Site Agent are recorded here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The Site Agent and central Overwatch are versioned independently.

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
- Defensively bound an outgoing print-server frame's payload to `MaxPayload`, so
  the length always fits the 32-bit header field (parity with central).

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
