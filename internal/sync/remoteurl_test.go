package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/gitx"
)

// seedBare creates a bare repo at path holding one commit on main.
func seedBare(t *testing.T, T, path string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", path)
	seed := filepath.Join(t.TempDir(), "seed")
	git(t, T, "clone", "-q", path, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	writeCommit(t, seed, "a", "one\n", "one")
	git(t, seed, "push", "-q", "origin", "main")
}

// urlFixture declares one repo whose config-resolved origin is
// <remotes>/acme/proj, with the clone already on disk pointing at cloneFrom.
func urlFixture(t *testing.T, cloneFrom func(t *testing.T, T, remotes string) string) (container string, run func(fix bool) Result) {
	t.Helper()
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	actual := cloneFrom(t, T, remotes)

	container = filepath.Join(T, "clones", "proj")
	must(t, os.MkdirAll(filepath.Dir(container), 0o755))
	git(t, T, "clone", "-q", actual, container)

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.clones]
dir = "`+filepath.Join(T, "clones")+`"
[[root.clones.repo]]
id = "local:acme/proj"
workflow = "upstream-push"
branches = ["main"]
`), 0o644))
	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	return container, func(fix bool) Result {
		results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"),
			Frequency: time.Hour, FixLayout: fix})
		if len(results) != 1 {
			t.Fatalf("got %d results, want 1", len(results))
		}
		return results[0]
	}
}

func originOf(t *testing.T, dir string) string {
	t.Helper()
	url, ok := gitx.RemoteURL(dir, "origin")
	if !ok {
		t.Fatalf("%s has no origin remote", dir)
	}
	return url
}

// TestManagedRemoteURLIsReportedThenFixed: DESIGN §4.1 says sync detects and
// reports a remote that disagrees with its contract, and --fix applies it —
// nothing is found only under --fix, and nothing is changed without it. Origin
// was the exception: a plain sync rewrote its URL on the spot, which is the one
// reconciliation as likely to be config's error as the disk's (a mistyped
// `via`, a fork_owner on the wrong root), and it did so on the backgrounded
// --if-due run nobody reads.
func TestManagedRemoteURLIsReportedThenFixed(t *testing.T) {
	elsewhere := func(t *testing.T, T, remotes string) string {
		seedBare(t, T, filepath.Join(remotes, "acme", "proj")) // what config names
		other := filepath.Join(remotes, "elsewhere", "proj")   // where the clone points
		seedBare(t, T, other)
		return other
	}

	t.Run("plain sync reports and leaves it", func(t *testing.T) {
		container, run := urlFixture(t, elsewhere)
		before := originOf(t, container)
		res := run(false)
		if got := originOf(t, container); got != before {
			t.Errorf("origin was rewritten without --fix: %q → %q", before, got)
		}
		if !hasAction(res, "run: sync --fix") {
			t.Errorf("the mismatch was not reported; actions=%v", res.Actions)
		}
		// A remote-URL disagreement blocks nothing, so it must not outrank the
		// repo's real findings (see ensureSecondRemote).
		if res.Outcome == Attention {
			t.Errorf("outcome = Attention for a remote-URL note; actions=%v", res.Actions)
		}
	})

	t.Run("--fix applies it", func(t *testing.T) {
		container, run := urlFixture(t, elsewhere)
		res := run(true)
		if got := originOf(t, container); !strings.HasSuffix(got, filepath.Join("acme", "proj")) {
			t.Errorf("origin = %q, want the configured acme/proj; actions=%v", got, res.Actions)
		}
		if !hasAction(res, "(was ") {
			t.Errorf("the change was not reported with what it replaced; actions=%v", res.Actions)
		}
	})
}

// TestTrailingGitSuffixIsNotAMismatch: git treats a trailing ".git" as
// decoration, so a clone spelled one way and a registry the other name the same
// place. Before, sync "reconciled" the difference on every run — here, onto a
// path that doesn't exist, breaking the fetch of a repo that was fine.
func TestTrailingGitSuffixIsNotAMismatch(t *testing.T) {
	container, run := urlFixture(t, func(t *testing.T, T, remotes string) string {
		dotGit := filepath.Join(remotes, "acme", "proj.git")
		seedBare(t, T, dotGit)
		return dotGit
	})
	before := originOf(t, container)
	res := run(false)
	if res.Err != nil {
		t.Fatalf("sync failed: %v; actions=%v", res.Err, res.Actions)
	}
	if got := originOf(t, container); got != before {
		t.Errorf("origin was rewritten over a .git suffix: %q → %q", before, got)
	}
	if hasAction(res, "run: sync --fix") {
		t.Errorf("a .git suffix was reported as a mismatch; actions=%v", res.Actions)
	}
}
