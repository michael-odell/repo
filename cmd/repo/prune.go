package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/journal"
	"github.com/michael-odell/repo/internal/model"
	syncpkg "github.com/michael-odell/repo/internal/sync"
)

// cmdPrune reports — and, only when asked twice, removes — local task branches
// whose work has landed (DESIGN §5.3).
//
// Report-only by default. This is the first command that deletes anything, so
// it opens with the safest possible contract: run it as often as you like and
// it changes nothing. `--delete` is the second ask, and on a terminal it still
// confirms per repo. Auto-pruning as part of `sync` is deliberately not here.
func cmdPrune(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	del := fs.Bool("delete", false, "actually remove the prunable branches (asks first on a terminal)")
	yes := fs.Bool("yes", false, "with --delete, skip the confirmation prompt")
	dry := fs.Bool("dry-run", false, "show what --delete would do, without deleting or recording anything")
	explain := fs.String("explain", "", "print the evidence behind one branch's verdict, and stop")
	if err := fs.Parse(args); err != nil {
		return err
	}

	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	repos, err := unionRepos(reg)
	if err != nil {
		return err
	}
	selected, err := selectRepos(repos, fs.Args())
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("no repos selected")
	}
	sort.Slice(selected, func(i, j int) bool { return repoName(selected[i]) < repoName(selected[j]) })

	if *explain != "" {
		return explainBranch(os.Stdout, selected, *explain)
	}
	// Whether anyone is there to answer is settled once, here, rather than
	// being re-checked wherever a question comes up — the walk-through and the
	// tests then differ only in what they are reading from.
	return runPrune(os.Stdout, os.Stdin, selected,
		pruneOpts{Delete: *del, Yes: *yes, DryRun: *dry, Interactive: isTTY()})
}

// pruneOpts is what the flags decided, separated from how they were parsed so
// the sweep's own prune paths (DESIGN §5.3) and the tests reach the same code
// the command does.
type pruneOpts struct {
	Delete bool
	Yes    bool
	DryRun bool
	// Interactive is whether there is someone to ask. False means the
	// walk-through cannot run, which is a reason to delete nothing rather than
	// a reason to proceed unasked (DESIGN §5.3).
	Interactive bool
}

// deleting reports whether this run walks the deletion path at all. --dry-run
// implies it rather than requiring --delete beside it: the only question a dry
// run answers is "what would deleting do", so demanding both flags would be
// ceremony (DESIGN §5.3).
func (o pruneOpts) deleting() bool { return o.Delete || o.DryRun }

// runPrune classifies every selected repo, reports each branch's verdict, and
// removes the prunable ones when asked.
func runPrune(w io.Writer, in io.Reader, selected []model.Repo, opts pruneOpts) error {
	walk := &walkthrough{in: bufio.NewReader(in)}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.StripEscape)
	var log *journal.Log
	defer func() {
		if log != nil {
			log.Close()
		}
	}()

	total, pruned := 0, 0
	for _, r := range selected {
		container := r.Container()
		if !gitx.IsRepo(container) {
			continue
		}
		verdicts, err := syncpkg.Classify(container, r)
		if err != nil {
			// A repo prune cannot answer for is reported, not skipped: silence
			// here is indistinguishable from a repo with nothing to prune, and
			// the one thing worth knowing is that this repo was *not* examined
			// (DESIGN §5.3 — not knowing is something wrong, not something
			// parked).
			fmt.Fprintf(tw, "  %s\n    ✗\t  %s\n", repoName(r), err)
			continue
		}
		if len(verdicts) == 0 {
			continue
		}
		base := primaryBranch(r)

		fmt.Fprintf(tw, "  %s\n", repoName(r))
		var prunable []syncpkg.Verdict
		for _, v := range verdicts {
			glyph := "·"
			if v.Prunable {
				glyph = "✂"
				prunable = append(prunable, v)
			}
			fmt.Fprintf(tw, "    %s\t  %s\t%s\n", glyph, v.Name, v.Summary(base))
		}
		total += len(prunable)

		if !opts.deleting() || len(prunable) == 0 {
			continue
		}
		tw.Flush()
		approved, quit := approve(w, walk, repoName(r), prunable, base, opts)
		if len(approved) == 0 {
			fmt.Fprintf(w, "    skipped %s\n", repoName(r))
		}
		if quit && len(approved) == 0 {
			break
		}
		// The journal opens before the first deletion of the run, and a journal
		// that cannot be written stops the run rather than being noted and
		// worked around: the record is what makes a deletion answerable for
		// afterwards, so proceeding without one would quietly hand back a
		// weaker promise than the one this command makes (DESIGN §5.3).
		if log == nil && !opts.DryRun && len(approved) > 0 {
			if log, err = journal.Open(); err != nil {
				return fmt.Errorf("cannot open the prune journal, so nothing was deleted: %w", err)
			}
		}
		if len(approved) > 0 {
			pruned += pruneRepo(w, r, container, approved, log, opts)
		}
		if quit {
			fmt.Fprintf(w, "    stopped — %s and everything after it left alone\n", repoName(r))
			break
		}
	}
	tw.Flush()

	switch {
	case total == 0:
		fmt.Fprintln(w, "\nnothing prunable")
	case opts.DryRun:
		fmt.Fprintf(w, "\n%d branch(es) would be deleted — re-run without --dry-run\n", pruned)
	case !opts.Delete:
		fmt.Fprintf(w, "\n%d branch(es) prunable — re-run with --delete to remove them\n", total)
	default:
		fmt.Fprintf(w, "\n%d branch(es) deleted\n", pruned)
		// Only when something was actually written. The journal opens before the
		// first deletion is attempted, so a run whose branches were all withheld
		// still has one — pointing at it would offer a record of nothing.
		if log != nil && pruned > 0 {
			fmt.Fprintf(w, "recorded in %s\n", log.Path())
		}
	}
	return nil
}

// pruneRepo removes (or, under --dry-run, reports) one repo's prunable
// branches, returning how many went. Every deletion is recorded before it is
// announced, and the SHA is read *before* the branch goes: it is the whole
// value of the record, and reading it back afterwards is not an option.
func pruneRepo(w io.Writer, r model.Repo, container string, prunable []syncpkg.Verdict, log *journal.Log, opts pruneOpts) int {
	n := 0
	for _, v := range prunable {
		// The SHA came out of classification, which read it before anything was
		// deleted — the only moment it was available for free and the only
		// moment it was certain to still be there.
		sha := v.SHA
		if sha == "" {
			// No SHA is no restore line, and a deletion nobody can undo is not
			// one this command makes. Reaching here means classification could
			// not read the ref at all, which is itself worth seeing.
			fmt.Fprintf(w, "    ✗ %s: no object recorded for this ref — not deleted\n", v.Name)
			continue
		}
		if opts.DryRun {
			fmt.Fprintf(w, "    ✂ would delete %s (%s)\n", v.Name, shortSHA(sha))
			n++
			continue
		}
		entry := journal.Entry{
			Repo:    repoName(r),
			Branch:  v.Name,
			SHA:     sha,
			Verdict: v.State.String(),
			Mode:    "--delete",
		}
		// `-d` wherever git's own ancestry check can confirm the merge, so
		// two independent judgements have to agree before a branch goes;
		// `-D` only for the tiers that check cannot see (DESIGN §5.3).
		if err := gitx.DeleteBranch(container, v.Name, syncpkg.NeedsForceDelete(v)); err != nil {
			fmt.Fprintf(w, "    ✗ %s: %v\n", v.Name, err)
			continue
		}
		if err := log.Append(entry); err != nil {
			// The branch is already gone, so the restore line is printed where
			// it will at least be seen. A journal that fails mid-run is a fault
			// worth stopping for — the next branch's record would fail too.
			fmt.Fprintf(w, "    ! %s deleted but not recorded (%v) — restore with: %s\n",
				v.Name, err, entry.Restore())
			n++
			continue
		}
		fmt.Fprintf(w, "    ✂ deleted %s (was %s)\n", v.Name, shortSHA(sha))
		n++
	}
	return n
}

// approve decides which of a repo's prunable branches actually go.
//
// The default path asks per branch, with the evidence in front of you, because
// that is the test that transfers: a wrong verdict has to get past a person one
// branch at a time rather than hiding inside a count. `a` and `q` keep a bulk
// answer available for the branches you already believe — being asked twelve
// questions you have already answered is its own way of training someone to
// stop reading them.
func approve(w io.Writer, walk *walkthrough, name string, prunable []syncpkg.Verdict, base string, opts pruneOpts) (approved []syncpkg.Verdict, quit bool) {
	switch {
	case opts.Yes:
		return prunable, false
	case opts.DryRun:
		// The question is shown, not asked: an answer that changes nothing only
		// teaches the habit of typing y (DESIGN §5.3).
		fmt.Fprintf(w, "  would ask about %d branch(es) in %s, one at a time\n", len(prunable), name)
		return prunable, false
	case !opts.Interactive:
		fmt.Fprintf(w, "    (not a terminal — re-run with --yes to delete these)\n")
		return nil, false
	}

	fmt.Fprintf(w, "  %s\n", name)
	walk.startRepo()
	for _, v := range prunable {
		switch walk.ask(w, v, base) {
		case decideYes:
			approved = append(approved, v)
		case decideNo:
			// Declining is an answer about this run, not a fact stored
			// anywhere. The glob that would make it permanent is printed
			// instead, so "stop asking me about this one" stays an edit
			// someone makes and can read back (DESIGN §5.3).
			fmt.Fprintf(w, "      kept — to stop being asked, add prune_keep = [%q]\n", v.Name)
		case decideQuit:
			return approved, true
		}
	}
	return approved, false
}

// decision is one answer in the walk-through.
type decision int

const (
	decideNo decision = iota
	decideYes
	decideQuit
)

// walkthrough carries the state one prune run's questions share: where answers
// come from, and whether an `a` has already covered the current repo.
type walkthrough struct {
	in  *bufio.Reader
	all bool
}

// startRepo forgets a previous repo's bulk answer. "Yes to the rest" is a
// statement about the repo in front of you — carrying it into the next one
// would turn one endorsement into an unbounded one.
func (a *walkthrough) startRepo() { a.all = false }

func (a *walkthrough) ask(w io.Writer, v syncpkg.Verdict, base string) decision {
	if a.all {
		return decideYes
	}
	explainVerdict(w, v, base, "    ")
	for {
		fmt.Fprintf(w, "    delete %s? [y/N/a/q] ", v.Name)
		line, err := a.in.ReadString('\n')
		if err != nil && line == "" {
			// Input ended mid-question. Nothing was answered, so nothing is
			// taken: end-of-file is not consent.
			fmt.Fprintln(w)
			return decideQuit
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "y", "yes":
			return decideYes
		case "", "n", "no":
			return decideNo
		case "a", "all":
			a.all = true
			return decideYes
		case "q", "quit":
			return decideQuit
		default:
			fmt.Fprintf(w, "    y = delete · n = keep · a = yes to the rest of this repo · q = stop\n")
		}
	}
}

// explainBranch prints the evidence behind one named branch's verdict and
// stops, deleting nothing. It searches every selected repo rather than
// requiring the repo be named too: a branch name is usually enough to be
// unambiguous, and when it isn't, showing both is more useful than an error.
func explainBranch(w io.Writer, selected []model.Repo, branch string) error {
	found := 0
	for _, r := range selected {
		container := r.Container()
		if !gitx.IsRepo(container) {
			continue
		}
		verdicts, err := syncpkg.Classify(container, r)
		if err != nil {
			continue
		}
		for _, v := range verdicts {
			if v.Name != branch {
				continue
			}
			found++
			fmt.Fprintf(w, "  %s\n", repoName(r))
			explainVerdict(w, v, primaryBranch(r), "    ")
		}
	}
	if found == 0 {
		return fmt.Errorf("no branch named %q among the selected repos "+
			"(important branches are not classified: they are what the others are measured against)", branch)
	}
	return nil
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// shortSHA abbreviates an object name for display. The journal keeps the full
// one — this is for reading, that is for restoring.
func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// primaryBranch is the repo's first important branch — the reference every
// verdict is measured against.
func primaryBranch(r model.Repo) string {
	if len(r.Branches) > 0 {
		return r.Branches[0]
	}
	return "the important branch"
}
