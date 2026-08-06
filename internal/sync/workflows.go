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
	upRef := "upstream/" + x.ub
	if _, ok := gitx.RevParse(x.dir, upRef); !ok {
		// No fork/upstream fetched — behave like a plain origin-tracking repo.
		x.updateTracking("origin/" + x.ub)
		return
	}
	ahead, behind := aheadBehind(x.dir, x.ub, upRef)
	switch {
	case ahead == 0 && behind == 0:
		x.add("%s up to date with %s", x.ub, upRef)
		x.ok()
	case ahead == 0 && behind > 0:
		if !x.fastForwardTo(upRef, behind) {
			return // rewrite path took over; the fork push has nothing clean to send
		}
	case ahead > 0 && behind == 0:
		// Local commits not yet upstream — your work to PR; never clobbered.
		x.branchMark(x.ub, Attention, fmt.Sprintf("%d to PR", ahead))
		x.add("%d local commit(s) on %s ahead of upstream — open a PR", ahead, x.ub)
	default:
		x.applyRewrite(upRef) // diverged from upstream
		return
	}
	x.pushFork()
}

// pushFork advances the fork (origin) to match the important branch, per
// `push` (DESIGN §3.6, default `auto` for fork-pr). A fork that has diverged
// (commits the local branch lacks) is force-pushed only when `force_push`
// matches (DESIGN §5.2); otherwise it is left untouched and surfaced.
func (x *run) pushFork() {
	if x.res.Err != nil {
		return
	}
	forkRef := "origin/" + x.ub
	if _, ok := gitx.RevParse(x.dir, forkRef); ok {
		ahead, behind := aheadBehind(x.dir, x.ub, forkRef)
		switch {
		case ahead == 0 && behind == 0:
			return // fork already matches local
		case ahead == 0 && behind > 0:
			x.add("fork is %d ahead on %s — left as is", behind, x.ub)
			return
		case ahead > 0 && behind > 0:
			if !matchesAny(x.r.ForcePush, x.ub) {
				x.branchMark(x.ub, Attention, "fork diverged — push skipped")
				x.add("fork and local diverged on %s (+%d/-%d): not force-pushing (no force_push match)", x.ub, ahead, behind)
				return
			}
			x.forcePush(ahead, behind, "origin", "fork")
			return
		case ahead > 0 && behind == 0:
			x.pushOrReport(ahead, "origin")
		}
		return
	}
	// No commits on the fork yet — a brand-new branch there.
	switch x.r.Push {
	case "auto":
		if x.opts.DryRun {
			x.add("would push %s to fork", x.ub)
			return
		}
		if err := gitx.Push(x.dir, "origin", x.ub); err != nil {
			x.branchMark(x.ub, Attention, "fork push failed")
			x.add("push %s to fork failed: %v", x.ub, err)
			return
		}
		x.add("pushed %s to fork", x.ub)
		x.branchMark(x.ub, Updated, "pushed fork")
	default:
		x.branchMark(x.ub, Attention, "not yet on fork")
		x.add("%s not yet pushed to fork (push=%s)", x.ub, x.r.Push)
	}
}

// forcePush force-pushes a branch already known to diverge (ahead and behind
// both > 0) to remote — only ever called for a branch matched by `force_push`
// (DESIGN §5.2). label is the human-readable name of remote for messages
// ("fork", "origin").
func (x *run) forcePush(ahead, behind int, remote, label string) {
	if x.opts.DryRun {
		x.add("would force-push %s to %s (+%d/-%d)", x.ub, label, ahead, behind)
		x.branchMark(x.ub, Updated, fmt.Sprintf("would force-push (+%d/-%d)", ahead, behind))
		return
	}
	if err := gitx.ForcePush(x.dir, remote, x.ub); err != nil {
		x.branchMark(x.ub, Attention, "force-push failed")
		x.add("force-push %s to %s failed: %v", x.ub, label, err)
		return
	}
	x.add("force-pushed %s to %s (+%d/-%d)", x.ub, label, ahead, behind)
	x.branchMark(x.ub, Updated, fmt.Sprintf("force-pushed (+%d/-%d)", ahead, behind))
}

// updateVendor reconciles a vendored, read-only repo to its pin: a branch
// (fast-forward), an explicit tag (checkout), or latest-tag (re-resolve the
// highest semver each run). Never pushed. DESIGN §5.1.
func (x *run) updateVendor() {
	pin := x.r.Pin
	if pin == "" {
		pin = x.ub // default: track the first important branch
	}

	if pin == "latest-tag" {
		target := highestSemver(mustTags(x.dir))
		if target == "" {
			x.branchMark(x.ub, Attention, "no tags to pin")
			x.add("pin=latest-tag but the repo has no semver tags")
			return
		}
		x.vendorCheckoutTag(target)
		return
	}

	if _, ok := gitx.RevParse(x.dir, "refs/tags/"+pin); ok {
		x.vendorCheckoutTag(pin)
		return
	}

	// A branch pin: fast-forward to origin/<pin> (never pushed). The pin is the
	// branch this unit reconciles, which need not be branches[0] — everything
	// downstream (ahead/behind, the fast-forward, the report) is about the
	// pinned branch, not the nominal first important one.
	x.ub = pin
	originRef := "origin/" + pin
	if _, ok := gitx.RevParse(x.dir, originRef); !ok {
		x.branchMark(x.ub, Attention, "pin "+pin+" not found")
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
//
// This is the one place a checkout is a genuine precondition rather than an
// artifact of how the update is implemented (DESIGN §5.1): a vendored repo
// exists to have its *files* on disk at the pinned version — something is
// reading them — so leaving the working tree on another branch while quietly
// moving the ref would defeat the point. Everywhere else, sync moves whatever
// branch needs moving and leaves your checkout alone.
func (x *run) onVendorBranch(branch string) bool {
	if cur, _ := gitx.CurrentBranch(x.dir); cur == branch {
		return true
	}
	if x.opts.DryRun {
		x.add("would checkout branch %s", branch)
		return false
	}
	if err := gitx.Checkout(x.dir, branch); err != nil {
		x.fail(err)
		return false
	}
	x.add("checked out branch %s", branch)
	return true
}

// vendorCheckoutTag pins the working tree to a tag, treating a moved tag as a
// rewrite (matched against `force_pull`, DESIGN §5.2) and reporting an
// ordinary version bump otherwise.
func (x *run) vendorCheckoutTag(tag string) {
	tagCommit, ok := gitx.RevParse(x.dir, "refs/tags/"+tag+"^{commit}")
	if !ok {
		x.branchMark(x.ub, Attention, "tag "+tag+" missing")
		x.add("pinned tag %s not present after fetch", tag)
		return
	}

	// A moved tag (local tag object differs from the remote's) is a rewrite:
	// plain fetch never overwrites an existing tag, so a divergence here means
	// upstream force-moved it.
	if localTag, ok := gitx.RevParse(x.dir, "refs/tags/"+tag); ok {
		if remoteTag, ok := gitx.RemoteTagSHA(x.dir, "origin", tag); ok && remoteTag != localTag {
			if !matchesAny(x.r.ForcePull, tag) {
				x.branchMark(x.ub, Attention, fmt.Sprintf("tag %s moved upstream — stopped", tag))
				x.add("tag %s content moved (no force_pull match): staying at the reviewed tag", tag)
				return
			}
			if x.opts.DryRun {
				x.add("would follow moved tag %s", tag)
			} else if err := gitx.ForceFetchTag(x.dir, "origin", tag); err != nil {
				x.fail(err)
				return
			} else {
				x.add("followed moved tag %s", tag)
				tagCommit, _ = gitx.RevParse(x.dir, "refs/tags/"+tag+"^{commit}")
			}
		}
	}

	if head, _ := gitx.RevParse(x.dir, "HEAD"); head == tagCommit {
		x.add("pinned at %s", tag)
		x.ok()
		return
	}
	prev := gitx.TagAtHead(x.dir)
	if x.opts.DryRun {
		x.add("would checkout %s", tag)
		x.branchMark(x.ub, Updated, vendorBump(prev, tag))
		return
	}
	if err := gitx.Checkout(x.dir, "refs/tags/"+tag); err != nil {
		x.fail(err)
		return
	}
	x.add("checked out %s", tag)
	x.branchMark(x.ub, Updated, vendorBump(prev, tag))
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
