package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
)

// TestMatchesAny locks in the glob semantics force_push/force_pull rely on
// (DESIGN §3.6, §5.2): "*" alone means every branch (plain path.Match doesn't
// cross "/" boundaries, which would otherwise surprise a "wip/foo" branch),
// otherwise it's ordinary path.Match against the branch name.
func TestMatchesAny(t *testing.T) {
	cases := []struct {
		patterns []string
		name     string
		want     bool
	}{
		{nil, "main", false},
		{[]string{"*"}, "wip/foo", true},
		{[]string{"main"}, "main", true},
		{[]string{"main"}, "release", false},
		{[]string{"wip/*"}, "wip/foo", true},
		{[]string{"wip/*"}, "wip/foo/bar", false},
		{[]string{"wip/*"}, "spike", false},
	}
	for _, c := range cases {
		if got := matchesAny(c.patterns, c.name); got != c.want {
			t.Errorf("matchesAny(%v, %q) = %v, want %v", c.patterns, c.name, got, c.want)
		}
	}
}

// setupUpstreamRepo builds a bare origin with one commit on main, a registry
// declaring a single upstream-push repo (extra appended verbatim to the repo
// table), syncs it once (a plain clone), and returns a run closure for
// subsequent syncs.
func setupUpstreamRepo(t *testing.T, extra string) (origin, clone string, run func() Result) {
	t.Helper()
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin = filepath.Join(remotes, "acme", "proj")
	must(t, os.MkdirAll(filepath.Dir(origin), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", origin)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	writeCommit(t, seed, "a", "one\n", "one")
	git(t, seed, "push", "-q", "origin", "main")

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
`+extra+`
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	out := filepath.Join(T, "out")
	if results := Run(reg, repos, Options{StateDir: out, Frequency: time.Hour}); len(results) != 1 {
		t.Fatalf("initial sync: got %d results, want 1", len(results))
	}
	clone = filepath.Join(T, "clones", "proj")
	run = func() Result {
		results := Run(reg, repos, Options{StateDir: out, Frequency: time.Hour})
		if len(results) != 1 {
			t.Fatalf("got %d results, want 1", len(results))
		}
		return results[0]
	}
	return origin, clone, run
}

func refExists(t *testing.T, dir, ref string) bool {
	t.Helper()
	err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", ref).Run()
	return err == nil
}

// hasBranchNote reports whether res carries a BranchNote for name with the
// given attention flag.
func hasBranchNote(res Result, name string, attention bool) bool {
	for _, b := range res.Branches {
		if b.Name == name && b.Attention == attention {
			return true
		}
	}
	return false
}

// TestTaskBranchesAutoPushesNewBranch: with task_branches=auto, a brand-new
// local branch (never pushed) gets pushed to origin on the next sync,
// unprompted (DESIGN §3.6) — the disaster-recovery case: work survives a lost
// machine without the user remembering to push every side branch.
func TestTaskBranchesAutoPushesNewBranch(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, `task_branches = "auto"`)
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "f", "wip\n", "wip")
	git(t, clone, "checkout", "-q", "main")

	res := run()
	if !refExists(t, origin, "feature") {
		t.Errorf("origin missing feature branch: task_branches=auto should have pushed it")
	}
	if !hasBranchNote(res, "feature", false) {
		t.Errorf("no non-attention branch note for feature; branches=%+v", res.Branches)
	}
}

// TestTaskBranchesReportLeavesBranchUnpushed: the upstream-push default,
// task_branches=report, only flags a new local branch — it must not push it
// (deferring to PR time, DESIGN §3.6).
func TestTaskBranchesReportLeavesBranchUnpushed(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, "") // default: task_branches=report
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "f", "wip\n", "wip")
	git(t, clone, "checkout", "-q", "main")

	res := run()
	if refExists(t, origin, "feature") {
		t.Errorf("origin has feature branch: task_branches=report must not push")
	}
	if !hasBranchNote(res, "feature", true) {
		t.Errorf("no attention branch note for feature; branches=%+v", res.Branches)
	}
	// Task-branch findings are informational sub-bullets, never the repo's own
	// headline (DESIGN §5.6) — the important branch is fine, so the repo is too.
	if res.Outcome != UpToDate {
		t.Errorf("outcome = %v, want UpToDate (task branches don't affect repo Outcome)", res.Outcome)
	}
}

// revParse returns dir's SHA for ref.
func revParse(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	must(t, err)
	return trim(string(out))
}

// TestTaskBranchesPullOnlyPullsBehindBranch: task_branches=pull-only fast-
// forwards a task branch that's purely behind its own remote — no local
// commits ever put on it, exactly what a plain `git clone` leaves checked out
// on a vendor repo pinned elsewhere (DESIGN §3.6) — instead of flagging its
// mere existence the way the old `disallow` value did.
func TestTaskBranchesPullOnlyPullsBehindBranch(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, `task_branches = "pull-only"`)

	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "f", "wip\n", "wip")
	git(t, clone, "push", "-q", "origin", "feature")
	git(t, clone, "checkout", "-q", "main")

	// Advance origin's feature branch from a second clone, so the local
	// "feature" branch is purely behind with no local commits of its own.
	parent := t.TempDir()
	other := filepath.Join(parent, "other")
	git(t, parent, "clone", "-q", origin, other)
	git(t, other, "checkout", "-q", "feature")
	writeCommit(t, other, "g", "more\n", "more")
	git(t, other, "push", "-q", "origin", "feature")

	res := run()
	wantSHA := revParse(t, origin, "feature")
	if got := revParse(t, clone, "feature"); got != wantSHA {
		t.Errorf("local feature = %s, want it fast-forwarded to match origin %s", got, wantSHA)
	}
	if !hasBranchNote(res, "feature", false) {
		t.Errorf("no non-attention branch note for feature (should be silently pulled); branches=%+v", res.Branches)
	}
}

// TestTaskBranchesPullOnlyFlagsLocalCommits: pull-only still surfaces a task
// branch that carries local commits of its own — passive scaffolding is fine,
// actual local work still gets attention and is still never pushed (DESIGN
// §3.6).
func TestTaskBranchesPullOnlyFlagsLocalCommits(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, `task_branches = "pull-only"`)
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "f", "wip\n", "wip")
	git(t, clone, "checkout", "-q", "main")

	res := run()
	if refExists(t, origin, "feature") {
		t.Errorf("origin has feature branch: task_branches=pull-only must not push")
	}
	if !hasBranchNote(res, "feature", true) {
		t.Errorf("no attention branch note for feature (local commits are still surfaced); branches=%+v", res.Branches)
	}
}

// TestForcePushForcePushesMatchedDivergedFork builds a fork-pr repo whose fork
// (origin) has a commit the local clone lacks, while local also has its own
// local-only commit (a genuine divergence). Without a force_push match the
// push must be skipped and surfaced; with one it force-pushes.
func TestForcePushForcePushesMatchedDivergedFork(t *testing.T) {
	for _, tc := range []struct {
		name       string
		forcePush  string
		wantPushed bool
	}{
		{"no match", "", false},
		{"matched", `force_push = ["main"]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			T := t.TempDir()
			remotes := filepath.Join(T, "remotes")
			up := filepath.Join(remotes, "up", "proj")
			fork := filepath.Join(remotes, "fork", "proj")
			must(t, os.MkdirAll(filepath.Dir(up), 0o755))
			must(t, os.MkdirAll(filepath.Dir(fork), 0o755))
			git(t, T, "init", "-q", "-b", "main", "--bare", up)
			git(t, T, "init", "-q", "-b", "main", "--bare", fork)

			seed := filepath.Join(T, "seed")
			git(t, T, "clone", "-q", up, seed)
			git(t, seed, "checkout", "-q", "-b", "main")
			writeCommit(t, seed, "a", "one\n", "one")
			git(t, seed, "push", "-q", "origin", "main")
			git(t, seed, "push", "-q", fork, "main")

			regPath := filepath.Join(T, "registry.toml")
			must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.clones]
dir = "`+filepath.Join(T, "clones")+`"
[[root.clones.repo]]
id = "local:up/proj"
fork = "local:fork/proj"
workflow = "fork-pr"
branches = ["main"]
`+tc.forcePush+`
`), 0o644))

			reg, err := config.Load([]string{regPath})
			must(t, err)
			repos, err := reg.Repos()
			must(t, err)

			out := filepath.Join(T, "out")
			if results := Run(reg, repos, Options{StateDir: out, Frequency: time.Hour}); len(results) != 1 {
				t.Fatalf("initial sync: got %d results, want 1", len(results))
			}

			clone := filepath.Join(T, "clones", "proj")
			// A commit only the fork has (pushed independently of local).
			git(t, seed, "commit", "--allow-empty", "-qm", "fork-only")
			git(t, seed, "push", "-q", fork, "main")
			// A commit only local has.
			writeCommit(t, clone, "b", "local-only\n", "local-only")

			results := Run(reg, repos, Options{StateDir: out, Frequency: time.Hour})
			if len(results) != 1 {
				t.Fatalf("got %d results, want 1", len(results))
			}
			forkHasLocalCommit := refExists(t, fork, "main") &&
				trim(mustOutput(t, "git", "-C", fork, "log", "--format=%s", "main", "-1")) == "local-only"
			if forkHasLocalCommit != tc.wantPushed {
				t.Errorf("fork tip is local-only commit = %v, want %v (outcome=%v actions=%v)",
					forkHasLocalCommit, tc.wantPushed, results[0].Outcome, results[0].Actions)
			}
		})
	}
}

func mustOutput(t *testing.T, name string, args ...string) string {
	t.Helper()
	out, err := exec.Command(name, args...).Output()
	must(t, err)
	return string(out)
}

// TestUntrackedFilesReported: an untracked file is flagged (Attention) but
// never blocks an otherwise-clean update, unlike a dirty (modified-tracked)
// working tree (DESIGN §5.1, principle 3).
func TestUntrackedFilesReported(t *testing.T) {
	origin, clone, run := setupUpstreamRepo(t, "")
	must(t, os.WriteFile(filepath.Join(clone, "scratch.txt"), []byte("notes\n"), 0o644))

	// Give the origin a new commit so a real update is also in play.
	seed := filepath.Join(filepath.Dir(filepath.Dir(clone)), "seed2")
	git(t, filepath.Dir(clone), "clone", "-q", origin, seed)
	writeCommit(t, seed, "c", "two\n", "two")
	git(t, seed, "push", "-q", "origin", "main")

	res := run()
	if res.Outcome != Attention {
		t.Errorf("outcome = %v, want Attention (untracked file present); actions=%v", res.Outcome, res.Actions)
	}
	if headCount(t, clone) != "2" {
		t.Errorf("clone did not fast-forward despite only an untracked file present")
	}
	if !exists(filepath.Join(clone, "scratch.txt")) {
		t.Errorf("untracked file was lost")
	}
}
