package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestMirrorDoesNotAdvancePastReview builds a bare upstream that is one commit
// ahead of a bare fork, then syncs a supply-chain-mirror repo cloned from the
// fork. The result must be ReviewPending and the local clone must NOT advance.
func TestMirrorDoesNotAdvancePastReview(t *testing.T) {
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	up := filepath.Join(remotes, "up", "proj")
	fork := filepath.Join(remotes, "fork", "proj")
	must(t, os.MkdirAll(filepath.Dir(up), 0o755))
	must(t, os.MkdirAll(filepath.Dir(fork), 0o755))
	git(t, T, "init", "-q", "--bare", up)
	git(t, T, "init", "-q", "--bare", fork)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", up, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "one")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", fork, "main") // fork = reviewed point (commit 1)
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\ntwo\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "two")
	git(t, seed, "push", "-q", "origin", "main") // upstream = commit 1+2

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[[repo]]
id = "local:up/proj"
fork = "local:fork/proj"
workflow = "supply-chain-mirror"
branches = ["main"]
home_root = "`+filepath.Join(T, "clones")+`"
path = "flat"
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Outcome != ReviewPending {
		t.Errorf("outcome = %v, want ReviewPending; actions=%v", results[0].Outcome, results[0].Actions)
	}

	// The clone must remain at the reviewed point (1 commit), not the upstream 2.
	clone := filepath.Join(T, "clones", "proj")
	out, err := exec.Command("git", "-C", clone, "rev-list", "--count", "HEAD").Output()
	must(t, err)
	if got := trim(string(out)); got != "1" {
		t.Errorf("clone advanced to %s commits, want 1 (must not pass review)", got)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
