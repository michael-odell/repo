package sync

import (
	"os"
	"path/filepath"
	"testing"
)

// show_branches controls how much of the branch inventory is enumerated
// (DESIGN §5.6). Findings always show; observations are the configurable part,
// and must never change what a repo's row says about its health.

func note(res Result, name string) (BranchNote, bool) {
	for _, b := range res.Branches {
		if b.Name == name {
			return b, true
		}
	}
	return BranchNote{}, false
}

// parkedWork gives clone a task branch that is fully in sync with its own
// remote — so it produces no finding — but carries a commit main doesn't have.
// That is the state show_branches=unmerged exists to surface.
func parkedWork(t *testing.T, clone, branch string) {
	t.Helper()
	git(t, clone, "checkout", "-q", "-b", branch)
	writeCommit(t, clone, branch+".txt", "wip\n", "wip on "+branch)
	git(t, clone, "push", "-q", "origin", branch)
	git(t, clone, "checkout", "-q", "main")
}

// TestUnmergedListsParkedWorkWithoutAlarm is the point of the feature: work you
// left on a branch is named, and the repo still reads as healthy.
func TestUnmergedListsParkedWorkWithoutAlarm(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `show_branches = "unmerged"`)
	parkedWork(t, clone, "spike")

	res := run()
	if res.Outcome != UpToDate {
		t.Fatalf("outcome = %v, want UpToDate (an observation is not a problem); actions=%v", res.Outcome, res.Actions)
	}
	if want := "2 branches up to date"; res.Detail != want {
		t.Errorf("detail = %q, want %q (the row still reports health, not the observation)", res.Detail, want)
	}
	b, ok := note(res, "spike")
	if !ok {
		t.Fatalf("no line for spike; branches=%+v", res.Branches)
	}
	if b.Outcome != Info {
		t.Errorf("spike outcome = %v, want Info", b.Outcome)
	}
	if want := "1 ahead of main"; b.Summary != want {
		t.Errorf("spike summary = %q, want %q", b.Summary, want)
	}
}

// TestUnmergedIgnoresMergedBranches: a branch whose commits have all landed is
// not undone work — it's a prune candidate, which is a different signal (§5.3).
func TestUnmergedIgnoresMergedBranches(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `show_branches = "unmerged"`)
	git(t, clone, "checkout", "-q", "-b", "landed")
	git(t, clone, "push", "-q", "origin", "landed")
	git(t, clone, "checkout", "-q", "main")

	res := run()
	if len(res.Branches) != 0 {
		t.Errorf("branches = %+v, want none (a merged branch carries no unlanded work)", res.Branches)
	}
	if res.Outcome != UpToDate {
		t.Errorf("outcome = %v, want UpToDate", res.Outcome)
	}
}

// TestNotableSuppressesObservations: the tier that reproduces the old
// behaviour exactly — findings only, nothing extra.
func TestNotableSuppressesObservations(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `show_branches = "notable"`)
	parkedWork(t, clone, "spike")

	res := run()
	if len(res.Branches) != 0 {
		t.Errorf("branches = %+v, want none under show_branches=notable", res.Branches)
	}
	if want := "2 branches up to date"; res.Detail != want {
		t.Errorf("detail = %q, want %q", res.Detail, want)
	}
}

// TestAllListsEveryBranch: the top of the dial names important branches and
// merged task branches too.
func TestAllListsEveryBranch(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `show_branches = "all"`)
	parkedWork(t, clone, "spike")
	git(t, clone, "checkout", "-q", "-b", "landed")
	git(t, clone, "push", "-q", "origin", "landed")
	git(t, clone, "checkout", "-q", "main")

	res := run()
	if res.Outcome != UpToDate {
		t.Fatalf("outcome = %v, want UpToDate; actions=%v", res.Outcome, res.Actions)
	}
	for name, want := range map[string]string{
		"main":   "up to date",
		"spike":  "1 ahead of main",
		"landed": "merged",
	} {
		b, ok := note(res, name)
		if !ok {
			t.Errorf("no line for %s; branches=%+v", name, res.Branches)
			continue
		}
		if b.Summary != want {
			t.Errorf("%s summary = %q, want %q", name, b.Summary, want)
		}
	}
}

// TestObservationNeverOverwritesAFinding: observe runs last, so a branch that
// needed attention keeps saying so rather than being downgraded to an FYI.
func TestObservationNeverOverwritesAFinding(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `show_branches = "unmerged"`)
	git(t, clone, "checkout", "-q", "-b", "spike")
	writeCommit(t, clone, "s.txt", "wip\n", "wip") // ahead of main *and* never pushed
	git(t, clone, "checkout", "-q", "main")

	res := run()
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if want := "spike: never pushed"; res.Detail != want {
		t.Errorf("detail = %q, want %q (the finding, not the observation)", res.Detail, want)
	}
}

// TestLoneFindingBreaksOutWhenObservationsAccompanyIt is the rule that keeps the
// row honest: a finding folds onto the repo's row only when it is the only thing
// being shown. Once an observation is also listed, the name on the row would
// imply the other branches had nothing to say — so everything becomes a bullet
// and the row rolls up to a count of *findings* only.
func TestLoneFindingBreaksOutWhenObservationsAccompanyIt(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `show_branches = "unmerged"`)
	parkedWork(t, clone, "spike")
	must(t, os.WriteFile(filepath.Join(clone, "scratch.txt"), []byte("notes\n"), 0o644))

	res := run()
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if want := "1 branch needs attention"; res.Detail != want {
		t.Errorf("detail = %q, want %q (a count, not a name, once other lines exist)", res.Detail, want)
	}
	if _, ok := note(res, "main"); !ok {
		t.Errorf("main's finding lost its bullet; branches=%+v", res.Branches)
	}
	if _, ok := note(res, "spike"); !ok {
		t.Errorf("spike's observation lost its bullet; branches=%+v", res.Branches)
	}
}

// TestNoneSuppressesEveryBranchLine: the floor of the dial. The repo row is the
// whole report — but it still carries the worst outcome, so nothing is hidden,
// only un-enumerated.
func TestNoneSuppressesEveryBranchLine(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `show_branches = "none"`)
	parkedWork(t, clone, "spike")
	must(t, os.WriteFile(filepath.Join(clone, "scratch.txt"), []byte("notes\n"), 0o644))

	res := run()
	if len(res.Branches) != 0 {
		t.Errorf("branches = %+v, want none under show_branches=none", res.Branches)
	}
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention (none suppresses lines, never severity); actions=%v",
			res.Outcome, res.Actions)
	}
	// A count, not "main: 1 untracked file(s)" — on a multi-branch repo a bare
	// name would read as a promise that the other branches are fine, which this
	// mode cannot make.
	if want := "1 branch needs attention"; res.Detail != want {
		t.Errorf("detail = %q, want %q", res.Detail, want)
	}
}

// TestNoneKeepsPlainDetailOnSingleBranchRepo: with one branch there is no other
// branch to mislead about, and the repo row *is* the branch row — so it keeps
// saying what's wrong rather than degrading to a count that says less.
func TestNoneKeepsPlainDetailOnSingleBranchRepo(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `show_branches = "none"`)
	must(t, os.WriteFile(filepath.Join(clone, "scratch.txt"), []byte("notes\n"), 0o644))

	res := run()
	if want := "1 untracked file(s)"; res.Detail != want {
		t.Errorf("detail = %q, want %q", res.Detail, want)
	}
}
