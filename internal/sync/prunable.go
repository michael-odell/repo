package sync

import (
	"errors"
	"fmt"
	"time"

	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// Verdict is one task branch's standing against its repo's primary important
// branch: whether its work has landed, and whether the branch could therefore be
// removed (DESIGN §5.3). Both `sync`'s observations and `repo prune` read the
// same verdict, so the line you see during a sweep is the decision prune would
// act on — not a second, similar-looking calculation that might disagree.
type Verdict struct {
	Name string
	// SHA is what the ref holds, carried out of classification rather than
	// re-read at deletion time: it is the whole content of the journal's
	// restore line, and after the branch is gone there is nothing left to ask.
	SHA      string
	Updated  time.Time // when the ref last moved (tip date or reflog, later wins)
	State    gitx.MergeState
	Ahead    int    // commits the important branch lacks (0 once merged)
	Worktree string // the tree holding this branch, "" when none does
	Prunable bool
	Blocker  string // why not, when !Prunable
	// Evidence is how the tiers reached this verdict, recorded by the pass that
	// decided rather than reconstructed afterwards (DESIGN §5.3). Carried on
	// every verdict because collecting it costs nothing extra — the tiers ran
	// anyway — and because an explanation computed later could disagree with
	// the decision it claims to explain.
	Evidence gitx.Evidence
	// Corroboration is what the -D cross-check found, when it was asked. Empty
	// for a branch nothing needed to corroborate — an ancestry-tier merge, or
	// one a cheaper blocker already settled (DESIGN §5.3).
	Corroboration gitx.Corroboration
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
//
// cor corroborates the rewritten tiers before their branches are called
// prunable, so the label means what it says (DESIGN §5.3). A nil cor skips
// that, leaving the tiers' word alone — see Corroborator.
func Classify(container string, r model.Repo, cor Corroborator) ([]Verdict, error) {
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
	baseSHA, ok := gitx.RevParse(container, base)
	if !ok {
		return nil, fmt.Errorf("no local branch %q to measure against", base)
	}
	important := map[string]bool{}
	for _, b := range r.Branches {
		important[b] = true
	}
	branches, err := gitx.LocalBranchRefs(container)
	if err != nil {
		return nil, err
	}

	var out []Verdict
	for _, ref := range branches {
		b := ref.Name
		if important[b] {
			continue
		}
		var ev gitx.Evidence
		state, err := gitx.MergedStateEvidence(container, b, base, scanLimitOf(r), &ev)
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
				SHA:      ref.SHA,
				Updated:  ref.Updated,
				Evidence: ev,
				Unknown:  true,
				Worktree: gitx.WorktreeFor(container, b),
				Blocker:  why,
			})
			continue
		}
		ahead, _ := aheadBehind(container, b, base)
		v := Verdict{
			Name:     b,
			SHA:      ref.SHA,
			Updated:  ref.Updated,
			Evidence: ev,
			State:    state,
			Ahead:    ahead,
			Worktree: gitx.WorktreeFor(container, b),
		}
		// Ordered by who is refusing. Git's own answers come first — the work
		// isn't landed, or a tree is standing on the branch — and only then the
		// two settings, which don't dispute the merge question at all: they say
		// the branch stays regardless of it. Reporting "kept (prune_keep)" for
		// a branch that is also unmerged would name the weaker reason and
		// suggest the setting is what stands between you and losing the work.
		switch {
		case !state.Merged():
			v.Blocker = "unmerged"
		case v.Worktree != "":
			v.Blocker = "checked out"
		case matchesAny(r.PruneKeep, b):
			v.Blocker = "kept (prune_keep)"
		case tooYoung(v, r.PruneMinAge):
			v.Blocker = fmt.Sprintf("moved %s ago (prune_min_age %s)",
				roughAge(time.Since(v.Updated)), roughAge(r.PruneMinAge))
		default:
			// Corroboration is asked last, and it is the only question here
			// that costs anything. Every blocker above keeps the branch
			// whatever corroboration would say, so asking earlier would buy
			// nothing; and it applies only where removal needs -D, since git's
			// own -d already agrees at the ancestry tier (DESIGN §5.3).
			if cor != nil && NeedsForceDelete(v) && wantsCorroboration(r, cor) {
				v.Corroboration = cor.Corroborate(
					container, b, base, ref.SHA, baseSHA, corroborateBudgetOf(r))
				if !v.Corroboration.OK {
					v.Blocker = "could not corroborate"
					break
				}
			}
			v.Prunable = true
		}
		out = append(out, v)
	}
	return out, nil
}

// tooYoung reports whether a ref has moved too recently to be removed.
//
// A zero Updated means nothing could be established about when the branch last
// moved, and an unknown age never satisfies a minimum: the setting exists to
// hold back branches that might still be in use, and "I couldn't tell" is not
// evidence that one isn't.
func tooYoung(v Verdict, min time.Duration) bool {
	if min <= 0 {
		return false
	}
	return v.Updated.IsZero() || time.Since(v.Updated) < min
}

// roughAge renders a duration at the resolution the setting is written in.
// "moved 3d ago (prune_min_age 14d)" is the sentence someone can act on;
// "moved 78h13m4s ago" makes them do arithmetic to reach the same place.
func roughAge(d time.Duration) string {
	switch {
	case d >= 14*24*time.Hour:
		return fmt.Sprintf("%dw", int(d/(7*24*time.Hour)))
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return "moments"
	}
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
