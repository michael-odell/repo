package main

import (
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

	return runPrune(os.Stdout, selected, pruneOpts{Delete: *del, Yes: *yes, DryRun: *dry})
}

// pruneOpts is what the flags decided, separated from how they were parsed so
// the sweep's own prune paths (DESIGN §5.3) and the tests reach the same code
// the command does.
type pruneOpts struct {
	Delete bool
	Yes    bool
	DryRun bool
}

// deleting reports whether this run walks the deletion path at all. --dry-run
// implies it rather than requiring --delete beside it: the only question a dry
// run answers is "what would deleting do", so demanding both flags would be
// ceremony (DESIGN §5.3).
func (o pruneOpts) deleting() bool { return o.Delete || o.DryRun }

// runPrune classifies every selected repo, reports each branch's verdict, and
// removes the prunable ones when asked.
func runPrune(w io.Writer, selected []model.Repo, opts pruneOpts) error {
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
		if !opts.Yes && !confirmPrune(w, repoName(r), prunable, opts) {
			fmt.Fprintf(w, "    skipped %s\n", repoName(r))
			continue
		}
		// The journal opens before the first deletion of the run, and a journal
		// that cannot be written stops the run rather than being noted and
		// worked around: the record is what makes a deletion answerable for
		// afterwards, so proceeding without one would quietly hand back a
		// weaker promise than the one this command makes (DESIGN §5.3).
		if log == nil && !opts.DryRun {
			if log, err = journal.Open(); err != nil {
				return fmt.Errorf("cannot open the prune journal, so nothing was deleted: %w", err)
			}
		}
		pruned += pruneRepo(w, r, container, prunable, log, opts)
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
		if log != nil {
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
		sha, _ := gitx.RevParse(container, v.Name)
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

// confirmPrune asks before removing branches from one repo. With no TTY it
// returns false rather than proceeding: a non-interactive run that meant to
// delete says so with --yes, and one that didn't must not delete by accident
// (the same contract --lose-ignored uses for a relayout).
//
// A dry run prints the question instead of asking it. Waiting on an answer that
// changes nothing would only teach the habit of typing y (DESIGN §5.3).
func confirmPrune(w io.Writer, name string, vs []syncpkg.Verdict, opts pruneOpts) bool {
	names := make([]string, 0, len(vs))
	for _, v := range vs {
		names = append(names, v.Name)
	}
	question := fmt.Sprintf("delete %d branch(es) from %s (%s)?", len(vs), name, strings.Join(names, ", "))
	if opts.DryRun {
		fmt.Fprintf(w, "  would ask: %s\n", question)
		return true
	}
	if !isTTY() {
		fmt.Fprintf(w, "    (not a terminal — re-run with --yes to delete these)\n")
		return false
	}
	fmt.Fprintf(w, "  %s [y/N] ", question)
	var resp string
	_, _ = fmt.Scanln(&resp)
	resp = strings.ToLower(strings.TrimSpace(resp))
	return resp == "y" || resp == "yes"
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
