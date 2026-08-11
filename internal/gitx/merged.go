package gitx

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ErrTooFarDiverged reports that the patch-id tiers were declined rather than
// attempted, because the branch and its base are further apart than the caller's
// scan limit allows. It is not a failure: the branch's standing is simply
// unestablished, so callers must treat it as unknown — never as unmerged, and
// never as safe to remove.
var ErrTooFarDiverged = errors.New("too far diverged to classify")

const (
	defaultScanLimit = 1000
	scanLimitEnv     = "REPO_MERGE_SCAN_LIMIT"

	// ScanUnlimited runs the patch tiers however far apart the two refs are.
	ScanUnlimited = -1
	// ScanNever skips them entirely, leaving ancestry (which is cheap and exact)
	// as the only evidence considered.
	ScanNever = 0
)

// DefaultScanLimit is the limit for a repo whose config says nothing: the
// environment's value if it sets one, else 1000 commits. Generous enough that
// ordinary parked work is always classified, small enough that one abandoned
// branch on an old repo can't stall a sweep. Config states it per repo (DESIGN
// §5.3); this is only the floor beneath that.
func DefaultScanLimit() int {
	if v := os.Getenv(scanLimitEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= ScanUnlimited {
			return n
		}
	}
	return defaultScanLimit
}

// MergeState classifies how (or whether) a branch's work has landed on a base
// branch. `git branch --merged` answers only the first of these, which is why
// prune is tiered (DESIGN §5.3): a squash- or rebase-merged branch is fully
// landed and still looks like unfinished work to an ancestry test.
type MergeState int

// The states name the *evidence* that the work landed, never the merge style
// that produced it — the two don't map one-to-one, and only the evidence is
// something the object graph can actually be asked about. Squashing a
// single-commit branch yields a commit with that commit's exact patch, so it is
// found by MergedPatch and never reaches the squash tier; nothing distinguishes
// it there from a rebase or a cherry-pick.
const (
	Unmerged       MergeState = iota // carries commits base does not have, in any form
	MergedAncestor                   // base literally contains the branch's commits
	MergedPatch                      // every commit's patch is present in base under some SHA
	MergedSquash                     // base carries the branch's whole diff as a single patch
)

// Merged reports whether the state is any flavour of landed.
func (m MergeState) Merged() bool { return m != Unmerged }

// String names the state for reports. The two patch tiers deliberately share
// one word: which of them fired says how the *search* went, not how the branch
// was merged, and naming a merge style there would be a guess dressed as a
// finding. What a reader can act on is the split this does draw — "merged" is
// the ancestry case git's own `-d` will agree to, "merged (rewritten)" is the
// content landing under other SHAs, which `-d` refuses (see NeedsForceDelete).
func (m MergeState) String() string {
	switch m {
	case MergedAncestor:
		return "merged"
	case MergedPatch, MergedSquash:
		return "merged (rewritten)"
	default:
		return "unmerged"
	}
}

// MergedState classifies branch against base, trying each tier in turn and
// stopping at the first that confirms — cheapest first, so the common answers
// cost the least and only a genuinely unmerged branch pays for all three.
//
// Tier 3 is the only one that needs a scratch object: `git cherry` compares
// commits, and a squash merge collapsed the branch into one commit whose patch
// matches no individual commit of the branch. Rebuilding the branch as a single
// commit against the merge base gives that collapsed patch something to match.
// The object is unreferenced and gc reclaims it — no ref, config, or working
// tree is touched. It is written with runAsProbe's own identity, so the tier
// works on a machine where git has none to auto-detect.
// scanLimit is how far apart the two refs may be before the patch tiers are
// declined: ScanUnlimited to always run them, ScanNever to never, or a commit
// count. It is a parameter rather than a package-level setting because the
// answer is per repo — the monorepo that needs it raised and the archive that
// needs it off are usually in the same sweep.
func MergedState(dir, branch, base string, scanLimit int) (MergeState, error) {
	// Tier 1: ancestry. Also the answer for a branch with nothing of its own.
	ahead, behind, err := AheadBehindRefs(dir, base, branch)
	if err != nil {
		return Unmerged, err
	}
	if ahead == 0 {
		return MergedAncestor, nil
	}

	// Tiers 2 and 3 are patch-id comparisons: `git cherry` generates and hashes
	// the diff of every commit on *both* sides of the divergence, so their cost
	// scales with how far apart the two have drifted, not with the branch's own
	// size. A branch forked from a busy trunk years ago can therefore cost
	// thousands of diffs — several of those in parallel is enough to exhaust a
	// laptop's memory, and the answer was never going to be interesting. Decline
	// instead, and say so: an unanswered branch is reported and never pruned,
	// which is the safe direction (DESIGN §5.3).
	if scanLimit == ScanNever {
		return Unmerged, fmt.Errorf("%w: patch tiers off for this repo (merge_scan_limit)", ErrTooFarDiverged)
	}
	if scanLimit != ScanUnlimited && ahead+behind > scanLimit {
		return Unmerged, fmt.Errorf("%w: %d commits apart, limit %d (raise merge_scan_limit or %s)",
			ErrTooFarDiverged, ahead+behind, scanLimit, scanLimitEnv)
	}

	// Tier 2: per-commit patch-ids. Catches a replay onto a moved base, where
	// every commit was rewritten but its diff survived intact. A merge commit has
	// no single patch to compare, so `git cherry` passes over it in silence —
	// which makes "every commit landed" a claim about only the commits it looked
	// at, and the merges have to be accounted for separately.
	if unmatched, err := cherryUnmatched(dir, base, branch); err == nil && unmatched == 0 {
		if held, err := mergesHoldOwnWork(dir, base, branch); err == nil && !held {
			return MergedPatch, nil
		}
	}

	// Tier 3: the branch's whole diff as one patch-id.
	mergeBase, err := run(dir, "merge-base", base, branch)
	if err != nil || mergeBase == "" {
		return Unmerged, err
	}
	tree, err := run(dir, "rev-parse", branch+"^{tree}")
	if err != nil {
		return Unmerged, err
	}
	synthetic, err := runAsProbe(dir, "commit-tree", tree, "-p", mergeBase, "-m", "repo: squash-merge probe")
	if err != nil {
		return Unmerged, err
	}
	if unmatched, err := cherryUnmatched(dir, base, synthetic); err == nil && unmatched == 0 {
		return MergedSquash, nil
	}
	return Unmerged, nil
}

// cherryUnmatched counts the commits of head whose patch is not already present
// in base — `git cherry`'s "+" lines. Zero means everything head carries has
// landed, whatever the SHAs say.
func cherryUnmatched(dir, base, head string) (int, error) {
	out, err := run(dir, "cherry", base, head)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, line := range splitLines(out) {
		if strings.HasPrefix(line, "+") {
			n++
		}
	}
	return n, nil
}

// mergesHoldOwnWork reports whether any merge commit the branch is ahead by
// carries content of its own — a conflict resolved by hand, or an edit made
// while merging. Such a merge is the one place a branch can hold work that no
// commit anywhere states as a patch, so tier 2 is blind to it: `git cherry`
// compares patches, a merge has none, and the branch reads as fully landed while
// the resolution exists only on it. Reporting the whole branch unmerged is the
// safe reading — the tier below still gets its say, and a branch nobody can
// vouch for is never pruned (DESIGN §5.3).
//
// The combined diff draws exactly the needed line: empty for a merge that only
// joins what its parents already say, non-empty for one that says something more.
func mergesHoldOwnWork(dir, base, branch string) (bool, error) {
	merges, err := run(dir, "rev-list", "--merges", base+".."+branch)
	if err != nil {
		return false, err
	}
	for _, sha := range splitLines(merges) {
		// --cc leads with the commit's own sha and adds a diff only when the
		// merge is more than the sum of its parents.
		out, err := run(dir, "diff-tree", "--cc", sha)
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(strings.TrimPrefix(out, sha)) != "" {
			return true, nil
		}
	}
	return false, nil
}

// DeleteBranch removes a local branch. force selects `-D` over `-d`, which is
// required for the patch- and squash-merged tiers: git's own `-d` safety check
// is an ancestry test, so it refuses exactly the branches tiers 2 and 3 exist to
// identify. Callers use `-d` where they can, so git double-checks them, and pay
// for `-D` only where its check cannot see the merge (DESIGN §5.3).
func DeleteBranch(dir, branch string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}
	_, err := run(dir, "branch", flag, branch)
	return err
}
