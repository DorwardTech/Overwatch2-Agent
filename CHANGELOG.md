# Changelog — Overwatch Site Agent

All notable changes to the Overwatch Site Agent are recorded here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The Site Agent and central Overwatch are versioned independently.

## [1.1.4] — 2026-08-31

### Security

- **The legacy mode printed its database password at startup.** Running against
  a P&C Micro game database, the agent logged its connection string verbatim on
  the first line — user, host, database *and password* — so the credential sat
  in `docker logs` and in anything that collects them. The account is read-only,
  but the database it opens holds member records, and a password nobody meant
  to publish is still a password. The line now shows only the user, host and
  database.
- **The venue LAN could exhaust the agent's memory.** Both listeners the agent
  opens on the venue network are unauthenticated by design — the scoring
  software speaks plain TCP and cannot be changed — so anything on that network
  can connect to them. Three bounds now apply, because the agent has 128 MB and
  being killed for running out of it skips the graceful shutdown that saves
  unsent telemetry:
  - Concurrent connections to the game-data listener are capped (64, far past
    any real use). Past that, connections are refused rather than accepted and
    held; a refused client reconnects, whereas an exhausted agent goes down.
  - A request is now capped at 64 KiB. A message declares its own length before
    sending it, and the listener reserved that much up front — a client could
    declare the 10 MiB protocol maximum, send nothing, and repeat until the
    agent died. Requests are a few dozen bytes; the large limit exists for the
    replies.
  - A request that stops part-way through now times out. Waiting for a client
    to *start* a request is still unlimited, because scoring software holds its
    connection open and idle between games.
- **A single oversized message from the game server could end the agent.** The
  websocket connection had no message-size limit, so one huge frame — from a
  compromised or impersonated game server — was read into memory whole. Capped
  at 10 MiB, matching the limit already applied elsewhere.
- **The unsent-telemetry queue is now bounded by size as well as count.** A
  limit of 2000 entries is not a memory limit, because it says nothing about how
  large an entry is, and the pack-emitter listener accepts entries from anywhere
  on the venue LAN. At the maximum accepted size that was 128 MB of queue inside
  a 128 MB container. The queue now also stops at 32 MB, dropping oldest first
  exactly as it already did when full.

## [1.1.3] — 2026-08-30

### Fixed

- **A completed game's results could be filed against the wrong game number.**
  The results interface answers a request with a bare reply that says nothing
  about what was asked for, so a reply is only ever "the next message on this
  connection". That makes a read failure a property of the connection rather
  than of the game it happened on: a request that times out part-way leaves
  unread bytes behind, and a slow answer arrives after the agent has given up
  waiting for it. Either way the conversation is one message out of step, and
  the next game's request reads the previous game's answer. The agent then sent
  that data to Overwatch under the number it had asked for — one game's scores
  recorded against another game — and marked the game done, so nothing ever
  corrected it. Backfills and resyncs over a slow game server are exactly where
  this happens, because they fetch many games in a row over one connection.
  Any read failure now ends that connection and the next game starts a fresh
  one, and every answer is checked against the game it says it belongs to
  before it is stored or sent.
- **A game the venue's game server no longer holds was cached as though it
  were the game.** Asked for a game it has since discarded, the server replies
  with a short refusal; the agent stored that refusal as the game's data. The
  game then counted as cached — so it was offered to scoring software, which
  received the refusal in place of the results, and the agent never tried to
  fetch the real thing again. Such a reply is now recognised and the game is
  left uncached, to be retried later.
- **The local cache offered scoring software games it could not actually
  serve.** Games are recorded as soon as they are seen in the game server's
  list, which happens well before their data is fetched (and never at all while
  a game is being played). Those games were included in the list handed to
  scoring software, which then asked for one and was told the game could not be
  found. Scoring software takes that in its stride — it leaves the game blank
  and asks again later — so the entry sat in its list never filling in, polled
  indefinitely, in exactly the situation the cache exists to cover. Only games
  whose data is actually held are listed now.
- **One batch Overwatch refuses could hold back hours of telemetry.** Unsent
  batches are delivered oldest first, so a batch Overwatch rejects outright —
  because of its content rather than any outage — sat at the front of the queue
  being retried forever, with every newer batch stuck behind it until the queue
  finally overflowed. A rejection that identifies the batch itself as the
  problem now drops that batch, with a log line, and delivery continues. A
  refusal that can be put right — an expired token, a misconfigured address, or
  Overwatch being down or busy — still keeps everything queued, because that
  data has to survive until the fix is made. The same applies to buffered pack
  emitter readings.
- **A cached game could survive a power cut half-written.** A game's data was
  written and then immediately recorded as available, but nothing forced it to
  the disk. A venue box losing power in that window came back with a truncated
  file advertised as complete — served to scoring software as-is and never
  fetched again. Game data is now flushed to disk before it is marked
  available. (Records of which games exist are still written the cheap way;
  they cost nothing to rebuild.)
- **Scoring software that stopped reading could hold a connection open
  indefinitely.** If a client stopped collecting its replies — crashed
  mid-game, or a connection the network had already dropped without saying so —
  the agent's reply could block forever, keeping the connection and its
  resources tied up. Replies now have a time limit. Waiting for a client to
  *ask* something is deliberately still unlimited: scoring software holds its
  connection open and idle between games.

### Changed

- **A batch that fetches many games now stops after repeated connection
  failures** instead of working through the whole list. Each failure now also
  costs a reconnection, and a game server failing that consistently will not
  serve the rest of the list either — while the batch holds exclusive access to
  the results interface throughout. A single game the server refuses does not
  count towards this: that is one bad game, not a bad server.

## [1.1.2] — 2026-08-30

### Fixed

- **A backlog of finished games could be read from the venue's game server
  during a live game.** The results drain ran on the poll loop — the only place
  the agent records the game server's current state. While the drain worked
  through a queue of finished games that state could not be refreshed, so the
  safety check each fetch performs was reading a frozen signal: if a game
  started part-way through the drain, the games still queued behind it were
  fetched anyway, during active play. Back-to-back games with a slow game
  server is exactly the situation that builds such a backlog. The drain now
  runs off the poll loop, so the state keeps updating and every queued fetch
  re-checks a live signal. As a bonus, a slow drain no longer stalls telemetry
  (which could push a venue past Overwatch's offline threshold and flap the
  site offline just after a game finished).
- **A backfill or resync could connect to the game server during a live game.**
  Those commands are checked for safety when they are picked up, but they then
  wait for exclusive access to the results interface — a wait that can last
  tens of seconds behind a cache refresh. A game starting during that wait went
  unnoticed: the command connected, completed the handshake and requested the
  game list before its first per-game safety check. It now re-checks immediately
  before connecting and defers instead, matching the single-game fetch path.

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
