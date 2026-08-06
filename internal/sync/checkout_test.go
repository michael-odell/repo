package sync

import (
	"os"
	"path/filepath"
	"testing"
)

// Which branch a repo happens to have checked out is the user's business, not
// sync's (DESIGN §5.1). These tests pin that down from both directions: an
// important branch is reconciled whether or not it is HEAD, and a branch that
// *is* HEAD still takes its working tree along when it moves.

// currentBranch returns dir's checked-out branch name.
func currentBranch(t *testing.T, dir string) string {
	t.Helper()
	return trim(mustOutput(t, "git", "-C", dir, "rev-parse", "--abbrev-ref", "HEAD"))
}

// advanceOrigin pushes one more commit to origin's main from a throwaway clone,
// leaving the repo under test behind by exactly one.
func advanceOrigin(t *testing.T, origin, file, content, msg string) string {
	t.Helper()
	parent := t.TempDir()
	other := filepath.Join(parent, "other")
	git(t, parent, "clone", "-q", origin, other)
	writeCommit(t, other, file, content, msg)
	git(t, other, "push", "-q", "origin", "main")
	return revParse(t, other, "main")
}

// TestImportantBranchAdvancesWhileAnotherIsCheckedOut is the case that motivated
// this: a repo parked on a feature branch. main is behind origin and must be
// fast-forwarded in place — reporting "you're on the wrong branch" and skipping
// the update leaves main stale *and* says nothing about where it actually
// stands. The checkout must be left exactly where the user put it.
func TestImportantBranchAdvancesWhileAnotherIsCheckedOut(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, "")
	git(t, clone, "checkout", "-q", "-b", "feature")
	git(t, clone, "push", "-q", "origin", "feature") // in sync, so not notable itself
	want := advanceOrigin(t, origin, "b", "two\n", "two")

	res := run()
	if got := revParse(t, clone, "main"); got != want {
		t.Errorf("local main = %s, want %s (main must advance while feature is checked out)", got, want)
	}
	if got := currentBranch(t, clone); got != "feature" {
		t.Errorf("HEAD = %q, want %q (sync must not move the user's checkout)", got, "feature")
	}
	if res.Outcome != Updated {
		t.Fatalf("outcome = %v, want Updated; actions=%v", res.Outcome, res.Actions)
	}
	if want := "main: fast-forwarded +1"; res.Detail != want {
		t.Errorf("detail = %q, want %q", res.Detail, want)
	}
}

// TestImportantBranchAdvancesWhileDetachedHEAD: mid-bisect, or parked on a tag.
// A detached HEAD means no branch is checked out at all, which is simply the
// easy case for a ref-level fast-forward — it must not block the update either.
func TestImportantBranchAdvancesWhileDetachedHEAD(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, "")
	git(t, clone, "checkout", "-q", "--detach")
	detached := revParse(t, clone, "HEAD")
	want := advanceOrigin(t, origin, "b", "two\n", "two")

	res := run()
	if got := revParse(t, clone, "main"); got != want {
		t.Errorf("local main = %s, want %s (a detached HEAD must not block the update)", got, want)
	}
	if got := revParse(t, clone, "HEAD"); got != detached {
		t.Errorf("HEAD = %s, want %s (sync must leave a detached HEAD alone)", got, detached)
	}
	if res.Outcome != Updated {
		t.Fatalf("outcome = %v, want Updated; actions=%v", res.Outcome, res.Actions)
	}
}

// TestCheckedOutImportantBranchAdvancesWorkingTree is the other half of the
// contract, and the reason sync can't just move refs unconditionally: when the
// important branch *is* the checked-out one, its working tree has to advance
// with it, or the fast-forward would leave the user staring at stale files.
func TestCheckedOutImportantBranchAdvancesWorkingTree(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, "")
	want := advanceOrigin(t, origin, "a", "two\n", "two") // same file, new content

	res := run()
	if got := revParse(t, clone, "main"); got != want {
		t.Errorf("local main = %s, want %s", got, want)
	}
	body, err := os.ReadFile(filepath.Join(clone, "a"))
	must(t, err)
	if string(body) != "two\n" {
		t.Errorf("working tree file = %q, want %q (a checked-out branch must bring its tree along)", body, "two\n")
	}
	if res.Outcome != Updated {
		t.Fatalf("outcome = %v, want Updated; actions=%v", res.Outcome, res.Actions)
	}
}

// TestImportantBranchPushesAheadWhileAnotherIsCheckedOut: the push side of the
// same principle. Local commits on main reach origin under push=auto no matter
// which branch is currently checked out.
func TestImportantBranchPushesAheadWhileAnotherIsCheckedOut(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, `push = "auto"`)
	writeCommit(t, clone, "b", "two\n", "two")
	want := revParse(t, clone, "main")
	git(t, clone, "checkout", "-q", "-b", "feature")
	git(t, clone, "push", "-q", "origin", "feature")

	res := run()
	if got := revParse(t, origin, "main"); got != want {
		t.Errorf("origin main = %s, want %s (ahead commits must push from any checkout)", got, want)
	}
	if got := currentBranch(t, clone); got != "feature" {
		t.Errorf("HEAD = %q, want %q", got, "feature")
	}
	if res.Outcome != Updated {
		t.Fatalf("outcome = %v, want Updated; actions=%v", res.Outcome, res.Actions)
	}
}

// TestImportantBranchFollowsRewriteWhileAnotherIsCheckedOut: force_pull's
// non-fast-forward reset also has to work on a branch that isn't HEAD, since
// that's the whole point — otherwise a matched branch would silently stop
// following its remote whenever the user was parked elsewhere.
func TestImportantBranchFollowsRewriteWhileAnotherIsCheckedOut(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, `force_pull = ["main"]`)
	git(t, clone, "checkout", "-q", "-b", "feature")
	git(t, clone, "push", "-q", "origin", "feature")

	// Rewrite origin's main so the local copy is neither ahead nor a
	// fast-forward away — the rewrite case, not a plain advance.
	parent := t.TempDir()
	other := filepath.Join(parent, "other")
	git(t, parent, "clone", "-q", origin, other)
	writeCommit(t, other, "b", "two\n", "two")
	writeCommit(t, other, "b", "rewritten\n", "rewritten")
	git(t, other, "reset", "-q", "--hard", "HEAD~1")
	writeCommit(t, other, "c", "different\n", "different")
	git(t, other, "push", "-q", "--force", "origin", "main")
	want := revParse(t, other, "main")

	res := run()
	if got := revParse(t, clone, "main"); got != want {
		t.Errorf("local main = %s, want %s (a matched force_pull must follow while parked elsewhere)", got, want)
	}
	if got := currentBranch(t, clone); got != "feature" {
		t.Errorf("HEAD = %q, want %q", got, "feature")
	}
	if res.Outcome != Updated {
		t.Fatalf("outcome = %v, want Updated; actions=%v", res.Outcome, res.Actions)
	}
}
