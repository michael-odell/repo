package gitx

import (
	"errors"
	"os"
	"testing"
)

// originWithClone builds an origin holding one commit and a tag, plus a clone of
// it, and returns both. The tag is what the moved-tag tests go on to rewrite.
func originWithClone(t *testing.T, tag string) (origin, clone string) {
	t.Helper()
	origin, clone = t.TempDir(), t.TempDir()
	git(t, origin, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(origin+"/f", []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "f")
	git(t, origin, "commit", "-q", "-m", "one")
	git(t, origin, "tag", tag)
	git(t, clone, "clone", "-q", origin, ".")
	return origin, clone
}

// moveTag advances origin by a commit and repoints tag at it, which is the
// upstream behaviour the whole feature exists for.
func moveTag(t *testing.T, origin, tag string) {
	t.Helper()
	if err := os.WriteFile(origin+"/f", []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "commit", "-q", "-am", "two")
	git(t, origin, "tag", "-f", tag)
}

// TestFetchMovedTagIsTyped: a fetch that fails only because the remote moved a
// tag the clone already has must come back as *MovedTagsError naming that tag,
// not as an opaque failure — the caller decides what a moved tag means, and it
// can only do that if it can tell this case apart from a dead remote.
func TestFetchMovedTagIsTyped(t *testing.T) {
	origin, clone := originWithClone(t, "independent.latest")
	moveTag(t, origin, "independent.latest")

	err := Fetch(clone, "origin")
	var moved *MovedTagsError
	if !errors.As(err, &moved) {
		t.Fatalf("Fetch = %v; want *MovedTagsError", err)
	}
	if len(moved.Tags) != 1 || moved.Tags[0] != "independent.latest" {
		t.Errorf("Tags = %v, want [independent.latest]", moved.Tags)
	}
	if moved.Remote != "origin" {
		t.Errorf("Remote = %q, want origin", moved.Remote)
	}
}

// TestFetchMovedTagStillUpdatesBranches is the reason a moved tag must not
// abandon the repo: git rejects the tag and updates every other ref in the same
// run, so the remote-tracking branch the sync is actually about is current even
// though the fetch exited non-zero.
func TestFetchMovedTagStillUpdatesBranches(t *testing.T) {
	origin, clone := originWithClone(t, "independent.latest")
	before, _ := RevParse(clone, "refs/remotes/origin/main")
	moveTag(t, origin, "independent.latest")

	var moved *MovedTagsError
	if err := Fetch(clone, "origin"); !errors.As(err, &moved) {
		t.Fatalf("Fetch = %v; want *MovedTagsError", err)
	}
	after, ok := RevParse(clone, "refs/remotes/origin/main")
	if !ok || after == before {
		t.Errorf("origin/main = %q (was %q); the branch should have advanced despite the rejected tag", after, before)
	}
	// The tag itself must be left alone: nothing has opted into following it.
	head, _ := RevParse(clone, "refs/tags/independent.latest")
	if head == after {
		t.Error("the moved tag was followed; a plain fetch must never overwrite an existing tag")
	}
}

// TestFetchRealFailureStaysFatal: only a tag rejection earns the typed error. A
// remote that isn't there must keep failing as it always did, or the sync would
// carry on against a clone it never managed to update.
func TestFetchRealFailureStaysFatal(t *testing.T) {
	_, clone := originWithClone(t, "v1")
	git(t, clone, "remote", "set-url", "origin", t.TempDir()+"/nope")

	err := Fetch(clone, "origin")
	if err == nil {
		t.Fatal("Fetch on a missing remote = nil; want an error")
	}
	var moved *MovedTagsError
	if errors.As(err, &moved) {
		t.Errorf("Fetch on a missing remote = %v; want an ordinary failure", moved)
	}
}

// TestFetchCleanIsNil guards the ordinary path: a fetch with nothing to refuse
// reports success, so the porcelain plumbing can't turn a normal sync into a
// finding.
func TestFetchCleanIsNil(t *testing.T) {
	origin, clone := originWithClone(t, "v1")
	if err := os.WriteFile(origin+"/f", []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "commit", "-q", "-am", "two")
	git(t, origin, "tag", "v2") // a *new* tag is never a clobber

	if err := Fetch(clone, "origin"); err != nil {
		t.Fatalf("Fetch = %v; want nil", err)
	}
	if _, ok := RevParse(clone, "refs/tags/v2"); !ok {
		t.Error("the new tag did not arrive")
	}
}

// TestRejectedTagsRefusesBranches: a rejected *branch* means something other
// than a moved tag went wrong, and it must keep the whole fetch fatal even when
// a tag was rejected alongside it.
func TestRejectedTagsRefusesBranches(t *testing.T) {
	const porcelain = "! 1111111 2222222 refs/tags/moved\n" +
		"! 3333333 4444444 refs/remotes/origin/main\n"
	if tags, only := rejectedTags(porcelain); only {
		t.Errorf("rejectedTags = %v, true; a rejected branch must not read as tags-only", tags)
	}
	if tags, only := rejectedTags("! 1 2 refs/tags/a\n* 0 3 refs/tags/b\n"); !only || len(tags) != 1 || tags[0] != "a" {
		t.Errorf("rejectedTags = %v, %v; want [a], true", tags, only)
	}
}
