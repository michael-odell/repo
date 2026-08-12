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

	// Each tag gets its own row, like a branch: a repo whose upstream retags
	// every build moves a dozen at once, and a list of them squeezed into the
	// repo's one detail cell is unreadable exactly when it matters.
	notes := map[string]BranchNote{}
	for _, b := range res.Branches {
		if b.Kind == RefTag {
			notes["tag/"+b.Name] = b
		} else {
			notes["branch/"+b.Name] = b
		}
	}
	followed, ok := notes["tag/independent.latest"]
	if !ok {
		t.Fatalf("no row for the followed tag; rows = %+v", res.Branches)
	}
	if followed.Outcome != Updated || !strings.Contains(followed.Summary, "followed") {
		t.Errorf("followed row = %+v, want an Updated row saying it was followed", followed)
	}
	// The object it moved away from must be on that row: refs/tags has no
	// reflog, so after the fetch it exists nowhere else.
	if old := trim(string(before)); !strings.Contains(followed.Summary, old[:8]) {
		t.Errorf("followed row = %q, want the pre-move object %s", followed.Summary, old[:8])
	}
	refused, ok := notes["tag/v1.0"]
	if !ok {
		t.Fatalf("no row for the refused tag; rows = %+v", res.Branches)
	}
	if refused.Outcome != Attention || !strings.Contains(refused.Summary, "not followed") {
		t.Errorf("refused row = %+v, want an Attention row saying it was not followed", refused)
	}
	// Tags and branches are counted together but named honestly.
	if !strings.Contains(res.Detail, "refs") {
		t.Errorf("row detail = %q; a mixed run should roll up as refs", res.Detail)
	}
}

// TestOneMovedTagFoldsOntoTheRow: the machinery that gives many tags their own
// rows must not cost the common case a line. A single moved tag with nothing
// else to report folds onto the repo's row, named — an unnamed tag summary
// would say nothing about which tag moved, since a tag is never "the only one".
func TestOneMovedTagFoldsOntoTheRow(t *testing.T) {
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
	git(t, seed, "tag", "rolling")
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
`), 0o644))
	reg, err := config.Load([]string{regPath})
	must(t, err)
	repos, err := reg.Repos()
	must(t, err)
	Run(reg, repos, Options{StateDir: filepath.Join(T, "out1"), Frequency: time.Hour})

	// Move only the tag — main stays put, so the tag is the only thing to say.
	git(t, seed, "tag", "-f", "-a", "rolling", "-m", "moved")
	git(t, seed, "push", "-q", "--force", "origin", "rolling")

	res := Run(reg, repos, Options{StateDir: filepath.Join(T, "out2"), Frequency: time.Hour})[0]
	if len(res.Branches) != 0 {
		t.Errorf("rows = %+v; a lone finding should fold onto the repo row", res.Branches)
	}
	if !strings.Contains(res.Detail, "tag rolling") {
		t.Errorf("row detail = %q, want the tag named on the row", res.Detail)
	}
}

// vendorPinned builds a bare origin with a tag-pinned vendor repo and returns
// the seed clone, the registry, and the resolved repos. forceTags is written
// into the registry verbatim so a caller can test with and without it.
func vendorPinned(t *testing.T, forceTags string) (seed string, reg *config.Registry, repos []model.Repo, clone string) {
	t.Helper()
	T := t.TempDir()
	remotes := filepath.Join(T, "remotes")
	origin := filepath.Join(remotes, "acme", "vend")
	must(t, os.MkdirAll(filepath.Dir(origin), 0o755))
	git(t, T, "init", "-q", "-b", "main", "--bare", origin)

	seed = filepath.Join(T, "seed")
	git(t, T, "clone", "-q", origin, seed)
	git(t, seed, "checkout", "-q", "-b", "main")
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("reviewed\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "reviewed")
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
id = "local:acme/vend"
workflow = "vendor"
branches = ["main"]
pin = "v1.0"
`+forceTags+"\n"), 0o644))
	var err error
	reg, err = config.Load([]string{regPath})
	must(t, err)
	repos, err = reg.Repos()
	must(t, err)
	Run(reg, repos, Options{StateDir: filepath.Join(T, "out1"), Frequency: time.Hour})
	return seed, reg, repos, filepath.Join(T, "clones", "vend")
}

// moveVendorTag rewrites the pinned tag's content upstream — the supply-chain
// case: same version number, different code.
func moveVendorTag(t *testing.T, seed string) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(seed, "a"), []byte("swapped\n"), 0o644))
	git(t, seed, "add", "a")
	git(t, seed, "commit", "-qm", "swapped")
	git(t, seed, "tag", "-f", "v1.0")
	git(t, seed, "push", "-q", "origin", "main")
	git(t, seed, "push", "-q", "--force", "origin", "v1.0")
}

// TestVendorPinStaysAtReviewedContentWithoutForceTags: the vendor workflow no
// longer has its own rewrite check — the ref does the work. An unblessed moved
// tag is refused by the fetch, so refs/tags/v1.0 still names the reviewed
// object and the checkout lands on the reviewed content. The "stop" is a
// consequence of not moving a ref, not a special case.
func TestVendorPinStaysAtReviewedContentWithoutForceTags(t *testing.T) {
	seed, reg, repos, clone := vendorPinned(t, "")
	moveVendorTag(t, seed)

	res := Run(reg, repos, Options{StateDir: filepath.Join(t.TempDir(), "out2"), Frequency: time.Hour})[0]
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	body, err := os.ReadFile(filepath.Join(clone, "a"))
	must(t, err)
	if got := trim(string(body)); got != "reviewed" {
		t.Errorf("working tree = %q, want the reviewed content — an unblessed tag move must not reach the tree", got)
	}
	// And it is reported, on the tag's own row, for a vendor repo exactly as for
	// any other workflow.
	var found bool
	for _, b := range res.Branches {
		if b.Kind == RefTag && b.Name == "v1.0" {
			found = true
		}
	}
	if !found && !strings.Contains(res.Detail, "v1.0") {
		t.Errorf("the moved pin was not reported: detail=%q rows=%+v", res.Detail, res.Branches)
	}
}

// TestVendorPinFollowsWithForceTags: the same repo with the tag blessed follows
// the move and lands on the new content — `repo` works with an upstream that
// rewrites tags rather than refusing to sync — while still reporting it.
func TestVendorPinFollowsWithForceTags(t *testing.T) {
	seed, reg, repos, clone := vendorPinned(t, `force_tags = ["v*"]`)
	moveVendorTag(t, seed)

	res := Run(reg, repos, Options{StateDir: filepath.Join(t.TempDir(), "out2"), Frequency: time.Hour})[0]
	if res.Err != nil {
		t.Fatalf("Err = %v", res.Err)
	}
	body, err := os.ReadFile(filepath.Join(clone, "a"))
	must(t, err)
	if got := trim(string(body)); got != "swapped" {
		t.Errorf("working tree = %q, want the followed content — force_tags blessed this tag", got)
	}
	var trace string
	for _, a := range res.Actions {
		trace += a.Text + "\n"
	}
	if !strings.Contains(trace, "followed moved tag v1.0") {
		t.Errorf("the followed move was not reported:\n%s", trace)
	}
}
