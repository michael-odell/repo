package sync

// show_branches values (DESIGN §5.6), ordered by how much of the branch
// inventory each enumerates: none ⊂ notable ⊂ unmerged ⊂ all.
const (
	showNone     = "none"     // no branch lines at all; the repo row is the report
	showNotable  = "notable"  // only branches with a finding this run
	showUnmerged = "unmerged" // …plus task branches carrying unlanded work
	showAll      = "all"      // …plus the task branches whose work has landed
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
// itself. So only task branches are ever observed, `all` included: a quiet
// important branch has nothing to say that the repo's own row — its glyph, and
// the "N branches up to date" count that includes it — hasn't already said,
// and on a single-branch repo the line was the row repeated verbatim. A
// *finding* on an important branch is unaffected; findings aren't configurable.
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
			x.branchMark(v.Name, Attention, v.Summary())
			continue
		}
		// `unmerged` shows only outstanding work. `all` additionally shows the
		// landed branches with the verdict prune would act on, so the decision
		// can be watched during ordinary sweeps rather than only when someone
		// remembers to go looking for it.
		if !v.State.Merged() || x.r.ShowBranches == showAll {
			x.branchMark(v.Name, Info, v.Summary())
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

// Prune modes (DESIGN §5.3), ordered by how much the sweep does unasked:
// manual ⊂ report ⊂ interactive ⊂ auto. They govern `repo sync`; `repo prune`
// does not consult them, because a command someone typed is not a policy
// question.
//
// Exported because the sweep's prune pass lives in the command layer, where the
// walk-through, the journal and the output already are — one implementation, so
// what a sweep does and what `repo prune` does cannot drift apart. The set is
// held to config's enum table by TestEveryPruneModeIsImplemented.
const (
	PruneManual      = "manual"      // don't classify, don't offer
	PruneReport      = "report"      // classify and name what was found
	PruneInteractive = "interactive" // …and walk it with whoever is there
	PruneAuto        = "auto"        // …and delete what the ladder allows
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
		x.classified, x.classifyErr = Classify(x.container, x.r)
		if x.classified == nil && x.classifyErr == nil {
			// No task branches: remember that, rather than reclassifying each
			// time something asks.
			x.classified = []Verdict{}
		}
	}
	return x.classified, x.classifyErr
}

// countPrunable records the branches prune would remove — for the footer that
// tells you they are there, and for the sweep's own `interactive`/`auto` pass,
// which acts on these verdicts rather than classifying a second time (DESIGN
// §5.3).
//
// It runs under every mode but `manual`, which is the one that says "don't ask
// the question". `show_branches` is not consulted: what a sweep *tells* you and
// what prune *would do* are different questions (§5.6), so a repo listing no
// branch lines still contributes its count — the footer is one line either way,
// and suppressing it would hide the feature from exactly the configuration that
// enumerates the least.
func (x *run) countPrunable() {
	if x.res.Err != nil || x.branch == "" || x.r.Prune == PruneManual {
		return
	}
	verdicts, err := x.verdicts()
	if err != nil {
		return
	}
	for _, v := range verdicts {
		if v.Prunable {
			x.res.Prunable = append(x.res.Prunable, v)
		}
	}
}
