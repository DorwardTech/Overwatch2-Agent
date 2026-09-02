# Running the Site Agent on Windows

The site agent is one executable that runs as a **Windows service**: it starts
with the machine, needs nobody logged in, restarts itself if it fails, and
keeps its own log. There is no Docker involved. It is the same agent as the
container image — same settings, same behaviour — built for Windows.

Install it on any always-on Windows machine on the venue LAN. The game server
PC itself is the usual choice; the one thing to know about that is under
[Running on the game server PC](#running-on-the-game-server-pc).

## What you need

- Windows 10 / 11 or Windows Server 2016 or later, 64-bit. Builds are published
  for **x64** (`amd64`, almost every PC) and **Arm64**.
- An **administrator** account to install the service.
- Outbound HTTPS to the central Overwatch server, and LAN access to the game
  server. Nothing inbound is needed unless you enable the print-server proxy
  or the control panel (see [Firewall](#firewall)).
- The site's **agent token** — Overwatch → Sites → edit the site → *Generate
  token*.

## Install

1. **Download** `overwatch-agent_<version>_windows_setup.exe` from the
   [Releases](https://github.com/DorwardTech/Overwatch2-Agent/releases) page.
   One installer covers both **x64** and **Arm64**; it installs the build this
   machine can run.

2. **Run it** and say yes to the administrator prompt.

   > Windows will most likely say *"Windows protected your PC"* first. The
   > installer is not signed with a certificate, so SmartScreen has nothing to
   > check it against. Choose **More info → Run anyway**. To be sure you have
   > the file we published, check it against `SHA256SUMS.txt` from the same
   > release before running it:
   >
   > ```powershell
   > Get-FileHash .\overwatch-agent_1.3.3_windows_setup.exe -Algorithm SHA256
   > ```

   The installer copies the agent to `C:\Program Files\Overwatch Agent\` and
   then does exactly what the hand install below does: it registers the
   `OverwatchAgent` service (display name *Overwatch Site Agent*) to start
   automatically as `NT AUTHORITY\LocalService`, sets it to restart on failure,
   creates the data directory `C:\ProgramData\Overwatch Agent\`, takes
   ownership of it and locks it down to administrators, the system and this
   service alone, writes a starter configuration to `C:\ProgramData\Overwatch
   Agent\agent.env`, and adds an **Overwatch Agent Setup** entry to the Start
   Menu.

3. **Set it up.** Leave *Set this venue up now* ticked on the last page and the
   setup page opens by itself. (You can also open **Overwatch Agent Setup** from
   the Start Menu at any time, and say yes to the administrator prompt.) A page
   opens in your browser with a form:

   - **Overwatch address** and **site token** — from Overwatch: *Sites* → edit
     this venue → generate a token. Press **Test this connection**: it tells you
     straight away whether the address is right and the token was accepted.
   - **Game server address** — `127.0.0.1` if the agent is on the game server
     itself, otherwise its address on the venue network. **Test this
     connection** too.
   - **What the agent should do** — leave these alone unless you have been told
     otherwise.

   Then press **Save and start the agent**. The page installs or restarts the
   service and shows the agent's log so you can watch it connect.

   If the Start Menu entry is missing, the same page opens with
   `.\overwatch-agent.exe setup` from an elevated prompt. Nothing about the
   page is available over the network: it is only reachable from this computer,
   by the browser it opened, and it closes when you press **Finish**.

Within a minute of starting, the site shows **online** in Overwatch.

### Installing from the archive instead

The installer is a wrapper. If you are deploying by script, or you would rather
see every step, the per-architecture archives on the same release page install
the identical agent:

1. **Download** `overwatch-agent_<version>_windows_amd64.zip` (or `_arm64`) and
   check it against `SHA256SUMS.txt` from the same release:

   ```powershell
   Get-FileHash .\overwatch-agent_1.3.3_windows_amd64.zip -Algorithm SHA256
   ```

2. **Extract** it to `C:\Program Files\Overwatch Agent\`. Keep the executable
   under *Program Files*: the service runs as a limited account that cannot read
   files in a user's profile.

3. **Register the service** from an elevated PowerShell (right-click → *Run as
   administrator*):

   ```powershell
   cd "C:\Program Files\Overwatch Agent"
   .\overwatch-agent.exe install
   ```

   This is the same command the installer runs, and it prints what it did — read
   it before moving on. The data directory is recorded on the service's own
   command line, so the service always uses the one you installed with.

Then set the venue up from the Start Menu entry as above.

The installer itself takes the usual Inno Setup switches, so a scripted rollout
can run it unattended:

```powershell
.\overwatch-agent_1.3.3_windows_setup.exe /VERYSILENT /NORESTART
```

A silent install registers and configures the service but does not open the
setup page — fill in `agent.env` by hand or push one out, then `overwatch-agent
start`.

### Editing the configuration by hand

Everything the page writes is plain text in `agent.env`, and you can edit it
directly if you prefer. Open it from an elevated prompt, set at least:

| Setting | Value |
|---|---|
| `CENTRAL_API_URL` | `https://ow2.lasertag.net.au/api/agent/ingest` |
| `AGENT_TOKEN` | the site's token, `OW2_<id>_<secret>` |
| `OZONE_WS_HOST` | `127.0.0.1` on the game server PC, otherwise its LAN IP |

```powershell
notepad "C:\ProgramData\Overwatch Agent\agent.env"
.\overwatch-agent.exe start
.\overwatch-agent.exe status
Get-Content "C:\ProgramData\Overwatch Agent\logs\agent.log" -Tail 50 -Wait
```

Open it from the elevated prompt as above. Do **not** browse to it in Explorer
and click *Continue* on the permission prompt: that grants your account
permanent Full Control of the folder holding the site token and the cached game
data, quietly undoing what the installer set up.

## Where things live

| | Path |
|---|---|
| Executable | `C:\Program Files\Overwatch Agent\overwatch-agent.exe` |
| Configuration | `C:\ProgramData\Overwatch Agent\agent.env` |
| Log | `C:\ProgramData\Overwatch Agent\logs\agent.log` (rotated at 10 MB, five kept) |
| Game cache | `C:\ProgramData\Overwatch Agent\cache\` |
| Unsent telemetry (across restarts) | `C:\ProgramData\Overwatch Agent\buffer.json` |
| Service events | Event Viewer → Windows Logs → Application, source `OverwatchAgent` |

The data directory is `%ProgramData%\Overwatch Agent` unless you install with
`--data-dir <path>`, in which case everything above moves under that path. Give
it a folder of its own: installing takes ownership of that folder and replaces
its permissions, so `install` refuses a drive root, a system folder, or a
folder that already holds files that are not the agent's. The configuration
file can be placed elsewhere with `--config <path>` at install time; keep that
file readable by administrators and the service only, as the installer does for
its own.

## Configuration

The easiest way to change any of this is **Overwatch Agent Setup** in the Start
Menu, which edits the same file and leaves your own comments and any settings it
does not show untouched.

The agent reads `agent.env` at startup: one `KEY=VALUE` per line, `#` starts
a comment, and a value may be quoted. Nothing else is interpreted, so Windows
paths are written as they are (`LOG_FILE=D:\logs\agent.log`). Any variable
already present in the process environment takes precedence over the file.

The settings are the same as everywhere else — see the README's configuration
table — with these defaults on Windows:

| Setting | Windows default | Elsewhere |
|---|---|---|
| `CACHE_DIR` | `<data>\cache` | `./cache` |
| `BUFFER_FILE` | `<data>\buffer.json` | off |
| `PACK_IR_BUFFER_FILE` | `<data>\packir-buffer.json` | off |
| `LOG_FILE` | `<data>\logs\agent.log` | off (console only) |

After changing the file by hand, restart the service:
`.\overwatch-agent.exe restart`. The setup page does that for you.

## Running on the game server PC

When the agent runs on the same machine as the game server, everything
default just works: `OZONE_WS_HOST=127.0.0.1`, no firewall rules, no inbound
ports.

The one exception is the optional **print-server proxy**. Its default listen
address is port `12123`, which on the game server PC is the real print server's
port, so the proxy cannot bind it. Give the proxy another port and point TORN
(or whatever fetches scoresheets) at this machine and that port:

```
ENABLE_CACHE=true
ENABLE_PROXY=true
PROXY_LISTEN_ADDR=0.0.0.0:12124
```

The cache and proxy are opt-in; leave them off unless the venue is being
moved onto the cache.

## Firewall

Windows Firewall blocks inbound connections to a service unless a rule allows
them, and the agent needs no rule for its normal work — it only makes outbound
connections. It does listen locally on `HEALTH_ADDR` (port 8088 on every
interface by default) for its own health check; the firewall keeps that off the
LAN, and setting `HEALTH_ADDR=127.0.0.1:8088` in `agent.env` keeps it off the
LAN whatever the firewall says. Add rules only for what you enable, and only
for the venue LAN — never expose these to the internet:

```powershell
# Print-server proxy, if TORN runs on another machine (use the port you chose)
netsh advfirewall firewall add rule name="Overwatch Agent proxy" dir=in action=allow protocol=TCP localport=12124 profile=private,domain program="C:\Program Files\Overwatch Agent\overwatch-agent.exe"

# Control panel / admin API, if ADMIN_API_ADDR binds a LAN address
netsh advfirewall firewall add rule name="Overwatch Agent control panel" dir=in action=allow protocol=TCP localport=8097 profile=private,domain program="C:\Program Files\Overwatch Agent\overwatch-agent.exe"
```

## Logs

The log is `C:\ProgramData\Overwatch Agent\logs\agent.log`. It rotates at
10 MB into `agent.log.1` … `agent.log.5`, oldest dropped. Follow it live with:

```powershell
Get-Content "C:\ProgramData\Overwatch Agent\logs\agent.log" -Tail 50 -Wait
```

The service also records starting, stopping and a failed start in the Windows
**Application** event log under source `OverwatchAgent`. If the service will
not start, the event says why in one line and the log file has the detail.

## Managing the service

All from an elevated prompt in the install folder (`status` works from any
prompt):

| Command | |
|---|---|
| `.\overwatch-agent.exe setup` | open the configuration page in a browser |
| `.\overwatch-agent.exe status` | state, start type, account, command line, version |
| `.\overwatch-agent.exe start` | start, and wait until it reports running |
| `.\overwatch-agent.exe stop` | stop, and wait until it has drained |
| `.\overwatch-agent.exe restart` | apply a configuration change |
| `.\overwatch-agent.exe version` | the version of the executable |
| `.\overwatch-agent.exe healthcheck` | exit code 0 when the running agent answers |

The service is an ordinary Windows service: *Services* (`services.msc`),
`sc query OverwatchAgent` and `Restart-Service OverwatchAgent` work as usual.

A **Reboot agent** command sent from Overwatch stops the agent gracefully;
the service control manager restarts it a few seconds later, as it would after
any failure. Restarts are attempted after 5 s, 15 s and then every minute.

## Upgrading

Run the new installer. It stops the agent, replaces it and starts it again; the
venue's settings are not asked for a second time.

By hand, from the archive:

1. Download and verify the new release.
2. `.\overwatch-agent.exe stop`
3. Replace `overwatch-agent.exe` in `C:\Program Files\Overwatch Agent\`.
4. `.\overwatch-agent.exe start`

Either way the configuration, cache and log are in the data directory and are
untouched.
The agent reports its version to Overwatch on its next check-in, so the site's
agent version updates on the Sites screen.

## Uninstalling

If you used the installer: **Settings → Apps → Overwatch Site Agent → Uninstall**.
It stops and removes the service, its event log source and the Start Menu entry,
then asks whether to delete the data directory as well. Answer **No** if you are
reinstalling or moving the agent — the site token and the settings are in there.

By hand, or after installing from the archive:

```powershell
.\overwatch-agent.exe uninstall
```

This stops and removes the service, its event log source and the Start Menu
entry. The data directory is **left in place** because it holds the site token
and cached game data; delete `C:\ProgramData\Overwatch Agent\` yourself once
you are sure.

## Security notes

- The service runs as `NT AUTHORITY\LocalService`, not as an administrator or
  the system. It can reach the network and the data directory, and nothing
  else of consequence.
- The data directory is owned by Administrators and restricted at install time
  to Administrators, SYSTEM and **this service's own identity**
  (`NT SERVICE\OverwatchAgent`), because it holds the **site token** and, when
  the cache is enabled, the venue's **game data including player details**.
  Granting the service's own identity rather than LocalService keeps it out of
  reach of the many other Windows services that also run as LocalService. On a
  system that cannot resolve the per-service identity the installer falls back
  to LocalService and says so in its output.
- Edit `agent.env` from an elevated prompt. Explorer's *Continue* button on a
  permission prompt does not open the file — it permanently adds your account
  to the folder's permissions.
- The executable is not code-signed. Windows SmartScreen may warn on first run
  of a downloaded file; verify the SHA-256 against the release's
  `SHA256SUMS.txt` and choose *Run anyway*.
- The only port the agent opens by default is the local health endpoint
  (`HEALTH_ADDR`, `:8088`), which the firewall does not expose and which
  `HEALTH_ADDR=127.0.0.1:8088` binds to the loopback address outright. The
  proxy, the pack-IR listener and the control panel listen only when you turn
  them on; if you do, keep them on the venue LAN as described above.

## Troubleshooting

**Run it in a console** to see exactly what it does. Stop the service first
(they would compete for the same ports), then:

```powershell
.\overwatch-agent.exe stop
.\overwatch-agent.exe run
```

It logs to the console *and* the log file; `Ctrl+C` stops it cleanly. Start
the service again afterwards.

Start with **Overwatch Agent Setup**: its two test buttons say which of the
address, the token and the game server is wrong, and it shows the log without
a terminal.

| Symptom | Look at |
|---|---|
| `start` says the service stopped straight away | the Application event log and the last lines of `agent.log` — usually a missing `CENTRAL_API_URL` / `AGENT_TOKEN`, or a file the service account cannot read. The event log names the reason: the agent checks its configuration before reporting the service as started |
| `install` refuses the folder given to `--data-dir` | it is a drive root, a system folder, or already holds files that are not the agent's — installing would rewrite its permissions. Give the agent an empty folder |
| access-denied error starting the service | the executable is in a user profile: move it under *Program Files* and reinstall |
| `install` says access denied | the prompt is not elevated |
| site stays offline | the setup page's **Test this connection** buttons; in `agent.log` a wrong token is a `401` and a wrong host a `connect failed` |
| the setup page will not open | it needs administrator rights — use the Start Menu entry, or run `overwatch-agent setup` from an elevated prompt |
| proxy failed to start | port `12123` is the real print server's on this PC — set `PROXY_LISTEN_ADDR` |

## Building from source

Any platform with Go 1.25+ can build the Windows executable:

```bash
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X overwatch/agent/internal/version.Value=$(sed -n 's/^var Value = "\(.*\)"$/\1/p' internal/version/version.go)" \
  -o overwatch-agent.exe ./cmd/agent
```

On Windows itself, in PowerShell: `$env:CGO_ENABLED=0; go build -o overwatch-agent.exe ./cmd/agent`.

The installer is [Inno Setup](https://jrsoftware.org/isinfo.php) 6.3 or later
built from [`packaging/windows/overwatch-agent.iss`](packaging/windows/overwatch-agent.iss),
which carries both architectures and installs the one the machine can run. It
takes its payload from the two release archives, so stage those and point the
compiler at them — the script's own header has the exact commands. CI compiles
it on every change, so the script is never first exercised at release time.
