package sync

import (
	"os"
	"path/filepath"
	"testing"
)

// Working-tree state is checked once per unit, so it is reported for every tree
// a repo has rather than only for a single clone's one tree (DESIGN §5.1,
// principle 4). Uncommitted changes stop that tree's branch from moving;
// untracked files are surfaced but block nothing.

// TestWorktreeDirtyTreeReportedAndBlocksItsBranch: the gap that motivated this —
// a worktree-layout repo never checked its trees at all, so a dirty worktree was
// neither reported nor protected.
func TestWorktreeDirtyTreeReportedAndBlocksItsBranch(t *testing.T) {
	origin, container, run := setupWorktreeUpstreamRepo(t, "")
	mainWT := filepath.Join(container, "main")
	must(t, os.WriteFile(filepath.Join(mainWT, "a"), []byte("my uncommitted edit\n"), 0o644))

	before := revParse(t, container, "main")
	parent := t.TempDir()
	other := filepath.Join(parent, "other")
	git(t, parent, "clone", "-q", origin, other)
	writeCommit(t, other, "a", "upstream change\n", "upstream change")
	git(t, other, "push", "-q", "origin", "main")

	res := run()
	if got := revParse(t, container, "main"); got != before {
		t.Errorf("main moved to %s, want it left at %s (a dirty tree must not be moved)", got, before)
	}
	body, err := os.ReadFile(filepath.Join(mainWT, "a"))
	must(t, err)
	if string(body) != "my uncommitted edit\n" {
		t.Errorf("working tree file = %q, want the uncommitted edit preserved", body)
	}
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if want := "main: 1 uncommitted change(s) — update skipped"; res.Detail != want {
		t.Errorf("detail = %q, want %q", res.Detail, want)
	}
}

// TestWorktreeDirtyTreeDoesNotBlockSiblingWorktrees: the per-unit half of the
// rule. One dirty worktree is a fact about that tree, not about the repo — every
// other important branch must still be reconciled, and each tree's state gets its
// own line rather than one overwriting the other.
func TestWorktreeDirtyTreeDoesNotBlockSiblingWorktrees(t *testing.T) {
	origin, container, run := setupWorktreeUpstreamRepo(t, "")
	must(t, os.WriteFile(filepath.Join(container, "main", "a"), []byte("edit\n"), 0o644))

	// Advance both branches on origin.
	parent := t.TempDir()
	other := filepath.Join(parent, "other")
	git(t, parent, "clone", "-q", origin, other)
	writeCommit(t, other, "a", "upstream main\n", "upstream main")
	git(t, other, "push", "-q", "origin", "main")
	git(t, other, "checkout", "-q", "-t", "origin/release")
	writeCommit(t, other, "r", "upstream release\n", "upstream release")
	git(t, other, "push", "-q", "origin", "release")
	wantRelease := revParse(t, other, "release")

	res := run()
	if got := revParse(t, container, "release"); got != wantRelease {
		t.Errorf("release = %s, want %s (a dirty main must not hold release back)", got, wantRelease)
	}
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if !hasBranchNote(res, "main", true) {
		t.Errorf("main's dirty-tree finding missing; branches=%+v", res.Branches)
	}
	if !hasBranchNote(res, "release", false) {
		t.Errorf("release's fast-forward missing; branches=%+v", res.Branches)
	}
}

// TestWorktreeUntrackedFilesReportedWithoutBlocking: untracked files are the
// non-blocking half — surfaced per tree, but a fast-forward doesn't write over
// them, so it still happens. The warning must also survive the fast-forward that
// follows it, rather than being overwritten by the milder finding.
func TestWorktreeUntrackedFilesReportedWithoutBlocking(t *testing.T) {
	origin, container, run := setupWorktreeUpstreamRepo(t, "")
	releaseWT := filepath.Join(container, "release")
	must(t, os.WriteFile(filepath.Join(releaseWT, "scratch.txt"), []byte("notes\n"), 0o644))

	parent := t.TempDir()
	other := filepath.Join(parent, "other")
	git(t, parent, "clone", "-q", origin, other)
	git(t, other, "checkout", "-q", "-t", "origin/release")
	writeCommit(t, other, "r", "upstream release\n", "upstream release")
	git(t, other, "push", "-q", "origin", "release")
	want := revParse(t, other, "release")

	res := run()
	if got := revParse(t, container, "release"); got != want {
		t.Errorf("release = %s, want %s (untracked files must not block a fast-forward)", got, want)
	}
	if !exists(filepath.Join(releaseWT, "scratch.txt")) {
		t.Errorf("untracked file was lost")
	}
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	// The sole notable branch folds onto the row (§5.6), so the warning has to
	// show up there — and as the warning, not as the milder fast-forward that
	// came after it.
	if want := "release: 1 untracked file(s)"; res.Detail != want {
		t.Errorf("detail = %q, want %q (the fast-forward must not overwrite the warning)", res.Detail, want)
	}
}

// TestDirtyTreeBlocksTaskBranchPull: a task branch checked out in a dirty tree
// gets the same protection an important branch does — pull-only would otherwise
// fast-forward that tree out from under the edit.
func TestDirtyTreeBlocksTaskBranchPull(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, `task_branches = "pull-only"`)
	git(t, clone, "checkout", "-q", "-b", "feature")
	git(t, clone, "push", "-q", "origin", "feature")
	before := revParse(t, clone, "feature")

	// Advance origin's feature, then dirty the tree that has it checked out.
	parent := t.TempDir()
	other := filepath.Join(parent, "other")
	git(t, parent, "clone", "-q", origin, other)
	git(t, other, "checkout", "-q", "-t", "origin/feature")
	writeCommit(t, other, "a", "upstream change\n", "upstream change")
	git(t, other, "push", "-q", "origin", "feature")
	must(t, os.WriteFile(filepath.Join(clone, "a"), []byte("my uncommitted edit\n"), 0o644))

	res := run()
	if got := revParse(t, clone, "feature"); got != before {
		t.Errorf("feature moved to %s, want it left at %s (dirty tree)", got, before)
	}
	body, err := os.ReadFile(filepath.Join(clone, "a"))
	must(t, err)
	if string(body) != "my uncommitted edit\n" {
		t.Errorf("working tree file = %q, want the uncommitted edit preserved", body)
	}
	if !hasBranchNote(res, "feature", true) {
		t.Errorf("feature's skipped pull was not reported; branches=%+v", res.Branches)
	}
}

// TestDirtyTreeStillPushesTaskBranches pins a deliberate change: a dirty tree
// used to abort the whole repo, so task branches were never even looked at.
// Pushing a committed branch cannot endanger uncommitted work — it doesn't touch
// a working tree at all — so the dirty tree now blocks only the branch whose tree
// it is, and unpushed work still reaches the remote.
func TestDirtyTreeStillPushesTaskBranches(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, `task_branches = "auto"`)
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "f", "wip\n", "wip")
	git(t, clone, "checkout", "-q", "main")
	must(t, os.WriteFile(filepath.Join(clone, "a"), []byte("my uncommitted edit\n"), 0o644))

	res := run()
	if !refExists(t, origin, "feature") {
		t.Errorf("origin missing feature: a dirty tree must not stop an unrelated branch from being pushed")
	}
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention (the dirty tree is still surfaced); actions=%v", res.Outcome, res.Actions)
	}
	if !hasBranchNote(res, "main", true) {
		t.Errorf("main's dirty-tree finding missing; branches=%+v", res.Branches)
	}
}
