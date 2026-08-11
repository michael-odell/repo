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
	for i := range repos {
		resolveBranches(&repos[i])
	}
	return repos, nil
}

// resolveBranches settles which branches sync treats as important, by the same
// rule for a declared repo and a discovered one: what separates the two is what
// config says about them, not how they are read. In precedence order —
//
//  1. `branches` stated for the repo or a root it sits under wins outright:
//     config overrides inference (DESIGN §3.2), and a fork/mirror whose
//     important branches aren't its origin's default needs to be able to say so.
//  2. else the clone's own answer (gitx.InferDefaultBranch) — never a working
//     tree's checkout, which a task branch left in place would otherwise win.
//  3. else the [defaults]/builtin value, which gets a say only when there is no
//     clone to ask. That is the one case it is good for: a repo sync is about to
//     provision has no HEAD to read yet.
//  4. else nothing, and sync says it can't tell rather than asserting a branch
//     that may not exist — the trap being a blanket `branches = ["main"]`
//     reporting "main missing on origin" against a repo whose trunk is master.
func resolveBranches(r *model.Repo) {
	if r.BranchesStated {
		return
	}
	dir := r.Container()
	if !gitx.IsRepo(dir) {
		return // nothing cloned to ask: keep the assumption, it's all there is
	}
	if b, ok := gitx.InferDefaultBranch(dir); ok {
		r.Branches = []string{b}
		return
	}
	r.Branches = nil
}

// discoveredRepo synthesizes the merged model for a repo found on disk. Its
// container and origin come from disk (not the registry) and everything else is
// inherited from the root it sits under (DESIGN §3.2: config overrides remote
// inference), falling back to what disk/remotes report where config is silent.
// Its important branches are left to resolveBranches, which treats it no
// differently from a declared repo: a root that states `branches` governs the
// repos discovered under it too, and otherwise the clone answers for itself.
func discoveredRepo(reg *config.Registry, f discover.Found) model.Repo {
	inh := reg.InheritedFor(f.Roots)
	workflow := strOrDefault(inh.Workflow, f.Workflow) // config wins over inference
	pushDefault, taskDefault, showDefault := config.WorkflowDefaults(workflow)
	stated := reg.StatedBranches(f.Roots, config.Settings{})
	r := model.Repo{
		ID:                  f.ID,
		Roots:               f.Roots,
		Dir:                 f.Dir,
		Branches:            stated,
		BranchesStated:      stated != nil,
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
	return r
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
