package sync

import (
	"path/filepath"

	"github.com/michael-odell/repo/internal/gitx"
)

// Moving a branch, and reading where it stands, never depend on which branch
// happens to be checked out (DESIGN §5.1). Every branch operation in this
// package goes through the three functions below, so "important branch" and
// "task branch" differ only in the *policy* applied to what they find — which
// branch is HEAD is an implementation detail of the working tree, not an input
// to the decision.
//
// All three take the same (dir, branch, ref) triple. dir is any working tree of
// the repo, or the container of a bare+worktree one; branch is the local branch
// being moved or measured; ref is what it is being measured against or moved
// to. Nothing here requires branch to be checked out, and nothing here requires
// it not to be.

// aheadBehind reports how far branch stands ahead of and behind ref. Errors
// (a missing ref, a broken repo) read as 0/0 — every caller checks the ref
// exists first, and reporting "in sync" on an unexpected failure is the
// conservative answer, since it moves nothing.
func aheadBehind(dir, branch, ref string) (ahead, behind int) {
	a, b, err := gitx.AheadBehindRefs(dir, ref, branch)
	if err != nil {
		return 0, 0
	}
	return a, b
}

// fastForward advances branch to ref, failing rather than merging when the move
// is not a fast-forward. When some working tree has branch checked out, the move
// happens there so that tree's files advance with it; otherwise the ref moves on
// its own and no working tree is touched.
func fastForward(dir, branch, ref string) error {
	if wt := gitx.WorktreeFor(dir, branch); wt != "" {
		return gitx.FastForwardCurrent(wt, ref)
	}
	return gitx.FastForwardRef(dir, branch, ref)
}

// dirtyTree returns the working tree holding branch when that tree has *any*
// uncommitted changes, else "". A branch with no working tree has nothing to
// disturb, so it is never dirty in this sense — the same asymmetry fastForward
// and forceReset are built on.
//
// This deliberately ignores `expected_uncommitted`: it guards the operations
// that destroy work outright (forceReset) and explains the ones git itself
// refuses. Declaring a file expected says "don't nag me, and go ahead and try
// the safe move" — it never says "reset --hard over it". See treeMods for the
// filtered counterpart and DESIGN §5.1 for the attention/protection split.
func dirtyTree(dir, branch string) string {
	wt := gitx.WorktreeFor(dir, branch)
	if wt == "" {
		return ""
	}
	if dirty, _ := gitx.IsDirty(wt); dirty {
		return wt
	}
	return ""
}

// expectedPath reports whether p — a repo-relative path — is named by any of the
// patterns. These use the same globs as force_push/force_pull (DESIGN §3.6) and
// are tested against the base name as well as the full path, since path.Match
// doesn't cross "/": `*.plugin.zsh` should name that file wherever the thing
// generating it decides to put it.
func expectedPath(patterns []string, p string) bool {
	return matchesAny(patterns, p) || matchesAny(patterns, filepath.Base(p))
}

// unexpected returns the paths not named by patterns — what is left to report.
func unexpected(paths, patterns []string) []string {
	if len(patterns) == 0 {
		return paths
	}
	var out []string
	for _, p := range paths {
		if !expectedPath(patterns, p) {
			out = append(out, p)
		}
	}
	return out
}

// treeMods returns dir's local residue with the repo's `expected_*` globs
// applied: the tracked files it has modified, and the untracked files it
// carries, minus everything the registry says to expect there (DESIGN §3.6).
//
// A tree whose residue is entirely expected reads as clean for the two purposes
// the user controls — what gets reported, and whether sync attempts a move — and
// only those. The data-safety rails keep using dirtyTree's unfiltered view, so
// an expected file can still stop a reset --hard and can still block a layout
// conversion. Errors read as no residue, matching how the rest of this package
// treats a git query it can't answer.
func (x *run) treeMods(dir string) (uncommitted, untracked []string) {
	mods, _ := gitx.DirtyFiles(dir)
	files, _ := gitx.UntrackedFiles(dir)
	return unexpected(mods, x.r.ExpectedUncommitted), unexpected(files, x.r.ExpectedUntracked)
}

// blockedTree returns the working tree holding branch when that tree carries
// *unexpected* uncommitted changes — the state in which sync declines to move
// the branch at all, rather than attempting it and letting git arbitrate.
func (x *run) blockedTree(dir, branch string) string {
	wt := gitx.WorktreeFor(dir, branch)
	if wt == "" {
		return ""
	}
	if uncommitted, _ := x.treeMods(wt); len(uncommitted) > 0 {
		return wt
	}
	return ""
}

// forceReset moves branch to ref even when the move is not a fast-forward — the
// force_pull path (DESIGN §5.2), whose callers have already established that no
// local commits are being discarded. Same working-tree rule as fastForward.
func forceReset(dir, branch, ref string) error {
	if wt := gitx.WorktreeFor(dir, branch); wt != "" {
		return gitx.ResetHardCurrent(wt, ref)
	}
	return gitx.ForceSetRef(dir, branch, ref)
}
