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

// upstreamPushRepo provisions a single-tree upstream-push clone from a fresh
// bare origin with one commit on main, applying any extra TOML lines to the
// repo's entry (e.g. `fetch_skip = [...]`). Returns the registry/repos (for a
// second Run) and the clone's working-tree path.
func upstreamPushRepo(t *testing.T, extraTOML string) (reg *config.Registry, repos []model.Repo, clone string) {
	t.Helper()
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin := filepath.Join(remotes, "acme", "proj")
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
branches = ["main"]
`+extraTOML+`
`), 0o644))

	r, err := config.Load([]string{regPath})
	must(t, err)
	rs, err := r.Repos()
	must(t, err)

	clone = filepath.Join(T, "clones", "proj")
	if res := Run(r, rs, Options{StateDir: filepath.Join(T, "out1"), Frequency: time.Hour}); res[0].Outcome > Updated {
		t.Fatalf("provisioning sync = %v (%s); actions=%v", res[0].Outcome, res[0].Detail, res[0].Actions)
	}
	return r, rs, clone
}

func addRemote(t *testing.T, dir, name, url string) {
	t.Helper()
	git(t, dir, "remote", "add", name, url)
}

// tryRevParse is revParse without the fatal-on-error: some assertions here
// need to check a ref is absent, not read it.
func tryRevParse(t *testing.T, dir, ref string) (string, error) {
	t.Helper()
	out, err := exec.Command("git", "-C", dir, "rev-parse", ref).Output()
	return trim(string(out)), err
}

// TestSyncFetchesUnmanagedRemote: a remote added by hand (not origin, not a
// workflow's second remote) must still be fetched — DESIGN §3.6's "fetch
// every remote git knows about, manage only the contract's own".
func TestSyncFetchesUnmanagedRemote(t *testing.T) {
	reg, repos, clone := upstreamPushRepo(t, "")

	extra := t.TempDir()
	git(t, extra, "init", "-q", "-b", "main")
	writeCommit(t, extra, "b", "extra\n", "extra commit")
	want := revParse(t, extra, "main")

	addRemote(t, clone, "extra", extra)

	Run(reg, repos, Options{StateDir: filepath.Join(t.TempDir(), "out2"), Frequency: time.Hour})

	got, err := tryRevParse(t, clone, "refs/remotes/extra/main")
	if err != nil {
		t.Fatalf("refs/remotes/extra/main not fetched: %v", err)
	}
	if got != want {
		t.Errorf("refs/remotes/extra/main = %s, want %s (the extra remote's tip)", got, want)
	}
}

// TestFetchSkipExcludesRemote: a remote named by fetch_skip must never be
// fetched, even though sync now fetches every other remote by default.
func TestFetchSkipExcludesRemote(t *testing.T) {
	reg, repos, clone := upstreamPushRepo(t, `fetch_skip = ["extra"]`)

	extra := t.TempDir()
	git(t, extra, "init", "-q", "-b", "main")
	writeCommit(t, extra, "b", "extra\n", "extra commit")

	addRemote(t, clone, "extra", extra)

	Run(reg, repos, Options{StateDir: filepath.Join(t.TempDir(), "out2"), Frequency: time.Hour})

	if _, err := tryRevParse(t, clone, "refs/remotes/extra/main"); err == nil {
		t.Error("refs/remotes/extra/main exists; fetch_skip should have excluded it")
	}
}

// TestUnmanagedRemoteFetchFailureIsAttention: a fetch failure on a remote
// nothing else depends on must surface as Attention, not fail the repo —
// origin's own reconciliation is unaffected by an unrelated remote's failure.
func TestUnmanagedRemoteFetchFailureIsAttention(t *testing.T) {
	reg, repos, clone := upstreamPushRepo(t, "")

	addRemote(t, clone, "extra", filepath.Join(t.TempDir(), "does-not-exist"))

	res := Run(reg, repos, Options{StateDir: filepath.Join(t.TempDir(), "out2"), Frequency: time.Hour})[0]
	if res.Err != nil {
		t.Errorf("Err = %v; an unmanaged remote's fetch failure must not fail the repo", res.Err)
	}
	if res.Outcome != Attention {
		t.Errorf("outcome = %v (%s), want Attention", res.Outcome, res.Detail)
	}
	var trace string
	for _, a := range res.Actions {
		trace += a.Text + "\n"
	}
	if !strings.Contains(trace, "fetch extra failed") {
		t.Errorf("trace does not report the failed extra fetch:\n%s", trace)
	}
}

// TestForceTagsDoesNotApplyToUnmanagedRemote: force_tags blesses a tag move by
// name alone, with no notion of which remote proposed it. An unmanaged remote
// carrying a same-named tag pointing elsewhere must never have that move
// followed, even when force_tags names it — the privilege stays with the
// remotes a person configuring the repo actually named (DESIGN §3.6).
func TestForceTagsDoesNotApplyToUnmanagedRemote(t *testing.T) {
	reg, repos, clone := upstreamPushRepo(t, `force_tags = ["release-*"]`)

	local := revParse(t, clone, "main")
	git(t, clone, "tag", "release-1.0")
	git(t, clone, "push", "-q", "origin", "release-1.0")

	extra := t.TempDir()
	git(t, extra, "init", "-q", "-b", "main")
	writeCommit(t, extra, "b", "extra\n", "extra commit")
	git(t, extra, "tag", "release-1.0")
	extraTag := revParse(t, extra, "release-1.0")
	if extraTag == local {
		t.Fatal("test setup: extra's release-1.0 must differ from origin's")
	}

	addRemote(t, clone, "extra", extra)

	res := Run(reg, repos, Options{StateDir: filepath.Join(t.TempDir(), "out2"), Frequency: time.Hour})[0]
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}

	got := revParse(t, clone, "release-1.0")
	if got != local {
		t.Errorf("release-1.0 = %s, want %s (origin's) — force_tags must not apply to an unmanaged remote", got, local)
	}

	if res.Outcome != Attention {
		t.Errorf("outcome = %v (%s), want Attention for the refused release-1.0", res.Outcome, res.Detail)
	}
	// A lone finding folds onto the row rather than getting its own bullet
	// (finalizeBranches), same as TestOneMovedTagFoldsOntoTheRow.
	refused := strings.Contains(res.Detail, "release-1.0") && strings.Contains(res.Detail, "not followed")
	for _, b := range res.Branches {
		if b.Kind == RefTag && b.Name == "release-1.0" && b.Outcome == Attention && strings.Contains(b.Summary, "not followed") {
			refused = true
		}
	}
	if !refused {
		t.Errorf("no refused-tag finding for release-1.0: detail=%q rows=%+v", res.Detail, res.Branches)
	}
}
