package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/michael-odell/repo/internal/model"
	syncpkg "github.com/michael-odell/repo/internal/sync"
)

func cmdSync(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	ifDue := fs.Bool("if-due", false, "only sync repos whose cadence is due")
	force := fs.Bool("force", false, "ignore cadence")
	fix := fs.Bool("fix", false, "apply the config↔disk reconciliations sync reports (after syncing)")
	loseIgnored := fs.Bool("lose-ignored", false, "with --fix, discard .gitignore'd files without prompting")
	noPrune := fs.Bool("no-prune", false, "don't act on landed branches this run; still report them")
	var dryRun, dryRunN, verbose, verboseV bool
	fs.BoolVar(&dryRun, "dry-run", false, "show planned actions without changing anything")
	fs.BoolVar(&dryRunN, "n", false, "alias for --dry-run")
	fs.BoolVar(&verbose, "verbose", false, "explain the decision for every repo")
	fs.BoolVar(&verboseV, "v", false, "alias for --verbose")
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

	prog := newProgress(len(selected))
	opts := syncpkg.Options{
		Progress:    prog.update,
		DryRun:      dryRun || dryRunN,
		Verbose:     verbose || verboseV,
		Force:       *force,
		IfDue:       *ifDue,
		FixLayout:   *fix,
		LoseIgnored: *loseIgnored,
		Frequency:   7 * 24 * time.Hour,
		StateDir:    filepath.Join(outDir(), "last-sync"),
	}
	results := syncpkg.Run(reg, selected, opts)
	prog.stopAndClear()
	// Whether the walk that follows will act decides how the footer reads:
	// pointing at `repo prune` and then pruning them itself would be the report
	// disagreeing with the run three lines later.
	willPrune := !*noPrune && anyRepoPrunes(selected, results)
	renderSync(os.Stdout, results, opts, willPrune)
	// After the report, so the count in the footer is what the walk is about,
	// and in the serial phase, where a question can be asked and read.
	if !*noPrune {
		sweepPrune(os.Stdout, os.Stdin, selected, results, opts)
	}
	for _, r := range results {
		if r.Err != nil {
			return fmt.Errorf("%d repo(s) failed", countFailed(results))
		}
	}
	return nil
}

// selectRepos scopes the sweep by the positional selectors (DESIGN §7). With no
// selector, every repo is included. Each selector matches, in order of
// preference: a root name (every repo in that root), a directory path (every repo
// whose container is at or below it), or a single repo (id or short name).
func selectRepos(repos []model.Repo, selectors []string) ([]model.Repo, error) {
	if len(selectors) == 0 {
		return repos, nil
	}
	var out []model.Repo
	seen := map[string]bool{}
	addRepo := func(r model.Repo) {
		key := r.Container()
		if !seen[key] {
			seen[key] = true
			out = append(out, r)
		}
	}
	for _, sel := range selectors {
		matched := false
		for _, r := range repos {
			if hasRootName(r.Roots, sel) {
				addRepo(r)
				matched = true
			}
		}
		if matched {
			continue
		}
		if path := expandPath(sel); strings.ContainsAny(sel, "/~") {
			for _, r := range repos {
				if pathAtOrBelow(r.Container(), path) {
					addRepo(r)
					matched = true
				}
			}
			if matched {
				continue
			}
		}
		r, ok := find(repos, sel)
		if !ok {
			return nil, fmt.Errorf("no root, path, or repo matching %q", sel)
		}
		addRepo(r)
	}
	return out, nil
}

func hasRootName(roots []string, want string) bool {
	for _, r := range roots {
		if r == want {
			return true
		}
	}
	return false
}

func pathAtOrBelow(p, prefix string) bool {
	p, prefix = filepath.Clean(p), filepath.Clean(prefix)
	return p == prefix || strings.HasPrefix(p, prefix+string(filepath.Separator))
}

func expandPath(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// renderSync writes the report. willPrune says whether this run is about to act
// on the landed branches it is naming, which changes only the footer's advice —
// telling someone to run `repo prune` immediately before doing it for them is
// the kind of disagreement §5.6 exists to prevent.
func renderSync(w io.Writer, results []syncpkg.Result, opts syncpkg.Options, willPrune bool) {
	// Sorted on a copy. The caller's slice is paired with its repos by position
	// (see prunePairs), and the walk that runs after this report deletes
	// branches on the strength of that pairing — a report that reorders what it
	// was handed is how a question answered about one repo came to be carried
	// out against another (v0.11.0).
	results = slices.Clone(results)
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	// A branch row's glyph is itself indented past the repo row's (not just
	// its name) so a nested branch reads as clearly indented at a glance,
	// rather than every glyph — repo or branch — landing in one flat column
	// (DESIGN §5.6). The workflow sits between the name and the detail, greyed:
	// it explains the row rather than being news, so it should be findable
	// without competing with the finding.
	if opts.Verbose {
		for _, r := range results {
			// The repo's own outcome belongs on its line here exactly as it does
			// in the table. Leaving it to the trace below assumed every outcome
			// had left a trace line, and a failure records its reason as the
			// outcome itself — so --verbose, the mode you reach for *because*
			// something failed, was the one that showed a bare ✗ and a name.
			detail := ""
			if r.Detail != "" {
				detail = "  " + r.Detail
			}
			fmt.Fprintf(w, "  %s %s %s%s\n", color(outcomeColor(r.Outcome), syncGlyph(r.Outcome)), r.Name,
				color(ansiGray, "("+r.Workflow+", "+r.Elapsed.Round(time.Millisecond).String()+")"), detail)
			// Each line's stamp is how long the step it reports took: a trace
			// line is written after its work, so the gap from the previous line
			// is that work's duration. Printing the gap rather than the offset
			// puts the number next to the thing it measures.
			var prev time.Duration
			for _, a := range r.Actions {
				fmt.Fprintf(w, "    · %s %s\n",
					color(ansiGray, fmt.Sprintf("%7s", (a.At-prev).Round(time.Millisecond))), a.Text)
				prev = a.At
			}
			for _, b := range r.Branches {
				fmt.Fprintf(w, "    %s %s: %s\n", color(outcomeColor(b.Outcome), syncGlyph(b.Outcome)), b.Label(), b.Summary)
			}
		}
		fmt.Fprintln(w)
	} else {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', tabwriter.StripEscape)
		for _, r := range results {
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", colorCell(outcomeColor(r.Outcome), syncGlyph(r.Outcome)), r.Name,
				colorCell(ansiGray, r.Workflow), r.Detail)
			// The empty workflow cell keeps a branch's summary under the repo
			// details it elaborates, rather than under the workflow column.
			for _, b := range r.Branches {
				fmt.Fprintf(tw, "    %s\t  %s\t\t%s\n", colorCell(outcomeColor(b.Outcome), syncGlyph(b.Outcome)), b.Label(), b.Summary)
			}
		}
		tw.Flush()
	}

	counts := map[syncpkg.Outcome]int{}
	for _, r := range results {
		counts[r.Outcome]++
	}
	mode := ""
	if opts.DryRun {
		mode = " (dry-run)"
	}
	// Worst first, and the ⚠ bucket is "flagged" rather than "need attention":
	// with failures on the board, "1 need attention" beside "21 failed" claims
	// the failures don't — and buried at the end, the number that matters was
	// the last thing read. Each word here names one glyph's rows and nothing
	// wider.
	fmt.Fprintf(w, "\n%d failed · %d flagged · %d review pending · %d deferred · %d updated · %d up to date%s\n",
		counts[syncpkg.Failed], counts[syncpkg.Attention], counts[syncpkg.ReviewPending],
		counts[syncpkg.Deferred], counts[syncpkg.Updated], counts[syncpkg.UpToDate], mode)

	// A sweep's cost is rarely evenly spread, and the repo that ate it leaves no
	// trace in a report organised by outcome — it may well be a ✓. Name it when
	// it was slow enough to have been the thing you were waiting on.
	if slow := slowest(results); slow != nil {
		fmt.Fprintf(w, "slowest: %s %s\n", slow.Name, slow.Elapsed.Round(time.Second))
	}

	// Landed branches are the one finding a sweep has no other way to mention:
	// they are observations, not findings, so they never lift a repo's glyph
	// (§5.6) — and below show_branches = "landed" they have no line either,
	// which left prune invisible to anyone who hadn't gone looking for it. A count,
	// not a list: enumerating branches is show_branches' job, and doing it twice
	// in one report is the row/bullet disagreement §5.6 exists to prevent.
	if branches, repos := prunableTally(results); branches > 0 {
		hint := " — repo prune"
		if willPrune {
			hint = ""
		}
		fmt.Fprintf(w, "%d branch(es) prunable across %d repo(s)%s\n", branches, repos, hint)
	}
}

// anyRepoPrunes reports whether the pass after the report will act on anything:
// some selected repo has candidates and a mode that removes them. Under
// `interactive` with no terminal the walk degrades to `report`, so that is not
// acting either — and the footer should say `repo prune` in exactly that case,
// since running it by hand is then the only way these branches go.
func anyRepoPrunes(repos []model.Repo, results []syncpkg.Result) bool {
	pairs, mismatch := prunePairs(repos, results)
	if mismatch != "" {
		return false // the walk will decline too, and say why
	}
	for _, p := range pairs {
		if p.res.Err != nil || len(p.res.Prunable) == 0 {
			continue
		}
		switch p.repo.Prune {
		case syncpkg.PruneAuto:
			return true
		case syncpkg.PruneInteractive:
			if isTTY() {
				return true
			}
		}
	}
	return false
}

// prunableTally totals the branches prune would offer to remove, and how many
// repos hold them.
func prunableTally(results []syncpkg.Result) (branches, repos int) {
	for _, r := range results {
		if len(r.Prunable) > 0 {
			branches += len(r.Prunable)
			repos++
		}
	}
	return branches, repos
}

// slowestThreshold is where a repo stops being ordinary and starts being the
// reason the sweep felt slow. Below it, naming one repo would imply a problem
// that isn't there.
const slowestThreshold = 30 * time.Second

// slowest returns the repo worth naming for its duration, or nil when nothing
// took long enough to be worth mentioning. Only meaningful with more than one
// repo: on a single-repo sweep its duration is the sweep's, which the user just
// watched.
func slowest(results []syncpkg.Result) *syncpkg.Result {
	if len(results) < 2 {
		return nil
	}
	var worst *syncpkg.Result
	for i := range results {
		if results[i].Elapsed >= slowestThreshold && (worst == nil || results[i].Elapsed > worst.Elapsed) {
			worst = &results[i]
		}
	}
	return worst
}

func syncGlyph(o syncpkg.Outcome) string {
	switch o {
	case syncpkg.Updated:
		return "↻"
	case syncpkg.Attention:
		return "⚠"
	case syncpkg.ReviewPending:
		return "⚑"
	case syncpkg.Deferred:
		return "⋯"
	case syncpkg.Failed:
		return "✗"
	case syncpkg.Info:
		// An observation, not an outcome — visually quieter than every glyph
		// that means something happened or needs to (DESIGN §5.6). Distinct
		// from the "·" that prefixes --verbose trace lines.
		return "◦"
	default:
		return "✓"
	}
}

// outcomeColor is syncGlyph's color: bold red for Failed specifically, since
// that's the one most worth catching at a glance and the plain ✗/✓ glyphs
// read as similar without it.
func outcomeColor(o syncpkg.Outcome) string {
	switch o {
	case syncpkg.Updated:
		return ansiCyan
	case syncpkg.Attention:
		return ansiYellow
	case syncpkg.ReviewPending:
		return ansiMagenta
	case syncpkg.Deferred:
		return ansiGray
	case syncpkg.Failed:
		return ansiBoldRed
	case syncpkg.Info:
		return ansiGray
	default:
		return ansiGreen
	}
}

func countFailed(results []syncpkg.Result) int {
	n := 0
	for _, r := range results {
		if r.Err != nil {
			n++
		}
	}
	return n
}
