# repo

A personal git repository manager: it knows the collection of git repositories on
a machine (declared in a registry, discovered on disk) and maintains them —
provisioning, syncing important branches, updating forks, tracking supply-chain
mirrors, pinning vendored deps, and generating shell artifacts for navigation and
completion.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- Implementation plan: [docs/PLAN.md](docs/PLAN.md)

Status: **early implementation.** `status`, `scan`, `sync` (incl. `--fix` layout
migration), and `apply` work; `clone`, `prune`, `home`, `path`, and `review` are
stubbed.

## Membership: declared ∪ discovered

The registry is not the sole source of truth. The operational set is the union of
**declared** repos (listed in config, so they get provisioned on a fresh machine or
carry non-default metadata) and **discovered** repos (anything found on disk under a
configured root's `dir`). A clone already carries most of what the registry would
state — its identity from `origin`, its settings from the root whose `dir` is the
deepest prefix of its path, its workflow inferred from its remotes — so ordinary
cloning stays config-free. You only write config to *provision* a repo or to
*override* what disk/inference would otherwise conclude. See DESIGN §3.2.

## Configuration

The registry is a TOML file, or a path-style list of `*.toml` fragments merged
together (`REPO_REGISTRY_PATH`, colon-separated; every `*.toml` inside a listed
directory is included). By default `repo` reads `~/.config/repo/`. The loader
**rejects unknown keys** and validates the merged result (root structure, enum
values, identity/fork parsing, host resolvability), so a broken or stale config
fails loudly instead of misbehaving.

### Shape of a registry

```toml
# ── defaults: the settings bundle every root/repo inherits unless overridden ──
[defaults]
worktrees  = false          # single working tree (true → bare + worktree-per-branch)
branches   = ["main"]       # important branches, synced/worktree'd
on_rewrite = "stop"         # stop | follow      — history-rewrite policy (§5.2)
prune      = "auto"         # auto | report | manual  — stale-branch policy (§5.3)
host       = "github"       # default host for bare-name clones
fork_owner = "github:michael-odell"   # forks derive here unless overridden

# ── hosts: an identity's host key → the physical base URL (per machine) ──
[hosts.github]
base = "git@github.com:"
[hosts.ghe]
base = "git@ghe.example.com:"

# ── roots: named directory nodes; settings inherit down the tree by dir prefix ──
[root.src]
dir      = "~/src"          # a root's dir IS its home
layout   = "flat"          # flat → <dir>/<repo>   (owner → <dir>/<owner>/<repo>)
workflow = "upstream-push"
repos = [                   # normal repos: one bare id per line, everything inherited
  "github:michael-odell/repo",
  "github:michael-odell/homelab",
]

[root.contrib]
dir      = "~/contrib"
layout   = "owner"
workflow = "vendor"
pin      = "latest-tag"     # vendor only: a branch, a tag, or latest-tag (§3.6)
repos = [
  "github:prometheus/prometheus",
]

[root.plugins]
dir    = "~/.zsh/plugins"
layout = "flat"
repos = [
  "github:michael-odell/zsh-history",
]

# an exception: a [[root.<name>.repo]] table carries ONLY the irreducible fields
# (containment sets membership; location/host/layout/fork are still inherited/derived)
[[root.plugins.repo]]
id       = "github:romkatv/powerlevel10k"
workflow = "supply-chain-mirror"   # undetectable until the untrusted remote exists
branches = ["master"]
# fork → derived as github:michael-odell/powerlevel10k
```

### Roots and inheritance

Configuration attaches to **roots**, not tags. A root is `[root.<name>]` with a
`dir` (required) plus any part of the settings bundle. Settings flow *down the
directory tree*: for a given repo,

    [defaults] → every root whose dir is a prefix of the repo's path
                 (shallowest → deepest) → the repo's own entry

with the **longest matching prefix winning per field**. So a nested root
(`dir = "~/contrib/prometheus"` under `dir = "~/contrib"`) overrides its parent for
that subtree with no new mechanism. A root holds its declared members two ways:

- `repos` — an array of bare `host:owner/name` id strings (the common case).
- `[[root.<name>.repo]]` — tables for exceptions, carrying only fields that can't be
  derived (a `workflow` not yet detectable, a non-default `branches`, an off-pattern
  `fork`). The `id` field is required; everything derivable is still derived.

### Settings reference

Every field below is valid in `[defaults]`, any `[root.*]`, or a `[[root.*.repo]]`
table (a repo table also takes `id` and `fork`; a root also takes `dir`, `repos`).

| field        | values                                          | meaning |
|--------------|-------------------------------------------------|---------|
| `layout`     | `flat` \| `owner`                               | `<dir>/<repo>` vs `<dir>/<owner>/<repo>` |
| `worktrees`  | bool                                            | single tree vs bare + one worktree per important branch |
| `branches`   | list of strings                                 | important branches (synced, given worktrees) |
| `workflow`   | `upstream-push` \| `fork-pr` \| `supply-chain-mirror` \| `vendor` | remote contract (below) |
| `on_rewrite` | `stop` \| `follow`                              | what to do when a synced branch's history was rewritten |
| `prune`      | `auto` \| `report` \| `manual`                  | stale local-branch handling |
| `host`       | a `[hosts.*]` key                               | default host for bare-name clones |
| `fork_owner` | `host:owner`                                    | derive a fork as `<fork_owner>/<name>` when the workflow needs one |
| `pin`        | branch \| tag \| `latest-tag`                   | vendor only: what to track |
| `hooks`      | list of `{ after = "...", run = "..." }`        | commands run after a lifecycle event (e.g. `after = "fetch"`) |

### Workflows and forks

Each workflow is a **remote contract** — the named remotes it manages. `sync --fix`
reconciles only those names; any other remote you add is left alone.

| workflow              | managed remotes                            | intent |
|-----------------------|--------------------------------------------|--------|
| `upstream-push`       | `origin` = definitive                      | push branches straight to origin |
| `fork-pr`             | `origin` = fork, `upstream` = definitive   | push to your fork, PR to upstream |
| `supply-chain-mirror` | `origin` = fork, `untrusted` = definitive  | track an untrusted source, advance only after review (§5.4) |
| `vendor`              | `origin` = definitive (read-only), pinned  | pulled to match `pin`; never pushed |

Workflow is resolved first and independently: explicit repo `workflow` → root →
inference from existing remotes (`origin` only → `upstream-push`; `origin`+`upstream`
→ `fork-pr`; `origin`+`untrusted` → `supply-chain-mirror`) → default. Only *then*, if
the chosen workflow needs a fork, is the fork resolved: an explicit per-repo `fork` →
else derived from the effective `fork_owner` → else a config error. An explicit
per-repo `fork` may imply `fork-pr`; an ambient `fork_owner` never does (it only
*supplies* a fork a workflow already requires). See DESIGN §3.6.

### Per-machine resolution overlay

To keep a shared registry machine-independent, a local-only fragment can fold logical
identities onto a private host without touching identity:

```toml
[resolve]
via      = "gogsprod:mirrors/"   # physical = hosts[gogsprod].base + "mirrors/" + owner/repo
apply_to = "*"                   # root names, or "*"
[resolve.overrides]
"ghe:cban-ops/pt-helm" = "gogsprod:team/pt-helm"
```

Resolution: `overrides[id]` → else `via + owner/repo` when matched by `apply_to` →
else `hosts[id.host].base + owner/repo`. See DESIGN §3.7 for keeping private repos out
of public dotfiles (private fragments contribute hosts, roots, and defaults; a private
machine's repos can be purely discovered so no private name is written down anywhere).

### Environment variables

| variable             | purpose                                       | default             |
|----------------------|-----------------------------------------------|---------------------|
| `REPO_REGISTRY_PATH` | registry fragment files/dirs to merge         | `~/.config/repo`    |
| `REPO_ROOTS`         | override directories to scan for repos        | the `[root.*]` dirs |
| `REPO_OUT`           | where generated shell artifacts are written   | `~/.local/repo`     |

`REPO_REGISTRY_PATH` and `REPO_ROOTS` are colon-separated path-style lists;
`REPO_OUT` is a single directory.

## Commands

Run `repo --help` for the full list and `repo <command> --help` for details.

- `status` — report drift across the declared ∪ discovered union (read-only)
- `scan` — walk the discovery roots and list every repo found, with its inferred
  id, effective (inherited) workflow, and root
- `sync` — reconcile repos toward the registry; `--fix` migrates a container to its
  configured layout (single ↔ worktree, data-safe) after history is pushed. Takes
  positional root/path/name selectors.
- `apply` — regenerate the shell navigation/completion artifacts into `$REPO_OUT`
- `list` / `resolve` / `version` — completion and debug helpers
- `clone`, `prune`, `home`, `path`, `review` — planned, not yet implemented

## Build

Local, plain build:

```sh
go build ./cmd/repo
```

Release builds (static, `CGO_ENABLED=0`, multi-platform) are produced only in CI
via goreleaser and published to GitHub Releases.
