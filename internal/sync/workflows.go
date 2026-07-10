package sync

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/michael-odell/repo/internal/gitx"
)

// updateForkPR advances the important branch to the definitive upstream, then
// fast-forward-pushes it to the fork (origin). DESIGN §5.1. A universal
// local-FF-then-push produces the same fork state as `gh repo sync` on any host,
// so it is used unconditionally; a server-side gh fast path is a future
// optimization, not a second code path.
func (x *run) updateForkPR() {
	upRef := "upstream/" + x.branch
	if _, ok := gitx.RevParse(x.container, upRef); !ok {
		// No fork/upstream fetched — behave like a plain origin-tracking repo.
		x.updateTracking("origin/" + x.branch)
		return
	}
	ahead, behind, _ := gitx.AheadBehind(x.container, upRef)
	switch {
	case ahead == 0 && behind == 0:
		x.add("%s up to date with %s", x.branch, upRef)
		x.ok()
	case ahead == 0 && behind > 0:
		if x.opts.DryRun {
			x.add("would fast-forward %s +%d to %s", x.branch, behind, upRef)
			x.updated(fmt.Sprintf("+%d (dry-run)", behind))
		} else {
			if err := gitx.FastForwardCurrent(x.container, upRef); err != nil {
				x.applyRewrite(upRef)
				return
			}
			x.add("fast-forwarded %s +%d to %s", x.branch, behind, upRef)
			x.updated(fmt.Sprintf("+%d", behind))
		}
	case ahead > 0 && behind == 0:
		// Local commits not yet upstream — your work to PR; never clobbered.
		x.attention(fmt.Sprintf("%d to PR", ahead))
		x.add("%d local commit(s) on %s ahead of upstream — open a PR", ahead, x.branch)
	default:
		x.applyRewrite(upRef) // diverged from upstream
		return
	}
	x.pushFork()
}

// pushFork fast-forward-pushes the important branch to the fork (origin). A fork
// that has diverged (commits the local branch lacks) is left untouched and
// surfaced — never force-pushed. DESIGN §5.1.
func (x *run) pushFork() {
	if x.res.Err != nil {
		return
	}
	forkRef := "origin/" + x.branch
	if _, ok := gitx.RevParse(x.container, forkRef); ok {
		ahead, behind, _ := gitx.AheadBehind(x.container, forkRef)
		switch {
		case ahead == 0 && behind == 0:
			return // fork already matches local
		case ahead == 0 && behind > 0:
			x.add("fork is %d ahead on %s — left as is", behind, x.branch)
			return
		case ahead > 0 && behind > 0:
			x.attention("fork diverged — push skipped")
			x.add("fork and local diverged on %s (+%d/-%d): not force-pushing", x.branch, ahead, behind)
			return
		}
	}
	if x.opts.DryRun {
		x.add("would push %s to fork", x.branch)
		return
	}
	if err := gitx.Push(x.container, "origin", x.branch); err != nil {
		x.attention("fork push failed")
		x.add("push %s to fork failed: %v", x.branch, err)
		return
	}
	x.add("pushed %s to fork", x.branch)
	if x.res.Outcome == UpToDate {
		x.updated("pushed fork")
	}
}

// updateVendor reconciles a vendored, read-only repo to its pin: a branch
// (fast-forward), an explicit tag (checkout), or latest-tag (re-resolve the
// highest semver each run). Never pushed. DESIGN §5.1.
func (x *run) updateVendor() {
	pin := x.r.Pin
	if pin == "" {
		pin = x.branch // default: track the first important branch
	}

	if pin == "latest-tag" {
		target := highestSemver(mustTags(x.container))
		if target == "" {
			x.attention("no tags to pin")
			x.add("pin=latest-tag but the repo has no semver tags")
			return
		}
		x.vendorCheckoutTag(target)
		return
	}

	if _, ok := gitx.RevParse(x.container, "refs/tags/"+pin); ok {
		x.vendorCheckoutTag(pin)
		return
	}

	// A branch pin: fast-forward to origin/<pin> (never pushed).
	originRef := "origin/" + pin
	if _, ok := gitx.RevParse(x.container, originRef); !ok {
		x.attention("pin " + pin + " not found")
		x.add("pin %q is neither a tag nor an origin branch", pin)
		return
	}
	if x.onVendorBranch(pin) {
		x.updateTracking(originRef)
	}
}

// onVendorBranch ensures the pinned branch is checked out, creating a local
// tracking branch on first checkout. Returns false (having recorded intent or a
// failure) when the caller must not proceed.
func (x *run) onVendorBranch(branch string) bool {
	if cur, _ := gitx.CurrentBranch(x.container); cur == branch {
		return true
	}
	if x.opts.DryRun {
		x.add("would checkout branch %s", branch)
		return false
	}
	if err := gitx.Checkout(x.container, branch); err != nil {
		x.fail(err)
		return false
	}
	x.add("checked out branch %s", branch)
	return true
}

// vendorCheckoutTag pins the working tree to a tag, treating a moved tag as a
// rewrite (honoring on_rewrite) and reporting an ordinary version bump.
func (x *run) vendorCheckoutTag(tag string) {
	tagCommit, ok := gitx.RevParse(x.container, "refs/tags/"+tag+"^{commit}")
	if !ok {
		x.attention("tag " + tag + " missing")
		x.add("pinned tag %s not present after fetch", tag)
		return
	}

	// A moved tag (local tag object differs from the remote's) is a rewrite:
	// plain fetch never overwrites an existing tag, so a divergence here means
	// upstream force-moved it.
	if localTag, ok := gitx.RevParse(x.container, "refs/tags/"+tag); ok {
		if remoteTag, ok := gitx.RemoteTagSHA(x.container, "origin", tag); ok && remoteTag != localTag {
			if x.r.OnRewrite != "follow" {
				x.attention(fmt.Sprintf("tag %s moved upstream — stopped", tag))
				x.add("tag %s content moved (on_rewrite=stop): staying at the reviewed tag", tag)
				return
			}
			if x.opts.DryRun {
				x.add("would follow moved tag %s", tag)
			} else if err := gitx.ForceFetchTag(x.container, "origin", tag); err != nil {
				x.fail(err)
				return
			} else {
				x.add("followed moved tag %s", tag)
				tagCommit, _ = gitx.RevParse(x.container, "refs/tags/"+tag+"^{commit}")
			}
		}
	}

	if head, _ := gitx.RevParse(x.container, "HEAD"); head == tagCommit {
		x.add("pinned at %s", tag)
		x.ok()
		return
	}
	prev := gitx.TagAtHead(x.container)
	if x.opts.DryRun {
		x.add("would checkout %s", tag)
		x.updated(vendorBump(prev, tag))
		return
	}
	if err := gitx.Checkout(x.container, "refs/tags/"+tag); err != nil {
		x.fail(err)
		return
	}
	x.add("checked out %s", tag)
	x.updated(vendorBump(prev, tag))
}

func vendorBump(prev, tag string) string {
	if prev != "" && prev != tag {
		return prev + " → " + tag
	}
	return tag
}

func mustTags(dir string) []string {
	t, _ := gitx.Tags(dir)
	return t
}

// highestSemver returns the highest vX.Y.Z(+build) tag, ignoring pre-releases,
// or "" when none parse.
func highestSemver(tags []string) string {
	best := ""
	var bestV [3]int
	for _, t := range tags {
		v, ok := parseSemver(t)
		if !ok {
			continue
		}
		if best == "" || less(bestV, v) {
			best, bestV = t, v
		}
	}
	return best
}

func parseSemver(t string) ([3]int, bool) {
	s := strings.TrimPrefix(t, "v")
	if strings.ContainsRune(s, '-') {
		return [3]int{}, false // skip pre-releases for latest-tag
	}
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) == 0 || len(parts) > 3 {
		return [3]int{}, false
	}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
