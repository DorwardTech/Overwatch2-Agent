# Changelog — Overwatch Site Agent

All notable changes to the Overwatch Site Agent are recorded here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The Site Agent and central Overwatch are versioned independently.

## [Unreleased]

### Fixed

- Restore the central-dispatched cache commands (status refresh, cache resync,
  cache purge) that had been dropped in an earlier refactor, so operating an
  agent from Overwatch works again.
- Published container images now report the real release version instead of
  `dev`: the Docker build defaults the version to the current release and the
  release workflow stamps the git tag's version onto tagged builds.

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
