package sync

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/gitx"
)

// locationFixture declares one owner-layout repo — so its configured container
// is <wd>/acme/proj — and clones it into each of the given (wrong) paths,
// relative to the root. It returns the runner, mirroring what unionRepos does
// for a declared repo whose clone the scan found somewhere else.
func locationFixture(t *testing.T, worktrees bool, at ...string) (wd string, run func(fix bool) Result) {
	t.Helper()
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin := filepath.Join(remotes, "acme", "proj")
	seedBare(t, T, origin)

	wd = filepath.Join(T, "wd")
	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.wd]
dir = "`+wd+`"
layout = "owner"
[[root.wd.repo]]
id = "local:acme/proj"
workflow = "upstream-push"
worktrees = `+boolLit(worktrees)+`
branches = ["main"]
`), 0o644))
	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	var found []string
	for _, rel := range at {
		dir := filepath.Join(wd, rel)
		must(t, os.MkdirAll(filepath.Dir(dir), 0o755))
		if worktrees {
			// A bare+worktree container, as sync would have provisioned one.
			must(t, os.MkdirAll(dir, 0o755))
			git(t, T, "clone", "-q", "--bare", origin, filepath.Join(dir, ".bare"))
			must(t, os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: ./.bare\n"), 0o644))
			git(t, dir, "worktree", "add", "-q", filepath.Join(dir, "main"), "main")
		} else {
			git(t, T, "clone", "-q", origin, dir)
		}
		found = append(found, dir)
	}
	repos[0].FoundAt = found

	return wd, func(fix bool) Result {
		results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"),
			Frequency: time.Hour, FixLayout: fix})
		if len(results) != 1 {
			t.Fatalf("got %d results, want 1", len(results))
		}
		return results[0]
	}
}

func boolLit(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// TestMisplacedRepoIsSyncedWhereItIsThenMoved: DESIGN §4.1's third
// reconciliation. The failure it replaces is worse than a missing feature —
// sync read the configured path's emptiness as "not cloned", cloned a second
// copy there, and the union then hid the original behind its own declaration,
// so whatever was in it stopped being listed by anything.
func TestMisplacedRepoIsSyncedWhereItIsThenMoved(t *testing.T) {
	t.Run("plain sync reconciles it in place and says where", func(t *testing.T) {
		wd, run := locationFixture(t, false, "proj") // flat, under an owner-layout root
		res := run(false)
		if res.Err != nil {
			t.Fatalf("sync failed: %v; actions=%v", res.Err, res.Actions)
		}
		if !hasAction(res, "run: sync --fix") {
			t.Errorf("the location mismatch was not reported; actions=%v", res.Actions)
		}
		if gitx.IsRepo(filepath.Join(wd, "acme", "proj")) {
			t.Error("sync cloned a second copy at the configured path")
		}
		if !gitx.IsRepo(filepath.Join(wd, "proj")) {
			t.Error("the clone that exists was not left alone")
		}
		// It blocks nothing, so it must not outrank the repo's real findings.
		if res.Outcome == Attention {
			t.Errorf("outcome = Attention for a location note; actions=%v", res.Actions)
		}
	})

	t.Run("--fix moves it", func(t *testing.T) {
		wd, run := locationFixture(t, false, "proj")
		res := run(true)
		if res.Err != nil {
			t.Fatalf("sync failed: %v; actions=%v", res.Err, res.Actions)
		}
		if !gitx.IsRepo(filepath.Join(wd, "acme", "proj")) {
			t.Errorf("not moved to the configured path; actions=%v", res.Actions)
		}
		if _, err := os.Stat(filepath.Join(wd, "proj")); err == nil {
			t.Error("the old location still exists after the move")
		}
	})

	t.Run("--fix repairs a moved worktree container", func(t *testing.T) {
		wd, run := locationFixture(t, true, "proj")
		res := run(true)
		if res.Err != nil {
			t.Fatalf("sync failed: %v; actions=%v", res.Err, res.Actions)
		}
		moved := filepath.Join(wd, "acme", "proj")
		if gitx.ClassifyLayout(moved) != gitx.LayoutWorktree {
			t.Fatalf("not moved as a worktree container; actions=%v", res.Actions)
		}
		// The point of the repair: the worktree still resolves to its parent
		// from its new address, so git works inside it.
		if _, ok := gitx.RevParse(filepath.Join(moved, "main"), "HEAD"); !ok {
			t.Errorf("the moved worktree's links were not repaired; actions=%v", res.Actions)
		}
	})
}

// TestSeveralClonesAndNoneConfiguredIsRefused: numbered clones are a real habit
// (charts, charts2), so "which one did you mean" is a question a sweep can
// actually face — and a directory move hangs off the answer, so it refuses
// rather than guessing, and does not clone yet another copy either.
func TestSeveralClonesAndNoneConfiguredIsRefused(t *testing.T) {
	wd, run := locationFixture(t, false, "proj", "proj2")
	res := run(true)
	if res.Outcome != Attention {
		t.Errorf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if gitx.IsRepo(filepath.Join(wd, "acme", "proj")) {
		t.Error("an ambiguous scan still produced a clone at the configured path")
	}
	for _, rel := range []string{"proj", "proj2"} {
		if !gitx.IsRepo(filepath.Join(wd, rel)) {
			t.Errorf("%s was disturbed", rel)
		}
	}
	if !hasAction(res, "won't choose between them") {
		t.Errorf("the ambiguity was not explained; actions=%v", res.Actions)
	}
}

// TestDestinationInTheWayIsNotOverwritten: the configured path holding a clone
// already is the two-clones problem, not a move target.
func TestDestinationInTheWayIsNotOverwritten(t *testing.T) {
	wd, run := locationFixture(t, false, "proj")
	occupied := filepath.Join(wd, "acme", "proj")
	must(t, os.MkdirAll(occupied, 0o755))
	must(t, os.WriteFile(filepath.Join(occupied, "in-the-way"), []byte("mine\n"), 0o644))

	res := run(true)
	if res.Outcome != Attention {
		t.Errorf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if _, err := os.Stat(filepath.Join(occupied, "in-the-way")); err != nil {
		t.Error("the destination's contents were destroyed by the move")
	}
	if !gitx.IsRepo(filepath.Join(wd, "proj")) {
		t.Error("the source was moved even though the destination was occupied")
	}
}
