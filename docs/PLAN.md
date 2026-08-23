# Implementation plan

Companion to [DESIGN.md](DESIGN.md). Staged so that **all read-only work lands
first** — parsing, discovery, drift, and the shell contract are proven and tested
before `sync` makes its first write. Each stage is independently committable and
useful.

## Foundational decisions

- **Module:** `github.com/michael-odell/repo`, go directive `1.24` (floor).
- **Builds:** local development uses a plain `go build ./cmd/repo`. Static
  (`CGO_ENABLED=0`), multi-platform release binaries are produced **only in CI**
  via goreleaser and published to GitHub Releases; the bootstrap shim downloads and
  checksum-verifies those. (Downloading releases needs GitHub creds for now, which
  is acceptable — dotfiles already needs them; the repo may go public later.)
- **Git & gh via CLI shell-out**, never `go-git`. `gh` is used when available and
  only against GitHub / GitHub-Enterprise hosts; otherwise fall back to
  fetch+push.
- **Dependencies, minimal:** a TOML library (`BurntSushi/toml`) + `x/sync/errgroup`
  (bounded-parallel sweeps with per-repo error capture). Stdlib `flag` + a small
  dispatch table for the CLI — no Cobra (completions are hand-written and read
  `REPO_HOME`).

## Package layout

```
cmd/repo/           # main: dispatch, subcommands
internal/
  ident/            # host:owner/repo parse, short-name/ambiguity
  config/           # TOML types, REPO_REGISTRY_PATH composition, defaults->tag->repo merge, overlay
  resolve/          # logical id -> physical URL (via/overrides/hosts)
  discover/         # walk REPO_ROOTS, read remotes, infer identity/tag/workflow
  gitx/             # thin git-CLI wrapper
  model/            # merged Repo (declared U discovered) + workflow enum
  sync/             # engine: provision/fetch/update/hooks/drift/prune, per-workflow, isolation, report
  artifact/         # emit prjpath.zsh / homes.zsh / plugins.zsh + staleness hash
  report/           # summary rendering
```

## Stages

### Stage 0 — skeleton  ✅ done
Module, layout, `flag` dispatch with stubbed subcommands, `repo version`, CI
(vet/build/test) + release workflow (goreleaser).
**Proof:** `repo --help` / `repo version` build and run.

### Stage 1 — config core (pure, no git)  ✅ done
`ident` parsing; TOML types; fragment composition over `REPO_REGISTRY_PATH`;
`defaults -> tag -> repo` inheritance; `[hosts.*]`; resolution overlay. Heavy unit
tests (pure logic).
**Proof:** `repo list` and a debug `repo resolve <id>` load the DESIGN §3.8 example
and print resolved URLs, including the constrained-box `via` fold.

### Stage 2 — git layer + discovery + `status` (read-only)  ✅ done
`gitx` wrapper; `discover` over `REPO_ROOTS`; union model; live drift (`rev-list`
ahead/behind, dirty). Sweep isolation (errgroup + per-repo capture + summary) lands
here — `status` is the safe place to prove one failure never aborts the sweep (the
`wd-repos-update` fix).
**Proof:** `repo status` over real `~/src`/`~/wd`, mutates nothing, reports drift.

### Stage 3 — artifacts (`repo apply`)  ✅ done · shell wiring pending review
`repo apply` generates `prjpath.zsh`/`homes.zsh`/`plugins.zsh` into `~/.local/repo`
with the staleness hash; wire `.zshenv`/`.zshrc` to source-with-fallback; teach
`cs`/`_cs` to prefer `REPO_HOME`. Fully reversible.
**Proof:** new shells source generated artifacts; `cs pt-<TAB>` completes from the
map; plugin list is generated — while `sync` still doesn't exist.

### Stage 4 — `sync` engine, scoped to plugins (first mutation)  ✅ done
provision (clone/remotes), fetch, `upstream-push` + `supply-chain-mirror` updates,
`on_rewrite`, drift, prune tiers, `--if-due` cadence, `-n` dry-run, report; scope to
`--tag zsh-plugin`. Then flip `plugins-update` to delegate.
**Proof:** `repo sync --tag zsh-plugin` clones/updates plugins; a mirror plugin with
upstream ahead shows "review pending" and does not advance. **This is the vertical
slice validating the whole architecture end to end.**

**Update:** `sync` is no longer scoped to plugins — it now operates over the full
declared ∪ discovered union (DESIGN §3.2), the same set as `status`, via a shared
`unionRepos` builder. Discovered-only repos carry their real on-disk location and
existing origin. `fork-pr`, `vendor`, and worktrees, originally deferred to
Stage 6, are implemented now (see Stage 6 below) — they landed early alongside
the prune stages, which share the same engine.

### Stage 5 — `[dir.*]` overlays + `repo config`  ✅ done
`[dir.*]` settings-only nodes (DESIGN §3.9): parse, validate (`dir` required,
`layout` rejected, name namespace separate from `[root.*]`), fold into the same
`dir`-prefix chain `RootFor`/`effective` already compute for roots, and keep out
of `ScanRoots()`. `repo config <id | path>` (DESIGN §7): resolve a repo (declared
member or on-disk discovery) and print its fully resolved settings as TOML by
default (config in, config out); `--explain` instead prints, per field, which
link in the chain last set it. Both are config/CLI-layer only — no git-layer
changes — so this lands ahead of Stage 6 per the read-only-first ordering, and
gives Stage 6 a way to inspect resolved settings while building it.
**Proof:** a `[dir.contrib-prometheus]` node scoped under an owner-layout root
overrides `workflow`/`pin`/`branches` for just that owner without adding a walk;
`repo config prometheus/prometheus` prints a TOML block with `workflow = "vendor"`
for a repo that inherited it three links deep, `--explain` shows that value as
`workflow: vendor (dir.contrib-prometheus)`, and a deliberately mistyped `dir`
shows up as the override simply never appearing in either output — the failure
mode this stage exists to make visible.

### Stage 6 — worktrees, fork-pr/vendor, work strangulation  🚧 in progress
Landed already, built alongside the prune stages below since they share the
sync engine and there was no reason to gate worktree-aware repos out of tag/
prune work: bare+worktree provisioning (`provisionWorktree`/`syncWorktree`,
DESIGN §4); `fork-pr` push (local fast-forward-then-push; the `gh repo sync`
fast path stays a deferred optimisation — same fork state either way, so it's
not required); `vendor` pins (branch/tag/`latest-tag`); `repo prune`'s
confirmation UX (`--delete`'s per-branch walkthrough, the `-D` cross-check).

**Fixed:** `supply-chain-mirror`'s second remote was being provisioned,
fetched, and reviewed against as `upstream` — the fork-pr name — instead of
`untrusted` (DESIGN §3.6). `discover.go` got the `untrusted` name when it was
introduced (commit `eace548`, 2026-07-12); the sync engine's own provisioning
and `mirrorReview` never did, so a tool-provisioned mirror could never be
re-detected as one by `discover`, and every mirror clone carried a remote its
own contract says it must not have — unnoticed because the existing test
(`TestMirrorDoesNotAdvancePastReview`) checked the outcome and commit count,
never the remote name. `sync --fix` now renames a stale `upstream` to
`untrusted` on a hardening clone (or, if a plain sync already created
`untrusted` alongside it, removes the now-redundant `upstream` instead of
attempting an impossible rename); without `--fix` the mismatch is reported
every run — but, corrected the same day on live use, *not* via `x.attention`:
that outranks `ReviewPending` (rank/mark), so on a real mirror whose old
`upstream` was already ahead of origin, the very fix meant to make review-gate
detection work again buried it behind a "rename this remote" notice instead —
worse than before the fix, and a "green ✓ up to date" is exactly how a repo
with a masked ReviewPending reads. Layout mismatches earn `x.attention`
because nothing else can proceed until they're fixed; a stale remote name
blocks nothing (DESIGN §4.1 calls remote reconciliation low-risk), so
detecting it is now a trace line only. Separately, every provisioning path's
fetch of the second remote had always swallowed its error silently — inert
for a remote with years of successful fetches behind it, but exactly what a
*newly created* `untrusted` remote's first fetch now goes through for anyone
upgrading from an `upstream`-named clone; a failure there left nothing for
`mirrorReview` to compare against, which also reads as a clean "up to date"
rather than "the review gate couldn't check its source". That fetch now
reports failure as Attention.

Still open:
- `--fix`'s other two DESIGN §4.1 reconciliations: general remote-contract
  repair (a wrong URL, beyond the untrusted/upstream rename) and location —
  moving a container found at the wrong root/layout path. Only layout-shape
  conversion (`relayout.go`) and the untrusted/upstream rename exist so far.
- Prune's own order (below), items 6-8: worktree removal, `prune =
  "interactive"` in the sweep, `prune = "auto"`.
- `repo home`/`repo path`/`repo clone`/`repo review` are still stubs.
- `wd-repos-update` -> `repo sync work` (the `work` root — tags are gone
  per DESIGN §10, so this is a root-name selector now, not `--tag work`).

**Proof:** work repos managed as owner-nested worktrees; the PRJPATH script retired.
**Tags:** whatever `prune` grows here, tags are not branches — no merge question to
answer, no reflog to recover from, and pruning is scoped to the fetch refspec so a
narrowed `tags` makes it partial. Read DESIGN §5.3 (the tag constraints under prune)
before adding `--prune-tags` or any tag sweep; the short version is that it must be
an explicit glob list defaulting to nothing, never an inference.

**Prune's own order** (DESIGN §5.3, written before the code and amended as the code
disagrees with it). Each step is independently useful and leaves prune safer than it
found it:

1. ✅ `--dry-run`, and the journal at `$XDG_STATE_HOME/repo/prune.log` (state, not
   data: the spec puts logs and history there) — the record and the preview land
   *before* anything new can delete.
2. ✅ `prune_keep` / `prune_min_age`, and the tip SHA + ref age they need on the
   verdict.
3. ✅ `prune` default → `report`, the setting wired to the sweep, and the footer
   naming what it found. The verdicts get computed once per repo and shared.
4. ✅ `--explain`, then the per-branch walk-through `repo prune --delete` uses it
   for. Explain comes first because the prompt is the explanation plus a question.
5. ✅ The reverse-apply cross-check before every `-D`, timed and reported.
6. Worktree removal, so `worktrees = true` repos stop being permanently blocked.
7. `prune = "interactive"`: the same walk-through, in the sweep's serial phase,
   TTY-only, degrading to `report` when there is nobody to ask.
8. `auto`, last — after the reports above have been watched being right.

### Stage 7 — distribution hardening
The POSIX shim in `dotfiles/bin/repo` (download+verify from Releases -> `go build`
fallback -> no-op offline); release automation; the small cold-bootstrap seed + a
real container test of it.

## After MVP
`link` (ghlink++), `review` full UX, project-as-mode, worktree-per-task for agents.
