package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/gitx"
)

// seedMirrorRemotes creates the definitive ("up") and fork bare repos a
// supply-chain-mirror repo needs, both holding the same commit so the review
// gate has nothing pending and the relayout is what the test is measuring.
func seedMirrorRemotes(t *testing.T, T string) (remotes, up string) {
	t.Helper()
	remotes = filepath.Join(T, "remotes")
	up = filepath.Join(remotes, "up", "proj")
	fork := filepath.Join(remotes, "fork", "proj")
	must(t, os.MkdirAll(filepath.Dir(up), 0o755))
	must(t, os.MkdirAll(filepath.Dir(fork), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", up)
	git(t, T, "init", "-q", "-b", "main", "--bare", fork)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", up, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "one")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", fork, "main")
	return remotes, up
}

// mirrorRun writes a mirror registry at the requested layout and syncs it.
func mirrorRun(t *testing.T, T, remotes string, worktrees bool, fix bool) Result {
	t.Helper()
	worktreesLit := "false"
	if worktrees {
		worktreesLit = "true"
	}
	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.clones]
dir = "`+filepath.Join(T, "clones")+`"
[[root.clones.repo]]
id = "local:up/proj"
fork = "local:fork/proj"
workflow = "supply-chain-mirror"
worktrees = `+worktreesLit+`
branches = ["main"]
`), 0o644))
	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)
	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour, FixLayout: fix})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	return results[0]
}

func remoteURL(t *testing.T, dir, name string) (string, bool) {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "remote", "get-url", name).Output()
	if err != nil {
		return "", false
	}
	return trim(string(out)), true
}

// TestRelayoutKeepsTheMirrorRemoteName: a layout conversion rebuilds the
// container from a local clone and puts the remotes back, and it must put the
// second one back under the *workflow's* name. Both directions hardcoded
// "upstream" — the fork-pr spelling — so converting a supply-chain-mirror gave
// it the one remote §3.6 says it must not have, dropped `untrusted` entirely
// (leaving the review gate with nothing to compare against), and left
// discovery re-inferring the repo as fork-pr from origin+upstream (§3.2).
func TestRelayoutKeepsTheMirrorRemoteName(t *testing.T) {
	t.Run("single to worktree", func(t *testing.T) {
		T := t.TempDir()
		remotes, up := seedMirrorRemotes(t, T)

		// A correctly-provisioned mirror that is a single clone where config
		// now says worktrees = true: --fix converts it.
		container := filepath.Join(T, "clones", "proj")
		must(t, os.MkdirAll(filepath.Dir(container), 0o755))
		git(t, T, "clone", "-q", filepath.Join(remotes, "fork", "proj"), container)
		git(t, container, "remote", "add", "untrusted", up)

		res := mirrorRun(t, T, remotes, true, true)
		if res.Err != nil {
			t.Fatalf("err=%v; actions=%v", res.Err, res.Actions)
		}
		if got := gitx.ClassifyLayout(container); got != gitx.LayoutWorktree {
			t.Fatalf("layout = %v, want worktree; actions=%v", got, res.Actions)
		}
		assertMirrorRemotes(t, container, up, res)
	})

	t.Run("worktree to single", func(t *testing.T) {
		T := t.TempDir()
		remotes, up := seedMirrorRemotes(t, T)

		container := filepath.Join(T, "clones", "proj")
		if res := mirrorRun(t, T, remotes, true, false); res.Err != nil {
			t.Fatalf("provisioning the worktree mirror failed: %v; actions=%v", res.Err, res.Actions)
		}
		if got := gitx.ClassifyLayout(container); got != gitx.LayoutWorktree {
			t.Fatalf("fixture layout = %v, want worktree", got)
		}

		res := mirrorRun(t, T, remotes, false, true)
		if res.Err != nil {
			t.Fatalf("err=%v; actions=%v", res.Err, res.Actions)
		}
		if got := gitx.ClassifyLayout(container); got != gitx.LayoutSingle {
			t.Fatalf("layout = %v, want single; actions=%v", got, res.Actions)
		}
		assertMirrorRemotes(t, container, up, res)
	})
}

func assertMirrorRemotes(t *testing.T, container, up string, res Result) {
	t.Helper()
	got, ok := remoteURL(t, container, "untrusted")
	if !ok {
		t.Errorf("no untrusted remote after the conversion; actions=%v", res.Actions)
	} else if got != up {
		t.Errorf("untrusted = %q, want %q", got, up)
	}
	if url, ok := remoteURL(t, container, "upstream"); ok {
		t.Errorf("conversion created an upstream remote (%q) on a supply-chain-mirror", url)
	}
}
