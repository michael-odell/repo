# repo

A personal git repository manager: it knows the collection of git repositories on
a machine (declared in a registry, discovered on disk) and maintains them —
provisioning, syncing important branches, updating forks, tracking supply-chain
mirrors, pinning vendored deps, and generating shell artifacts for navigation and
completion.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- Implementation plan: [docs/PLAN.md](docs/PLAN.md)

Status: **early implementation** (Stage 0 — skeleton).

## Configuration

The registry is a TOML file (or a directory of `*.toml` fragments) describing your
repos. By default `repo` reads `~/.config/repo/` (every `*.toml` inside is merged).
See [docs/DESIGN.md §3](docs/DESIGN.md) for the schema; the `layout` key selects the
on-disk layout (`flat` → `~/src/<repo>`, `owner` → `~/root/<owner>/<repo>`).

Behavior is tuned by a few environment variables (all path-style, colon-separated,
except `REPO_OUT`):

| variable             | purpose                                            | default            |
|----------------------|----------------------------------------------------|--------------------|
| `REPO_REGISTRY_PATH` | registry fragment files/dirs to merge              | `~/.config/repo`   |
| `REPO_ROOTS`         | directories to scan for repos                      | registry `home_root`s |
| `REPO_OUT`           | where generated shell artifacts are written        | `~/.local/repo`    |

Run `repo --help` for the command list and `repo <command> --help` for details.

## Build

Local, plain build:

```sh
go build ./cmd/repo
```

Release builds (static, `CGO_ENABLED=0`, multi-platform) are produced only in CI
via goreleaser and published to GitHub Releases.
