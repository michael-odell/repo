package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/gitx"
)

// TestWorktreeProvisionForkPR provisions a fresh worktrees=true fork-pr repo: a
// bare container with a per-branch worktree, each fast-forwarded to the
// definitive upstream and pushed to the fork.
func TestWorktreeProvisionForkPR(t *testing.T) {
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	up := filepath.Join(remotes, "up", "proj")
	fork := filepath.Join(remotes, "fork", "proj")
	must(t, os.MkdirAll(filepath.Dir(up), 0o755))
	must(t, os.MkdirAll(filepath.Dir(fork), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", up)
	git(t, T, "init", "-q", "-b", "main", "--bare", fork)

	// Seed upstream with main and release; the fork starts one commit behind on
	// each branch so sync must fast-forward and push.
	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", up, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	writeCommit(t, seed, "a", "one\n", "one")
	git(t, seed, "checkout", "-q", "-b", "release")
	git(t, seed, "checkout", "-q", "main")
	git(t, seed, "push", "-q", up, "main", "release")
	git(t, seed, "push", "-q", fork, "main", "release") // fork = reviewed point
	writeCommit(t, seed, "a", "one\ntwo\n", "two")
	git(t, seed, "push", "-q", up, "main") // upstream main advances

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[[repo]]
id = "local:up/proj"
fork = "local:fork/proj"
workflow = "fork-pr"
worktrees = true
branches = ["main", "release"]
home_root = "`+filepath.Join(T, "wd")+`"
layout = "owner"
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour})
	if len(results) != 1 || results[0].Outcome != Updated {
		t.Fatalf("outcome = %v (want Updated); actions=%v err=%v", results[0].Outcome, results[0].Actions, results[0].Err)
	}

	container := filepath.Join(T, "wd", "up", "proj")
	if got := gitx.ClassifyLayout(container); got != gitx.LayoutWorktree {
		t.Fatalf("container layout = %v, want worktree", got)
	}
	if _, err := os.Stat(filepath.Join(container, ".bare")); err != nil {
		t.Errorf("missing .bare: %v", err)
	}
	// main worktree fast-forwarded to upstream (2 commits) and pushed to fork.
	if got := headCount(t, filepath.Join(container, "main")); got != "2" {
		t.Errorf("main worktree at %s commits, want 2", got)
	}
	if got := bareCount(t, fork, "main"); got != "2" {
		t.Errorf("fork main at %s commits, want 2 (should be pushed)", got)
	}
	// release worktree exists at its (unchanged) commit.
	if got := headCount(t, filepath.Join(container, "release")); got != "1" {
		t.Errorf("release worktree at %s commits, want 1", got)
	}
}

// TestLayoutMismatchSurfacesWithoutReorganizing verifies that a config wanting
// worktrees over an existing single clone is reconciled as a single tree (data
// still syncs) and flagged for sync --fix-layout, never grafted into worktrees.
func TestLayoutMismatchSurfacesWithoutReorganizing(t *testing.T) {
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin := filepath.Join(remotes, "up", "proj")
	must(t, os.MkdirAll(filepath.Dir(origin), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", origin)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	writeCommit(t, seed, "a", "one\n", "one")
	git(t, seed, "push", "-q", origin, "main")

	// Pre-existing single clone at the container path where worktrees are wanted.
	container := filepath.Join(T, "wd", "up", "proj")
	must(t, os.MkdirAll(filepath.Dir(container), 0o755))
	git(t, T, "clone", "-q", origin, container)

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[[repo]]
id = "local:up/proj"
workflow = "upstream-push"
worktrees = true
branches = ["main"]
home_root = "`+filepath.Join(T, "wd")+`"
layout = "owner"
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour})
	if len(results) != 1 || results[0].Outcome != Attention {
		t.Fatalf("outcome = %v (want Attention); actions=%v", results[0].Outcome, results[0].Actions)
	}
	// Still a single clone — no worktree subdir was grafted in.
	if got := gitx.ClassifyLayout(container); got != gitx.LayoutSingle {
		t.Errorf("layout = %v, want single (must not be reorganized)", got)
	}
	if _, err := os.Stat(filepath.Join(container, "main")); err == nil {
		t.Errorf("a main/ worktree was grafted into the single clone")
	}
}
