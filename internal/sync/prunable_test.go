package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// merge lands branch into main in clone and pushes the result.
func merge(t *testing.T, clone, branch string, squash bool) {
	t.Helper()
	git(t, clone, "checkout", "-q", "main")
	if squash {
		git(t, clone, "merge", "-q", "--squash", branch)
		git(t, clone, "commit", "-q", "-m", "squashed "+branch)
	} else {
		git(t, clone, "merge", "-q", "--no-ff", "-m", "merge "+branch, branch)
	}
	git(t, clone, "push", "-q", "origin", "main")
}

// TestMergedBranchIsNotRePushedAfterUpstreamDeletion is the bug this fixes.
// Merging a branch and deleting it on the remote is the ordinary end of a piece
// of work; `fetch --prune` then removes the remote-tracking ref, which used to
// read as "never pushed" and — under task_branches=auto — push the branch
// straight back, resurrecting what was just deleted.
//
// Tracking config cannot tell these apart: a branch pushed without `-u` has a
// remote ref and no config, so config-absence would misread every such branch.
// Merge state can.
func TestMergedBranchIsNotRePushedAfterUpstreamDeletion(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, `task_branches = "auto"`)
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "f", "work\n", "the work")
	git(t, clone, "push", "-q", "origin", "feature") // no -u: no tracking config
	merge(t, clone, "feature", false)
	git(t, clone, "push", "-q", "origin", "--delete", "feature")

	res := run()
	if refExists(t, origin, "feature") {
		t.Errorf("origin has feature again: a merged, deleted branch was resurrected")
	}
	if res.Outcome != UpToDate {
		t.Errorf("outcome = %v, want UpToDate (a landed branch is not a problem); actions=%v",
			res.Outcome, res.Actions)
	}
	if !hasAction(res, "nothing to push") {
		t.Errorf("no trace explaining the skipped push; actions=%v", res.Actions)
	}
}

// TestSquashMergedBranchIsNotRePushedEither: the same fix has to survive the
// merge style that defeats an ancestry test, which is the whole reason the
// tiers exist — a squash-merged branch still has commits of its own.
func TestSquashMergedBranchIsNotRePushedEither(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, `task_branches = "auto"`)
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "f", "work\n", "the work")
	writeCommit(t, clone, "f", "work\nmore\n", "more work") // two commits: only tier 3 can see the squash
	git(t, clone, "push", "-q", "origin", "feature")
	merge(t, clone, "feature", true)
	git(t, clone, "push", "-q", "origin", "--delete", "feature")

	res := run()
	if refExists(t, origin, "feature") {
		t.Errorf("origin has feature again: a squash-merged branch was resurrected")
	}
	if !hasAction(res, "merged (squashed)") {
		t.Errorf("squash merge not recognised; actions=%v", res.Actions)
	}
}

// TestEmptyBranchIsNotCalledDeleted: a branch someone just created carries
// nothing outstanding, exactly like a merged one — but it never had an upstream
// to lose, so saying so would invent a history it never had.
func TestEmptyBranchIsNotCalledDeleted(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, `task_branches = "auto"`)
	git(t, clone, "branch", "scratch")

	res := run()
	if refExists(t, origin, "scratch") {
		t.Errorf("origin has scratch: an empty branch carries no work worth pushing")
	}
	if res.Outcome != UpToDate {
		t.Errorf("outcome = %v, want UpToDate; actions=%v", res.Outcome, res.Actions)
	}
}

// TestClassifyVerdicts covers the prune decision `repo prune` acts on and
// `show_branches = all` displays — the same call, so the line you watch during
// a sweep is the decision, not a lookalike.
func TestClassifyVerdicts(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `show_branches = "all"`)
	// landed: squash-merged, so only tier 3 can see it
	git(t, clone, "checkout", "-q", "-b", "landed")
	writeCommit(t, clone, "l", "landed\n", "landed work")
	writeCommit(t, clone, "l", "landed\nmore\n", "more landed work")
	merge(t, clone, "landed", true)
	// parked: real outstanding work
	git(t, clone, "checkout", "-q", "-b", "parked")
	writeCommit(t, clone, "p", "parked\n", "parked work")
	git(t, clone, "push", "-q", "origin", "parked")
	// held: merged, but checked out — git would refuse, and so do we
	git(t, clone, "checkout", "-q", "-b", "held")
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "push", "-q", "origin", "main")

	verdicts, err := Classify(clone, mainRepo())
	must(t, err)

	got := map[string]Verdict{}
	for _, v := range verdicts {
		got[v.Name] = v
	}
	if v := got["landed"]; !v.Prunable || v.State != gitx.MergedSquash {
		t.Errorf("landed = %+v, want prunable MergedSquash", v)
	}
	if v := got["parked"]; v.Prunable || v.Blocker != "unmerged" {
		t.Errorf("parked = %+v, want not prunable (unmerged)", v)
	}
	if _, ok := got["main"]; ok {
		t.Errorf("main classified; important branches are the reference, not candidates")
	}

	// And the sweep shows the same verdict it would act on.
	res := run()
	b, ok := note(res, "landed")
	if !ok {
		t.Fatalf("no line for landed; branches=%+v", res.Branches)
	}
	if want := "merged (squashed) — prunable"; b.Summary != want {
		t.Errorf("landed summary = %q, want %q", b.Summary, want)
	}
}

// TestClassifyKeepsBranchesItCannotAnswerFor: a branch whose standing git can't
// determine must appear, marked and never prunable. Skipping it made a
// classifier that failed on everything look exactly like a repo with nothing to
// report — which is how a broken squash tier emptied the branch report on every
// machine without a configured git identity instead of erroring loudly.
//
// `repo prune` is where this surfaces: it classifies without fetching. A sync
// over a repo damaged this badly fails at the fetch and never reaches the
// branch report, which is its own honest answer.
func TestClassifyKeepsBranchesItCannotAnswerFor(t *testing.T) {
	_, clone, _ := setupUpstreamRepo(t, `show_branches = "unmerged"`)
	git(t, clone, "checkout", "-q", "-b", "parked")
	writeCommit(t, clone, "p", "parked\n", "parked work")
	git(t, clone, "checkout", "-q", "main")
	// A ref naming an object that isn't there: a branch by name, unanswerable by
	// any tier. Written as a file because git's own plumbing declines to create
	// one — which is the point, this is the shape a damaged repo arrives in.
	must(t, os.WriteFile(filepath.Join(clone, ".git", "refs", "heads", "broken"),
		[]byte("0000000000000000000000000000000000000001\n"), 0o644))

	verdicts, err := Classify(clone, mainRepo())
	must(t, err)
	var got *Verdict
	for i := range verdicts {
		if verdicts[i].Name == "broken" {
			got = &verdicts[i]
		}
	}
	if got == nil {
		t.Fatalf("broken branch dropped from the verdicts: %+v", verdicts)
	}
	if !got.Unknown || got.Prunable {
		t.Errorf("broken = %+v, want Unknown and not prunable", *got)
	}
	if !strings.Contains(got.Summary("main"), "can't classify") {
		t.Errorf("summary = %q, want it to say the branch couldn't be classified", got.Summary("main"))
	}
	// The healthy branches around it must still be classified normally — one bad
	// branch is not a reason to stop answering for the rest.
	if len(verdicts) < 2 {
		t.Errorf("only %d verdicts; a bad branch must not take its siblings with it: %+v", len(verdicts), verdicts)
	}
}

// TestClassifyRefusesCheckedOutBranch: being checked out blocks removal
// regardless of merge state — git will not delete a branch a worktree is on,
// and the verdict must not claim otherwise.
func TestClassifyRefusesCheckedOutBranch(t *testing.T) {
	_, clone, _ := setupUpstreamRepo(t, "")
	git(t, clone, "checkout", "-q", "-b", "held") // merged (empty) and checked out

	verdicts, err := Classify(clone, mainRepo())
	must(t, err)
	for _, v := range verdicts {
		if v.Name != "held" {
			continue
		}
		if v.Prunable {
			t.Errorf("held = %+v, want not prunable while checked out", v)
		}
		if v.Blocker != "checked out" {
			t.Errorf("held blocker = %q, want %q", v.Blocker, "checked out")
		}
		if v.Worktree == "" {
			t.Errorf("held worktree not recorded")
		}
		return
	}
	t.Errorf("held not classified; verdicts=%+v", verdicts)
}

// hasAction reports whether any trace line contains want.
func hasAction(res Result, want string) bool {
	for _, a := range res.Actions {
		if strings.Contains(a, want) {
			return true
		}
	}
	return false
}

// mainRepo is the minimal model.Repo Classify needs: the important-branch list
// it measures everything else against.
func mainRepo() model.Repo { return model.Repo{Branches: []string{"main"}} }
