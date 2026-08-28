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
