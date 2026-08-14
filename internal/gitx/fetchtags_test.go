package gitx

import (
	"errors"
	"os"
	"strings"
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

	_, err := Fetch(clone, "origin", TagPolicy{})
	var moved *MovedTagsError
	if !errors.As(err, &moved) {
		t.Fatalf("Fetch = %v; want *MovedTagsError", err)
	}
	if len(moved.Tags) != 1 || moved.Tags[0].Tag != "independent.latest" {
		t.Errorf("Tags = %v, want [independent.latest]", moved.Tags)
	}
	if moved.Remote != "origin" {
		t.Errorf("Remote = %q, want origin", moved.Remote)
	}
	// The refused tag's local object is still there to name — nothing was
	// destroyed — but the caller can't say what upstream wanted without it.
	if m := moved.Tags[0]; m.From == "" || m.To == "" || m.From == m.To {
		t.Errorf("Tags[0] = %+v; want distinct non-empty From/To", m)
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
	if _, err := Fetch(clone, "origin", TagPolicy{}); !errors.As(err, &moved) {
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

	_, err := Fetch(clone, "origin", TagPolicy{})
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

	if _, err := Fetch(clone, "origin", TagPolicy{}); err != nil {
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
	if tags, only := rejectedTags("! 1 2 refs/tags/a\n* 0 3 refs/tags/b\n"); !only || len(tags) != 1 ||
		tags[0].Tag != "a" || tags[0].From != "1" || tags[0].To != "2" {
		t.Errorf("rejectedTags = %v, %v; want [{a 1 2}], true", tags, only)
	}
}

// TestFetchArgsShapes pins the refspecs each policy produces, since the whole
// feature is which refs are asked for and which carry the "+" that lets git
// overwrite them.
func TestFetchArgsShapes(t *testing.T) {
	// Any explicit tag refspec replaces the remote's configured one, so the
	// branch refspec has to come along or branches stop being fetched at all.
	const heads = "+refs/heads/*:refs/remotes/origin/*"
	for _, c := range []struct {
		name   string
		policy TagPolicy
		want   string
	}{
		{"default fetches every tag and leaves the configured refspec alone",
			TagPolicy{}, "--tags"},
		{`explicit ["*"] is the same as the default`,
			TagPolicy{Fetch: []string{"*"}}, "--tags"},
		{"empty list fetches no tags at all",
			TagPolicy{Fetch: []string{}}, heads + " --no-tags"},
		{"a forced pattern rides alongside the full scope",
			TagPolicy{Force: []string{"*.latest"}},
			heads + " --tags +refs/tags/*.latest:refs/tags/*.latest"},
		{"a narrowed scope suppresses auto-following",
			TagPolicy{Fetch: []string{"v*"}},
			heads + " --no-tags refs/tags/v*:refs/tags/v*"},
		{"scope and force compose",
			TagPolicy{Fetch: []string{"v*"}, Force: []string{"v*-rc"}},
			heads + " --no-tags refs/tags/v*:refs/tags/v* +refs/tags/v*-rc:refs/tags/v*-rc"},
		{"no tags fetched means nothing to force",
			TagPolicy{Fetch: []string{}, Force: []string{"*"}}, heads + " --no-tags"},
	} {
		if got := strings.Join(c.policy.fetchArgs("origin"), " "); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

// TestForceTagsFollowsOnlyWhatItNames is the end-to-end claim the config makes:
// a blessed tag is overwritten in place while an unlisted one is still refused,
// in the same fetch.
func TestForceTagsFollowsOnlyWhatItNames(t *testing.T) {
	origin, clone := originWithClone(t, "independent.latest")
	git(t, origin, "tag", "v1.0")
	git(t, clone, "fetch", "-q", "--tags", "origin")
	moveTag(t, origin, "independent.latest")
	git(t, origin, "tag", "-f", "v1.0") // both tags move

	policy := TagPolicy{Force: []string{"*.latest"}}
	res, err := Fetch(clone, "origin", policy)

	// v1.0 was not blessed, so the fetch still reports it as refused.
	var moved *MovedTagsError
	if !errors.As(err, &moved) {
		t.Fatalf("Fetch = %v; want v1.0 still refused", err)
	}
	if len(moved.Tags) != 1 || moved.Tags[0].Tag != "v1.0" {
		t.Errorf("refused = %v, want [v1.0] only", moved.Tags)
	}
	// …while the blessed one was followed.
	head, _ := RevParse(clone, "refs/remotes/origin/main")
	blessed, _ := RevParse(clone, "refs/tags/independent.latest")
	if blessed != head {
		t.Errorf("independent.latest = %s, want %s — force_tags should have followed it", blessed, head)
	}
	if unblessed, _ := RevParse(clone, "refs/tags/v1.0"); unblessed == head {
		t.Error("v1.0 moved; only the tags force_tags names may be overwritten")
	}
	// The move must be reported with the object it moved away from: refs/tags
	// has no reflog, so after this fetch that id exists nowhere else.
	if len(res.Followed) != 1 || res.Followed[0].Tag != "independent.latest" {
		t.Fatalf("Followed = %v, want one move of independent.latest", res.Followed)
	}
	if m := res.Followed[0]; m.To != head || m.From == "" || m.From == m.To {
		t.Errorf("Followed[0] = %+v; want From = the pre-move object, To = %s", m, head)
	}
}

// TestNarrowedTagsExcludesTheRest: `tags` is a real scope, which needs
// --no-tags — otherwise git auto-follows any tag reachable from the branches it
// fetched and the list quietly isn't a list.
func TestNarrowedTagsExcludesTheRest(t *testing.T) {
	origin, clone := originWithClone(t, "v1.0")
	git(t, origin, "tag", "build.deadbeef") // reachable from main, so auto-follow would take it

	if _, err := Fetch(clone, "origin", TagPolicy{Fetch: []string{"v*"}}); err != nil {
		t.Fatalf("Fetch = %v", err)
	}
	if _, ok := RevParse(clone, "refs/tags/build.deadbeef"); ok {
		t.Error("build.deadbeef arrived despite tags = [\"v*\"]")
	}
	if _, ok := RevParse(clone, "refs/tags/v1.0"); !ok {
		t.Error("v1.0 did not arrive despite matching tags")
	}
}
