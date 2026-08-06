package sync

import (
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
}

// Summary is the verdict as one report line.
func (v Verdict) Summary(base string) string {
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
		state, err := gitx.MergedState(container, b, base)
		if err != nil {
			continue // a branch we can't classify is one we never act on
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

// NeedsForceDelete reports whether removing this branch requires `git branch -D`
// rather than `-d`. Git's own `-d` check is an ancestry test, so it refuses the
// patch- and squash-merged tiers even though they have fully landed — those are
// exactly the branches whose removal a human should confirm (DESIGN §5.3).
func NeedsForceDelete(v Verdict) bool {
	return v.State != gitx.MergedAncestor
}
