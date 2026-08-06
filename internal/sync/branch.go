package sync

import "github.com/michael-odell/repo/internal/gitx"

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

// dirtyTree returns the working tree holding branch when that tree has
// uncommitted changes, else "". This is the state in which no update may move
// branch (principle 4, "never touch a dirty tree"): moving the branch would drag
// its working tree along, over changes that exist nowhere else. A branch with no
// working tree has nothing to disturb, so it is never dirty in this sense — the
// same asymmetry fastForward and forceReset are built on.
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

// forceReset moves branch to ref even when the move is not a fast-forward — the
// force_pull path (DESIGN §5.2), whose callers have already established that no
// local commits are being discarded. Same working-tree rule as fastForward.
func forceReset(dir, branch, ref string) error {
	if wt := gitx.WorktreeFor(dir, branch); wt != "" {
		return gitx.ResetHardCurrent(wt, ref)
	}
	return gitx.ForceSetRef(dir, branch, ref)
}
