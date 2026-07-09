# repo

A personal git repository manager: it knows the collection of git repositories on
a machine (declared in a registry, discovered on disk) and maintains them —
provisioning, syncing important branches, updating forks, tracking supply-chain
mirrors, pinning vendored deps, and generating shell artifacts for navigation and
completion.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- Implementation plan: [docs/PLAN.md](docs/PLAN.md)

Status: **early implementation** (Stage 0 — skeleton).

## Build

Local, plain build:

```sh
go build ./cmd/repo
```

Release builds (static, `CGO_ENABLED=0`, multi-platform) are produced only in CI
via goreleaser and published to GitHub Releases.
