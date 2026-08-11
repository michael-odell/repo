package sync

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
)

// TestMovedTagDoesNotAbandonTheRepo: some upstreams rewrite tags as ordinary
// practice. Git refuses to overwrite an existing tag, so `fetch --tags` rejects
// exactly those refs, updates every other ref, and exits non-zero — and taking
// that exit status at face value abandoned the whole repo, leaving its branches
// unsynced on that run and every run after, since the tag stays rejected.
//
// The moved tag must surface as a finding on a repo that still syncs: main
// advances, and the run reports Attention rather than a failure.
func TestMovedTagDoesNotAbandonTheRepo(t *testing.T) {
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin := filepath.Join(remotes, "acme", "proj")
	must(t, os.MkdirAll(filepath.Dir(origin), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", origin)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "one")
	git(t, seed, "tag", "independent.latest")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", "origin", "independent.latest")

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.clones]
dir = "`+filepath.Join(T, "clones")+`"
[[root.clones.repo]]
id = "local:acme/proj"
branches = ["main"]
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)

	// First sync provisions the clone, which brings the tag down with it.
	if res := Run(reg, repos, Options{StateDir: filepath.Join(T, "out1"), Frequency: time.Hour}); res[0].Outcome > Updated {
		t.Fatalf("provisioning sync = %v (%s); actions=%v", res[0].Outcome, res[0].Detail, res[0].Actions)
	}

	// Upstream moves the tag and advances main — the situation this is about.
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\ntwo\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "two")
	git(t, seed, "tag", "-f", "independent.latest")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", "--force", "origin", "independent.latest")

	// A fresh state dir so the cadence check doesn't skip the run.
	results := Run(reg, repos, Options{StateDir: filepath.Join(T, "out2"), Frequency: time.Hour})
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	res := results[0]
	if res.Err != nil {
		t.Errorf("Err = %v; a moved tag must not fail the repo", res.Err)
	}
	if res.Outcome == Failed {
		t.Errorf("outcome = Failed (%s); want the moved tag reported, not the repo abandoned", res.Detail)
	}
	if res.Outcome != Attention {
		t.Errorf("outcome = %v (%s), want Attention; actions=%v", res.Outcome, res.Detail, res.Actions)
	}

	// The point of not stopping: the branch still got synced.
	clone := filepath.Join(T, "clones", "proj")
	out, err := exec.Command("git", "-C", clone, "rev-list", "--count", "main").Output()
	must(t, err)
	if got := trim(string(out)); got != "2" {
		t.Errorf("main is at %s commit(s), want 2 — the sync stopped at the rejected tag", got)
	}

	// And the tag itself was left where it was: nothing has opted into moving it.
	local, err := exec.Command("git", "-C", clone, "rev-list", "-n1", "independent.latest").Output()
	must(t, err)
	head, err := exec.Command("git", "-C", clone, "rev-parse", "main").Output()
	must(t, err)
	if trim(string(local)) == trim(string(head)) {
		t.Error("the moved tag was followed; a plain fetch must never overwrite an existing tag")
	}
}

// TestForceTagsFollowsAndReportsTheOldObject: blessing a tag says "stop
// stopping for this", never "stop telling me". The move must reach the report
// carrying the object the tag pointed at *before* — refs/tags has no reflog, so
// once the fetch lands that id exists nowhere else, and it is the whole
// recovery story for a rewrite you didn't want.
func TestForceTagsFollowsAndReportsTheOldObject(t *testing.T) {
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin := filepath.Join(remotes, "acme", "proj")
	must(t, os.MkdirAll(filepath.Dir(origin), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", origin)

	seed := filepath.Join(T, "seed")
	git(t, T, "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "one")
	git(t, seed, "tag", "independent.latest")
	git(t, seed, "tag", "v1.0")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", "origin", "--tags")

	regPath := filepath.Join(T, "registry.toml")
	must(t, os.WriteFile(regPath, []byte(`
[hosts.local]
base = "`+remotes+`/"
[root.clones]
dir = "`+filepath.Join(T, "clones")+`"
[[root.clones.repo]]
id = "local:acme/proj"
branches = ["main"]
force_tags = ["*.latest"]
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)
	Run(reg, repos, Options{StateDir: filepath.Join(T, "out1"), Frequency: time.Hour})

	clone := filepath.Join(T, "clones", "proj")
	before, err := exec.Command("git", "-C", clone, "rev-parse", "independent.latest").Output()
	must(t, err)

	// Upstream moves both tags; only one of them is blessed.
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("one\ntwo\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "two")
	git(t, seed, "tag", "-f", "independent.latest")
	git(t, seed, "tag", "-f", "v1.0")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", "--force", "origin", "--tags")

	res := Run(reg, repos, Options{StateDir: filepath.Join(T, "out2"), Frequency: time.Hour})[0]
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}

	// The blessed tag followed; the unblessed one did not.
	head, err := exec.Command("git", "-C", clone, "rev-parse", "main").Output()
	must(t, err)
	blessed, err := exec.Command("git", "-C", clone, "rev-parse", "independent.latest").Output()
	must(t, err)
	if trim(string(blessed)) != trim(string(head)) {
		t.Errorf("independent.latest = %s, want %s — force_tags should have followed it",
			trim(string(blessed)), trim(string(head)))
	}
	unblessed, err := exec.Command("git", "-C", clone, "rev-parse", "v1.0").Output()
	must(t, err)
	if trim(string(unblessed)) == trim(string(head)) {
		t.Error("v1.0 moved; only tags force_tags names may be overwritten")
	}

	// The trace must name the old object, not merely report that something moved.
	var trace string
	for _, a := range res.Actions {
		trace += a.Text + "\n"
	}
	if !strings.Contains(trace, "followed moved tag independent.latest") {
		t.Errorf("trace does not report the followed move:\n%s", trace)
	}
	if old := trim(string(before)); !strings.Contains(trace, old) {
		t.Errorf("trace does not carry the pre-move object %s:\n%s", old, trace)
	}
	// And the unblessed tag is still surfaced as a finding.
	if res.Outcome != Attention {
		t.Errorf("outcome = %v (%s), want Attention for the refused v1.0", res.Outcome, res.Detail)
	}

	// The compact row is what a sweep actually shows, and a followed rewrite
	// has to survive on it alongside whatever else the repo did — a move that
	// only reaches --verbose is a move nobody reads.
	if !strings.Contains(res.Detail, "followed independent.latest moved") {
		t.Errorf("row detail = %q; the followed move must stay on the row", res.Detail)
	}
	if !strings.Contains(res.Detail, "v1.0") {
		t.Errorf("row detail = %q; the refused tag must stay on the row too", res.Detail)
	}
}
