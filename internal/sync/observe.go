package sync

// show_branches values (DESIGN §5.6), ordered by how much of the branch
// inventory each enumerates: none ⊂ notable ⊂ unmerged ⊂ all.
const (
	showNone     = "none"     // no branch lines at all; the repo row is the report
	showNotable  = "notable"  // only branches with a finding this run
	showUnmerged = "unmerged" // …plus task branches carrying unlanded work
	showAll      = "all"      // …plus every other branch, important ones included
)

// observe adds the informational branch lines the repo's `show_branches` asks
// for (DESIGN §5.6). These are observations, not findings: they are recorded at
// Info, which ranks below UpToDate, so they can never change the repo's own
// outcome or glyph. A repo carrying parked work still reads ✓.
//
// The measurement is against the repo's primary important branch, not against
// each branch's own remote — "work you haven't landed" is a statement about the
// mainline, and it is what makes the important branches the natural exclusion
// list rather than a special case: a branch cannot be unmerged relative to
// itself.
//
// Runs after taskBranches so a branch that already produced a finding keeps it;
// an observation never overwrites something that needed attention.
func (x *run) observe() {
	if x.res.Err != nil {
		return
	}
	switch x.r.ShowBranches {
	case showUnmerged, showAll:
	default:
		return
	}
	if x.branch == "" {
		return // no reference branch to measure against
	}

	verdicts, err := Classify(x.container, x.r)
	if err != nil {
		return
	}
	for _, v := range verdicts {
		if x.hasNote(v.Name) {
			continue
		}
		// A branch git couldn't answer for is a finding, not an observation:
		// "I don't know" is a state someone has to look at, so it lifts the
		// repo's glyph rather than sitting quietly among the parked work.
		// Rarely reached here — a repo damaged enough to defeat classification
		// usually fails at the fetch first, and the run ends there — but `repo
		// prune` classifies without fetching, and this keeps the two agreeing
		// about what an unanswerable branch means.
		if v.Unknown {
			x.branchMark(v.Name, Attention, v.Summary(x.branch))
			continue
		}
		// `unmerged` shows only outstanding work. `all` additionally shows the
		// landed branches with the verdict prune would act on, so the decision
		// can be watched during ordinary sweeps rather than only when someone
		// remembers to go looking for it.
		if !v.State.Merged() || x.r.ShowBranches == showAll {
			x.branchMark(v.Name, Info, v.Summary(x.branch))
		}
	}
	if x.r.ShowBranches == showAll {
		for _, b := range x.r.Branches {
			if !x.hasNote(b) {
				x.branchMark(b, Info, "up to date")
			}
		}
	}
}

// hasNote reports whether a branch already carries a finding from this run.
func (x *run) hasNote(name string) bool {
	for _, b := range x.res.Branches {
		if b.Name == name {
			return true
		}
	}
	return false
}
