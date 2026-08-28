package sync

import (
	"errors"
	"fmt"
	"time"

	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// Verdict is one task branch's standing against its repo's important
// branches: whether its work has landed on any of them, and whether the branch
// could therefore be removed (DESIGN §5.3). Both `sync`'s observations and
// `repo prune` read the same verdict, so the line you see during a sweep is the decision prune would
// act on — not a second, similar-looking calculation that might disagree.
type Verdict struct {
	Name string
	// SHA is what the ref holds, carried out of classification rather than
	// re-read at deletion time: it is the whole content of the journal's
	// restore line, and after the branch is gone there is nothing left to ask.
	SHA     string
	Updated time.Time // when the ref last moved (tip date or reflog, later wins)
	State   gitx.MergeState
	// Base is the important branch this verdict is about: the one that confirmed
	// the merge, or — when none did — the primary, which is what Ahead counts
	// against. Carried on the verdict rather than passed to Summary by the
	// caller, because "merged" in a repo with more than one important branch is
	// unreadable without saying into what, and a caller that had to supply it
	// could supply a different one than the tiers used.
	Base  string
	Ahead int // commits Base lacks (0 once merged)
	// Worktree is the linked tree holding this branch, "" when none does or
	// when the one that does is the container's own. Removing it is part of
	// removing the branch (DESIGN §5.3) — under worktrees = true every task
	// branch lives in one, so a tree that merely blocked made prune a no-op
	// there, permanently rather than until someone checked something else out.
	Worktree string
	// WorktreeIgnored counts the .gitignore'd files that removing it discards.
	// Non-zero is not a blocker but a consent question: §4.1's rule is that
	// ignored-only residue goes when someone says so, and `git worktree remove`
	// discards it without comment, so the asking has to happen here.
	WorktreeIgnored int
	Prunable        bool
	Blocker         string // why not, when !Prunable
	// Evidence is how the tiers reached this verdict, recorded by the pass that
	// decided rather than reconstructed afterwards (DESIGN §5.3). Carried on
	// every verdict because collecting it costs nothing extra — the tiers ran
	// anyway — and because an explanation computed later could disagree with
	// the decision it claims to explain.
	Evidence gitx.Evidence
	// Unknown marks a branch git could not answer for at all, as distinct from
	// one answered "unmerged". Never prunable, and reported as a finding rather
	// than an observation: not knowing is something wrong, not something parked.
	Unknown bool
}

// Summary is the verdict as one report line.
func (v Verdict) Summary() string {
	if v.Unknown {
		return v.Blocker // "N ahead of base" would be a number we don't have
	}
	if !v.State.Merged() {
		s := fmt.Sprintf("%d ahead of %s", v.Ahead, v.Base)
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
// A branch is prunable when its work has landed by any tier — against any
// important branch — and nothing else stands in the way. A worktree holding it
// is not one of those things: removing the tree is part of removing the branch,
// unless it holds work to lose or is the container's own (DESIGN §5.3).
//
// The tiers are the whole merge question — nothing checks their work. A
// content-presence cross-check used to gate the rewritten tiers on the change
// still being in base's current tree; it is gone, because a tier confirming
// means the patch is carried by a commit *reachable from base*, which a later
// revert cannot remove (DESIGN §5.3).
func Classify(container string, r model.Repo) ([]Verdict, error) {
	// Every important branch that actually exists here is a base worth asking
	// against, in config order so the primary answers first where several
	// could. A base that doesn't resolve is one fact about the repo, not one
	// per branch, so it is established once: letting each branch fail its own
	// rev-list repeats git's "ambiguous argument" for every branch in the repo
	// and names the missing one nowhere. Why it's missing is config's business
	// (a stated `branches` outranks what the clone says, §3.6), so this reports
	// what is so rather than guessing which end is wrong.
	important := map[string]bool{}
	var bases []string
	for _, b := range r.Branches {
		important[b] = true
		if _, ok := gitx.RevParse(container, b); ok {
			bases = append(bases, b)
		}
	}
	primary := branch0(r)
	switch {
	case primary == "":
		return nil, fmt.Errorf("no important branch to measure against")
	case len(bases) == 0:
		return nil, fmt.Errorf("no local branch %q to measure against", primary)
	}
	branches, err := gitx.LocalBranchRefs(container)
	if err != nil {
		return nil, err
	}
	trees, primaryTree := gitx.WorktreeIndex(container)

	var out []Verdict
	for _, ref := range branches {
		b := ref.Name
		if important[b] {
			continue
		}
		wt, held := trees[b]
		linked, ignored, treeBlocker := "", 0, ""
		if held {
			switch {
			case wt.Path == primaryTree:
				// The container's own tree. Nothing may remove it, so this
				// branch is blocked until something else is checked out —
				// which is the transient case, unlike a linked tree.
				treeBlocker = "checked out"
			default:
				linked = wt.Path
				dirty, _ := gitx.IsDirty(wt.Path)
				untracked, _ := gitx.UntrackedFiles(wt.Path)
				if dirty || len(untracked) > 0 {
					treeBlocker = "worktree holds uncommitted work"
					linked = ""
					break
				}
				files, _ := gitx.IgnoredFiles(wt.Path)
				ignored = len(files)
			}
		}
		state, base, ev, err := classifyAgainst(container, b, bases, scanLimitOf(r))
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
				Base:     base,
				Evidence: ev,
				Unknown:  true,
				Worktree: linked,
				Blocker:  why,
			})
			continue
		}
		ahead, _ := aheadBehind(container, b, base)
		v := Verdict{
			Name:            b,
			SHA:             ref.SHA,
			Updated:         ref.Updated,
			Evidence:        ev,
			State:           state,
			Base:            base,
			Ahead:           ahead,
			Worktree:        linked,
			WorktreeIgnored: ignored,
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
		case treeBlocker != "":
			v.Blocker = treeBlocker
		case matchesAny(r.PruneKeep, b):
			v.Blocker = "kept (prune_keep)"
		case tooYoung(v, r.PruneMinAge):
			v.Blocker = fmt.Sprintf("moved %s ago (prune_min_age %s)",
				roughAge(time.Since(v.Updated)), roughAge(r.PruneMinAge))
		default:
			v.Prunable = true
		}
		out = append(out, v)
	}
	return out, nil
}

// classifyAgainst runs the tiers against each important branch in turn and
// returns the first confirmation, the base that gave it, and the evidence from
// every base tried.
//
// Landed on *any* important branch is landed: work merged to `release` is merged
// whether or not `main` has seen it, and measuring against `branches[0]` alone
// reported it as outstanding forever — the same false negative the tiers exist
// to eliminate, one level up. The bases are tried in config order, so where
// several could confirm, the primary is the one the report names.
//
// Where nothing confirms, the base returned is the primary: it is what `Ahead`
// is counted against, and one number cannot mean several branches. An error
// from *any* base makes the whole verdict unknown rather than unmerged, because
// a base that declined to answer has not said no — and a branch nothing can
// vouch for is never pruned (DESIGN §5.3).
func classifyAgainst(container, branch string, bases []string, scanLimit int) (gitx.MergeState, string, gitx.Evidence, error) {
	var (
		ev       gitx.Evidence
		firstErr error
		// The divergence the primary saw, kept aside because each later base
		// overwrites it and an unconfirmed verdict is reported against the
		// primary. Without this, `-v`'s header counted one branch's commits and
		// named another's.
		primaryAhead, primaryBehind int
	)
	for i, base := range bases {
		state, err := gitx.MergedStateEvidence(container, branch, base, scanLimit, &ev)
		if i == 0 {
			primaryAhead, primaryBehind = ev.Ahead, ev.Behind
		}
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if state.Merged() {
			return state, base, ev, nil
		}
	}
	ev.Ahead, ev.Behind = primaryAhead, primaryBehind
	return gitx.Unmerged, bases[0], ev, firstErr
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
