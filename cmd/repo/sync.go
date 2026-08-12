package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
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
	renderSync(os.Stdout, results, opts)
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

func renderSync(w io.Writer, results []syncpkg.Result, opts syncpkg.Options) {
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
