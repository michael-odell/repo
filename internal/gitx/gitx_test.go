package gitx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestDefaultBranchReadsCloneSymref: a plain `git clone` sets
// refs/remotes/origin/HEAD to the remote's actual default branch — DefaultBranch
// must read that, not assume "main".
func TestDefaultBranchReadsCloneSymref(t *testing.T) {
	origin := t.TempDir()
	git(t, origin, "init", "-q", "-b", "trunk", ".")
	if err := os.WriteFile(origin+"/f", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "f")
	git(t, origin, "commit", "-q", "-m", "one")

	clone := t.TempDir()
	git(t, clone, "clone", "-q", origin, ".")

	got, ok := DefaultBranch(clone, "origin")
	if !ok || got != "trunk" {
		t.Errorf("DefaultBranch = %q, %v; want \"trunk\", true", got, ok)
	}
}

// TestDefaultBranchMissingSymref: without refs/remotes/origin/HEAD (e.g. it
// was pruned, or never set), DefaultBranch must report false rather than
// guessing — the caller falls back to a known mainline name instead.
func TestDefaultBranchMissingSymref(t *testing.T) {
	origin := t.TempDir()
	git(t, origin, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(origin+"/f", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "f")
	git(t, origin, "commit", "-q", "-m", "one")

	clone := t.TempDir()
	git(t, clone, "clone", "-q", origin, ".")
	git(t, clone, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")

	if got, ok := DefaultBranch(clone, "origin"); ok {
		t.Errorf("DefaultBranch = %q, true; want false with the symref removed", got)
	}
}

// TestLocalBranchRefsSurvivesABrokenRef: only the date reads the object, and
// one ref pointing at a missing one makes for-each-ref fail for the entire
// repo. A damaged repo is exactly when the branch inventory matters most, so
// the healthy branches keep their names and tips and the answer degrades to
// "when it moved is unknown" rather than to nothing at all.
func TestLocalBranchRefsSurvivesABrokenRef(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main", ".")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "one")
	if err := os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "broken"),
		[]byte("0000000000000000000000000000000000000001\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	refs, err := LocalBranchRefs(dir)
	if err != nil {
		t.Fatalf("a single broken ref took the whole inventory with it: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("got %d refs, want main and broken: %+v", len(refs), refs)
	}
	for _, r := range refs {
		if r.SHA == "" {
			t.Errorf("%s has no object name, which the refs themselves carry", r.Name)
		}
	}
}
