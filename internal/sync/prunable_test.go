package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	// An ancestry test calls this branch unmerged, so the "rewritten" verdict
	// could only have come from a patch tier.
	if !hasAction(res, "merged (rewritten)") {
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
	if want := "merged (rewritten) — prunable"; b.Summary != want {
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
	if !strings.Contains(got.Summary(), "can't classify") {
		t.Errorf("summary = %q, want it to say the branch couldn't be classified", got.Summary())
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
		if strings.Contains(a.Text, want) {
			return true
		}
	}
	return false
}

// mainRepo is the minimal model.Repo Classify needs: the important-branch list
// it measures everything else against.
func mainRepo() model.Repo { return model.Repo{Branches: []string{"main"}} }

// TestClassifyNamesAMissingBase: when the important branch doesn't exist in
// this clone, nothing can be measured — and the useful thing to say is which
// branch is missing, once, rather than letting every branch fail its own
// rev-list and repeat git's "ambiguous argument" with the name buried in it.
func TestClassifyNamesAMissingBase(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main", ".")
	writeCommit(t, dir, "f", "x\n", "one")
	git(t, dir, "checkout", "-q", "-b", "feature")

	_, err := Classify(dir, model.Repo{Branches: []string{"trunk"}})
	if err == nil {
		t.Fatal("classified a repo whose important branch it does not have")
	}
	if !strings.Contains(err.Error(), `"trunk"`) {
		t.Errorf("the error does not name the missing branch: %v", err)
	}
}

// TestPruneKeepOutranksTheVerdict: the tiers answer "has this landed", and
// prune_keep does not dispute the answer — it says the branch stays anyway. So
// the branch must still read as landed, with the *setting* named as what is
// holding it, or the report would look like a classification failure.
func TestPruneKeepOutranksTheVerdict(t *testing.T) {
	dir := landedBranchRepo(t, "wip/spike")

	vs, err := Classify(dir, model.Repo{Branches: []string{"main"}, PruneKeep: []string{"wip/*"}})
	if err != nil {
		t.Fatal(err)
	}
	v := findVerdict(t, vs, "wip/spike")
	if v.Prunable {
		t.Error("prune_keep did not hold the branch")
	}
	if !v.State.Merged() {
		t.Error("prune_keep changed the merge verdict; it is only supposed to change what happens next")
	}
	if !strings.Contains(v.Blocker, "prune_keep") {
		t.Errorf("blocker %q does not name the setting doing the keeping", v.Blocker)
	}
}

// TestPruneMinAgeHoldsAFreshBranch: a branch whose work landed a minute ago is
// exactly the one still likely to be in play, and it is the case an age gate
// exists for.
func TestPruneMinAgeHoldsAFreshBranch(t *testing.T) {
	dir := landedBranchRepo(t, "feature")

	vs, err := Classify(dir, model.Repo{Branches: []string{"main"}, PruneMinAge: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	v := findVerdict(t, vs, "feature")
	if v.Prunable {
		t.Error("a branch that moved moments ago was offered for deletion under a 1h minimum")
	}
	if !strings.Contains(v.Blocker, "prune_min_age") {
		t.Errorf("blocker %q does not name the setting doing the holding", v.Blocker)
	}

	// The same branch with no minimum set: the gate is opt-in, and an unset
	// setting must not quietly protect everything.
	vs, err = Classify(dir, model.Repo{Branches: []string{"main"}})
	if err != nil {
		t.Fatal(err)
	}
	if !findVerdict(t, vs, "feature").Prunable {
		t.Error("an unset prune_min_age held the branch back anyway")
	}
}

// TestVerdictCarriesTheTipSHA: the journal's restore line is built from this,
// and after the branch is deleted there is nowhere left to read it from.
func TestVerdictCarriesTheTipSHA(t *testing.T) {
	dir := landedBranchRepo(t, "feature")
	want, ok := gitx.RevParse(dir, "feature")
	if !ok {
		t.Fatal("no tip to compare against")
	}

	vs, err := Classify(dir, model.Repo{Branches: []string{"main"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := findVerdict(t, vs, "feature").SHA; got != want {
		t.Errorf("verdict SHA = %q, want %q", got, want)
	}
}

// landedBranchRepo builds a repo whose named branch is merged into main and
// left unchecked-out, so nothing but policy can block it.
func landedBranchRepo(t *testing.T, branch string) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main", ".")
	writeCommit(t, dir, "f", "x\n", "one")
	git(t, dir, "checkout", "-q", "-b", branch)
	writeCommit(t, dir, "g", "y\n", "two")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--ff-only", branch)
	return dir
}

func findVerdict(t *testing.T, vs []Verdict, name string) Verdict {
	t.Helper()
	for _, v := range vs {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no verdict for %q in %+v", name, vs)
	return Verdict{}
}

// TestPruneManualStopsTheSweepCounting: `manual` is the escape hatch for anyone
// who finds classification too slow, so it has to actually stop the question
// being asked — not merely hide the answer.
func TestPruneManualStopsTheSweepCounting(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `prune = "manual"`+"\n"+`show_branches = "none"`)
	git(t, clone, "checkout", "-q", "-b", "landed")
	writeCommit(t, clone, "f", "work\n", "the work")
	merge(t, clone, "landed", false)

	if res := run(); res.Prunable != 0 {
		t.Errorf("Prunable = %d under prune = \"manual\", want 0", res.Prunable)
	}
}

// TestTheSweepCountsWhatPruneWouldOffer: the footer's number comes from the
// same classification the command acts on, so a branch counted here is a branch
// prune would actually offer to remove.
func TestTheSweepCountsWhatPruneWouldOffer(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `show_branches = "none"`)
	git(t, clone, "checkout", "-q", "-b", "landed")
	writeCommit(t, clone, "f", "work\n", "the work")
	merge(t, clone, "landed", false)

	res := run()
	if res.Prunable != 1 {
		t.Fatalf("Prunable = %d, want 1 (show_branches must not decide what prune counts)", res.Prunable)
	}
}

// TestPruneKeepIsRespectedBySweepCount: policy narrows what prune offers, so it
// has to narrow the footer too — a count that included branches prune would
// refuse to touch would send you to a command with nothing to do.
func TestPruneKeepIsRespectedBySweepCount(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `prune_keep = ["landed"]`)
	git(t, clone, "checkout", "-q", "-b", "landed")
	writeCommit(t, clone, "f", "work\n", "the work")
	merge(t, clone, "landed", false)

	if res := run(); res.Prunable != 0 {
		t.Errorf("Prunable = %d with the branch named in prune_keep, want 0", res.Prunable)
	}
}

// TestLandedOnASecondaryImportantBranchIsLanded: a repo may carry several
// important branches, and work merged to any of them has landed. Measuring
// against branches[0] alone reported a released branch as outstanding work
// forever — the same false negative the tiers exist to eliminate, one level up.
//
// The verdict also has to name the branch that answered: "merged" in a repo
// with more than one important branch is unreadable without saying into what.
func TestLandedOnASecondaryImportantBranchIsLanded(t *testing.T) {
	_, clone, _ := setupUpstreamRepo(t, `show_branches = "all"`)
	// A release line that main knows nothing about.
	git(t, clone, "checkout", "-q", "-b", "release")
	writeCommit(t, clone, "r", "release\n", "release line")
	// shipped lands on release and nowhere else.
	git(t, clone, "checkout", "-q", "-b", "shipped")
	writeCommit(t, clone, "s", "shipped\n", "shipped work")
	git(t, clone, "checkout", "-q", "release")
	git(t, clone, "merge", "-q", "--no-ff", "-m", "merge shipped", "shipped")
	git(t, clone, "checkout", "-q", "main")

	r := model.Repo{Branches: []string{"main", "release"}}
	verdicts, err := Classify(clone, r)
	must(t, err)

	got := map[string]Verdict{}
	for _, v := range verdicts {
		got[v.Name] = v
	}
	v, ok := got["shipped"]
	if !ok {
		t.Fatalf("shipped was not classified at all: %+v", verdicts)
	}
	if !v.Prunable {
		t.Errorf("a branch merged to release was not prunable: %s", v.Summary())
	}
	if v.Base != "release" {
		t.Errorf("verdict names base %q, want the branch that actually confirmed", v.Base)
	}
	// release itself is a reference, never a candidate.
	if _, listed := got["release"]; listed {
		t.Error("an important branch was classified as a task branch")
	}
}

// TestUnmergedIsReportedAgainstThePrimary: with nothing confirming, "N ahead"
// has to count against one branch, and one number cannot mean several. The
// primary is what it means — including in the evidence header, which each
// later base would otherwise have overwritten.
func TestUnmergedIsReportedAgainstThePrimary(t *testing.T) {
	_, clone, _ := setupUpstreamRepo(t, `show_branches = "all"`)
	git(t, clone, "checkout", "-q", "-b", "release")
	writeCommit(t, clone, "r", "release\n", "release line")
	git(t, clone, "checkout", "-q", "main")
	git(t, clone, "checkout", "-q", "-b", "parked")
	writeCommit(t, clone, "p", "parked\n", "parked work")
	git(t, clone, "checkout", "-q", "main")

	verdicts, err := Classify(clone, model.Repo{Branches: []string{"main", "release"}})
	must(t, err)

	for _, v := range verdicts {
		if v.Name != "parked" {
			continue
		}
		if v.Prunable || v.Base != "main" {
			t.Errorf("parked = %+v, want unmerged against main", v)
		}
		if v.Ahead != 1 || v.Evidence.Ahead != 1 {
			t.Errorf("ahead = %d (evidence %d), want 1 against main", v.Ahead, v.Evidence.Ahead)
		}
		return
	}
	t.Fatal("parked was not classified")
}

// TestAnImportantBranchThatIsNotHereIsSkipped: `branches` is a statement about
// the repo, not a promise every entry is cloned. A base that doesn't resolve is
// passed over; only having none of them is an error worth stopping for.
func TestAnImportantBranchThatIsNotHereIsSkipped(t *testing.T) {
	_, clone, _ := setupUpstreamRepo(t, `show_branches = "all"`)
	git(t, clone, "checkout", "-q", "-b", "landed")
	writeCommit(t, clone, "l", "landed\n", "landed work")
	merge(t, clone, "landed", false)

	verdicts, err := Classify(clone, model.Repo{Branches: []string{"main", "never-cloned"}})
	must(t, err)
	for _, v := range verdicts {
		if v.Name == "landed" && v.Prunable {
			return
		}
	}
	t.Fatalf("a missing important branch cost the others their verdict: %+v", verdicts)
}
