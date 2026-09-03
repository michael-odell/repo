package config

import (
	"fmt"
	"strings"
	"testing"
)

// TestAnUnimplementedValueIsRejected is the rule from DESIGN §3.4: the loader
// already refuses a key nothing knows, and this refuses a *value* nothing acts
// on — the case that reads exactly like a working configuration and behaves
// exactly like an absent one. `prune = "auto"` sat in a live registry that way
// for months.
//
// The enum is a synthetic one, because every real value is implemented today;
// the mechanism has to be tested regardless, since the day it matters is the
// day someone adds a value ahead of its code.
func TestAnUnimplementedValueIsRejected(t *testing.T) {
	var got []string
	add := func(f string, a ...any) { got = append(got, fmt.Sprintf(f, a...)) }
	allowed := []enumValue{done("here"), planned("later")}

	check := enumChecker(add, "root.x")
	later := "later"
	check("mode", &later, allowed)
	if len(got) != 1 {
		t.Fatalf("an unimplemented value produced %d errors, want 1: %v", len(got), got)
	}
	if !strings.Contains(got[0], "not implemented yet") {
		t.Errorf("the error does not distinguish this from a typo: %q", got[0])
	}
	// And it must not offer the dud back as a suggestion — that is advice to
	// hit the same wall again.
	if strings.Contains(got[0], "want one of here, later") {
		t.Errorf("the error offers the unimplemented value as an option: %q", got[0])
	}

	got = nil
	here := "here"
	check("mode", &here, allowed)
	if len(got) != 0 {
		t.Errorf("an implemented value was rejected: %v", got)
	}

	got = nil
	typo := "hree"
	check("mode", &typo, allowed)
	if len(got) != 1 || strings.Contains(got[0], "not implemented") {
		t.Errorf("a misspelling was not reported as one: %v", got)
	}
}

// TestPruneModesAreAllImplemented: PruneModes is what the sweep's own test
// checks itself against, so an empty or partial list would quietly weaken it.
func TestPruneModesAreAllImplemented(t *testing.T) {
	if got, want := len(PruneModes()), len(validPrune); got != want {
		t.Fatalf("PruneModes() has %d of %d prune values; the sweep implements them all", got, want)
	}
}

// contains is a test-only helper now that validation compares against enumValue
// rather than plain strings.
func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// TestHookEventMustBeOneSomethingRuns applies DESIGN §3.4's rule to hooks. The
// sync engine matches `after` exactly and skips everything else, so before this
// check a hook naming any other event — or omitting `after`, or omitting `run` —
// parsed cleanly, was inherited and reported by `repo config` like any other
// setting, and never ran, on any sweep, with nothing said.
func TestHookEventMustBeOneSomethingRuns(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hooks string
		want  string // substring of the expected error; empty means valid
	}{
		{name: "fetch runs", hooks: `[ { after = "fetch", run = "make deps" } ]`},
		{name: "unknown event", hooks: `[ { after = "pull", run = "make deps" } ]`,
			want: `hooks[0].after = "pull" (want one of fetch)`},
		{name: "typo", hooks: `[ { after = "fetchh", run = "make deps" } ]`,
			want: `hooks[0].after = "fetchh"`},
		{name: "no event", hooks: `[ { run = "make deps" } ]`,
			want: "hooks[0]: missing `after`"},
		{name: "no command", hooks: `[ { after = "fetch" } ]`,
			want: "hooks[0]: missing `run`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeTOML(t, dir, `
[hosts.github]
base = "git@github.com:"

[root.wd]
dir   = "~/wd"
hooks = `+tc.hooks+`
repos = ["github:acme/thing"]
`)
			reg, err := Load([]string{dir})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			err = reg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("valid hook rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("want an error mentioning %q, got nil", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should mention %q; got:\n%v", tc.want, err)
			}
		})
	}
}

// TestHookOnARepoEntryIsChecked: a hook is usually written on the one repo that
// needs it, which is the least-watched place in the registry — the tier checks
// alone would miss it.
func TestHookOnARepoEntryIsChecked(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[hosts.github]
base = "git@github.com:"

[root.wd]
dir = "~/wd"

[[root.wd.repo]]
id    = "github:acme/thing"
hooks = [ { after = "clone", run = "make deps" } ]
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = reg.Validate()
	if err == nil {
		t.Fatal("want an error for a repo-level hook event nothing runs, got nil")
	}
	if !strings.Contains(err.Error(), `hooks[0].after = "clone"`) {
		t.Errorf("error should name the repo's hook; got:\n%v", err)
	}
}

// TestHookEventsAreAllImplemented mirrors TestPruneModesAreAllImplemented:
// HookEvents is what the engine's own test checks itself against.
func TestHookEventsAreAllImplemented(t *testing.T) {
	if got, want := len(HookEvents()), len(validHookEvents); got != want {
		t.Fatalf("HookEvents() has %d of %d hook events; the engine implements them all", got, want)
	}
}
