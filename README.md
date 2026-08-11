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
prune      = "auto"         # auto | report | manual  — stale-branch policy (§5.3)
host       = "github"       # default host for bare-name clones
fork_owner = "github:michael-odell"   # forks derive here unless overridden
# push/task_branches (auto|manual|never, auto|report|pull-only) default per
# workflow (§3.6) — override here or per-root/repo when a default doesn't fit.
# force_push/force_pull (branch-name globs, e.g. ["wip/*"]) default to [] —
# never force in either direction unless a branch is explicitly listed (§5.2).

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
| `push`       | `auto` \| `manual` \| `never`                   | important branch's ahead-only commits: push automatically, leave for you, or flag as unexpected — default varies per workflow (§3.6) |
| `task_branches` | `auto` \| `report` \| `pull-only`             | every other local branch: push automatically, defer to PR time, or keep it passively pulled and flag only local commits — default varies per workflow (§3.6) |
| `show_branches` | `none` \| `notable` \| `unmerged` \| `all`    | how much of the branch inventory `sync` lists (below) — default varies per workflow (§5.6) |
| `force_push` | list of branch-name globs                       | branches `sync` may force-push when local history was rewritten (default `[]`, i.e. never) (§5.2) |
| `force_pull` | list of branch-name globs                       | branches `sync` may force-pull/reset when the remote's history was rewritten (default `[]`, i.e. never) (§5.2) |
| `expected_untracked` | list of path globs                      | untracked files that are expected rather than notable — suppresses the report, never the data-safety rules (§3.6) |
| `expected_uncommitted` | list of path globs                    | as above, for tracked files with local modifications (§3.6) |
| `merge_scan_limit` | int                                       | how far apart a branch and its base may be before merge detection skips its expensive patch-comparison tiers: `0` off, `-1` no limit, `N` commits, unset = 1000 (§5.3) |
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

### What `sync` reports about branches

`show_branches` decides how much of a repo's branch inventory is enumerated
under its row. Findings (something happened, or needs to) always show; this
controls the *observations* around them:

| value | lists |
|-------|-------|
| `none` | nothing — the repo row is the whole report |
| `notable` | branches with a finding this run |
| `unmerged` | …plus task branches holding work the important branch lacks |
| `all` | …plus every remaining branch, important ones included, each with the verdict `prune` would act on |

`all` is the one to reach for when you want dispositions — including which
branches have landed and could go:

```
  ⚠    acme/proj                upstream-push  1 branch needs attention
    ⚠    PRECXP-91-spike                       never pushed
    ◦    PRECXP-74-dev-cluster                 1 ahead of main
    ◦    refactor-auth                         merged (squashed) — prunable
    ◦    tidy-logging                          merged — prunable
    ◦    main                                  up to date
```

`merged (squashed)` is the point of the tiered detection (§5.3): `git branch
--merged` only answers the ancestry question, so a squash- or rebase-merged
branch looks like unfinished work forever. These verdicts are the same call
`repo prune` acts on, not a lookalike that might disagree — so the decision can
be watched during ordinary sweeps.

Note this is *not* `task_branches`, which decides what `sync` **does** with
those branches (push them, leave them, keep them pulled). `show_branches`
decides what it **tells you**.

### Environment variables

| variable             | purpose                                       | default             |
|----------------------|-----------------------------------------------|---------------------|
| `REPO_REGISTRY_PATH` | registry fragment files/dirs to merge         | `~/.config/repo`    |
| `REPO_ROOTS`         | override directories to scan for repos        | the `[root.*]` dirs |
| `REPO_OUT`           | where generated shell artifacts are written   | `~/.local/repo`     |
| `REPO_GIT_TIMEOUT`   | deadline for local git invocations            | `2m`                |
| `REPO_GIT_NETWORK_TIMEOUT` | deadline for git invocations that reach a remote | `10m`     |
| `REPO_MERGE_SCAN_LIMIT` | default `merge_scan_limit` for repos config doesn't set one on | `1000` |

`REPO_REGISTRY_PATH` and `REPO_ROOTS` are colon-separated path-style lists;
`REPO_OUT` is a single directory. The timeouts take Go durations (`90s`, `5m`).

## Commands

Run `repo --help` for the full list and `repo <command> --help` for details.

- `status` — report drift across the declared ∪ discovered union (read-only)
- `scan` — walk the discovery roots and list every repo found, with its inferred
  id, effective (inherited) workflow, and root
- `sync` — reconcile repos toward the registry; `--fix` migrates a container to its
  configured layout (single ↔ worktree, data-safe) after history is pushed. Takes
  positional root/path/name selectors. While it runs, a live block on stderr
  shows progress and gives every repo in flight its own line with how long it
  has been running; `--verbose` explains the decision for every repo and gives
  each one's duration.
- `apply` — regenerate the shell navigation/completion artifacts into `$REPO_OUT`
  from the declared ∪ discovered union
- `list` — enumerate the declared ∪ discovered union (for completion)
- `resolve` — resolve a declared or discovered repo's name to its physical URL
  (debug)
- `version` — print version information
- `clone`, `prune`, `home`, `path`, `review` — planned, not yet implemented

## Build

Local, plain build:

```sh
go build ./cmd/repo
```

or `just build`. See the `justfile` for other targets:

- `just check` — vet, build, test (same as CI)
- `just release-build` — multi-platform goreleaser build into `dist/`, unpublished, for
  testing the release artifacts locally (requires `goreleaser`, e.g. `brew install goreleaser`)
- `just tag-patch` / `just tag-minor` / `just tag-major` — tag a new patch, minor, or
  major version off the latest tag (e.g. `v0.2.5` -> `v0.2.6`, `v0.3.0`, or `v1.0.0`)
  and optionally push it
- `just release` — real goreleaser release + publish to GitHub Releases; requires an
  annotated `vX.Y.Z` tag on `HEAD` and `GITHUB_TOKEN`. This is also what CI runs on tag
  push (`.github/workflows/release.yml`)

Release builds (static, `CGO_ENABLED=0`, multi-platform) are produced only in CI
via goreleaser and published to GitHub Releases.
