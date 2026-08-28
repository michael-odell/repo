package gitx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func commit(t *testing.T, dir, file, content, msg string) {
	t.Helper()
	writeFile(t, filepath.Join(dir, file), content)
	gitT(t, dir, "add", file)
	gitT(t, dir, "commit", "-q", "-m", msg)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := writeFileRaw(path, content); err != nil {
		t.Fatal(err)
	}
}

// newRepo returns a repo on main with one commit, plus a `feature` branch
// carrying two commits of its own.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main", ".")
	gitT(t, dir, "config", "user.email", "a@b")
	gitT(t, dir, "config", "user.name", "A")
	commit(t, dir, "f", "base\n", "base")
	gitT(t, dir, "checkout", "-q", "-b", "feature")
	commit(t, dir, "f", "base\none\n", "c1")
	commit(t, dir, "f", "base\none\ntwo\n", "c2")
	gitT(t, dir, "checkout", "-q", "main")
	return dir
}

// TestMergedStateTiers walks the three ways work lands, each of which defeats
// the tier below it (DESIGN §5.3). The point of the ladder is that an ancestry
// test — all `git branch --merged` can do — misses the latter two entirely, so
// a fully landed branch reads as unfinished work forever.
func TestMergedStateTiers(t *testing.T) {
	t.Run("unmerged", func(t *testing.T) {
		dir := newRepo(t)
		assertState(t, dir, "feature", "main", Unmerged)
	})

	t.Run("ancestor", func(t *testing.T) {
		dir := newRepo(t)
		gitT(t, dir, "merge", "-q", "--ff-only", "feature")
		assertState(t, dir, "feature", "main", MergedAncestor)
	})

	// A single-commit branch squashed into base produces that commit's exact
	// patch, so the patch tier finds it and the squash tier is never reached.
	// The label follows the evidence rather than guessing at the merge style.
	t.Run("squash of one commit is found by the patch tier", func(t *testing.T) {
		dir := t.TempDir()
		gitT(t, dir, "init", "-q", "-b", "main", ".")
		gitT(t, dir, "config", "user.email", "a@b")
		gitT(t, dir, "config", "user.name", "A")
		commit(t, dir, "f", "base\n", "base")
		gitT(t, dir, "checkout", "-q", "-b", "one")
		commit(t, dir, "f", "base\nonly\n", "sole commit")
		gitT(t, dir, "checkout", "-q", "main")
		gitT(t, dir, "merge", "-q", "--squash", "one")
		gitT(t, dir, "commit", "-q", "-m", "squashed one")
		assertState(t, dir, "one", "main", MergedPatch)
	})

	t.Run("patch: replayed onto a moved base", func(t *testing.T) {
		dir := newRepo(t)
		commit(t, dir, "side", "moved\n", "main moved on") // forces new SHAs
		gitT(t, dir, "cherry-pick", "feature~1", "feature")
		commit(t, dir, "later", "later\n", "later main work")
		assertState(t, dir, "feature", "main", MergedPatch)
	})

	t.Run("squash: one commit carrying the whole diff", func(t *testing.T) {
		dir := newRepo(t)
		gitT(t, dir, "merge", "-q", "--squash", "feature")
		gitT(t, dir, "commit", "-q", "-m", "squashed feature")
		commit(t, dir, "later", "later\n", "later main work")
		assertState(t, dir, "feature", "main", MergedSquash)
	})

	// A branch that keeps working after its merge is unmerged again — the
	// classification is about what is outstanding now, not what once landed.
	t.Run("squash then new work", func(t *testing.T) {
		dir := newRepo(t)
		gitT(t, dir, "merge", "-q", "--squash", "feature")
		gitT(t, dir, "commit", "-q", "-m", "squashed feature")
		gitT(t, dir, "checkout", "-q", "feature")
		commit(t, dir, "f", "base\none\ntwo\nthree\n", "c3")
		gitT(t, dir, "checkout", "-q", "main")
		assertState(t, dir, "feature", "main", Unmerged)
	})
}

// TestPatchTierAccountsForMergeCommits: `git cherry` compares patches and a merge
// commit has none, so it passes over merges without a word. A branch whose only
// commit ahead of base is a merge therefore drew no "+" line at all, which tier 2
// read as "every commit landed" — and the merge's hand-written resolution, the one
// kind of work that exists on no commit as a patch, was pruned away with the
// branch. A merge that only joins its parents still has to stay prunable, or
// every long-lived branch that ever caught up with main becomes unprunable.
func TestPatchTierAccountsForMergeCommits(t *testing.T) {
	// setup returns a repo where `feature` is ahead of main by one merge commit
	// and nothing else, having merged main in after main moved on.
	setup := func(t *testing.T) string {
		t.Helper()
		dir := t.TempDir()
		gitT(t, dir, "init", "-q", "-b", "main", ".")
		gitT(t, dir, "config", "user.email", "a@b")
		gitT(t, dir, "config", "user.name", "A")
		commit(t, dir, "f", "base\n", "base")
		gitT(t, dir, "branch", "feature")
		commit(t, dir, "f", "base\nmain work\n", "main work")
		gitT(t, dir, "checkout", "-q", "feature")
		gitT(t, dir, "merge", "-q", "--no-ff", "main", "-m", "merge main into feature")
		return dir
	}

	t.Run("a merge holding work of its own is not landed", func(t *testing.T) {
		dir := setup(t)
		writeFile(t, filepath.Join(dir, "resolution"), "exists only on the branch\n")
		gitT(t, dir, "add", "resolution")
		gitT(t, dir, "commit", "-q", "--amend", "--no-edit")
		gitT(t, dir, "checkout", "-q", "main")

		if _, ok := RevParse(dir, "main:resolution"); ok {
			t.Fatal("fixture: main already carries the resolution")
		}
		assertState(t, dir, "feature", "main", Unmerged)
	})

	t.Run("a merge that only joins its parents is landed", func(t *testing.T) {
		dir := setup(t)
		gitT(t, dir, "checkout", "-q", "main")
		state, err := MergedState(dir, "feature", "main", DefaultScanLimit())
		if err != nil {
			t.Fatalf("MergedState: %v", err)
		}
		if !state.Merged() {
			t.Errorf("MergedState = %v; a branch adding nothing but a clean merge has landed", state)
		}
	})
}

// TestSquashTierWorksWithoutAConfiguredIdentity: tier 3 writes a scratch commit
// object, and git refuses to write one unless it can determine a committer.
// Auto-detection is declined wherever the hostname has no domain — CI runners,
// containers — so a tier relying on the ambient identity passes on a laptop and
// fails everywhere else. The blast radius was far wider than the tier: an
// unclassifiable branch is skipped, so every branch on a repo vanished from the
// report at once.
//
// The repo here has no identity in its config and none to auto-detect; the
// fixture's own commits pass one per invocation, where the code under test
// cannot inherit it.
func TestSquashTierWorksWithoutAConfiguredIdentity(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	writeFile(t, cfg, "[user]\n\tuseConfigOnly = true\n")
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main", ".")
	commitAs := func(file, content, msg string) {
		t.Helper()
		writeFile(t, filepath.Join(dir, file), content)
		gitT(t, dir, "add", file)
		gitT(t, dir, "-c", "user.name=A", "-c", "user.email=a@b", "commit", "-q", "-m", msg)
	}

	commitAs("f", "base\n", "base")
	gitT(t, dir, "checkout", "-q", "-b", "feature")
	commitAs("f", "base\none\n", "c1")
	commitAs("f", "base\none\ntwo\n", "c2")
	gitT(t, dir, "checkout", "-q", "main")
	gitT(t, dir, "merge", "-q", "--squash", "feature")
	commitAs("f", "base\none\ntwo\n", "squashed feature")
	commitAs("later", "later\n", "later main work")

	assertState(t, dir, "feature", "main", MergedSquash)
}

// TestScanLimitDeclinesFarDivergence: the patch-id tiers walk every commit on
// both sides of the divergence, generating and hashing a diff for each, so an
// abandoned branch on an old repo can cost thousands of diffs — six of those in
// parallel is enough to exhaust a machine. Past the limit the tiers are declined
// rather than attempted, and the branch is reported as unclassified.
func TestScanLimitDeclinesFarDivergence(t *testing.T) {
	t.Setenv(scanLimitEnv, "2") // feature is 2 ahead / 2 behind: 4 apart
	dir := newRepo(t)
	gitT(t, dir, "merge", "-q", "--squash", "feature")
	gitT(t, dir, "commit", "-q", "-m", "squashed feature")
	commit(t, dir, "later", "later\n", "later main work")

	state, err := MergedState(dir, "feature", "main", DefaultScanLimit())
	if !errors.Is(err, ErrTooFarDiverged) {
		t.Fatalf("MergedState = %v, %v; want ErrTooFarDiverged", state, err)
	}
	if state.Merged() {
		t.Errorf("state = %v; a declined branch must never read as merged", state)
	}
	if !strings.Contains(err.Error(), scanLimitEnv) {
		t.Errorf("error = %q, want it to name %s so the limit can be raised", err, scanLimitEnv)
	}

	// Raising the limit gets the real answer back: the guard is about cost, not
	// about the branch being unclassifiable.
	t.Setenv(scanLimitEnv, "1000")
	assertState(t, dir, "feature", "main", MergedSquash)
}

// TestScanLimitKeepsTheAncestryTier: the cheap tier runs before the guard, so
// the ordinary case — a branch whose commits are literally in the base — is
// still recognised however far the two have drifted. Only the expensive tiers
// are given up, which is what keeps the guard from turning landed branches into
// permanent unknowns.
func TestScanLimitKeepsTheAncestryTier(t *testing.T) {
	t.Setenv(scanLimitEnv, "1")
	dir := newRepo(t)
	gitT(t, dir, "merge", "-q", "--ff-only", "feature")
	assertState(t, dir, "feature", "main", MergedAncestor)
}

// TestDeleteBranchForceNeededForSquash pins why the tiers matter operationally:
// git's own `-d` check is an ancestry test, so it refuses a squash-merged
// branch that tier 3 has confirmed is fully landed.
func TestDeleteBranchForceNeededForSquash(t *testing.T) {
	dir := newRepo(t)
	gitT(t, dir, "merge", "-q", "--squash", "feature")
	gitT(t, dir, "commit", "-q", "-m", "squashed feature")

	if err := DeleteBranch(dir, "feature", false); err == nil {
		t.Errorf("git -d deleted a squash-merged branch; the tiers would be unnecessary if it could see the merge")
	}
	if err := DeleteBranch(dir, "feature", true); err != nil {
		t.Errorf("git -D failed: %v", err)
	}
	if _, ok := RevParse(dir, "refs/heads/feature"); ok {
		t.Errorf("branch survived deletion")
	}
}

func assertState(t *testing.T, dir, branch, base string, want MergeState) {
	t.Helper()
	got, err := MergedState(dir, branch, base, DefaultScanLimit())
	if err != nil {
		t.Fatalf("MergedState: %v", err)
	}
	if got != want {
		t.Errorf("MergedState(%s, %s) = %v, want %v", branch, base, got, want)
	}
}

func writeFileRaw(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
