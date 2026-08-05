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
// a rev-list against refs, never the working tree. Findings are surfaced as
// per-branch sub-bullets (BranchNote, DESIGN §5.6), never folded into the
// repo's own Outcome/Detail — a messy task branch is informational, not a
// reason to flag the whole repo.
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
		ahead, behind, _ = gitx.AheadBehindRefs(x.container, originRef, branch)
	}

	switch {
	case !hasRemote:
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
		x.branchNote(branch, fmt.Sprintf("%d behind its own remote", behind), true)
	}
}

// pullTaskBranch fast-forwards a purely-behind task branch to its own remote
// in place, without checking it out — the pull-only case (DESIGN §3.6). Any
// number of local branches with zero commits of their own (the one a plain
// `git clone` leaves checked out, or others deliberately kept around purely
// to track read-only) are treated the same way: kept current rather than
// flagged, no allowlist of names or count needed. A branch that picks up
// local commits later falls through to the ahead>0 cases above instead,
// which still report loudly.
func (x *run) pullTaskBranch(branch, originRef string, behind int) {
	if x.opts.DryRun {
		x.branchNote(branch, fmt.Sprintf("would pull +%d", behind), false)
		return
	}
	if cur, _ := gitx.CurrentBranch(x.container); cur == branch {
		// Checked out in this container after all (e.g. an aborted/misconfigured
		// pin left it as HEAD) — leave it to the normal update path rather than
		// risk a fetch into the current branch.
		x.branchNote(branch, fmt.Sprintf("%d behind its own remote", behind), true)
		return
	}
	if err := gitx.FetchInto(x.container, "origin", branch); err != nil {
		x.branchNote(branch, fmt.Sprintf("pull failed: %v", err), true)
		return
	}
	x.branchNote(branch, fmt.Sprintf("pulled +%d", behind), false)
}

// taskBranchPush pushes or reports a task branch's ahead-only (or brand new)
// state per `task_branches` (auto pushes; report/pull-only both just report —
// pull-only only differs from report in the behind-only case above).
func (x *run) taskBranchPush(branch, detail string) {
	if x.r.TaskBranches != "auto" {
		x.branchNote(branch, detail, true)
		return
	}
	if x.opts.DryRun {
		x.branchNote(branch, "would push ("+detail+")", false)
		return
	}
	if err := gitx.Push(x.container, "origin", branch); err != nil {
		x.branchNote(branch, fmt.Sprintf("push failed: %v", err), true)
		return
	}
	x.branchNote(branch, "pushed ("+detail+")", false)
}

// taskBranchDiverged handles a task branch that has been locally rewritten
// (rebase/amend) relative to its own remote — force-pushed only when
// `force_push` matches and task_branches=auto (DESIGN §5.2).
func (x *run) taskBranchDiverged(branch string, ahead, behind int) {
	if x.r.TaskBranches == "auto" && matchesAny(x.r.ForcePush, branch) {
		if x.opts.DryRun {
			x.branchNote(branch, fmt.Sprintf("would force-push (+%d/-%d)", ahead, behind), false)
			return
		}
		if err := gitx.ForcePush(x.container, "origin", branch); err != nil {
			x.branchNote(branch, fmt.Sprintf("force-push failed: %v", err), true)
			return
		}
		x.branchNote(branch, fmt.Sprintf("force-pushed (+%d/-%d)", ahead, behind), false)
		return
	}
	reason := "no force_push match"
	if x.r.TaskBranches != "auto" {
		reason = fmt.Sprintf("task_branches=%s", x.r.TaskBranches)
	}
	x.branchNote(branch, fmt.Sprintf("diverged (+%d/-%d): not force-pushing (%s)", ahead, behind, reason), true)
}
