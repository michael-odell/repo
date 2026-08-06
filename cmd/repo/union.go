package main

import (
	"path/filepath"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/discover"
	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// unionRepos returns the operational set status, sync, list, resolve, and apply
// all act on: every declared repo, plus every repo discovered on disk that the
// registry does not already declare (DESIGN §3.2). Identity dedupes the two — a declared entry
// wins, so its metadata (branches, hooks, workflow) governs — and repeat clones
// of one id collapse to the first found. Discovered-only repos are synthesized
// with their real location and existing remote so both commands see one set.
func unionRepos(reg *config.Registry) ([]model.Repo, error) {
	repos, err := reg.Repos()
	if err != nil {
		return nil, err
	}
	// Dedupe on two keys: identity, and physical directory. Directory matters
	// because a fork/mirror repo's declared identity is its upstream while its
	// on-disk origin is the fork — same clone, different id — so identity alone
	// would list it twice.
	seenID := map[string]bool{}
	seenDir := map[string]bool{}
	for _, r := range repos {
		seenID[r.ID.String()] = true
		seenDir[filepath.Clean(r.Container())] = true
	}
	found, err := discover.Discover(resolveRoots(reg), reg)
	if err != nil {
		return nil, err
	}
	for _, f := range found {
		if seenDir[filepath.Clean(f.Dir)] {
			continue
		}
		if !f.ID.Zero() && seenID[f.ID.String()] {
			continue // declared, or an earlier clone of the same id, wins
		}
		seenDir[filepath.Clean(f.Dir)] = true
		if !f.ID.Zero() {
			seenID[f.ID.String()] = true
		}
		repos = append(repos, discoveredRepo(reg, f))
	}
	return repos, nil
}

// discoveredRepo synthesizes the merged model for a repo found on disk. Its
// container and origin come from disk (not the registry), and its lone important
// branch is inferred the same way a declared repo's default would be: origin's
// actual default branch (its HEAD symref) first, else a known mainline name
// (main/master/develop) among its local branches — never whatever happens to be
// checked out, which is what a task branch left checked out would otherwise get
// mistaken for. When neither signal resolves, Branches is left empty rather than
// guessed; sync flags that repo for an explicit `branches` override instead of
// silently treating some arbitrary branch as important. Everything else is
// inherited from the root it sits under (DESIGN §3.2: config overrides remote
// inference), falling back to what disk/remotes report where config is silent.
func discoveredRepo(reg *config.Registry, f discover.Found) model.Repo {
	inh := reg.InheritedFor(f.Roots)
	workflow := strOrDefault(inh.Workflow, f.Workflow) // config wins over inference
	pushDefault, taskDefault, showDefault := config.WorkflowDefaults(workflow)
	r := model.Repo{
		ID:                  f.ID,
		Roots:               f.Roots,
		Dir:                 f.Dir,
		OriginURL:           f.Remotes["origin"],
		Workflow:            workflow,
		Layout:              strOrDefault(inh.Layout, model.LayoutFlat),
		Worktrees:           inh.Worktrees != nil && *inh.Worktrees,
		Push:                strOrDefault(inh.Push, pushDefault),
		TaskBranches:        strOrDefault(inh.TaskBranches, taskDefault),
		ShowBranches:        strOrDefault(inh.ShowBranches, showDefault),
		ForcePush:           inh.ForcePush,
		ForcePull:           inh.ForcePull,
		ExpectedUntracked:   inh.ExpectedUntracked,
		ExpectedUncommitted: inh.ExpectedUncommitted,
		Prune:               strOrDefault(inh.Prune, builtinPrune),
		Pin:                 inh.Pin,
		Hooks:               inh.Hooks,
	}
	if r.OriginURL == "" {
		for _, u := range f.Remotes {
			r.OriginURL = u
			break
		}
	}
	if b, ok := gitx.DefaultBranch(f.Dir, "origin"); ok {
		r.Branches = []string{b}
	} else if b := mainlineBranch(f.Dir); b != "" {
		r.Branches = []string{b}
	}
	return r
}

// mainlineBranch returns whichever of a small set of conventional names
// (checked in order of how common each is) exists as a local branch, or ""
// when none do — the fallback signal for a discovered repo whose clone
// predates `git clone` recording origin's HEAD symref (see DefaultBranch).
func mainlineBranch(dir string) string {
	locals, err := gitx.LocalBranches(dir)
	if err != nil {
		return ""
	}
	have := map[string]bool{}
	for _, b := range locals {
		have[b] = true
	}
	for _, name := range []string{"main", "master", "develop"} {
		if have[name] {
			return name
		}
	}
	return ""
}

func strOrDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

// repoName is the display name for a repo: owner/repo, definitive regardless
// of how many repos elsewhere share a short name — falling back to the
// directory leaf for a discovered repo with no usable identity. Selection
// (matching a CLI argument against a repo) is separate, in find(), and still
// accepts the bare short name.
func repoName(r model.Repo) string {
	if r.ID.Zero() {
		return filepath.Base(r.Container())
	}
	return r.ID.OwnerRepo()
}

const builtinPrune = "auto"
