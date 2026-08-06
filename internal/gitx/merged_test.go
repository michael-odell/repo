package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
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
	got, err := MergedState(dir, branch, base)
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
