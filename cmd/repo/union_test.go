package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/discover"
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

func testRegistry(t *testing.T, wd string) *config.Registry {
	t.Helper()
	regPath := filepath.Join(t.TempDir(), "registry.toml")
	if err := os.WriteFile(regPath, []byte(`
[root.wd]
dir = "`+wd+`"
layout = "flat"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := config.Load([]string{regPath})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestDiscoveredRepoUsesOriginDefaultBranch: origin's HEAD symref (what a plain
// `git clone` sets) wins even when the checked-out branch is something else
// entirely — a task branch left checked out must never be mistaken for the
// important one.
func TestDiscoveredRepoUsesOriginDefaultBranch(t *testing.T) {
	wd := t.TempDir()
	origin := filepath.Join(wd, "remote", "proj")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "init", "-q", "-b", "trunk", ".")
	if err := os.WriteFile(filepath.Join(origin, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "f")
	git(t, origin, "commit", "-q", "-m", "one")

	proj := filepath.Join(wd, "proj")
	git(t, wd, "clone", "-q", origin, proj)
	git(t, proj, "checkout", "-q", "-b", "wip")

	reg := testRegistry(t, wd)
	found, err := discover.Discover(resolveRoots(reg), reg)
	if err != nil {
		t.Fatal(err)
	}
	var f *discover.Found
	for i := range found {
		if found[i].Dir == proj {
			f = &found[i]
		}
	}
	if f == nil {
		t.Fatalf("proj not found among %+v", found)
	}
	r := discoveredRepo(reg, *f)
	if got := r.Branches; len(got) != 1 || got[0] != "trunk" {
		t.Errorf("Branches = %v, want [trunk] (origin's default, not the checked-out \"wip\")", got)
	}
}

// TestDiscoveredRepoFallsBackToMainlineName: without an origin default-branch
// symref, a known mainline name among the local branches wins.
func TestDiscoveredRepoFallsBackToMainlineName(t *testing.T) {
	wd := t.TempDir()
	origin := filepath.Join(wd, "remote", "proj")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "init", "-q", "-b", "master", ".")
	if err := os.WriteFile(filepath.Join(origin, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "f")
	git(t, origin, "commit", "-q", "-m", "one")

	proj := filepath.Join(wd, "proj")
	git(t, wd, "clone", "-q", origin, proj)
	git(t, proj, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	git(t, proj, "checkout", "-q", "-b", "wip")

	reg := testRegistry(t, wd)
	found, err := discover.Discover(resolveRoots(reg), reg)
	if err != nil {
		t.Fatal(err)
	}
	var f *discover.Found
	for i := range found {
		if found[i].Dir == proj {
			f = &found[i]
		}
	}
	if f == nil {
		t.Fatalf("proj not found among %+v", found)
	}
	r := discoveredRepo(reg, *f)
	if got := r.Branches; len(got) != 1 || got[0] != "master" {
		t.Errorf("Branches = %v, want [master] (mainline-name fallback)", got)
	}
}

// TestDiscoveredRepoLeavesBranchesEmptyWhenUnresolved: neither signal present
// (no origin symref, no mainline name locally) — Branches must be left empty
// rather than falling back to the checked-out branch.
func TestDiscoveredRepoLeavesBranchesEmptyWhenUnresolved(t *testing.T) {
	wd := t.TempDir()
	origin := filepath.Join(wd, "remote", "proj")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "init", "-q", "-b", "trunk", ".")
	if err := os.WriteFile(filepath.Join(origin, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "f")
	git(t, origin, "commit", "-q", "-m", "one")

	proj := filepath.Join(wd, "proj")
	git(t, wd, "clone", "-q", origin, proj)
	git(t, proj, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")
	git(t, proj, "checkout", "-q", "-b", "wip")

	reg := testRegistry(t, wd)
	found, err := discover.Discover(resolveRoots(reg), reg)
	if err != nil {
		t.Fatal(err)
	}
	var f *discover.Found
	for i := range found {
		if found[i].Dir == proj {
			f = &found[i]
		}
	}
	if f == nil {
		t.Fatalf("proj not found among %+v", found)
	}
	r := discoveredRepo(reg, *f)
	if len(r.Branches) != 0 {
		t.Errorf("Branches = %v, want none (unresolved important branch must be flagged, not guessed)", r.Branches)
	}
}
