# Changelog — Overwatch Site Agent

What changed in each release of the Site Agent, the program that runs at each
venue and sends its game server's data to Overwatch. The newest release is
first.

The Site Agent and Overwatch have their own version numbers.

## [1.10.1] — 2026-11-11

A security update. Docker venues pull the 1.10.1 image; Windows venues run the 1.10.1 installer.

### Security

- **Built with Go 1.26.11.** It fixes a known problem in Go, the language the agent is written in, that could crash a program serving web pages. The agent serves its setup page and health check that way. Nothing else changes.

## [1.10.0] — 2026-11-01

The agent now copes with a game server that has no print server, or a print server that has stopped answering. Two new settings, both optional: the defaults suit every venue.

### Added

- **The agent notices a print server that does not answer.** Some game servers have no print server. The agent used to try each finished game about ten times over two hours, logging a timeout every time, then give up, and Overwatch showed the results as abandoned. Now, after 3 failed connections in a row, spread over at least ten minutes, the agent treats the print server as absent. It says so once in the log, and stops fetching results and refreshing its local copy of games. The ten minutes allow for a power cut, when the game server's other services can come back minutes before its print server.
- **It keeps checking quietly, between games only.** The first check is after a minute, then the wait doubles after each failed check, up to every 15 minutes, with nothing in the log. When the print server answers again, the agent says so once and fetches the games that were waiting, oldest first.
- **A short outage costs no results.** If the print server answered earlier since the agent started and then stopped, the agent checks it again after each game ends, once the finish delay (`GAME_FINISH_DELAY`) is over. Meanwhile the newest 50 finished games wait to be fetched. A print server that has not answered at all since the agent started is not checked after every game, and finished games are not kept waiting for it, because there is nothing to fetch them from. A finished game is never fetched before the finish delay is over, even if the print server comes back during it.
- **A check never costs a waiting game anything.** If a game starts during a check, the check is put off to the next gap between games. It uses up none of a waiting game's tries and logs nothing, so a game waiting at a venue without a print server is never given up. An operator's command cut short the same way is put off, and Overwatch sends it again.
- **An operator's request always tries.** A re-fetch, backfill, resync or cache resync sent from Overwatch, or **Resync cache** on the agent's own admin page, connects even to a print server treated as absent, still only between games. If it answers, the agent is back to normal at once. If not, the command says why. A failed request never counts towards treating the print server as absent.
- **Overwatch can show whether the print server answers.** The agent now reports the print server as present, absent, unknown (until it first answers or is treated as absent) or disabled (when game results, the cache and the message bus are all off). Overwatch 1.31.0 and later show a venue with no print server as such on **Fleet** and the site's **Live** page, instead of as abandoned results.
- **Two new settings:**
  - `PRINT_SERVER_ABSENT_AFTER`: how many failed connections in a row before the print server is treated as absent. Default `3`. `0` turns this off, and the agent behaves as before.
  - `PRINT_SERVER_RECHECK_MAX`: the longest wait between checks, in seconds. Default `900`, kept between `60` and `3600`.
  - `ENABLE_GAME_RESULTS`, `ENABLE_CACHE` and `ENABLE_MSG_BUS` still decide whether the agent uses the print server at all.

### Changed

- **Resync cache on Admin → Game cache now reports a failure** when the print server does not answer, with the connection error, instead of reporting success. One that meets a game in progress is put off, like the other commands that use the print server.
- **The settings say what needs a print server.** The example settings (which still turn game results on), the setup page's **Collect scoresheets after each game** and **Keep a local copy of finished games, and serve them to the scoresheet printer** boxes, and the README now say they need a print server. A venue without one should leave game results, the cache and the message bus off.
- **The setup page no longer has a Use this computer (127.0.0.1) button.** The game server is a separate machine, and the agent never runs on it. The setup page's hints and messages, the Windows guide, the example settings, the README and the Docker Compose comments no longer say otherwise.

## [1.9.2] — 2026-10-31

A clearer message when the agent cannot listen for the game server's announcement. There is nothing to set up.

### Fixed

- **"Could not listen… forbidden by its access permissions" now says why.** On Windows, the setup page's **Find**, `overwatch-agent discover` and the log used to show only the system's error when UDP port 12106 could not be opened. That happens when another program on the computer holds the port without sharing it, or when Windows has reserved the port (Hyper-V, WSL and Docker Desktop can). The message now says so, names the program holding the port when it can, and says what to do instead: free the port, or enter the game server's address rather than finding it.
- On Linux the same applies to "address already in use", without the program's name.

## [1.9.1] — 2026-10-31

Team colours for the live page. There is nothing to set up.

### Fixed

- **The game server's team list was never sent.** The agent looked for the teams in the wrong place in the game server's reply, so every update went without them. It now sends each team's name, colour number and colour, so the **Live** page can show each team in its real colour.

## [1.9.0] — 2026-10-29

There is nothing to set up.

### Added

- **The agent tells Overwatch which system the game server runs:** Phasor Strike, Helios Pro, Helios 2, Begeara 3, Helios 3, or none. Overwatch names it on the **Sites** page. Nothing else from the game server's site information is sent.
- **It asks only between games:** once per connection, then once an hour. It never asks while a game is starting or running. A game server that does not know the request costs one short wait an hour, between games, and never holds up the agent during play.
- Until the game server answers, Overwatch keeps what it had.

## [1.8.0] — 2026-10-25

A quieter log when other programs use the game server. There is nothing to set up.

### Added

- **The agent's status report counts the messages it discards**, from when the agent started: replies to other programs' requests, messages that arrived between the agent's own requests, messages dropped because too many were waiting, and readings that lost a message while waiting for their replies.

### Changed

- **The once-a-minute "discarded" log line is gone.** The game server sends every program's replies to every connection. So while another program used it, the agent logged how many messages it had discarded about once a minute, all day, from 1.6.5 on. That is normal, and the line buried the ones that matter. Now the log explains it once, the first time it happens, then sums it up at most once an hour, and only when something new was discarded.
- **A warning when a reading loses a message.** A message lost while the agent waits for the game server's replies could have been the game server's status. The agent then cannot confirm whether a game is on, so it keeps off the print server until a clean reading. This is the only discard that matters, and the log now warns about it, at most every five minutes. Other discarded messages are only counted.

## [1.7.1] — 2026-10-25

Fixes for finding the game server with `auto`, for a computer clock that jumps, and for "Time left" at legacy venues. There is nothing to set up.

### Fixed

- **A clock change could make `auto` choose the wrong server.** If the computer's clock jumped forward (after a time sync, say), every server heard so far looked gone at once, and the first one heard again could be chosen while another was still on the network. A change of time no longer affects this. A server must also now be the only one heard for 11 seconds in a row before it is chosen. If another server is heard in that time, the agent waits again. Choosing at startup can therefore take a few seconds longer: up to about 16 seconds rather than 11.
- **While it waits, the agent says what it actually heard.** Before, almost every wait said it was waiting for an announcement, with advice about host networking and the firewall, even when the network was fine. Now the log and the readiness check (`/readyz`) say what was heard. For example:
  - a server for site 0564 arena 0 was heard, but the arena setting asks for arena 1;
  - every announcement heard came from a different address than the one it names, so set the game server address instead;
  - one site and arena is announced from two addresses.

  The networking advice is given only when nothing at all was heard. Each message is logged when it changes, and again every five minutes while it stands.
- **A different server at the chosen address is noticed before anything uses it.** With `auto`, if the machine at the chosen address starts announcing a different site or arena, the agent restarts and chooses again. It used to check only once a minute, after work had already started: fetching finished games from the print server, running commands from Overwatch, refreshing the cache and sending updates. Now every reading checks first, and nothing from the reading that notices is sent or started. The print server is off-limits from the moment the change is heard, so nothing reaches the other machine's print server. A "game finished" message from that machine is ignored too. An agent with a fixed address is not affected.
- **The print-server safety check no longer trusts the computer's clock.** The agent only uses the print server while a recent status reading says the game server is between games. If the computer's clock was set back, an old reading could look recent again. A change of time no longer affects this. After the agent's computer wakes from sleep, the print server stays off-limits until the agent has read the game server's status again.
- **Legacy (Nexus) venues: "Time left" showed the time played.** The agent now sends the game's length less the time played, never less than zero. When Nexus does not report how long the game is, the agent sends 0 rather than guess.
- A server heard again after more than 30 seconds of silence now starts afresh: its earlier announcements no longer count towards hearing it twice.

## [1.7.0] — 2026-10-24

The agent can find the game server by itself. The game server announces its address, site and arena on the local network every few seconds (UDP port 12106). Set the game server address to `auto` and the agent listens, chooses the server it hears, and connects to it. A fixed address works as before.

### Added

- **Game server address `auto`.** The agent listens for at least 11 seconds, long enough to hear every server on the network at least twice, then connects to the one it heard. It never guesses. If it hears more than one site or arena, or one server announced from two addresses, it waits and logs what it heard.
  - The site and arena settings narrow the choice to one server. Without them, the agent uses the single server it hears, and logs that it is keeping to it for this run only.
  - If nothing is heard, it keeps waiting, logs the likely reasons every five minutes, and the readiness check (`/readyz`) says why it is not ready.
- **Only announcements that can be trusted count.** An announcement must come from the address it names, and from a network this computer is directly connected to. Anything else is listed but never chosen.
- **One address per run.** The address is chosen once, before the agent first connects, and does not change while it runs. Nothing reaches the game server or its print server before the choice is made.
- **Following a server that moves.** If the chosen server stops answering for a minute, announces itself from a new address, and that address answers a read-only status request, the agent restarts and connects to the new address. A server that still answers is never left. If the machine at the chosen address starts announcing a different site or arena, the agent also restarts and chooses again. After any restart the print server stays off-limits until the game server reports it is between games. The log gives the reason for each restart.
- **The agent also listens with a fixed address, on by default.** It only reports what it hears (the site, the arena and the game server's own clock) and never changes the address. If the port cannot be opened, the agent logs that and carries on.
  - The agent opens the port for sharing. A program on the same computer that opens UDP 12106 afterwards without sharing it will then fail to open it.
  - `ENABLE_LIB_LISTENER=false` turns the listener off.
  - Docker agents on a bridge network hear nothing: they report that they are listening, with no announcements.
- **The agent's admin page** shows the address in use and when the server was last heard, and lists the servers heard while the agent cannot choose. The agent's status report to Overwatch also says what it heard and chose.
- **`overwatch-agent discover [--seconds N]`** lists the servers heard on the network, with site, arena, address, sender and clock, and says which one `auto` would choose. It only listens. In a container, run it with `docker exec <agent> /agent discover`.
- **Setup page: Find on the network.** It shows a card for each server heard, and whether it answered. **Use this address** fills in the address. **Follow it if its address changes** fills in `auto` with the site and arena. Nothing is filled in until you press a button. **Test** works with `auto` too.
- **Windows:** the installer adds an inbound firewall rule for UDP 12106, limited to the agent's program and the local subnet, and uninstalling removes it. If the rule cannot be added, the install still succeeds and prints the command to add it by hand.

### Changed

- The agent refuses to connect to an empty address. Before, it connected to this computer.
- The setup page's **Test** never sends a request to the print server's port, even when that port is typed in as the game server's.
- The readiness check (`/readyz`) says why while the agent is not ready.
- A health check that cannot open its port is now logged.

## [1.6.5] — 2026-10-24

Includes everything from 1.6.4, which was never published, and finishes its work. The agent is stricter about which replies from the game server it trusts, so a mixed-up reply can no longer let it near the print server during a game.

### Fixed

- **An old reply could make the agent miss that a game had started.** 1.6.4 matched each reply to its request by name, but a status reply left waiting from earlier (another program's, or a duplicate) could still pass for a new one. If it said "between games" while a game had since started, the agent forgot the game and the print server became reachable during play. A reply now counts only if it arrived after the agent's request.
- **The agent is more careful before it uses the print server.** A reading that a game is starting or under way takes effect the moment it arrives. A reading that the game server is between games opens the print server, or marks a game finished, only once both of that round's replies are in and agree. If the agent cannot be sure of a reading (a message was lost, or another reply could have been the status), it still acts on it if it says a game is on, but it will not mark a game finished or use the print server until a clean reading. A reading that fails part-way keeps the print server off-limits. A game server that repeats itself still lets finished games be fetched.

### Changed

- The once-a-minute log line about discarded messages counts each kind: replies to another request, messages that arrived before the request they could have answered, and messages dropped because too many were waiting.
- When the agent holds back print-server work because it could not confirm the game server's state, it now says so, and tries again after the next clean reading. During a game it still says a game is in progress. While it waits, the agent reads the game server at the in-game rate, so that clean reading comes sooner.
- If a finished game's follow-up (fetching its results and refreshing the cache) finds the print server off-limits, or has to stop part-way, it now runs as soon as the print server is clear, instead of waiting for another game to end. A game is still never fetched before `GAME_FINISH_DELAY` has passed since the game server's message bus reported it finished.

## [1.6.4] — 2026-10-23

Never published: these fixes reached venues in 1.6.5.

### Fixed

- **Updates could go out without their packs.** With several programs connected to one game server, the agent could take the reply to one request as the answer to another. At one venue about half of the updates went out with no packs. That telemetry was lost, and Overwatch showed the venue as "Idle, game #0". The agent now checks which request each reply answers, and discards the ones that answer a different request. Once a minute the log says how many messages were discarded, if any were.
- **A container deployed without the compose files lost its cache and unsent telemetry on every redeploy.** A Coolify "Docker image" application is one example, even with a volume at `/data/cache`. Each redeploy downloaded the whole cache from Overwatch again and dropped any telemetry held during an outage.
  - The agent now keeps its cache, unsent telemetry and unsent pack-IR readings in `/data/cache` by default whenever it can write there, as the compose files set them. A Docker volume mounted there works with no settings.
  - A host directory owned by root is not used: the agent keeps the old defaults, and logs which directory to `chown` to 65532.
  - `CACHE_DIR`, `BUFFER_FILE` and `PACK_IR_BUFFER_FILE` still override all of this. Windows is unchanged.

## [1.6.3] — 2026-10-23

A security update. Docker venues pull the 1.6.3 image; Windows venues run the 1.6.3 installer.

### Security

- **Built with Go 1.26.10.** Go 1.25, which built 1.6.2, is no longer supported, and its last release has twelve known vulnerabilities in parts the agent uses: its web connections, secure connections (TLS), certificate checking and the setup page. All are fixed in 1.26.10.
- One other library the agent includes was updated for a security fix. The agent never used the affected part.

### Changed

- The setup page now rejects an address with a stray colon outside square brackets. Such an address could never be reached anyway.

### Notes

- The Windows build still runs on Windows 10 / Server 2016 or later.
- Building the agent from source now needs Go 1.26 or later.

## [1.6.2] — 2026-09-25

### Fixed

- **Overwatch could show a game still running after it had ended.** When a game ends, the agent switches to its slower between-games rate, and the reading that saw the game end could be skipped as too soon. At a venue recording every second in play and every 30 seconds between games, the change could take most of a minute to reach Overwatch. A reading that shows a change of state is now always sent, and the slower rate resumes straight after.

## [1.6.1] — 2026-09-25

Behind-the-scenes improvements. Nothing changes for you.

## [1.6.0] — 2026-09-25

Telemetry now arrives at the rates you set. A venue set to one-second readings during games was getting one every 2.4 seconds, and no setting could fix it.

### Fixed

- **Sending to Overwatch no longer delays the next reading.** The agent waited for Overwatch to answer each update before it read the game server again, so the real rate was the interval plus the time that round trip took. At one venue set to 1 second, that measured 2.4 seconds. Updates are now sent separately, and readings keep to the interval.
- **Held telemetry is sent while the agent is reconnecting.** Telemetry buffered during an outage used to wait until the agent reached the game server again. It is now sent to Overwatch even while the agent cannot reach the game server.
- **Between-games telemetry keeps to its own rate during print-server work.** While the agent works with the print server it watches the game server at the fast rate, so it notices a game starting. That used to record telemetry at the fast rate too: one cache refresh a minute was enough to make a 15-second between-games rate average 7.9 seconds. The agent still watches closely, but now records at the rate the venue asked for. The fuller report the agent sends about once a minute (team info, the game list, licences and the agent's own status) is never skipped, so Overwatch's view of the agent's health stays current.

### Notes

- `IDLE_POLL_INTERVAL` at or above 30 seconds crosses Overwatch's default **Agent offline** alert threshold, and the agent warns about it at startup. Raise that alert rule **before** raising the interval, or the site flaps offline in between.

## [1.5.1] — 2026-09-05

### Fixed

- **A game starting during print-server work is noticed sooner.** The agent never contacts the print server during a game, and checks just before each connection. But between games it read the game server only every 15 seconds, so a game starting just after a reading could go unnoticed for up to that long, while a results fetch or a backfill kept starting new requests. The agent now reads the game server at the in-game rate while it has print-server work in progress: every 5 seconds instead of 15 on the default settings. A request already under way still runs until it finishes or times out.
- The agent also sends telemetry at the in-game rate during that time. Fetching results takes seconds; a backfill takes minutes, and an operator asks for one at a time.

## [1.5.0] — 2026-09-04

### Changed

- **The setup page turns on the local game cache for a new venue.** The box started unticked, so a Windows venue installed the easy way had no local copy of its finished games, nothing answering the printing software during play, and nothing to restore from after an outage. It now starts ticked, but only for a venue that has never been configured. A venue that has been through the setup page before keeps its `ENABLE_CACHE` setting, whatever it was.
- **The agent's own default stays off**, so updating an agent, including at Linux venues, does not turn the cache on.
- **The setup page and the Windows guide say the box opens a listener for the printing software**, for a computer that should open no ports.

## [1.4.0] — 2026-09-04

Nexus venues can now be set up from the setup page.

### Added

- **The setup page now works for Nexus venues.** Before, it always asked for a game server address, which a Nexus venue does not have, so the form could not be saved. A Nexus venue had to be set up by editing `agent.env` by hand.
- **The page asks which system the venue runs** and shows the fields for that system. A venue on the current game system is asked for its game server. A Nexus venue is asked for its **Nexus database** (`NEXUS_DSN`) and its **management app address** (`LASERTAG_URL`), with `AGENT_MODE=legacy`.
- **Both choices have a test button** that checks the connections before anything is saved. For Nexus, one press checks the database and the management app and reports each one separately, because they go wrong for different reasons and need different fixes.
- The starter `agent.env` and the Windows guide (WINDOWS.md) now explain both kinds of venue and their settings.

### Changed

- **Switching a venue between the two systems keeps the other system's settings.** They are commented out instead of deleted. So a Nexus venue's database password survives someone saving the setup page, and switching back turns the same settings on again with their old values.
- For a Nexus venue, the page hides the scoresheet, cache, proxy and message bus options. They never did anything at a Nexus venue.
- `AGENT_MODE` is now always written to the file when you save, so the file shows which system the venue runs. Before, it was left out when it matched the default.

### Fixed

- **`AGENT_MODE` no longer cares about capital letters or stray spaces.** Before, `AGENT_MODE=Legacy`, or `legacy` with a stray space, was quietly treated as a venue on the current game system, with no error or warning. The agent then tried to reach a game server the venue does not have.

## [1.3.3] — 2026-09-02

### Added

- **A Windows installer.** Download `overwatch-agent_<version>_windows_setup.exe` and double-click it. You no longer need to extract an archive and type commands into an administrator PowerShell prompt.
- The installer puts the agent in *Program Files* and then sets it up exactly as the manual install does: the same service, the same locked-down data folder, the same starter configuration and the same Start Menu entry. When it finishes, it offers to open the setup page, so the whole job is one download and one form.
- One installer covers both x64 and Arm64 machines and installs the right build, so there is nothing to choose.
- **To upgrade, run the newer installer.** It stops the agent, replaces it and starts it again. It does not ask for the venue's settings again.
- **To uninstall, use Settings → Apps.** It asks before deleting the site token, cached games and logs.

### Notes

- The separate x64 and Arm64 archives are still published for scripted rollouts, and the installer accepts the usual silent-install switches.
- The installer is not signed with a certificate, so Windows SmartScreen warns the first time you run it. The Windows guide (WINDOWS.md) explains this and gives the checksum to check the file against.

## [1.3.2] — 2026-09-02

### Fixed

- **Games restored from Overwatch are now checked before they go into the cache.** This covers a cache restored when a venue box is rebuilt, and games pulled in with **Collect from Overwatch**. If a game's data says it is a different game from the number it was stored under, the agent logs it and skips it. Before, a game stored in Overwatch under the wrong number could be copied back into the cache, and scoring software would be given the wrong game's results under that number for as long as the cache kept it.
- A skipped game is fetched properly from the venue's game server at the next refresh. Game data that names no game number at all is still accepted.

## [1.3.1] — 2026-09-02

### Security

- **The agent no longer passes its site token on through a redirect.** If the Overwatch address answered with a redirect to another server, for example because of a mistyped address or a lapsed domain, the agent sent its site token on to that server. Overwatch never redirects, so the agent now sends nothing on and reports the redirect as an unexpected answer.

### Changed

- **The setup page checks the Overwatch address before testing it.** Before, pressing the test button on a partly pasted address gave a confusing low-level error. Now the test and the save check the address the same way, and the page says what is wrong: a missing `https://`, a missing server name, a username and password left in the address, a port typed into the address field, or a port outside 1–65535.

## [1.3.0] — 2026-09-02

### Added

- **A setup page, so nobody has to edit a configuration file.** Run `overwatch-agent setup`, or on Windows open **Overwatch Agent Setup** from the Start Menu. A form opens in your browser. No command prompt is needed.
- **Test the connection to Overwatch.** The button says which of three problems it found: the address is wrong, the token was rejected, or the server could not be reached at all. The test only reads from Overwatch, so it records no made-up game data against the venue.
- **Test the connection to the game server.** The button connects to the venue's game server and shows what it answered. It never touches the game server's print server, so it is safe to press during a game.
- **Save and start.** The page saves the settings, installs or restarts the agent, and shows the agent's log so you can watch it come up.
- **Saving keeps your file.** Your comments stay, and so do settings the form does not show, such as a Nexus database connection. A setting you switch off is commented out with its value kept, so switching it back on restores it.
- The page can only be opened on the machine it runs on, and only in the browser it was opened in. It closes when you press **Finish**, or after it has been left for half an hour. Because it writes the site token, the data folder is locked down before the page starts.

## [1.2.1] — 2026-09-02

### Security

- **A garbled state reading from the game server can no longer be taken as "idle".** The agent reads from the game server only when it reports it is idle. Some out-of-range values could be misread as "idle", so a faulty game server, or a device on the venue network pretending to be one, could have let the agent read from the game server during a live game. A state the agent cannot read is now treated as neither idle nor in a game, so the agent keeps waiting.

### Changed

- **The game cache no longer rewrites unchanged files every minute.** This cuts wear on the venue box's storage.
- **Backing up a large number of finished games to Overwatch no longer uses more memory the bigger the backlog**, for example when a rebuilt cache is re-synced.

## [1.2.0] — 2026-09-02

The agent now runs on Windows.

### Added

- **The agent runs on Windows, as a Windows service.** Before, it ran only as a container image, which needed a Linux box or Docker at every venue. It now installs as a service on a Windows PC on the venue network. It starts with the machine with nobody logged in, restarts if it fails, and keeps its own log. The settings, the behaviour and the rules that keep it away from the game server during play are the same as in the container.
- **Install it with `overwatch-agent.exe install`.** The service runs as the low-privilege *Local Service* account, not as an administrator. The install creates the data folder `C:\ProgramData\Overwatch Agent` and writes a configuration template there. It locks the folder down to administrators, the system and the service, because it holds the site token and, with the cache on, the venue's game data. It refuses to lock down a folder that is not the agent's own, such as a drive root or a shared folder given by mistake.
- `start`, `stop`, `restart`, `status` and `uninstall` do what they say. A **Reboot agent** command from Overwatch stops the agent cleanly and Windows starts it again. Starts, stops and failed starts are recorded in the Windows Application event log. The Windows guide (WINDOWS.md) covers the steps.
- **Settings the agent cannot use show as a failed start**, with the reason in the event log, instead of a service that says it started and then stops a second later. When the service stops, the agent is given time to finish and save unsent telemetry, so Windows does not kill it as hung.
- Builds for Windows x64 and Arm64 are published with each release.
- **Settings can come from a file.** As well as environment variables, the agent reads a `KEY=VALUE` file, with the same lines as `.env.example`. Name it with `--config` or `AGENT_ENV_FILE`, or put it in the data folder as `agent.env`. A setting in the environment takes priority over the file.
- **The log can go to a file.** `LOG_FILE` names a file the agent logs to as well as the console. It rotates at 10 MB and keeps five old files, so it cannot fill the disk. On Windows it defaults to `logs\agent.log` in the data folder, and the service logs only there.
- **One data folder.** `AGENT_DATA_DIR` (or `--data-dir`) sets one place for everything the agent keeps: the cache, unsent telemetry, the log and the configuration file. On Windows it defaults to `%ProgramData%\Overwatch Agent`. Nothing changes in the container.

### Fixed

- **The `healthcheck` command now works when `HEALTH_ADDR` includes a host.** It used to build a broken address, such as `127.0.0.1127.0.0.1:8088`. The default `:8088` was not affected.

## [1.1.5] — 2026-09-01

### Fixed

- **The agent no longer acts on an old "idle" reading after it loses contact with the game server.** When the connection dropped, for example during a restart at the venue or a network fault, the last reading stood for the whole outage. If that reading was "idle", pressing Resync, or the work that runs when a game finishes, could read game data from the game server during live play. Now a reading older than two minutes does not count as idle, and an agent that has never had a reading is never treated as idle. The work waits, as it does during a game, and runs once the connection is back.

### Changed

- **Held-back work now says why.** When a backfill, resync or re-fetch is deferred, it says either that a game is in progress or that the agent has lost contact with the game server. Before, it always said "game in progress", even during an outage.

## [1.1.4] — 2026-08-31

### Security

- **In legacy (Nexus) mode, the agent no longer writes its database password to the log.** The first log line showed the full connection string, password included, so it appeared in `docker logs` and anywhere logs are collected. The account is read-only, but the database holds member records. The line now shows only the user, host and database.
- **Devices on the venue network can no longer make the agent run out of memory.** Anything on the venue network can connect to the agent without a password, because the scoring software cannot use one. Running out of memory would have restarted the agent without saving unsent telemetry. There are now three limits:
  - At most 64 connections at once to the port scoring software connects to. More are refused, and the client reconnects.
  - At most 64 KiB per request.
  - A time limit on a request that stops part-way. A connection left idle between games is still allowed.
- **One huge message from the game server can no longer bring the agent down.** Messages are now limited to 10 MiB.
- **The queue of unsent telemetry now has a size limit as well as a count limit.** It stops at 32 MB as well as 2,000 entries, dropping the oldest first as before. Before, readings sent from the venue network could grow the queue to 128 MB, the agent's whole memory limit.

## [1.1.3] — 2026-08-30

### Fixed

- **A finished game's results could be recorded against the wrong game number.** This happened mostly during backfills and resyncs over a slow game server, which fetch many games in a row. One game's scores were sent to Overwatch under another game's number and never corrected. The agent now starts a fresh connection after any read failure, and checks that every answer belongs to the game it asked for before storing or sending it.
- **A game the game server no longer holds is no longer cached.** The server's short refusal was stored as the game's data, so scoring software received the refusal instead of results, and the agent never tried again. Now the game is left uncached and retried later.
- **The game cache no longer offers scoring software games it cannot serve yet.** Games were listed before their data had been fetched, so scoring software showed them blank and kept asking for them. Only games whose data the agent holds are listed now.
- **One batch Overwatch rejects no longer holds up hours of telemetry.** Unsent batches are sent oldest first, so a batch Overwatch rejected because of its content was retried forever, with everything newer stuck behind it. That batch is now dropped, with a log line, and delivery carries on. Problems that can be fixed, such as an expired token, a wrong address, or Overwatch being down or busy, still keep everything queued until the fix is made. Buffered pack emitter readings work the same way.
- **A power cut can no longer leave a half-written game in the cache.** Before, a venue box that lost power at the wrong moment could come back with a cut-off game marked complete, which was served to scoring software and never fetched again. Game data is now saved to disk before it is marked available.
- **Scoring software that stops reading can no longer hold a connection open forever.** Replies now have a time limit. Scoring software can still keep an idle connection open between games.

### Changed

- **A fetch of many games now stops after repeated connection failures** instead of working through the whole list. A single game the server refuses does not count towards this.

## [1.1.2] — 2026-08-30

### Fixed

- **A backlog of finished games could be read from the game server during a live game.** If a game started while the agent was working through a queue of finished games, the rest of the queue was still fetched during play. This is most likely with back-to-back games and a slow game server. The agent now keeps reading the game server's state while it works through the queue, so each fetch checks whether a game has started.
- **A slow backlog no longer holds up telemetry.** Before, it could push a venue past Overwatch's offline threshold, so the site could show as offline just after a game finished.
- **A backfill or resync could connect to the game server during a live game.** If a game started while the command was waiting its turn, which can take tens of seconds, it went ahead anyway. It now checks again just before connecting and waits if a game has started.

## [1.1.1] — 2026-08-15

### Fixed

- **The standalone `docker-compose.yml` now keeps the agent's data between restarts.** It had no storage volume, so every restart started from scratch. Telemetry held during an outage was lost on the next restart or upgrade. On every redeploy the agent downloaded its whole game cache from Overwatch again: hundreds of requests that hit the site's rate limit and held up live telemetry until they finished. The compose file now keeps the game cache and unsent telemetry on a persistent volume.
- **The agent reports the right version again.** After the 1.1.0 release, agents still reported 1.0.2.

### Notes

- If a venue's agent was set up from the standalone compose file, use the updated file to get this fix.

## [1.1.0] — 2026-07-14

### Added

- **The agent can forward pack IR readings from a calibration bench.** A device on the venue network can send pack IR emitter-strength readings to the agent, which passes them on to Overwatch. Turn it on with `PACK_IR_ADDR`. It is for the venue network only and has no password. Readings are held during an outage and kept across restarts, the same as telemetry, and bad readings are rejected when they arrive. It works in both agent modes, and its status shows in the agent's status overview.

## [1.0.2] — 2026-07-12

Behind-the-scenes improvements. Nothing changes for you.

## [1.0.1] — 2026-07-12

### Fixed

- **The status refresh, cache resync and cache purge commands sent from Overwatch work again.** They had stopped working, so you could not operate an agent from Overwatch.
- **The agent reports its real version instead of `dev`.** You can also set `AGENT_VERSION` to change the version an already-built image reports.

## [1.0.0] — 2026-07-11

Initial release.

### Added

- **Live telemetry to Overwatch.** If the internet connection drops, the agent holds the data and sends it when the connection is back, so nothing is lost and the venue carries on as normal.
- **Finished games are collected only when the game server is idle**, never during a live game.
- **A local game cache.** Scoring software gets finished games from the agent straight away, instead of from the live game server.
- **Every cached game is backed up to Overwatch**, and restored automatically when the agent starts with an empty cache.
- **Collect from Overwatch** pulls games for a chosen time range from Overwatch into the local cache. It skips games already cached and is safe during a live game.
- **A control panel in the browser**, protected by a token. It shows the agent's status and cached games, has buttons to resync or purge the cache and to collect games from Overwatch, and links to this changelog and the public API reference.
- **One container image for standard PCs and ARM boards** (`linux/amd64`, `linux/arm64` and `linux/arm/v7`).
