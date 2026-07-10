package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/gitx"
)

// setupWorktreeContainer provisions a worktrees=true repo via Run, yielding a
// real bare+worktree container to exercise the reverse conversion against.
func setupWorktreeContainer(t *testing.T, T string, branches string) (container, regPath, remotes string) {
	t.Helper()
	remotes = filepath.Join(T, "remotes")
	origin := filepath.Join(remotes, "acme", "svc")
	must(t, os.MkdirAll(filepath.Dir(origin), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", origin)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(seed, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("original\n"), 0o644))
	git(t, seed, "add", ".gitignore", "tracked.txt")
	git(t, seed, "commit", "-qm", "init")
	git(t, seed, "branch", "release")
	git(t, seed, "push", "-q", origin, "main", "release")

	container = filepath.Join(T, "wd", "acme", "svc")
	regPath = filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[[repo]]
id = "local:acme/svc"
workflow = "upstream-push"
worktrees = true
branches = [`+branches+`]
home_root = "`+filepath.Join(T, "wd")+`"
layout = "owner"
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	rs, err := reg.Repos()
	must(t, err)
	if r := Run(reg, rs, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour}); r[0].Err != nil {
		t.Fatalf("provisioning worktree fixture failed: %v", r[0].Err)
	}
	if gitx.ClassifyLayout(container) != gitx.LayoutWorktree {
		t.Fatalf("fixture is not a worktree layout")
	}
	return container, regPath, remotes
}

// reloadSingle rewrites the registry to worktrees=false and runs sync
// --fix-layout, returning the result.
func reloadSingle(t *testing.T, T, regPath, remotes string, loseIgnored bool) Result {
	t.Helper()
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[[repo]]
id = "local:acme/svc"
workflow = "upstream-push"
worktrees = false
branches = ["main"]
home_root = "`+filepath.Join(T, "wd")+`"
layout = "owner"
`), 0o644))
	reg, err := config.Load([]string{regPath})
	must(t, err)
	rs, err := reg.Repos()
	must(t, err)
	res := Run(reg, rs, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour, FixLayout: true, LoseIgnored: loseIgnored})
	if len(res) != 1 {
		t.Fatalf("got %d results", len(res))
	}
	return res[0]
}

// TestFixLayoutWorktreeToSingleKeepsPrimaryResidue collapses a worktree layout
// to a single tree, preserving the primary worktree's uncommitted/untracked/
// ignored work and an unpushed local branch.
func TestFixLayoutWorktreeToSingleKeepsPrimaryResidue(t *testing.T) {
	T := t.TempDir()
	container, regPath, remotes := setupWorktreeContainer(t, T, `"main", "release"`)

	// Residue on the primary (main) worktree; an unpushed branch in the repo.
	mainWT := filepath.Join(container, "main")
	must(t, os.WriteFile(filepath.Join(mainWT, "tracked.txt"), []byte("MODIFIED\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(mainWT, "untracked.txt"), []byte("keep\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(mainWT, "ignored.txt"), []byte("junk\n"), 0o644))
	git(t, mainWT, "branch", "spike")

	res := reloadSingle(t, T, regPath, remotes, false)
	if res.Err != nil {
		t.Fatalf("err=%v actions=%v", res.Err, res.Actions)
	}
	if got := gitx.ClassifyLayout(container); got != gitx.LayoutSingle {
		t.Fatalf("layout = %v, want single; actions=%v", got, res.Actions)
	}
	if exists(container + ".pre-single") {
		t.Errorf("staging dir not removed")
	}
	if got := read(t, filepath.Join(container, "tracked.txt")); got != "MODIFIED\n" {
		t.Errorf("uncommitted change lost: %q", got)
	}
	if !exists(filepath.Join(container, "untracked.txt")) {
		t.Errorf("untracked file lost")
	}
	if !exists(filepath.Join(container, "ignored.txt")) {
		t.Errorf("ignored file lost")
	}
	if _, ok := gitx.RevParse(container, "refs/heads/spike"); !ok {
		t.Errorf("unpushed local branch 'spike' lost")
	}
	// The release worktree is gone.
	if exists(filepath.Join(container, "release", ".git")) {
		t.Errorf("release worktree still present")
	}
}

// TestFixLayoutWorktreeToSingleRefusesDirtyNonPrimary refuses to collapse when a
// non-primary worktree holds uncommitted work, leaving everything untouched.
func TestFixLayoutWorktreeToSingleRefusesDirtyNonPrimary(t *testing.T) {
	T := t.TempDir()
	container, regPath, remotes := setupWorktreeContainer(t, T, `"main", "release"`)

	// Uncommitted change in the release (non-primary) worktree.
	must(t, os.WriteFile(filepath.Join(container, "release", "tracked.txt"), []byte("WIP\n"), 0o644))

	res := reloadSingle(t, T, regPath, remotes, false)
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if got := gitx.ClassifyLayout(container); got != gitx.LayoutWorktree {
		t.Errorf("layout = %v, want worktree (must not collapse)", got)
	}
	if got := read(t, filepath.Join(container, "release", "tracked.txt")); got != "WIP\n" {
		t.Errorf("non-primary work was disturbed: %q", got)
	}
}

// TestFixLayoutWorktreeToSingleDiscardsIgnoredWithFlag collapses when a
// non-primary worktree has only ignored residue and --lose-ignored is set.
func TestFixLayoutWorktreeToSingleDiscardsIgnoredWithFlag(t *testing.T) {
	T := t.TempDir()
	container, regPath, remotes := setupWorktreeContainer(t, T, `"main", "release"`)

	// Only an ignored file in the release worktree.
	must(t, os.WriteFile(filepath.Join(container, "release", "ignored.txt"), []byte("junk\n"), 0o644))

	// With the flag: collapse, discarding the ignored residue without prompting.
	// (The no-flag path prompts, whose behavior depends on the TTY, so it is
	// exercised interactively rather than asserted here.)
	res := reloadSingle(t, T, regPath, remotes, true)
	if res.Err != nil {
		t.Fatalf("with --lose-ignored: err=%v actions=%v", res.Err, res.Actions)
	}
	if got := gitx.ClassifyLayout(container); got != gitx.LayoutSingle {
		t.Errorf("layout = %v, want single", got)
	}
}
