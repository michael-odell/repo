package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/model"
)

// headCount returns the number of commits reachable from HEAD in a repo.
func headCount(t *testing.T, dir string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-list", "--count", "HEAD").Output()
	must(t, err)
	return trim(string(out))
}

// bareHead returns the commit count of a branch in a bare repo.
func bareCount(t *testing.T, bare, branch string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", bare, "rev-list", "--count", branch).Output()
	must(t, err)
	return trim(string(out))
}

// TestForkPRFastForwardsUpstreamAndPushesFork builds a bare upstream two commits
// ahead of a bare fork, clones the fork as a fork-pr repo, and syncs. The local
// important branch must fast-forward to upstream and the fork must receive the
// same push.
func TestForkPRFastForwardsUpstreamAndPushesFork(t *testing.T) {
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
	git(t, seed, "push", "-q", fork, "main") // fork = commit 1
	writeCommit(t, seed, "a", "one\ntwo\n", "two")
	git(t, seed, "push", "-q", "origin", "main") // upstream = commit 1+2

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
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour})
	if len(results) != 1 || results[0].Outcome != Updated {
		t.Fatalf("outcome = %v (want Updated); actions=%v", results[0].Outcome, results[0].Actions)
	}

	clone := filepath.Join(T, "clones", "proj")
	if got := headCount(t, clone); got != "2" {
		t.Errorf("local clone at %s commits, want 2 (should FF to upstream)", got)
	}
	if got := bareCount(t, fork, "main"); got != "2" {
		t.Errorf("fork main at %s commits, want 2 (should be pushed)", got)
	}
}

// upstreamPushFixture builds a bare origin with one commit and a registry
// declaring a single upstream-push repo, with pushSetting appended verbatim to
// the repo table (e.g. `push = "auto"`, or "" to exercise the built-in default).
func upstreamPushFixture(t *testing.T, pushSetting string) (reg *config.Registry, repos []model.Repo, origin, clone string) {
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
`+pushSetting+`
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err = reg.Repos()
	must(t, err)

	// First sync just clones.
	if results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour}); len(results) != 1 {
		t.Fatalf("initial sync: got %d results, want 1", len(results))
	}
	clone = filepath.Join(T, "clones", "proj")
	writeCommit(t, clone, "b", "two\n", "two") // local commit, not on origin
	return reg, repos, origin, clone
}

// TestUpstreamPushAutoPushesAheadCommits: with push=auto, an ahead-only local
// commit on an upstream-push repo's important branch is pushed straight to
// origin rather than merely reported (DESIGN §3.6).
func TestUpstreamPushAutoPushesAheadCommits(t *testing.T) {
	reg, repos, origin, _ := upstreamPushFixture(t, `push = "auto"`)

	results := Run(reg, repos, Options{StateDir: filepath.Join(t.TempDir(), "out"), Frequency: time.Hour})
	if len(results) != 1 || results[0].Outcome != Updated {
		t.Fatalf("outcome = %v (want Updated); actions=%v", results[0].Outcome, results[0].Actions)
	}
	if got := bareCount(t, origin, "main"); got != "2" {
		t.Errorf("origin main at %s commits, want 2 (ahead commit should be pushed)", got)
	}
}

// TestUpstreamPushDefaultsToManual: with no `push` override, upstream-push
// defaults to `manual` (DESIGN §3.6) — a team-safe default, since the same
// remote contract fits both a solo and a shared repo. The ahead commit must be
// left on origin, only reported.
func TestUpstreamPushDefaultsToManual(t *testing.T) {
	reg, repos, origin, _ := upstreamPushFixture(t, "")

	// A fresh StateDir/timestamp store: reuse the same one the fixture's first
	// sync used isn't necessary since Attention doesn't depend on cadence state.
	out := filepath.Join(t.TempDir(), "out")
	results := Run(reg, repos, Options{StateDir: out, Frequency: time.Hour})
	if len(results) != 1 || results[0].Outcome != Attention {
		t.Fatalf("outcome = %v (want Attention); actions=%v", results[0].Outcome, results[0].Actions)
	}
	if got := bareCount(t, origin, "main"); got != "1" {
		t.Errorf("origin main at %s commits, want 1 (ahead commit must not be pushed)", got)
	}
}

// TestVendorLatestTagChecksOutHighest seeds an origin whose main advances past
// its newest tag; a vendor pinned to latest-tag must checkout the tag (detached)
// rather than follow main.
func TestVendorLatestTagChecksOutHighest(t *testing.T) {
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin := filepath.Join(remotes, "acme", "lib")
	must(t, os.MkdirAll(filepath.Dir(origin), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", origin)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	writeCommit(t, seed, "v", "1.0.0\n", "v1.0.0")
	git(t, seed, "tag", "v1.0.0")
	writeCommit(t, seed, "v", "1.1.0\n", "v1.1.0")
	git(t, seed, "tag", "v1.1.0")
	writeCommit(t, seed, "v", "dev\n", "post-release work") // main now past the tag
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", "origin", "--tags")

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.clones]
dir = "`+filepath.Join(T, "clones")+`"
layout = "owner"
[[root.clones.repo]]
id = "local:acme/lib"
workflow = "vendor"
pin = "latest-tag"
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour})
	if len(results) != 1 || results[0].Outcome != Updated {
		t.Fatalf("outcome = %v (want Updated); actions=%v", results[0].Outcome, results[0].Actions)
	}

	clone := filepath.Join(T, "clones", "acme", "lib")
	out, err := exec.Command("git", "-C", clone, "describe", "--tags", "--exact-match", "HEAD").Output()
	if err != nil {
		t.Fatalf("HEAD is not on a tag: %v", err)
	}
	if got := trim(string(out)); got != "v1.1.0" {
		t.Errorf("pinned at %s, want v1.1.0 (highest tag, not main HEAD)", got)
	}
}

// writeCommit writes a file and commits it in a working clone.
func writeCommit(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
	git(t, dir, "add", name)
	git(t, dir, "commit", "-qm", msg)
}
