# gorilla/websocket v1.5.3, patched for Overwatch

This is the package source of `github.com/gorilla/websocket` **v1.5.3** (the
latest tagged release), with one change. `agent/go.mod` and `simulator/go.mod`
use it in place of the published module through a `replace` directive.

## Why

[GO-2026-6278](https://pkg.go.dev/vuln/GO-2026-6278)
(GHSA-w67g-5rqw-f597): v1.5.3 makes each client frame's mask key with
`math/rand.Uint32`, not a cryptographic generator. v1.5.1 and v1.5.2 had
fixed this, and v1.5.3 brought it back. The vulnerability database's "fixed"
version, `v1.5.4-0.20240701034025-d67f41855da4`, is an untagged commit on
upstream's main branch, and it changes much more than the mask key:

- about 140 changed lines in `client.go`, `conn.go`, `proxy.go` and
  `server.go`, mostly the dial path;
- `x_net_proxy.go` (473 lines) deleted in favour of a new `golang.org/x/net`
  dependency.

That code is the agent's connection to the game server, so the owner chose
(25 Nov 2026) to patch the one function instead.

## The change

`conn.go`: upstream's own fix from commit d67f418, applied to v1.5.3. A
package variable `maskRand = crypto/rand.Reader` is added, and `newMaskKey()`
fills its four bytes from it with `io.ReadFull`. The import changes from
`math/rand` to `crypto/rand`. Nothing else in the code differs from v1.5.3.

`doc.go` was reformatted by `gofmt` (comments only) so the agent's
`gofmt -l .` check passes. The tests, examples and CI files of the upstream
module are left out; this directory has its own `go.mod`, so the agent's
`go vet ./...` and `go test ./...` don't reach into it.

`overwatch.patch` is that exact difference (conn.go and doc.go) against
upstream v1.5.3. Change this copy only together with it.

Checked against upstream's v1.5.3 test suite (run locally, not committed),
and everything passes with `-race`, with two notes:

- `TestPreparedMessage` must swap `maskRand` for a seeded `math/rand` source,
  as upstream's d67f418 version of that test does. v1.5.3's version reseeds
  the global `math/rand`, which no longer feeds the mask.
- `TestProxyDial` and `TestProxyAuthorizationDial` fail identically on
  unpatched v1.5.3 under Go 1.26, so they are unrelated to the change.

## Why govulncheck alone can't watch this module

govulncheck looks a module up by its replacement path, and this one is a local
directory. With the `replace` in place, it never asks the vulnerability
database about gorilla/websocket at all. It would report "No vulnerabilities
found" even if this copy lost its patch, or if a new advisory came out against
v1.5.3.

`audit.sh` covers that gap. CI runs it next to govulncheck: the Dependency
audits job in the monorepo, and the Vulnerability scan job in the public
agent repo. It does three things:

1. Fails unless this directory is exactly upstream v1.5.3's package files plus
   `overwatch.patch`.
2. Runs govulncheck on scratch copies of the modules with the `replace`
   removed, after making sure they then resolve to v1.5.3. It fails on any
   advisory other than GO-2026-6278.
3. Warns when upstream has tagged a release after v1.5.3, including one that
   retracts v1.5.3, and when the release list can't be fetched.

Run it by hand from the monorepo root:

```bash
agent/third_party/websocket/audit.sh agent simulator
```

In the public agent repo, run `third_party/websocket/audit.sh .` instead.

## When to remove it

Once upstream tags a release with the fix (v1.5.4 or later; `audit.sh` warns
when one appears), and its changes have been tested against the agent:

1. Delete the `replace` lines from `agent/go.mod` and `simulator/go.mod`.
2. Require the new version, then run `go mod tidy` in both.
3. Delete this directory, and the lines that copy it in all four Dockerfiles:
   - `agent/Dockerfile` (`COPY third_party ./third_party`);
   - `docker/agent/Dockerfile` (`COPY agent/third_party ./third_party`);
   - `docker/simulator/Dockerfile` (`COPY agent/third_party/websocket …`; its
     `WORKDIR /src/simulator` can stay);
   - `agent-public-mirror/Dockerfile`, which is a hand-kept overlay: edit it by
     hand, since the sync script keeps it as it is.
4. Remove the "Patched gorilla/websocket" step from `.github/workflows/ci.yml`
   and `agent-public-mirror/.github/workflows/ci.yml`.
5. Run `govulncheck ./...` in `agent/` and `simulator/`, then
   `scripts/sync-agent-mirror.sh`.

## Licence

BSD 2-Clause, see `LICENSE`, which is kept as upstream ships it. Copyright
The Gorilla WebSocket Authors (`AUTHORS`).
