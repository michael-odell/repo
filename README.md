# repo

A personal git repository manager: it knows the collection of git repositories on
a machine (declared in a registry, discovered on disk) and maintains them —
provisioning, syncing important branches, updating forks, tracking supply-chain
mirrors, pinning vendored deps, and generating shell artifacts for navigation and
completion.

- Design: [docs/DESIGN.md](docs/DESIGN.md)
- Implementation plan: [docs/PLAN.md](docs/PLAN.md)

Status: **early implementation.** `status`, `scan`, `sync` (incl. `--fix` layout
migration), `prune`, `apply`, and `config` work; `clone`, `home`, `path`, and
`review` are stubbed.

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
prune      = "report"       # report | interactive | auto | manual  (§5.3)
host       = "github"       # default host for bare-name clones
fork_owner = "github:michael-odell"   # forks derive here unless overridden
# push/task_branches (auto|manual|never, auto|report|pull-only) default per
# workflow (§3.6) — override here or per-root/repo when a default doesn't fit.
# force_push/force_pull (branch-name globs, e.g. ["wip/*"]) default to [] —
# never force in either direction unless a branch is explicitly listed (§5.2).
# tags defaults to ["*"]; force_tags to [] — an upstream that rewrites a tag is
# reported and not followed until you name the tag (§5.2). Tag globs are git
# refspec globs: one "*" per pattern, no "?" or "[...]".

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

`[dir.<name>]` is a lighter sibling: a settings-only overlay on part of a root's
tree, for when a subtree deserves its own settings but not a second scan location
— the owner-layout case, where one owner under a root wants its own `workflow`/
`pin`/`branches` without inventing a whole new root name and re-walking ground the
parent root already covers:

```toml
[root.contrib]
dir      = "~/contrib"
layout   = "owner"
workflow = "upstream-push"

[dir.contrib-prometheus]   # settings-only: not a scan location
dir      = "~/contrib/prometheus"
workflow = "vendor"
pin      = "latest-tag"
repos    = ["github:prometheus/prometheus"]
```

It takes a `dir` (required, and must sit under some root's `dir`) plus the same
settings bundle and membership shape as a root — except `layout`, which stays
root-only: a repo's container path is computed from its root's `dir` + `layout` +
identity alone, never from anything nested under it, so `sync --fix`'s location
reconciliation never depends on an override only reachable once a repo is already
there. `worktrees` has no such restriction — it reshapes a container in place
rather than relocating it. `[dir.*]` names are their own namespace, unrelated to
`[root.*]` names or to any directory name; a `[dir.contrib]` and a `[root.contrib]`
may coexist without colliding.

### Settings reference

Every field below is valid in `[defaults]`, any `[root.*]` or `[dir.*]`, or a
`[[root.*.repo]]`/`[[dir.*.repo]]` table (a repo table also takes `id` and `fork`;
a root or dir also takes `dir`, `repos`) — except `layout`, which `[dir.*]`
cannot set.

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
| `tags`       | list of tag-name globs                          | which tags to fetch at all (default `["*"]`, i.e. every tag; `[]` fetches none) (§3.6) |
| `force_tags` | list of tag-name globs                          | of those, which may be overwritten when upstream moves a tag it already published — the tag counterpart to `force_pull`, and the only setting that follows a moved tag, in every workflow (default `[]`, i.e. never) (§5.2) |
| `expected_untracked` | list of path globs                      | untracked files that are expected rather than notable — suppresses the report, never the data-safety rules (§3.6) |
| `expected_uncommitted` | list of path globs                    | as above, for tracked files with local modifications (§3.6) |
| `merge_scan_limit` | int                                       | how far apart a branch and its base may be before merge detection skips its expensive patch-comparison tiers: `0` off, `-1` no limit, `N` commits, unset = 10000 (§5.3) |
| `prune`      | `report` \| `interactive` \| `auto` \| `manual`   | what a sweep does about landed local branches: name them, walk them with you, remove the ones that clear the unattended bar, or don't look (§5.3) |
| `prune_keep` | list of branch-name globs                       | branches prune never removes whatever the tiers concluded — a name-based veto over inference (default `[]`) (§5.3) |
| `prune_min_age` | a duration (`14d`, `2w`, `48h`)              | how long a ref must have sat still before prune will remove it; measured from the later of the tip's date and the ref's last movement (default unset, i.e. no age gate) (§5.3) |
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
    ◦    refactor-auth                         merged (rewritten) — prunable
    ◦    tidy-logging                          merged — prunable
    ◦    main                                  up to date
```

`merged (rewritten)` is the point of the tiered detection (§5.3): `git branch
--merged` only answers the ancestry question, so a squash- or rebase-merged
branch looks like unfinished work forever. The two verdicts differ in who can
vouch for the branch, not in how it was merged:

| verdict | means | to prune |
|---------|-------|----------|
| `merged` | base literally contains the branch's commits | `git branch -d` accepts it too |
| `merged (rewritten)` | the branch's content is in base under other SHAs | needs `-D`; git's own check can't see the merge |

`repo` deliberately won't say *which* rewrite it was. Squashing a single-commit
branch produces that commit's exact patch, so on the object graph it is the same
thing as a rebase or a cherry-pick — the SHA changed and the content landed, and
anything more specific would be a guess.

These verdicts are the same call `repo prune` acts on, not a lookalike that might
disagree — so the decision can be watched during ordinary sweeps. Every sweep
also ends with a count of what prune could remove, so candidates are visible
without setting `show_branches = "all"`:

```
0 failed · 3 flagged · 0 review pending · 0 deferred · 1 updated · 20 up to date
12 branch(es) prunable across 3 repo(s) — repo prune
```

### Pruning: what it takes to remove a branch

A landed branch is removed with `git branch -d` wherever git's own ancestry
check agrees, so two independent judgements stand behind the deletion. The
rewritten tiers need `-D`, where that second judgement isn't available — so
before any force-delete, `repo` corroborates by a different route entirely:
it reverse-applies the branch's whole diff to a scratch index built from the
important branch (`git apply --cached --check -R`, sharing no code with the
patch-id tiers). If that fails, the branch stays. Nothing in your repo takes
part — not the index, not any working tree.

Every deletion is written to `$XDG_STATE_HOME/repo/prune.log` (default
`~/.local/state/repo/prune.log`) — tab-separated, one line per branch:

```
2026-08-12T09:14:03Z  acme/noodle  refactor-auth  9f3a1c2…  merged (rewritten)  --delete  git branch refactor-auth 9f3a1c2…
```

The restore command is in the record because the moment you need it is the
moment you least want to reconstruct it. A journal that can't be written stops
the run rather than deleting unrecorded.

Two settings hold branches back regardless of what the tiers concluded:
`prune_keep` (name globs) and `prune_min_age` (how long a ref must have sat
still). They don't dispute the merge verdict — a held branch still reads
`merged — kept (prune_keep)`.

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
| `REPO_MERGE_SCAN_LIMIT` | default `merge_scan_limit` for repos config doesn't set one on | `10000` |

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
  each one's duration. `-n`/`--dry-run` still fetches for real (branches only,
  never tags, never a remote it would have to create) so its report reflects
  current upstream state rather than the last real sync's — it just never
  moves a branch, pushes, converts a layout, checks out a pin, or runs a hook.
- `apply` — regenerate the shell navigation/completion artifacts into `$REPO_OUT`
  from the declared ∪ discovered union
- `list` — enumerate the declared ∪ discovered union (for completion)
- `resolve` — resolve a declared or discovered repo's name to its physical URL
  (debug)
- `config <id|name|path>` — print one repo's fully resolved settings as TOML
  (config in, config out) — the same shape you could paste as a
  `[[root.*.repo]]`/`[[dir.*.repo]]` override. `--explain` instead prints one line
  per field: its value, and which link in the root/dir chain last set it.
- `prune` — report which local branches have landed, and remove them when asked.
  Report-only by default. `--delete` asks per branch with the evidence in front
  of you (`y`/`n`/`a`/`q`); `--yes` skips the questions; `--dry-run` shows what
  deleting would do without touching anything; `--explain <branch>` prints the
  reasoning behind one verdict. Every deletion is recorded (below).
- `version` — print version information
- `clone`, `home`, `path`, `review` — planned, not yet implemented

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
