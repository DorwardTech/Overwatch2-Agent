# Changelog — Overwatch Site Agent

All notable changes to the Overwatch Site Agent are recorded here. The format is
based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this
project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The Site Agent and central Overwatch are versioned independently.

## [1.2.1] — 2026-09-02

### Security

- **An unreadable server state could be mistaken for an idle one.** The venue's
  game server reports its current mode, and the agent stores that number in a
  narrower form than it arrives in. A value too large to fit was silently cut
  down to size rather than rejected — and some large values cut down to exactly
  the number that means "idle", which is what the agent checks before it may
  read from the game server. A game server sending nonsense, or something on
  the venue network impersonating one, could therefore have opened that door
  during a live game. An out-of-range value is now recorded as a state the
  agent recognises as neither idle nor in-game, so it stays shut: not being
  able to read the state is not the same as reading that it is idle.

### Changed

- **The cache stopped rewriting thousands of unchanged files a minute.** The
  agent re-lists every game the venue's game server holds once a minute and
  rewrote one small file per listed game each time, whether or not anything
  about it had changed — steady write wear on the venue box's storage for no
  new information. Unchanged entries are now left alone. They are still
  refreshed periodically, because that timestamp is also what decides when an
  old game leaves the cache, and games the venue is still playing must not age
  out — there is a test for exactly that.
- **Backing up finished games to Overwatch no longer scales its memory with the
  backlog.** Each backup took its own full copy of the game's data the moment
  it was queued, so a bulk re-sync of a rebuilt cache could hold hundreds of
  copies at once inside an agent limited to 128 MB, competing with the buffer
  that exists to survive an outage. Backups now read the data when their turn
  comes, four at a time.

## [1.2.0] — 2026-09-02

### Added

- **The agent now runs on Windows, as a Windows service.** Until now the only
  way to run the agent was the container image, which meant a Linux box or a
  Docker installation at every venue. The same agent now installs itself as a
  Windows service on any Windows PC on the venue LAN — the game server PC
  itself is the obvious one — starts with the machine with nobody logged in,
  restarts if it fails, and keeps its own log. Nothing else is different: the
  settings, the behaviour and the rules that keep it away from the game
  server during play are the same code, built for Windows.

  `overwatch-agent.exe install` registers the service to run as the
  low-privilege *Local Service* account rather than as an administrator,
  creates the data directory under `C:\ProgramData\Overwatch Agent`, restricts
  it to administrators and the service — it holds the site token and, with
  the cache on, the venue's game data — and writes a configuration template
  there. `start`, `stop`, `restart`, `status` and `uninstall` do what they
  say. A *Reboot agent* command from Overwatch stops the agent cleanly and the
  service control manager starts it again, as the container runtime does.
  Starting, stopping and a failed start are recorded in the Windows
  Application event log. `WINDOWS.md` is the guide.

  The installer holds the venue's data to the same standard the container did:
  it takes ownership of the data directory and restricts it to administrators,
  the system and the service's own identity — not to the shared *Local Service*
  account every other Windows service of that kind runs as. It refuses to do
  that to a folder that is not the agent's own, so a drive root or a shared
  folder passed by mistake is rejected rather than quietly locked down. The
  service records its data directory on its own command line, so it can never
  end up reading a different one than the install prepared.

  A configuration the agent cannot use is a **failed start**, not a service
  that reports success and stops a second later: the settings are read and
  checked before Windows is told the service is running, and the reason is in
  the event log. A stop that takes a moment — the agent finishes what it is
  doing and writes out unsent telemetry — keeps telling Windows it is making
  progress, so it is never mistaken for a hung service and killed mid-write.

  Builds for Windows x64 and Arm64 are published with each release.

- **Configuration can come from a file.** The agent has only ever read its
  settings from environment variables, which suits a container and little
  else. It now also reads a `KEY=VALUE` file — the same lines as
  `.env.example` — named by `--config`, by `AGENT_ENV_FILE`, or found as
  `agent.env` in the data directory. Anything already set in the environment
  takes precedence over the file, so nothing a compose file or a shell sets
  can be overridden behind its back.

- **The log can go to a file.** `LOG_FILE` names a file the agent writes its
  log to as well as the console, rotated at 10 MB with five generations kept,
  so it can neither vanish nor fill the disk. On Windows it defaults to
  `logs\agent.log` in the data directory — a service has no console — and the
  service writes only there.

- **A data directory.** `AGENT_DATA_DIR` (or `--data-dir`) names one place for
  everything the agent keeps: the cache, the unsent-telemetry spill files, the
  log and the configuration file all default to paths under it. On Windows it
  defaults to `%ProgramData%\Overwatch Agent`. In the container nothing
  changes: no data directory is assumed there, and every default is exactly
  what it was.

### Fixed

- **`healthcheck` built the wrong address when `HEALTH_ADDR` named a host.** It
  put `127.0.0.1` in front of whatever the setting held, which was right for
  the default `:8088` and wrong for anything else — `127.0.0.1:8088` became
  `127.0.0.1127.0.0.1:8088`. It now probes the host given, or the loopback
  address when the setting binds every interface.

## [1.1.5] — 2026-09-01

### Fixed

- **Losing contact with the venue's game server left the agent acting on a stale
  all-clear.** The agent reads the game server's state only on its polling
  connection. When that connection drops — a restart at the venue, a network
  fault, anything the agent then retries for as long as the outage lasts — the
  last reading simply stands. If it read "idle", it stays "idle" for the whole
  outage, however long that is and whatever happens at the venue meanwhile.

  Two paths reach the game server without going through the poll loop: an
  operator pressing Resync on the site control panel, and the handler that runs
  when a game finishes. Either could therefore start reading game data during
  live play, on the strength of a reading taken before the connection dropped —
  and the cache refresh is the heaviest read there is, the full game list plus
  every game the agent does not already hold.

  The safety check now requires a reading that is *current*, not merely
  favourable. A reading older than two minutes — comfortably longer than the
  gap between polls, far shorter than any real outage — no longer counts as an
  all-clear, and an agent that has never polled is never clear. The work is
  deferred exactly as it is during a game, and picked up once the link is back.

### Changed

- **Deferred results work now reports which of the two reasons applied.** A
  backfill, resync or re-fetch that is held back says either that a game is
  active or that contact with the game server has been lost, instead of
  reporting "game in progress" for both. An operator watching a command sit in
  "deferred" throughout an outage was being told the one thing that was not
  happening.

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
