package gitx

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ErrTooFarDiverged reports that the patch-id tiers were declined rather than
// attempted, because the branch and its base are further apart than
// scanLimitEnv allows. It is not a failure: the branch's standing is simply
// unestablished, so callers must treat it as unknown — never as unmerged, and
// never as safe to remove.
var ErrTooFarDiverged = errors.New("too far diverged to classify")

const (
	defaultScanLimit = 1000
	scanLimitEnv     = "REPO_MERGE_SCAN_LIMIT"
)

// scanLimit is the largest divergence, in commits on both sides combined, that
// the patch-id tiers will walk. Generous enough that ordinary parked work is
// always classified; small enough that one abandoned branch on an old repo
// can't stall a sweep.
func scanLimit() int {
	if v := os.Getenv(scanLimitEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
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

// The states name the *evidence* that the work landed, not the merge style that
// produced it — the two don't map one-to-one. Squashing a single-commit branch
// yields a commit with that commit's exact patch, so it is found by MergedPatch
// and never reaches the squash tier. That isn't a misdetection: the two
// operations are indistinguishable on the object graph, and the prune decision
// is identical either way.
const (
	Unmerged       MergeState = iota // carries commits base does not have, in any form
	MergedAncestor                   // base literally contains the branch's commits
	MergedPatch                      // every commit's patch is present in base under some SHA
	MergedSquash                     // base carries the branch's whole diff as a single patch
)

// Merged reports whether the state is any flavour of landed.
func (m MergeState) Merged() bool { return m != Unmerged }

// String names the state for reports.
func (m MergeState) String() string {
	switch m {
	case MergedAncestor:
		return "merged"
	case MergedPatch:
		return "merged (same patches)"
	case MergedSquash:
		return "merged (squashed)"
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
func MergedState(dir, branch, base string) (MergeState, error) {
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
	if limit := scanLimit(); ahead+behind > limit {
		return Unmerged, fmt.Errorf("%w: %d commits apart, limit %d (raise %s)",
			ErrTooFarDiverged, ahead+behind, limit, scanLimitEnv)
	}

	// Tier 2: per-commit patch-ids. Catches a replay onto a moved base, where
	// every commit was rewritten but its diff survived intact.
	if unmatched, err := cherryUnmatched(dir, base, branch); err == nil && unmatched == 0 {
		return MergedPatch, nil
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
