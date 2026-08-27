package sync

import "time"

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

	verdicts, err := x.verdicts()
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

// prune modes (DESIGN §5.3), ordered by how much the sweep does unasked:
// manual ⊂ report ⊂ interactive ⊂ auto.
const (
	pruneManual      = "manual"      // don't classify, don't offer
	pruneReport      = "report"      // classify and name what was found
	pruneInteractive = "interactive" // …and walk it with whoever is there
	pruneAuto        = "auto"        // …and remove what clears the unattended bar
)

// verdicts classifies the repo's task branches, once per run.
//
// Two independent things want this answer — `show_branches` deciding what to
// enumerate (§5.6) and `prune` deciding what to do (§5.3) — and the tiers are
// the expensive part of a sweep. Computing it once means they cannot disagree
// about a branch either: the line you read and the deletion that follows come
// from the same call, which is the property §5.3 leans on.
func (x *run) verdicts() ([]Verdict, error) {
	if x.classified == nil && x.classifyErr == nil {
		x.classified, x.classifyErr = Classify(x.container, x.r, x.opts.cor)
		// Corroboration is the one part of classification whose cost is worth
		// naming rather than leaving inside the trace line's gap: it is the
		// only piece bounded by a budget, so it is the piece someone tuning one
		// needs to see (DESIGN §5.3).
		if n, spent := corroborated(x.classified); n > 0 {
			x.add("corroborated %d rewritten-tier branch(es) in %s",
				n, spent.Round(time.Millisecond))
		}
		if x.classified == nil && x.classifyErr == nil {
			// No task branches: remember that, rather than reclassifying each
			// time something asks.
			x.classified = []Verdict{}
		}
	}
	return x.classified, x.classifyErr
}

// countPrunable records how many branches prune would offer to remove, for the
// footer that tells you they are there (DESIGN §5.3).
//
// It runs under every mode but `manual`, which is the one that says "don't ask
// the question". `show_branches` is not consulted: what a sweep *tells* you and
// what prune *would do* are different questions (§5.6), so a repo listing no
// branch lines still contributes its count — the footer is one line either way,
// and suppressing it would hide the feature from exactly the configuration that
// enumerates the least.
func (x *run) countPrunable() {
	if x.res.Err != nil || x.branch == "" || x.r.Prune == pruneManual {
		return
	}
	verdicts, err := x.verdicts()
	if err != nil {
		return
	}
	for _, v := range verdicts {
		if v.Prunable {
			x.res.Prunable++
		}
	}
}
