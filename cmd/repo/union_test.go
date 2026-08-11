package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/model"
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
	return loadReg(t, `
[root.wd]
dir = "`+wd+`"
layout = "flat"
`)
}

func loadReg(t *testing.T, body string) *config.Registry {
	t.Helper()
	p := filepath.Join(t.TempDir(), "registry.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := config.Load([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// resolved returns the union's entry for the repo whose container is dir — the
// path every command actually goes through, declared and discovered alike.
func resolved(t *testing.T, reg *config.Registry, dir string) model.Repo {
	t.Helper()
	repos, err := unionRepos(reg)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range repos {
		if filepath.Clean(r.Container()) == filepath.Clean(dir) {
			return r
		}
	}
	t.Fatalf("no repo at %s in the union %+v", dir, repos)
	return model.Repo{}
}

// clone creates a remote whose default branch is `trunk`, clones it to
// wd/<name>, and leaves an unrelated branch checked out.
func clone(t *testing.T, wd, name, trunk string) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), name+".git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "init", "-q", "-b", trunk, ".")
	if err := os.WriteFile(filepath.Join(origin, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "f")
	git(t, origin, "commit", "-q", "-m", "one")

	dir := filepath.Join(wd, name)
	git(t, wd, "clone", "-q", origin, dir)
	git(t, dir, "checkout", "-q", "-b", "wip")
	return dir
}

// TestBranchesFromOriginDefault: origin's HEAD symref (what a plain `git clone`
// sets) wins even when the checked-out branch is something else entirely — a
// task branch left checked out must never be mistaken for the important one.
func TestBranchesFromOriginDefault(t *testing.T) {
	wd := t.TempDir()
	proj := clone(t, wd, "proj", "trunk")

	r := resolved(t, testRegistry(t, wd), proj)
	if got := r.Branches; len(got) != 1 || got[0] != "trunk" {
		t.Errorf("Branches = %v, want [trunk] (origin's default, not the checked-out \"wip\")", got)
	}
}

// TestBranchesFromMainlineName: without an origin default-branch symref, a known
// mainline name among the local branches wins.
func TestBranchesFromMainlineName(t *testing.T) {
	wd := t.TempDir()
	proj := clone(t, wd, "proj", "master")
	git(t, proj, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")

	r := resolved(t, testRegistry(t, wd), proj)
	if got := r.Branches; len(got) != 1 || got[0] != "master" {
		t.Errorf("Branches = %v, want [master] (mainline-name fallback)", got)
	}
}

// TestBranchesUnresolvedStayEmpty: neither signal present (no origin symref, no
// mainline name locally) — Branches must be left empty rather than falling back
// to the checked-out branch. sync flags the repo and asks for an explicit
// `branches`.
func TestBranchesUnresolvedStayEmpty(t *testing.T) {
	wd := t.TempDir()
	proj := clone(t, wd, "proj", "trunk")
	git(t, proj, "symbolic-ref", "--delete", "refs/remotes/origin/HEAD")

	r := resolved(t, testRegistry(t, wd), proj)
	if len(r.Branches) != 0 {
		t.Errorf("Branches = %v, want none (unresolved important branch must be flagged, not guessed)", r.Branches)
	}
}

// TestDeclaredRepoStillReadsItsClone is the regression that started this:
// declaring a repo — even for no reason beyond hanging a comment on it — must
// not silently swap the clone's own trunk for a blanket [defaults] assumption,
// which is how a master-only repo ends up reported as "main missing on origin".
func TestDeclaredRepoStillReadsItsClone(t *testing.T) {
	wd := t.TempDir()
	proj := clone(t, wd, "proj", "master")

	reg := loadReg(t, `
[defaults]
branches = ["main"]

[root.wd]
dir = "`+wd+`"
layout = "flat"

[[root.wd.repo]]
id = "gh:me/proj"
# a comment is the whole reason this entry exists
`)
	r := resolved(t, reg, proj)
	if got := r.Branches; len(got) != 1 || got[0] != "master" {
		t.Errorf("Branches = %v, want [master]: [defaults] is an assumption, the clone is a fact", got)
	}
	if !r.ID.Zero() && r.ID.String() != "gh:me/proj" {
		t.Errorf("declared entry lost its identity: %v", r.ID)
	}
}

// TestStatedBranchesOutrankTheClone: config that actually names branches for a
// repo — on the entry or on the root it sits under — still wins over inference
// (DESIGN §3.2), including for a repo that is only discovered. A root's
// `branches` used to be ignored entirely for discovered repos.
func TestStatedBranchesOutrankTheClone(t *testing.T) {
	wd := t.TempDir()
	declared := clone(t, wd, "declared", "trunk")
	found := clone(t, wd, "found", "trunk")

	reg := loadReg(t, `
[root.wd]
dir = "`+wd+`"
layout = "flat"
branches = ["release"]

[[root.wd.repo]]
id = "gh:me/declared"
branches = ["main", "next"]
`)
	if got := resolved(t, reg, declared).Branches; len(got) != 2 || got[0] != "main" || got[1] != "next" {
		t.Errorf("declared Branches = %v, want [main next] (the repo's own entry wins)", got)
	}
	if got := resolved(t, reg, found).Branches; len(got) != 1 || got[0] != "release" {
		t.Errorf("discovered Branches = %v, want [release] (the root states it)", got)
	}
}

// TestUnclonedRepoKeepsTheDefault: with no clone to read, the [defaults]/builtin
// value is the only answer available, and sync provisions against it.
func TestUnclonedRepoKeepsTheDefault(t *testing.T) {
	wd := t.TempDir()
	reg := loadReg(t, `
[defaults]
branches = ["main"]

[root.wd]
dir = "`+wd+`"
layout = "flat"

[[root.wd.repo]]
id = "gh:me/notyet"
`)
	r := resolved(t, reg, filepath.Join(wd, "notyet"))
	if got := r.Branches; len(got) != 1 || got[0] != "main" {
		t.Errorf("Branches = %v, want [main] (nothing cloned to ask)", got)
	}
}
