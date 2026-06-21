# Security Policy

The Overwatch Site Agent is an unattended, per-venue daemon. It connects out to
the Overwatch central server (HTTPS) and to the local O-Zone server (LAN), and —
when the cache/proxy is enabled — exposes a print-server proxy on the venue LAN.
We take its security seriously and appreciate responsible disclosure.

## Reporting a vulnerability

**Please report security issues privately — do not open a public GitHub issue,
pull request, or discussion for a suspected vulnerability.**

Use GitHub's private vulnerability reporting:

1. Go to the **[Security](https://github.com/DorwardTech/Overwatch2-Agent/security)**
   tab of this repository.
2. Click **“Report a vulnerability”**.
3. Describe the issue (see below).

This opens a private advisory visible only to you and the maintainers.

Please include, where possible:

- A clear description of the issue and its impact.
- Steps to reproduce (or a proof of concept).
- Affected version / image tag or commit SHA, and configuration (which features
  were enabled, e.g. `ENABLE_PROXY`, `ENABLE_CACHE`, `ADMIN_API_ADDR`).
- Any suggested remediation.

**What to expect:**

- Acknowledgement within **3 business days**.
- An initial assessment (severity, affected versions) within **7 business days**.
- Coordinated disclosure: we will agree a fix and a disclosure timeline with you
  before any public details are published, and credit you if you wish.

Please give us a reasonable opportunity to remediate before any public
disclosure.

## Supported versions

This project ships as a rolling release. Security fixes land on `main` and are
published to the container image as `:latest`.

| Version | Supported |
|---|---|
| Latest `main` / `ghcr.io/dorwardtech/overwatch2-agent:latest` | ✅ |
| Older image tags / commits | ❌ (please update) |

Always deploy the latest image; older pinned tags do not receive backported
fixes.

## Scope

**In scope** — this repository (the agent):

- The agent binary and its Go source (`cmd/`, `internal/`).
- The agent's network surfaces: the central HTTPS client, the O-Zone clients
  (WebSocket / print server / message bus), the optional print-server **proxy**
  (TCP `12123`), and the optional **admin API** (HTTP).
- The container image and its build (`Dockerfile`, the publish workflow).
- Handling of secrets and cached data by the agent.

**Out of scope:**

- The **Overwatch central** backend — a separate system. Report central-side
  issues through its own channel, not here.
- **O-Zone** itself and the laser-tag hardware.
- Vulnerabilities in third-party dependencies — please report those upstream
  (we will, of course, pull in fixed versions).
- Findings that require an already-compromised host, physical access, or a
  privileged position on the trusted venue LAN beyond what the design assumes
  (see below).

## Security model & hardening

Context that helps when assessing a report:

- **No inbound ports by default.** The agent only makes outbound connections
  (HTTPS to central, LAN to O-Zone). Inbound listeners exist *only* when you opt
  in: the print-server proxy (`ENABLE_PROXY`), the admin API (`ADMIN_API_ADDR`),
  and the health endpoint (`HEALTH_ADDR`).
- **The print-server proxy is intentionally unauthenticated.** It re-speaks
  O-Zone's plain-TCP print-server protocol so existing scoring software (TORN)
  can connect to it unmodified — that software cannot present a credential. It
  **must** be bound to the trusted venue LAN and **never** be exposed to the
  public internet. Treat exposing `12123` to untrusted networks as a
  misconfiguration, not an agent vulnerability.
- **The admin/control API requires a bearer token** (`ADMIN_API_TOKEN`) and only
  starts when both its address and token are set. It can mutate the cache, so
  protect the token like a password and bind the API to a trusted interface.
- **Secrets are environment-only.** `AGENT_TOKEN` and `ADMIN_API_TOKEN` are
  supplied via environment variables; none are baked into the image or committed
  to this repository. Do not commit real tokens to `.env`.
- **Hardened runtime.** The published image is distroless, runs as a non-root
  user, and the provided `docker-compose.yml` runs it with a read-only root
  filesystem.
- **Cached data.** When the cache is enabled, the agent stores O-Zone game
  payloads on disk (and optionally backs them up to central). These can contain
  player aliases and member IDs — keep the cache volume and any admin API access
  appropriately restricted.

## Good practice for operators

- Keep the proxy and admin API on the venue LAN; do not port-forward `12123`.
- Use a strong, unique `ADMIN_API_TOKEN` (e.g. `openssl rand -hex 32`) and rotate
  it by changing the variable and restarting the agent.
- Run the latest image so you receive security fixes.
