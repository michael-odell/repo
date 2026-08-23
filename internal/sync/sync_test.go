package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/model"
)

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// TestMirrorDoesNotAdvancePastReview builds a bare upstream that is one commit
// ahead of a bare fork, then syncs a supply-chain-mirror repo cloned from the
// fork. The result must be ReviewPending and the local clone must NOT advance.
func TestMirrorDoesNotAdvancePastReview(t *testing.T) {
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	up := filepath.Join(remotes, "up", "proj")
	fork := filepath.Join(remotes, "fork", "proj")
	must(t, os.MkdirAll(filepath.Dir(up), 0o755))
	must(t, os.MkdirAll(filepath.Dir(fork), 0o755))
	// Pin the bare repos' default branch to main; otherwise the clone of the
	// fork inherits the runner's init.defaultBranch (e.g. master on CI) for its
	// symbolic HEAD, which points at a ref that was never pushed, leaving the
	// clone in detached HEAD.
	git(t, T, "init", "-q", "-b", "main", "--bare", up)
	git(t, T, "init", "-q", "-b", "main", "--bare", fork)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", up, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "one")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", fork, "main") // fork = reviewed point (commit 1)
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\ntwo\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "two")
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
workflow = "supply-chain-mirror"
branches = ["main"]
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Outcome != ReviewPending {
		t.Errorf("outcome = %v, want ReviewPending; actions=%v", results[0].Outcome, results[0].Actions)
	}

	// The clone must remain at the reviewed point (1 commit), not the upstream 2.
	clone := filepath.Join(T, "clones", "proj")
	out, err := exec.Command("git", "-C", clone, "rev-list", "--count", "HEAD").Output()
	must(t, err)
	if got := trim(string(out)); got != "1" {
		t.Errorf("clone advanced to %s commits, want 1 (must not pass review)", got)
	}

	// The definitive source must be a remote named "untrusted" (DESIGN §3.6),
	// never "upstream" — the fork-pr name. Getting this wrong is what let a
	// tool-provisioned mirror go undetectable by discover.go, silently, for
	// months: the outcome check above still passes either way.
	if _, err := exec.Command("git", "-C", clone, "remote", "get-url", "upstream").Output(); err == nil {
		t.Errorf("clone carries an upstream remote; supply-chain-mirror must never have one (DESIGN §3.6)")
	}
	if out, err := exec.Command("git", "-C", clone, "remote", "get-url", "untrusted").Output(); err != nil {
		t.Errorf("clone has no untrusted remote: %v", err)
	} else if got := trim(string(out)); got != up {
		t.Errorf("untrusted = %q, want %q", got, up)
	}
}

// TestFixReconcilesStaleUpstreamOnMirror covers DESIGN §3.6's fork-pr → mirror
// hardening case (and, incidentally, any mirror clone provisioned before the
// untrusted remote name became workflow-aware): a clone carrying "upstream"
// instead of "untrusted" is reported on a plain sync and left alone; only
// --fix renames it. When both remotes already exist — the state a *previous*
// plain sync leaves behind, since it always creates the correctly-named
// remote alongside a stale one rather than withholding it until --fix — --fix
// removes the stale remote instead of attempting an impossible rename onto a
// name already taken.
func TestFixReconcilesStaleUpstreamOnMirror(t *testing.T) {
	for _, tc := range []struct {
		name               string
		precreateUntrusted bool
		fix                bool
		wantUpstream       bool
		wantReported       bool // the "should be untrusted" trace line, without --fix
	}{
		{name: "plain sync reports, does not rename", wantUpstream: true, wantReported: true},
		{name: "--fix renames when untrusted absent", fix: true},
		{name: "--fix removes stale when untrusted already exists", precreateUntrusted: true, fix: true},
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
			must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\n"), 0o644))
			git(t, seed, "add", "a")
			git(t, seed, "commit", "-qm", "one")
			git(t, seed, "push", "-q", "origin", "main")
			git(t, seed, "push", "-q", fork, "main")

			// A clone that predates this fix, or was hardened from fork-pr:
			// cloned from the fork, definitive remote under the fork-pr name.
			clone := filepath.Join(T, "clones", "proj")
			must(t, os.MkdirAll(filepath.Dir(clone), 0o755))
			git(t, T, "clone", "-q", fork, clone)
			git(t, clone, "remote", "add", "upstream", up)
			if tc.precreateUntrusted {
				git(t, clone, "remote", "add", "untrusted", up)
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
branches = ["main"]
`), 0o644))

			reg, err := config.Load([]string{regPath})
			must(t, err)
			repos, err := reg.Repos()
			must(t, err)

			results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour, FixLayout: tc.fix})
			if len(results) != 1 {
				t.Fatalf("got %d results, want 1", len(results))
			}
			// The stale-name report must never itself raise Attention — DESIGN
			// §4.1 calls remote reconciliation low-risk, and a repo whose branches
			// are otherwise fine must not read as needing intervention over a
			// leftover remote name (that finding would also outrank, and so bury,
			// a genuine one — see TestStaleUpstreamNeverMasksReviewPending).
			if results[0].Outcome == Attention {
				t.Errorf("outcome = Attention, want the stale-name report to never drive it; actions=%v", results[0].Actions)
			}
			gotReported := false
			for _, a := range results[0].Actions {
				if strings.Contains(a.Text, "should be untrusted") {
					gotReported = true
				}
			}
			if gotReported != tc.wantReported {
				t.Errorf("reported the stale name = %v, want %v; actions=%v", gotReported, tc.wantReported, results[0].Actions)
			}

			_, upErr := exec.Command("git", "-C", clone, "remote", "get-url", "upstream").Output()
			if hasUpstream := upErr == nil; hasUpstream != tc.wantUpstream {
				t.Errorf("has upstream remote = %v, want %v", hasUpstream, tc.wantUpstream)
			}
			untrustedOut, untErr := exec.Command("git", "-C", clone, "remote", "get-url", "untrusted").Output()
			if untErr != nil {
				t.Fatalf("clone has no untrusted remote: %v", untErr)
			}
			if got := trim(string(untrustedOut)); got != up {
				t.Errorf("untrusted = %q, want %q", got, up)
			}
		})
	}
}

// TestStaleUpstreamNeverMasksReviewPending is the regression this fix exists
// for: a pre-fix mirror clone (a stale "upstream" remote, untrusted genuinely
// ahead of origin) synced with the fixed code, no --fix, must still surface
// ReviewPending as the repo's headline outcome. An earlier version of this fix
// reported the stale name via x.attention, which outranks ReviewPending
// (rank(Attention) > rank(ReviewPending) in mark/rank) and silently buried the
// review-pending finding behind a "rename this remote" notice on every run.
func TestStaleUpstreamNeverMasksReviewPending(t *testing.T) {
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
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "one")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", fork, "main") // fork = reviewed point (commit 1)
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\ntwo\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "two")
	git(t, seed, "push", "-q", "origin", "main") // up = reviewed point +1

	// A clone built the old way: origin=fork, "upstream"=definitive, fetched —
	// the state any mirror clone managed before this fix is in.
	clone := filepath.Join(T, "clones", "proj")
	must(t, os.MkdirAll(filepath.Dir(clone), 0o755))
	git(t, T, "clone", "-q", fork, clone)
	git(t, clone, "remote", "add", "upstream", up)
	git(t, clone, "fetch", "-q", "upstream")

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
branches = ["main"]
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Outcome != ReviewPending {
		t.Errorf("outcome = %v, want ReviewPending; actions=%v", results[0].Outcome, results[0].Actions)
	}
}

// TestSecondRemoteFetchFailureIsReported is the other half of the silent-fetch
// gap this fix closes: every provisioning path used to ignore a failed fetch of
// the second (upstream/untrusted) remote entirely — `if _, err := gitx.Fetch
// (...); err == nil { ... }`, nothing on the else. Harmless for a remote with
// years of prior successful fetches sitting behind it, but ensureSecondRemote
// can now hand a *brand-new*, empty remote to this exact call (created moments
// earlier for a clone that predates this fix), whose first fetch failing left
// nothing to compare against — mirrorReview and updateForkPR both fail closed
// on a missing ref, so the repo read as a clean "up to date" instead of "the
// review gate couldn't check its source". A repo whose second remote points at
// nothing must come back Attention, not a silent pass.
func TestSecondRemoteFetchFailureIsReported(t *testing.T) {
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	fork := filepath.Join(remotes, "fork", "proj")
	must(t, os.MkdirAll(filepath.Dir(fork), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", fork)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", fork, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "one")
	git(t, seed, "push", "-q", "origin", "main")

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.clones]
dir = "`+filepath.Join(T, "clones")+`"
[[root.clones.repo]]
id = "local:up/does-not-exist"
fork = "local:fork/proj"
workflow = "supply-chain-mirror"
branches = ["main"]
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	// Fresh provisioning: origin (the fork) clones fine, but the definitive
	// source doesn't exist, so the untrusted fetch fails.
	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if results[0].Outcome != Attention {
		t.Errorf("outcome = %v, want Attention; actions=%v", results[0].Outcome, results[0].Actions)
	}
	found := false
	for _, a := range results[0].Actions {
		if strings.Contains(a.Text, "fetch untrusted failed") {
			found = true
		}
	}
	if !found {
		t.Errorf("no 'fetch untrusted failed' trace line; actions=%v", results[0].Actions)
	}
}

// TestDiscoveredRepoFlagsUnresolvedImportantBranch: a discovered repo (Dir
// set) whose important branch couldn't be inferred — no origin default-branch
// symref, no known mainline name — must be flagged for an explicit `branches`
// override, never silently fall back to guessing (e.g. whatever happens to be
// checked out).
func TestDiscoveredRepoFlagsUnresolvedImportantBranch(t *testing.T) {
	r := model.Repo{Dir: t.TempDir(), OriginURL: "https://example.com/x.git"}
	results := Run(nil, []model.Repo{r}, Options{StateDir: t.TempDir(), Frequency: time.Hour})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	res := results[0]
	if res.Outcome != Attention {
		t.Fatalf("outcome = %v, want Attention; actions=%v", res.Outcome, res.Actions)
	}
	if want := "can't tell which branch is important — add `branches = [...]`"; res.Detail != want {
		t.Errorf("detail = %q, want %q", res.Detail, want)
	}
}

// TestProvisionAdoptsTheClonesDefaultBranch: with config silent on `branches`,
// a repo being provisioned has no clone to read a default branch from, so it
// starts the run carrying the builtin ["main"]. Once the clone lands, the
// clone's answer must take over within the same run — otherwise a fresh machine's first sweep
// reports "main missing on origin" for every repo whose trunk is master, and
// each one quietly fixes itself on the second run.
func TestProvisionAdoptsTheClonesDefaultBranch(t *testing.T) {
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin := filepath.Join(remotes, "acme", "proj")
	must(t, os.MkdirAll(filepath.Dir(origin), 0o755))
	git(t, T, "init", "-q", "-b", "master", "--bare", origin)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-b", "master")
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "one")
	git(t, seed, "push", "-q", "origin", "master")

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.clones]
dir = "`+filepath.Join(T, "clones")+`"
[[root.clones.repo]]
id = "local:acme/proj"
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out"), Frequency: time.Hour})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if res := results[0]; res.Outcome != UpToDate && res.Outcome != Updated {
		t.Errorf("first sync outcome = %v (%s), want the clone's master to be recognised; actions=%v",
			res.Outcome, res.Detail, res.Actions)
	}
	// The assumed branch must not have been created on the way past.
	clone := filepath.Join(T, "clones", "proj")
	out, err := exec.Command("git", "-C", clone, "branch", "--list", "main").Output()
	must(t, err)
	if trim(string(out)) != "" {
		t.Errorf("provisioning created the assumed branch main: %q", string(out))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func trim(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
