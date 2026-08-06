package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
)

// setupWorktreeUpstreamRepo builds a bare origin with two branches (main,
// release), a worktrees=true upstream-push repo tracking both (extra appended
// verbatim to the repo table, e.g. a `hooks` entry), and syncs it once (the
// initial provisioning clone). Returns a run closure for subsequent syncs,
// mirroring setupUpstreamRepo but for the worktree layout.
func setupWorktreeUpstreamRepo(t *testing.T, extra string) (origin, container string, run func() Result) {
	t.Helper()
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin = filepath.Join(remotes, "acme", "proj")
	must(t, os.MkdirAll(filepath.Dir(origin), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", origin)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	writeCommit(t, seed, "a", "one\n", "one")
	git(t, seed, "checkout", "-q", "-b", "release")
	git(t, seed, "checkout", "-q", "main")
	git(t, seed, "push", "-q", "origin", "main", "release")

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.wd]
dir = "`+filepath.Join(T, "wd")+`"
layout = "owner"
[[root.wd.repo]]
id = "local:acme/proj"
workflow = "upstream-push"
worktrees = true
branches = ["main", "release"]
`+extra+`
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	out := filepath.Join(T, "out")
	if results := Run(reg, repos, Options{StateDir: out, Frequency: time.Hour}); len(results) != 1 {
		t.Fatalf("initial sync: got %d results, want 1", len(results))
	}
	container = filepath.Join(T, "wd", "acme", "proj")
	run = func() Result {
		results := Run(reg, repos, Options{StateDir: out, Frequency: time.Hour})
		if len(results) != 1 {
			t.Fatalf("got %d results, want 1", len(results))
		}
		return results[0]
	}
	return origin, container, run
}

// TestReportFoldsSingleNotableTaskBranchWithName: a repo with more than one
// local branch, but only one of them notable, folds that branch's finding
// onto the repo's own row — named, so a multi-branch repo doesn't leave the
// reader guessing which branch it's about (DESIGN §5.6).
func TestReportFoldsSingleNotableTaskBranchWithName(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, "") // default: task_branches=report
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "f", "wip\n", "wip")
	git(t, clone, "checkout", "-q", "main")

	res := run()
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention", res.Outcome)
	}
	if want := "feature: never pushed"; res.Detail != want {
		t.Errorf("detail = %q, want %q", res.Detail, want)
	}
	if len(res.Branches) != 0 {
		t.Errorf("branches = %+v, want none (a lone notable branch folds onto the row, no bullet)", res.Branches)
	}
}

// TestReportFoldsSingleNotableImportantBranchWithName: the worktree
// counterpart — with two important branches, only one of which is notable,
// the repo's own row names it exactly as a task branch's lone finding would,
// rather than reading identically to the single-branch case (DESIGN §5.6).
func TestReportFoldsSingleNotableImportantBranchWithName(t *testing.T) {
	origin, container, run := setupWorktreeUpstreamRepo(t, "")

	// Advance origin's main only; release stays untouched.
	other := t.TempDir()
	otherClone := filepath.Join(other, "seed2")
	git(t, other, "clone", "-q", origin, otherClone)
	writeCommit(t, otherClone, "b", "two\n", "two")
	git(t, otherClone, "push", "-q", "origin", "main")

	res := run()
	if res.Outcome != Updated {
		t.Fatalf("outcome = %v, want Updated; actions=%v", res.Outcome, res.Actions)
	}
	if want := "main: fast-forwarded +1"; res.Detail != want {
		t.Errorf("detail = %q, want %q", res.Detail, want)
	}
	if len(res.Branches) != 0 {
		t.Errorf("branches = %+v, want none (a lone notable branch folds onto the row, no bullet)", res.Branches)
	}
	if got := headCount(t, filepath.Join(container, "main")); got != "2" {
		t.Errorf("main worktree at %s commits, want 2", got)
	}
}

// TestReportRollsUpMultipleNotableBranches: once two or more branches are
// notable — important or task, mixed — the row collapses to a count and
// every one of them breaks out as its own sub-bullet, so a second problem
// branch is never silently dropped the way it would be if only the checked-
// out (important) branch could ever claim the row (DESIGN §5.6).
func TestReportRollsUpMultipleNotableBranches(t *testing.T) {
	origin, container, run := setupWorktreeUpstreamRepo(t, "")

	// Advance origin's main so it will fast-forward (Updated).
	other := t.TempDir()
	otherClone := filepath.Join(other, "seed2")
	git(t, other, "clone", "-q", origin, otherClone)
	writeCommit(t, otherClone, "b", "two\n", "two")
	git(t, otherClone, "push", "-q", "origin", "main")

	// A never-pushed task branch (Attention), created against the shared bare
	// repo via one of the worktrees.
	git(t, filepath.Join(container, "main"), "branch", "wip")

	res := run()
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if want := "2 branches need attention"; res.Detail != want {
		t.Errorf("detail = %q, want %q", res.Detail, want)
	}
	if !hasBranchNote(res, "main", false) {
		t.Errorf("no Updated branch note for main; branches=%+v", res.Branches)
	}
	if !hasBranchNote(res, "wip", true) {
		t.Errorf("no Attention branch note for wip; branches=%+v", res.Branches)
	}
	if len(res.Branches) != 2 {
		t.Errorf("branches = %+v, want exactly 2", res.Branches)
	}
}

// TestReportCountsBoringBranches: a repo with several local branches, all
// quietly in sync, reports how many it checked instead of reading identically
// to a single-branch repo — a quiet multi-branch repo should look *checked*,
// not merely unexamined (DESIGN §5.6).
func TestReportCountsBoringBranches(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, "")
	for _, b := range []string{"b1", "b2"} {
		git(t, clone, "checkout", "-q", "-b", b)
		git(t, clone, "push", "-q", "origin", b)
	}
	git(t, clone, "checkout", "-q", "main")

	res := run()
	if res.Outcome != UpToDate {
		t.Fatalf("outcome = %v, want UpToDate; actions=%v", res.Outcome, res.Actions)
	}
	if want := "3 branches up to date"; res.Detail != want {
		t.Errorf("detail = %q, want %q", res.Detail, want)
	}
}

// TestReportSingleBranchStaysUnnamed: the overwhelmingly common case — one
// repo, one branch, nothing notable — must read exactly as it did before this
// mechanism existed: no branch name, no count, just "up to date".
func TestReportSingleBranchStaysUnnamed(t *testing.T) {
	_, _, run := setupUpstreamRepo(t, "")

	res := run()
	if res.Outcome != UpToDate {
		t.Fatalf("outcome = %v, want UpToDate; actions=%v", res.Outcome, res.Actions)
	}
	if res.Detail != "up to date" {
		t.Errorf("detail = %q, want %q (no noise added to the single-branch case)", res.Detail, "up to date")
	}
	if len(res.Branches) != 0 {
		t.Errorf("branches = %+v, want none", res.Branches)
	}
}

// TestReportWorseRepoLevelFactOwnsRowWithBranchesStillListed: a strictly
// worse non-branch fact (here, a failed hook) keeps the repo's own row, but
// every notable branch still renders as its own sub-bullet underneath —
// nothing about a worse repo-level problem should hide branch-level ones
// (DESIGN §5.6).
func TestReportWorseRepoLevelFactOwnsRowWithBranchesStillListed(t *testing.T) {
	origin, container, run := setupWorktreeUpstreamRepo(t, `hooks = [ { after = "fetch", run = "exit 1" } ]`)

	other := t.TempDir()
	otherClone := filepath.Join(other, "seed2")
	git(t, other, "clone", "-q", origin, otherClone)
	writeCommit(t, otherClone, "b", "two\n", "two")
	git(t, otherClone, "push", "-q", "origin", "main")
	git(t, filepath.Join(container, "main"), "branch", "wip")

	res := run()
	// A hook failure and a branch Attention both rank Attention; the hook ran
	// last (after every branch), so it owns the row per mark()'s tie-break —
	// finalizeBranches must not overwrite it with the branch rollup text.
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if res.Detail != "hook failed" {
		t.Errorf("detail = %q, want %q (hook failure must own the row)", res.Detail, "hook failed")
	}
	if !hasBranchNote(res, "main", false) || !hasBranchNote(res, "wip", true) {
		t.Errorf("branch findings missing despite a worse repo-level fact; branches=%+v", res.Branches)
	}
}

// TestReportWorseRepoLevelFactOwnsRowWithSoleBranchStillListed: the single-
// branch counterpart to the test above. A non-branch fact (here, a failed
// hook) owns the row, and the one notable task branch must still surface as a
// sub-bullet rather than being dropped — finalizeBranches' case-1 path used to
// unconditionally clear res.Branches regardless of who owned the row, silently
// discarding the branch's own finding (e.g. "push this new branch") whenever
// some other fact happened to own the row too (DESIGN §5.6).
func TestReportWorseRepoLevelFactOwnsRowWithSoleBranchStillListed(t *testing.T) {
	_, clone, run := setupUpstreamRepo(t, `hooks = [ { after = "fetch", run = "exit 1" } ]`)
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "f", "wip\n", "wip")
	git(t, clone, "checkout", "-q", "main")

	res := run()
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if want := "hook failed"; res.Detail != want {
		t.Errorf("detail = %q, want %q (the non-branch fact must own the row)", res.Detail, want)
	}
	if !hasBranchNote(res, "feature", true) {
		t.Errorf("feature's finding missing despite a worse repo-level fact; branches=%+v", res.Branches)
	}
}
