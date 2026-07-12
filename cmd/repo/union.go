package main

import (
	"path/filepath"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/discover"
	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// unionRepos returns the operational set both status and sync act on: every
// declared repo, plus every repo discovered on disk that the registry does not
// already declare (DESIGN §3.2). Identity dedupes the two — a declared entry
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
// branch is whatever is checked out — so sync targets that rather than assuming
// "main" and flagging every repo on another branch. Everything else is inherited
// from the root it sits under (DESIGN §3.2: config overrides remote inference),
// falling back to what disk/remotes report where config is silent.
func discoveredRepo(reg *config.Registry, f discover.Found) model.Repo {
	inh := reg.InheritedFor(f.Roots)
	r := model.Repo{
		ID:        f.ID,
		Roots:     f.Roots,
		Dir:       f.Dir,
		OriginURL: f.Remotes["origin"],
		Workflow:  strOrDefault(inh.Workflow, f.Workflow), // config wins over inference
		Layout:    strOrDefault(inh.Layout, model.LayoutFlat),
		Worktrees: inh.Worktrees != nil && *inh.Worktrees,
		OnRewrite: strOrDefault(inh.OnRewrite, builtinOnRewrite),
		Prune:     strOrDefault(inh.Prune, builtinPrune),
		Pin:       inh.Pin,
		Hooks:     inh.Hooks,
	}
	if r.OriginURL == "" {
		for _, u := range f.Remotes {
			r.OriginURL = u
			break
		}
	}
	if b, err := gitx.CurrentBranch(f.Dir); err == nil && b != "" {
		r.Branches = []string{b}
	}
	return r
}

func strOrDefault(s, def string) string {
	if s != "" {
		return s
	}
	return def
}

// repoName is the display/selection name for a repo, falling back to the
// directory leaf for a discovered repo with no usable identity.
func repoName(r model.Repo) string {
	if r.ID.Zero() {
		return filepath.Base(r.Container())
	}
	return r.ID.Short()
}

const (
	builtinOnRewrite = "stop"
	builtinPrune     = "auto"
)
