package sync

import (
	"fmt"

	"github.com/michael-odell/repo/internal/gitx"
)

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

	important := map[string]bool{}
	for _, b := range x.r.Branches {
		important[b] = true
	}
	branches, err := gitx.LocalBranches(x.container)
	if err != nil {
		return
	}
	for _, b := range branches {
		if x.hasNote(b) {
			continue
		}
		if important[b] {
			if x.r.ShowBranches == showAll {
				x.branchMark(b, Info, "up to date")
			}
			continue
		}
		ahead, _ := aheadBehind(x.container, b, x.branch)
		switch {
		case ahead > 0:
			x.branchMark(b, Info, fmt.Sprintf("%d ahead of %s", ahead, x.branch))
		case x.r.ShowBranches == showAll:
			x.branchMark(b, Info, "merged")
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
