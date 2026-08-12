package sync

import (
	"errors"
	"fmt"

	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// Verdict is one task branch's standing against its repo's primary important
// branch: whether its work has landed, and whether the branch could therefore be
// removed (DESIGN §5.3). Both `sync`'s observations and `repo prune` read the
// same verdict, so the line you see during a sweep is the decision prune would
// act on — not a second, similar-looking calculation that might disagree.
type Verdict struct {
	Name     string
	State    gitx.MergeState
	Ahead    int    // commits the important branch lacks (0 once merged)
	Worktree string // the tree holding this branch, "" when none does
	Prunable bool
	Blocker  string // why not, when !Prunable
	// Unknown marks a branch git could not answer for at all, as distinct from
	// one answered "unmerged". Never prunable, and reported as a finding rather
	// than an observation: not knowing is something wrong, not something parked.
	Unknown bool
}

// Summary is the verdict as one report line.
func (v Verdict) Summary(base string) string {
	if v.Unknown {
		return v.Blocker // "N ahead of base" would be a number we don't have
	}
	if !v.State.Merged() {
		s := fmt.Sprintf("%d ahead of %s", v.Ahead, base)
		if v.Blocker != "" && v.Blocker != "unmerged" {
			s += ", " + v.Blocker
		}
		return s
	}
	if v.Prunable {
		return v.State.String() + " — prunable"
	}
	return fmt.Sprintf("%s — %s", v.State, v.Blocker)
}

// Classify returns a verdict for every task branch of a repo — every local
// branch that is not one of its important branches. Important branches are
// excluded because they are the *reference*: a branch cannot have landed
// relative to itself, and they are not candidates for removal in any case.
//
// A branch is prunable when its work has landed by any tier and nothing else
// stands in the way. Being checked out is a blocker rather than a merge
// question: git will not delete a branch some worktree is on, and neither
// should anything here.
func Classify(container string, r model.Repo) ([]Verdict, error) {
	base := branch0(r)
	if base == "" {
		return nil, fmt.Errorf("no important branch to measure against")
	}
	// A base that doesn't resolve is one fact about the repo, not one per
	// branch: every tier measures against it, so without it the answer is the
	// same for all of them. Established once and reported once, because the
	// alternative — letting each branch fail its own rev-list — repeats git's
	// "ambiguous argument" for every branch in the repo and names the missing
	// branch nowhere. Why it's missing is config's business (a stated
	// `branches` outranks what the clone says, §3.6), so this reports what is
	// so rather than guessing which end is wrong.
	if _, ok := gitx.RevParse(container, base); !ok {
		return nil, fmt.Errorf("no local branch %q to measure against", base)
	}
	important := map[string]bool{}
	for _, b := range r.Branches {
		important[b] = true
	}
	branches, err := gitx.LocalBranches(container)
	if err != nil {
		return nil, err
	}

	var out []Verdict
	for _, b := range branches {
		if important[b] {
			continue
		}
		state, err := gitx.MergedState(container, b, base, scanLimitOf(r))
		if err != nil {
			// "Declined to look" and "tried and failed" are both unknown, and
			// both unprunable, but only one of them is a fault — so the reason
			// travels with the verdict rather than being flattened into one
			// message someone would have to go and interpret.
			why := "can't classify: " + err.Error()
			if errors.Is(err, gitx.ErrTooFarDiverged) {
				why = err.Error()
			}
			// A branch whose standing can't be determined is never prunable —
			// nothing here can claim its work landed — but it still has to
			// appear. Dropping it made a broken classifier indistinguishable
			// from a repo with nothing to say, which is how a squash tier that
			// failed on every branch (see gitx.runAsProbe) emptied the report
			// instead of filling it with errors.
			out = append(out, Verdict{
				Name:     b,
				Unknown:  true,
				Worktree: gitx.WorktreeFor(container, b),
				Blocker:  why,
			})
			continue
		}
		ahead, _ := aheadBehind(container, b, base)
		v := Verdict{Name: b, State: state, Ahead: ahead, Worktree: gitx.WorktreeFor(container, b)}
		switch {
		case !state.Merged():
			v.Blocker = "unmerged"
		case v.Worktree != "":
			v.Blocker = "checked out"
		default:
			v.Prunable = true
		}
		out = append(out, v)
	}
	return out, nil
}

// scanLimitOf resolves how far merge detection may scan for this repo: what
// config states, else the ambient default. Unset is deliberately not "off" —
// see model.Repo.MergeScanLimit.
func scanLimitOf(r model.Repo) int {
	if r.MergeScanLimit != nil {
		return *r.MergeScanLimit
	}
	return gitx.DefaultScanLimit()
}

// NeedsForceDelete reports whether removing this branch requires `git branch -D`
// rather than `-d`. Git's own `-d` check is an ancestry test, so it refuses the
// patch- and squash-merged tiers even though they have fully landed — those are
// exactly the branches whose removal a human should confirm (DESIGN §5.3).
func NeedsForceDelete(v Verdict) bool {
	return v.State != gitx.MergedAncestor
}
