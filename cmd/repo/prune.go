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

// cmdPrune removes local task branches whose work has landed (DESIGN §5.3).
//
// It prunes, because that is its name, and it asks first: on a terminal every
// candidate is walked one at a time with its evidence in front of you. There is
// no --delete. A separate flag to make a command called *prune* actually prune
// buys nothing --dry-run does not buy better — one previews, the other
// confirms, and requiring both to mean yes only teaches the habit of passing
// both.
//
// With no terminal there is nobody to ask, so nothing is deleted unless --yes
// says so: a script that meant to prune says so, and a mistyped `repo prune`
// stays cheap.
func cmdPrune(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("prune", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "delete without asking (required when there is no terminal)")
	dry := fs.Bool("dry-run", false, "run every check, delete nothing, and print what would go")
	verbose := fs.Bool("verbose", false, "print the evidence behind every verdict")
	fs.BoolVar(verbose, "v", false, "shorthand for --verbose")
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

	// Whether anyone is there to answer is settled once, here, rather than
	// being re-checked wherever a question comes up — the walk-through and the
	// tests then differ only in what they are reading from.
	return runPrune(os.Stdout, os.Stdin, selected,
		pruneOpts{Yes: *yes, DryRun: *dry, Verbose: *verbose, Interactive: isTTY()})
}

// pruneOpts is what the flags decided, separated from how they were parsed so
// the sweep's own prune paths (DESIGN §5.3) and the tests reach the same code
// the command does.
type pruneOpts struct {
	Yes     bool
	DryRun  bool
	Verbose bool
	// Interactive is whether there is someone to ask. False means the
	// walk-through cannot run, which is a reason to delete nothing rather than
	// a reason to proceed unasked (DESIGN §5.3).
	Interactive bool
	// Mode is what the journal records as having done the deleting. Empty for
	// the command, which derives it below; the sweep sets "interactive" or
	// "auto", so a record says which of the two removed a branch.
	Mode string
}

// deleting reports whether this run may remove anything. Pruning is the
// default, so the only thing that stops it is having nobody to ask and no --yes
// standing in for them.
func (o pruneOpts) deleting() bool { return o.DryRun || o.Yes || o.Interactive }

// mode names what did the deleting, for the journal.
func (o pruneOpts) mode() string {
	switch {
	case o.Mode != "":
		return o.Mode
	case o.Yes:
		return "prune --yes"
	default:
		return "prune"
	}
}

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

		fmt.Fprintf(tw, "  %s\n", repoName(r))
		var prunable []syncpkg.Verdict
		for _, v := range verdicts {
			glyph := "·"
			if v.Prunable {
				glyph = "✂"
				prunable = append(prunable, v)
			}
			fmt.Fprintf(tw, "    %s\t  %s\t%s\n", glyph, v.Name, v.Summary())
			if opts.Verbose {
				// Written through the tabwriter rather than around it: these
				// lines carry no tabs, so they pass through as single cells and
				// the branch rows above and below stay in one set of columns.
				explainVerdict(tw, v, "        ")
			}
		}
		total += len(prunable)

		if !opts.deleting() || len(prunable) == 0 {
			continue
		}
		tw.Flush()
		approved, quit := approve(w, walk, repoName(r), prunable, opts)
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
	case !opts.deleting():
		// Nobody to ask and no --yes standing in for them. Reporting is all
		// that is left, and saying so beats a silent no-op.
		fmt.Fprintf(w, "\n%d branch(es) prunable — no terminal to ask, so nothing was deleted; re-run with --yes\n", total)
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
			fmt.Fprintf(w, "    ✂ would delete %s (%s)%s\n",
				v.Name, shortSHA(sha), worktreeNote(v))
			n++
			continue
		}
		// The tree goes first: git refuses to delete a branch a worktree still
		// holds, so the other order leaves both. A removal that fails leaves
		// both too, which is the same safe end state.
		if v.Worktree != "" {
			if err := gitx.WorktreeRemove(container, v.Worktree); err != nil {
				fmt.Fprintf(w, "    ✗ %s: could not remove its worktree — not deleted (%v)\n",
					v.Name, err)
				continue
			}
			fmt.Fprintf(w, "    ✂ removed worktree %s%s\n", shorten(v.Worktree), ignoredNote(v))
		}
		entry := journal.Entry{
			Repo:    repoName(r),
			Branch:  v.Name,
			SHA:     sha,
			Verdict: v.State.String(),
			Mode:    opts.mode(),
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

// worktreeNote says that removing this branch takes a working tree with it,
// for the dry run's preview and the walk-through's question. A deletion that
// silently removed a directory would be a bigger act than the line describing
// it (DESIGN §5.3).
func worktreeNote(v syncpkg.Verdict) string {
	if v.Worktree == "" {
		return ""
	}
	return fmt.Sprintf(" and its worktree %s%s", shorten(v.Worktree), ignoredNote(v))
}

// ignoredNote names what removing a worktree discards. `git worktree remove`
// refuses on uncommitted or untracked work but takes .gitignore'd residue
// without comment, so counting it is the only way anyone is told (§4.1).
func ignoredNote(v syncpkg.Verdict) string {
	if v.WorktreeIgnored == 0 {
		return ""
	}
	return fmt.Sprintf(" (discarding %d ignored file(s))", v.WorktreeIgnored)
}

// approve decides which of a repo's prunable branches actually go.
//
// The default path asks per branch, with the evidence in front of you, because
// that is the test that transfers: a wrong verdict has to get past a person one
// branch at a time rather than hiding inside a count. `a` and `q` keep a bulk
// answer available for the branches you already believe — being asked twelve
// questions you have already answered is its own way of training someone to
// stop reading them.
func approve(w io.Writer, walk *walkthrough, name string, prunable []syncpkg.Verdict, opts pruneOpts) (approved []syncpkg.Verdict, quit bool) {
	switch {
	case opts.DryRun:
		// The question is shown, not asked: an answer that changes nothing only
		// teaches the habit of typing y (DESIGN §5.3).
		if opts.Yes {
			return prunable, false
		}
		fmt.Fprintf(w, "  would ask about %d branch(es) in %s, one at a time\n", len(prunable), name)
		return prunable, false
	case opts.Yes:
		return prunable, false
	}

	fmt.Fprintf(w, "  %s\n", name)
	walk.startRepo()
	for _, v := range prunable {
		switch walk.ask(w, v) {
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

func (a *walkthrough) ask(w io.Writer, v syncpkg.Verdict) decision {
	if a.all {
		return decideYes
	}
	explainVerdict(w, v, "    ")
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
