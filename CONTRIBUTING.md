# Contributing

Thanks for your interest in improving the **Overwatch Site Agent**! This
repository is the public release of the per-venue agent. Bug reports, fixes, and
documentation improvements are all welcome.

By participating, you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md).

## Reporting bugs & requesting features

- **Security issues:** do **not** open a public issue — follow
  [SECURITY.md](SECURITY.md) (private reporting via the Security tab).
- **Bugs / features:** open an issue using the templates. Include the agent
  version / image tag, which optional features were enabled (proxy, cache,
  message bus, admin API), and relevant logs **with any tokens/secrets redacted**.

## Development setup

You need **Go 1.24+**.

```bash
git clone https://github.com/DorwardTech/Overwatch2-Agent.git
cd Overwatch2-Agent
go build ./cmd/agent
```

Run the checks CI enforces before opening a PR:

```bash
gofmt -l .      # must print nothing (use `gofmt -w .` to fix)
go vet ./...
go test ./...
```

The repo ships an in-process fake O-Zone (`internal/ozonesim`) used by the tests,
so the cache/proxy paths can be exercised without laser-tag hardware.

## Making changes

- Keep pull requests **focused** — one logical change per PR.
- Match the existing style: run `gofmt`, write idiomatic Go, prefer table-driven
  tests, and keep the agent **dependency-light**.
- **Add or update tests** for any behaviour change.
- If you add or change configuration, update **both** `.env.example` **and** the
  README configuration table — defaults must match `internal/config`.
- Never commit secrets. Tokens (`AGENT_TOKEN`, `ADMIN_API_TOKEN`) are supplied via
  environment variables only.
- Write clear commit messages that explain the *why*, not just the *what*.

## Submitting a pull request

1. Fork the repo and create a branch from `main`.
2. Make your change; ensure `gofmt`, `go vet`, and `go test ./...` all pass.
3. Open a PR and fill in the template. Link any related issue.
4. CI builds the agent image — keep it green.

For substantial or cross-cutting changes, please open an issue first to discuss
the approach with the maintainers.

## Contributions & licensing

This project is **source-available and proprietary** (see [LICENSE](LICENSE)).
By submitting a contribution, you certify that you wrote it (or otherwise have
the right to submit it), and you grant the copyright holder a perpetual,
irrevocable, worldwide right to use, modify, and distribute it as part of this
project under the project's license.
