package sync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/config"
)

// syncedRepo provisions one repo carrying the given settings, syncs it once so
// a cadence timestamp exists, and returns a runner for subsequent --if-due
// sweeps at the given built-in interval.
func syncedRepo(t *testing.T, settings string) func(builtin time.Duration) Result {
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
workflow = "upstream-push"
branches = ["main"]
`+settings+`
`), 0o644))

	reg, err := config.Load([]string{regPath})
	must(t, err)
	if err := reg.Validate(); err != nil {
		t.Fatalf("registry with %q: %v", settings, err)
	}
	repos, err := reg.Repos()
	must(t, err)

	out := filepath.Join(T, "out")
	if r := Run(reg, repos, Options{StateDir: out, Frequency: time.Hour}); r[0].Err != nil {
		t.Fatalf("initial sync: %v", r[0].Err)
	}
	return func(builtin time.Duration) Result {
		results := Run(reg, repos, Options{StateDir: out, Frequency: builtin, IfDue: true})
		if len(results) != 1 {
			t.Fatalf("got %d results, want 1", len(results))
		}
		return results[0]
	}
}

// TestSyncFrequencyOverridesTheBuiltinInterval: DESIGN §5.5's cadence is a
// setting, not a constant. Until it was one, `sync --if-due` skipped every repo
// on the same hardcoded 7 days — including the plugin or work repo that wants
// looking at on every startup run, which had no way to say so.
func TestSyncFrequencyOverridesTheBuiltinInterval(t *testing.T) {
	for _, tc := range []struct {
		name     string
		settings string
		builtin  time.Duration
		wantDue  bool
	}{
		{name: "unset follows the builtin", builtin: time.Hour},
		{name: "unset, builtin already elapsed", builtin: 0, wantDue: true},
		{name: "own interval, not yet elapsed", settings: `sync_frequency = "24h"`, builtin: 0},
		{name: "own interval shorter than the builtin", settings: `sync_frequency = "0s"`,
			builtin: 365 * 24 * time.Hour, wantDue: true},
		{name: "zero means always due", settings: `sync_frequency = "0"`,
			builtin: 365 * 24 * time.Hour, wantDue: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := syncedRepo(t, tc.settings)(tc.builtin)
			if gotDue := res.Detail != "not due"; gotDue != tc.wantDue {
				t.Errorf("due = %v, want %v (detail %q; actions=%v)",
					gotDue, tc.wantDue, res.Detail, res.Actions)
			}
		})
	}
}

// TestNotDueNamesTheReposOwnInterval: the skip line is the only place the
// cadence is ever visible, so printing the caller's built-in on a repo that
// overrode it would explain the run with a number nothing used.
func TestNotDueNamesTheReposOwnInterval(t *testing.T) {
	res := syncedRepo(t, `sync_frequency = "48h"`)(time.Hour)
	want := (48 * time.Hour).String()
	found := false
	for _, a := range res.Actions {
		if strings.Contains(a.Text, "not due") && strings.Contains(a.Text, want) {
			found = true
		}
	}
	if !found {
		t.Errorf("no skip line naming the repo's own %s interval; actions=%v", want, res.Actions)
	}
}
