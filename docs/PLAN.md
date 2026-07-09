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
`repo apply` generates `prjpath.zsh`/`homes.zsh`/`plugins.zsh` into `~/.local/repos`
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

### Stage 5 — worktrees, fork-pr/vendor, work strangulation  ⏭ next
bare+worktree provisioning; `fork-pr` push (+ `gh repo sync` when present); `vendor`
pins; `repo prune` confirmation UX; `repo home`/`path`. Then `wd-repos-update` ->
`repo sync --tag work`.
**Proof:** work repos managed as owner-nested worktrees; the PRJPATH script retired.

### Stage 6 — distribution hardening
The POSIX shim in `dotfiles/bin/repo` (download+verify from Releases -> `go build`
fallback -> no-op offline); release automation; the small cold-bootstrap seed + a
real container test of it.

## After MVP
`link` (ghlink++), `review` full UX, project-as-mode, worktree-per-task for agents.
