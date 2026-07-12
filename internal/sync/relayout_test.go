package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/gitx"
)

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	must(t, err)
	return string(b)
}

// TestFixLayoutSingleToWorktreeIsLossless converts a single clone that carries
// uncommitted, untracked, and ignored work plus an unpushed local branch into a
// bare+worktree layout, asserting nothing is lost.
func TestFixLayoutSingleToWorktreeIsLossless(t *testing.T) {
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin := filepath.Join(remotes, "acme", "svc")
	must(t, os.MkdirAll(filepath.Dir(origin), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", origin)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(seed, "tracked.txt"), []byte("original\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(seed, ".gitignore"), []byte("ignored.txt\n"), 0o644))
	git(t, seed, "add", "tracked.txt", ".gitignore")
	git(t, seed, "commit", "-qm", "init")
	git(t, seed, "branch", "release")
	git(t, seed, "push", "-q", origin, "main", "release")

	// Pre-existing single clone at the container path (the "set up wrong" case).
	container := filepath.Join(T, "wd", "acme", "svc")
	must(t, os.MkdirAll(filepath.Dir(container), 0o755))
	git(t, T, "clone", "-q", origin, container)
	git(t, container, "checkout", "-q", "main")

	// Working-tree residue that must survive the conversion.
	must(t, os.WriteFile(filepath.Join(container, "tracked.txt"), []byte("MODIFIED\n"), 0o644))   // uncommitted
	must(t, os.WriteFile(filepath.Join(container, "untracked.txt"), []byte("keep me\n"), 0o644))  // untracked
	must(t, os.WriteFile(filepath.Join(container, "ignored.txt"), []byte("build junk\n"), 0o644)) // ignored
	git(t, container, "branch", "spike")                                                          // unpushed local branch

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.wd]
dir = "`+filepath.Join(T, "wd")+`"
layout = "owner"
[[root.wd.repo]]
id = "local:acme/svc"
workflow = "upstream-push"
worktrees = true
branches = ["main", "release"]
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour, FixLayout: true})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("err=%v; actions=%v", results[0].Err, results[0].Actions)
	}

	// Converted to a worktree layout, original staging dir removed.
	if got := gitx.ClassifyLayout(container); got != gitx.LayoutWorktree {
		t.Fatalf("layout = %v, want worktree; actions=%v", got, results[0].Actions)
	}
	if exists(container + ".pre-worktree") {
		t.Errorf("staging dir was not removed")
	}
	for _, b := range []string{"main", "release"} {
		if !gitx.IsRepo(filepath.Join(container, b)) {
			t.Errorf("worktree %s missing", b)
		}
	}

	// Residue preserved on the main worktree (the branch the clone was on).
	mainWT := filepath.Join(container, "main")
	if got := read(t, filepath.Join(mainWT, "tracked.txt")); got != "MODIFIED\n" {
		t.Errorf("uncommitted change lost: tracked.txt = %q", got)
	}
	if !exists(filepath.Join(mainWT, "untracked.txt")) {
		t.Errorf("untracked file lost")
	}
	if !exists(filepath.Join(mainWT, "ignored.txt")) {
		t.Errorf("ignored file lost")
	}

	// Unpushed local branch preserved in the bare repo.
	if _, ok := gitx.RevParse(container, "refs/heads/spike"); !ok {
		t.Errorf("unpushed local branch 'spike' lost")
	}
}
