package sync

import (
	"fmt"

	"github.com/michael-odell/repo/internal/gitx"
)

// taskBranches handles every local branch other than the repo's configured
// important branches, per `task_branches` (DESIGN §3.6). Called once per repo,
// not per worktree unit, against the container — which works whether the
// container is a single working tree or a worktree-layout bare front-end,
// since a task branch is never checked out either way and every check here is
// a rev-list against refs, never the working tree. Findings are recorded via
// branchMark, exactly like an important branch's own findings (DESIGN §5.6):
// a messy task branch is just as capable of flagging the repo as a missing
// or diverged important one.
//
// Scope note: unlike `prune`'s three-tier confirmed-merged detection (DESIGN
// §5.3, not yet implemented as its own command), this only looks at
// ahead/behind/existence — there's no squash/rebase-aware "already merged"
// signal here yet.
func (x *run) taskBranches() {
	if x.res.Err != nil {
		return
	}
	important := map[string]bool{}
	for _, b := range x.r.Branches {
		important[b] = true
	}
	branches, err := gitx.LocalBranches(x.container)
	if err != nil {
		return
	}
	for _, b := range branches {
		if !important[b] {
			x.taskBranch(b)
		}
	}
}

// taskBranch handles one non-important branch per `task_branches`.
func (x *run) taskBranch(branch string) {
	originRef := "origin/" + branch
	_, hasRemote := gitx.RevParse(x.container, originRef)
	var ahead, behind int
	if hasRemote {
		ahead, behind = aheadBehind(x.container, branch, originRef)
	}

	switch {
	case !hasRemote:
		// No remote branch is two very different situations, and telling them
		// apart by tracking config does not work: a branch pushed without `-u`
		// has a remote ref and no config, so config-absence would call it
		// "never pushed" the moment its remote went away. Merge state is the
		// reliable signal. If everything this branch carries is already on the
		// important branch, the remote ref is gone because the work landed and
		// the branch was deleted upstream — pushing would resurrect it, and
		// call the resurrection "never pushed" (DESIGN §5.3).
		if state, err := gitx.MergedState(x.container, branch, x.branch, scanLimitOf(x.r)); err == nil && state.Merged() {
			// Why the remote ref is absent is unknowable — deleted after the
			// merge, or never pushed at all — and guessing would put a history
			// in the report that may never have happened. What matters is
			// decidable and the same either way: the branch has nothing the
			// important branch lacks, so there is nothing to push. Record the
			// reason in the trace and leave the branch's line to observe, which
			// reports it as the prune candidate it is (DESIGN §5.3).
			x.add("%s is %s and absent from origin: nothing to push", branch, state)
			return
		}
		x.taskBranchPush(branch, "never pushed")
	case ahead == 0 && behind == 0:
		// fully in sync with its own remote — nothing to report
	case ahead > 0 && behind == 0:
		x.taskBranchPush(branch, fmt.Sprintf("%d unpushed", ahead))
	case ahead > 0 && behind > 0:
		x.taskBranchDiverged(branch, ahead, behind)
	default: // ahead == 0 && behind > 0: trailing its own remote, no local intent
		if x.r.TaskBranches == "pull-only" {
			x.pullTaskBranch(branch, originRef, behind)
			return
		}
		x.branchMark(branch, Attention, fmt.Sprintf("%d behind its own remote", behind))
	}
}

// pullTaskBranch fast-forwards a purely-behind task branch to its own remote —
// the pull-only case (DESIGN §3.6). Any number of local branches with zero
// commits of their own (the one a plain `git clone` leaves checked out, or
// others deliberately kept around purely to track read-only) are treated the
// same way: kept current rather than flagged, no allowlist of names or count
// needed. A branch that picks up local commits later falls through to the
// ahead>0 cases above instead, which still report loudly.
//
// This goes through the same fastForward as an important branch does, so a task
// branch that happens to be the checked-out one is pulled like any other rather
// than being singled out — it just advances that working tree along with it.
func (x *run) pullTaskBranch(branch, originRef string, behind int) {
	if x.opts.DryRun {
		x.branchMark(branch, Updated, fmt.Sprintf("would pull +%d", behind))
		return
	}
	// A task branch can be the checked-out one, or live in an ad-hoc worktree.
	// Either way its tree gets the same protection an important branch's does:
	// report, don't move it (principle 4). Units are guarded by treeGuard before
	// their own update; this covers the trees no unit owns.
	if wt := x.blockedTree(x.container, branch); wt != "" {
		x.branchMark(branch, Attention, fmt.Sprintf("%d behind — uncommitted changes, pull skipped", behind))
		x.add("%s is %d behind but %s has uncommitted changes: skipping pull", branch, behind, shorten(wt))
		return
	}
	if err := fastForward(x.container, branch, originRef); err != nil {
		x.branchMark(branch, Attention, fmt.Sprintf("pull failed: %v", err))
		return
	}
	x.branchMark(branch, Updated, fmt.Sprintf("pulled +%d", behind))
}

// taskBranchPush pushes or reports a task branch's ahead-only (or brand new)
// state per `task_branches` (auto pushes; report/pull-only both just report —
// pull-only only differs from report in the behind-only case above).
func (x *run) taskBranchPush(branch, detail string) {
	if x.r.TaskBranches != "auto" {
		x.branchMark(branch, Attention, detail)
		return
	}
	if x.opts.DryRun {
		x.branchMark(branch, Updated, "would push ("+detail+")")
		return
	}
	if err := gitx.Push(x.container, "origin", branch); err != nil {
		x.branchMark(branch, Attention, fmt.Sprintf("push failed: %v", err))
		return
	}
	x.branchMark(branch, Updated, "pushed ("+detail+")")
}

// taskBranchDiverged handles a task branch that has been locally rewritten
// (rebase/amend) relative to its own remote — force-pushed only when
// `force_push` matches and task_branches=auto (DESIGN §5.2).
func (x *run) taskBranchDiverged(branch string, ahead, behind int) {
	if x.r.TaskBranches == "auto" && matchesAny(x.r.ForcePush, branch) {
		if x.opts.DryRun {
			x.branchMark(branch, Updated, fmt.Sprintf("would force-push (+%d/-%d)", ahead, behind))
			return
		}
		if err := gitx.ForcePush(x.container, "origin", branch); err != nil {
			x.branchMark(branch, Attention, fmt.Sprintf("force-push failed: %v", err))
			return
		}
		x.branchMark(branch, Updated, fmt.Sprintf("force-pushed (+%d/-%d)", ahead, behind))
		return
	}
	reason := "no force_push match"
	if x.r.TaskBranches != "auto" {
		reason = fmt.Sprintf("task_branches=%s", x.r.TaskBranches)
	}
	x.branchMark(branch, Attention, fmt.Sprintf("diverged (+%d/-%d): not force-pushing (%s)", ahead, behind, reason))
}
