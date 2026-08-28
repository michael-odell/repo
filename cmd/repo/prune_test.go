package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// cloneWithLandedBranch builds a clone whose `landed` branch is an ancestor of
// main — the plainest prunable case, and the only tier `git branch -d` will
// also vouch for. main stays checked out, so nothing blocks the deletion.
func cloneWithLandedBranch(t *testing.T, wd, name string) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), name+".git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(origin, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "f")
	git(t, origin, "commit", "-q", "-m", "one")

	dir := filepath.Join(wd, name)
	git(t, wd, "clone", "-q", origin, dir)
	git(t, dir, "checkout", "-q", "-b", "landed")
	if err := os.WriteFile(filepath.Join(dir, "g"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "g")
	git(t, dir, "commit", "-q", "-m", "two")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--ff-only", "landed")
	return dir
}

func selectedRepo(t *testing.T, wd, dir string) []model.Repo {
	t.Helper()
	return []model.Repo{resolved(t, testRegistry(t, wd), dir)}
}

// show reads an object's contents back out of the repo — used to prove that
// what a deleted branch carried is still reachable without it.
func show(t *testing.T, dir, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "show", rev).CombinedOutput()
	if err != nil {
		t.Fatalf("git show %s: %v\n%s", rev, err, out)
	}
	return string(out)
}

func hasBranch(t *testing.T, dir, branch string) bool {
	t.Helper()
	branches, err := gitx.LocalBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range branches {
		if b == branch {
			return true
		}
	}
	return false
}

// TestDryRunDeletesNothing is the guarantee the flag exists for: the deletion
// path runs in full — selection, classification, the confirmation it would ask
// — and the repo is left exactly as it was found.
func TestDryRunDeletesNothing(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")

	var out bytes.Buffer
	if err := runPrune(&out, strings.NewReader(""), selectedRepo(t, wd, dir), pruneOpts{DryRun: true}); err != nil {
		t.Fatal(err)
	}

	if !hasBranch(t, dir, "landed") {
		t.Error("a dry run deleted the branch")
	}
	if _, err := os.Stat(filepath.Join(state, "repo", "prune.log")); !os.IsNotExist(err) {
		t.Error("a dry run wrote to the journal")
	}
	if !strings.Contains(out.String(), "would delete landed") {
		t.Errorf("dry run did not say what it would do:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "re-run without --dry-run") {
		t.Errorf("dry run did not say how to do it for real:\n%s", out.String())
	}
}

// TestDryRunAsksNothing: waiting for an answer that changes nothing would only
// teach the habit of typing y, so the question is printed rather than asked —
// which is also what lets a dry run be useful with no terminal at all.
func TestDryRunAsksNothing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")

	var out bytes.Buffer
	if err := runPrune(&out, strings.NewReader(""), selectedRepo(t, wd, dir), pruneOpts{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would ask about 1 branch(es)") {
		t.Errorf("dry run did not show that it would ask:\n%s", out.String())
	}
	if strings.Contains(out.String(), "not a terminal") {
		t.Errorf("dry run declined for want of a terminal it never needed:\n%s", out.String())
	}
}

// TestDeleteRecordsWhatItRemoved: the record is the reason a deletion is
// answerable for later, so it carries the full SHA and the command that undoes
// it — and prune says where to find it rather than leaving it to be discovered.
func TestDeleteRecordsWhatItRemoved(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")
	sha, _ := gitx.RevParse(dir, "landed")

	var out bytes.Buffer
	if err := runPrune(&out, strings.NewReader(""), selectedRepo(t, wd, dir), pruneOpts{Delete: true, Yes: true}); err != nil {
		t.Fatal(err)
	}

	if hasBranch(t, dir, "landed") {
		t.Fatal("the landed branch survived --delete")
	}
	body, err := os.ReadFile(filepath.Join(state, "repo", "prune.log"))
	if err != nil {
		t.Fatalf("no journal after a deletion: %v", err)
	}
	record := string(body)
	if !strings.Contains(record, "git branch landed "+sha) {
		t.Errorf("the record cannot restore the branch it removed:\n%s", record)
	}
	if !strings.Contains(record, "merged") {
		t.Errorf("the record does not say what justified the deletion:\n%s", record)
	}
	if !strings.Contains(out.String(), "recorded in "+filepath.Join(state, "repo", "prune.log")) {
		t.Errorf("prune did not say where the record went:\n%s", out.String())
	}
}

// TestNothingIsDeletedWithoutAJournal: the record is what makes pruning
// answerable for, so losing it is a reason to stop — discovering it after the
// branches are gone is exactly the wrong order.
func TestNothingIsDeletedWithoutAJournal(t *testing.T) {
	// A regular file where the state directory belongs: MkdirAll cannot
	// proceed, which is the same failure a read-only or full disk produces.
	blocked := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", blocked)
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")

	var out bytes.Buffer
	err := runPrune(&out, strings.NewReader(""), selectedRepo(t, wd, dir), pruneOpts{Delete: true, Yes: true})
	if err == nil {
		t.Fatal("prune deleted branches it could not record")
	}
	if !strings.Contains(err.Error(), "nothing was deleted") {
		t.Errorf("the error does not say the repo was left alone: %v", err)
	}
	if !hasBranch(t, dir, "landed") {
		t.Error("the branch went despite the journal failing")
	}
}

// twoLandedBranches gives the walk-through something to walk: two branches,
// both landed, neither checked out.
func twoLandedBranches(t *testing.T, wd, name string) string {
	t.Helper()
	dir := cloneWithLandedBranch(t, wd, name)
	git(t, dir, "checkout", "-q", "-b", "second")
	if err := os.WriteFile(filepath.Join(dir, "h"), []byte("z"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "h")
	git(t, dir, "commit", "-q", "-m", "three")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--ff-only", "second")
	return dir
}

// TestWalkthroughAsksPerBranch is the whole point of asking one at a time: a
// yes for one branch is not a yes for the next, so a wrong verdict has to get
// past a person on its own rather than inside a bulk answer.
func TestWalkthroughAsksPerBranch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	dir := twoLandedBranches(t, wd, "proj")

	var out bytes.Buffer
	// Branches are walked in the order Classify returns them (git's ref order):
	// "landed" then "second". Keep the first, delete the second.
	err := runPrune(&out, strings.NewReader("n\ny\n"), selectedRepo(t, wd, dir),
		pruneOpts{Delete: true, Interactive: true})
	if err != nil {
		t.Fatal(err)
	}

	if !hasBranch(t, dir, "landed") {
		t.Error("a branch answered n was deleted anyway")
	}
	if hasBranch(t, dir, "second") {
		t.Error("a branch answered y survived")
	}
	if !strings.Contains(out.String(), "prune_keep") {
		t.Errorf("declining did not say how to make the answer permanent:\n%s", out.String())
	}
}

// TestWalkthroughShowsEvidenceBeforeAsking: a prompt is an explanation plus a
// question. Without the evidence it is just a confirmation dialog, which tests
// nothing about whether the verdict was right.
func TestWalkthroughShowsEvidenceBeforeAsking(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")

	var out bytes.Buffer
	if err := runPrune(&out, strings.NewReader("n\n"), selectedRepo(t, wd, dir),
		pruneOpts{Delete: true, Interactive: true}); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	for _, want := range []string{"ancestry", "verdict", "removal", "delete landed?"} {
		if !strings.Contains(body, want) {
			t.Errorf("the question was asked without %q in front of it:\n%s", want, body)
		}
	}
}

// TestWalkthroughQuitStopsEverything: `q` means stop, not skip — a branch after
// the one you quit on must not be taken while you are walking away.
func TestWalkthroughQuitStopsEverything(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	dir := twoLandedBranches(t, wd, "proj")

	var out bytes.Buffer
	if err := runPrune(&out, strings.NewReader("q\n"), selectedRepo(t, wd, dir),
		pruneOpts{Delete: true, Interactive: true}); err != nil {
		t.Fatal(err)
	}
	if !hasBranch(t, dir, "landed") || !hasBranch(t, dir, "second") {
		t.Error("quitting still deleted something")
	}
}

// TestWalkthroughEndOfInputIsNotConsent: input running out mid-question is not
// an answer, and must never be read as one.
func TestWalkthroughEndOfInputIsNotConsent(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")

	var out bytes.Buffer
	if err := runPrune(&out, strings.NewReader(""), selectedRepo(t, wd, dir),
		pruneOpts{Delete: true, Interactive: true}); err != nil {
		t.Fatal(err)
	}
	if !hasBranch(t, dir, "landed") {
		t.Error("end-of-input was treated as yes")
	}
}

// TestWalkthroughAllCoversOnlyItsRepo: "yes to the rest" is a statement about
// the repo in front of you; carrying it onward would turn one endorsement into
// an unbounded one.
func TestWalkthroughAllCoversOnlyItsRepo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	first := twoLandedBranches(t, wd, "aaa")
	second := cloneWithLandedBranch(t, wd, "zzz")

	reg := testRegistry(t, wd)
	repos := []model.Repo{resolved(t, reg, first), resolved(t, reg, second)}

	var out bytes.Buffer
	// "a" clears the first repo; the second repo asks again and gets nothing
	// but an empty line, which is a no.
	if err := runPrune(&out, strings.NewReader("a\n\n"), repos,
		pruneOpts{Delete: true, Interactive: true}); err != nil {
		t.Fatal(err)
	}
	if hasBranch(t, first, "landed") || hasBranch(t, first, "second") {
		t.Error("\"a\" did not cover the rest of its own repo")
	}
	if !hasBranch(t, second, "landed") {
		t.Error("\"a\" carried into the next repo")
	}
}

// TestExplainShowsEachTierItTried: an explanation that listed only the tier
// that answered would read as a conclusion with its working erased — the tiers
// that found nothing are most of what makes the answer believable.
func TestExplainShowsEachTierItTried(t *testing.T) {
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")

	var out bytes.Buffer
	if err := explainBranch(&out, selectedRepo(t, wd, dir), "landed"); err != nil {
		t.Fatal(err)
	}
	body := out.String()
	if !strings.Contains(body, "ancestry") {
		t.Errorf("no tier named in the explanation:\n%s", body)
	}
	if !strings.Contains(body, "git branch -d") {
		t.Errorf("explanation does not say how the branch would be removed:\n%s", body)
	}
}

// TestExplainSaysWhenItHasNothingToExplain: silence would read as "nothing to
// say about that branch", when the truth is usually a typo or an important
// branch, which is never classified.
func TestExplainSaysWhenItHasNothingToExplain(t *testing.T) {
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")

	var out bytes.Buffer
	err := explainBranch(&out, selectedRepo(t, wd, dir), "main")
	if err == nil {
		t.Fatal("explaining an important branch reported success")
	}
	if !strings.Contains(err.Error(), "important branches") {
		t.Errorf("the error does not explain why main has no verdict: %v", err)
	}
}

// squashMergedRepo leaves `feature` landed by squash: the content is in main
// under a different SHA, so only the rewritten tiers see it and removal needs
// -D — the case the cross-check exists for.
func squashMergedRepo(t *testing.T, wd, name string) string {
	t.Helper()
	dir := cloneWithLandedBranch(t, wd, name)
	git(t, dir, "checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "h"), []byte("one\ntwo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "h")
	git(t, dir, "commit", "-q", "-m", "first")
	if err := os.WriteFile(filepath.Join(dir, "h"), []byte("one\ntwo\nthree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-qam", "second")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--squash", "feature")
	git(t, dir, "commit", "-q", "-m", "squashed feature")
	return dir
}

// TestARevertedSquashMergeIsStillPrunable is the case the removed cross-check
// existed to withhold, and the reason removing it is safe.
//
// A branch is squash-merged and the squash is then reverted. The patch tiers
// still say merged — correctly, because they searched main's *history* and the
// squash commit is sitting in it. The old gate read main's current *tree*,
// found the change absent, and refused, on the reasoning that the branch held
// the last copy. It never did: the content is reachable from main whatever
// happens afterwards, which this asserts by reading it back once the branch is
// gone (DESIGN §5.3).
func TestARevertedSquashMergeIsStillPrunable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	dir := squashMergedRepo(t, wd, "proj")

	// main moves on: the squashed content is backed out again.
	landed, _ := gitx.RevParse(dir, "main")
	if err := os.Remove(filepath.Join(dir, "h")); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "commit", "-qam", "back that out")

	var out bytes.Buffer
	if err := runPrune(&out, strings.NewReader(""), selectedRepo(t, wd, dir),
		pruneOpts{Delete: true, Yes: true}); err != nil {
		t.Fatal(err)
	}
	body := out.String()

	if hasBranch(t, dir, "feature") {
		t.Fatalf("a landed branch was withheld because its work was later reverted:\n%s", body)
	}
	// Nothing in the output may imply the tool checked, or failed to check,
	// anything beyond the tiers.
	if strings.Contains(body, "corroborat") {
		t.Errorf("the report still speaks of corroboration:\n%s", body)
	}
	// The whole justification: the work outlives the branch, because the commit
	// carrying it is reachable from main.
	if got := show(t, dir, landed+":h"); got == "" {
		t.Error("the squashed work is not readable from main after the branch went")
	}
}

// TestPruningRemovesTheWorktreeToo: the tree goes first because git refuses to
// delete a branch one still holds — so the other order leaves both behind, and
// under worktrees = true that meant prune could never remove anything.
func TestPruningRemovesTheWorktreeToo(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")
	tree := filepath.Join(t.TempDir(), "landed")
	git(t, dir, "worktree", "add", "-q", tree, "landed")

	var out bytes.Buffer
	if err := runPrune(&out, strings.NewReader(""), selectedRepo(t, wd, dir),
		pruneOpts{Delete: true, Yes: true}); err != nil {
		t.Fatal(err)
	}
	body := out.String()

	if hasBranch(t, dir, "landed") {
		t.Fatalf("the branch survived because its worktree did:\n%s", body)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Errorf("the worktree was left behind: %v", err)
	}
	if !strings.Contains(body, "removed worktree") {
		t.Errorf("the run did not say a directory went:\n%s", body)
	}
}

// TestDryRunNamesTheWorktreeItWouldRemove: a deletion that quietly takes a
// directory with it is a bigger act than the line describing it, so the preview
// has to say so — this is the one place someone checks before consenting.
func TestDryRunNamesTheWorktreeItWouldRemove(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")
	tree := filepath.Join(t.TempDir(), "landed")
	git(t, dir, "worktree", "add", "-q", tree, "landed")

	var out bytes.Buffer
	if err := runPrune(&out, strings.NewReader(""), selectedRepo(t, wd, dir),
		pruneOpts{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "and its worktree") {
		t.Errorf("the preview did not mention the worktree:\n%s", out.String())
	}
	if _, err := os.Stat(tree); err != nil {
		t.Errorf("a dry run removed the worktree: %v", err)
	}
}
