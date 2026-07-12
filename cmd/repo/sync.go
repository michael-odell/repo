package main

import (
	"context"
	"flag"
	"fmt"
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

	opts := syncpkg.Options{
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

func renderSync(w *os.File, results []syncpkg.Result, opts syncpkg.Options) {
	sort.Slice(results, func(i, j int) bool { return results[i].Name < results[j].Name })

	if opts.Verbose {
		for _, r := range results {
			fmt.Fprintf(w, "%s %s\n", syncGlyph(r.Outcome), r.Name)
			for _, a := range r.Actions {
				fmt.Fprintf(w, "    · %s\n", a)
			}
		}
		fmt.Fprintln(w)
	} else {
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		for _, r := range results {
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", syncGlyph(r.Outcome), r.Name, r.Detail)
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
	fmt.Fprintf(w, "\n%d updated · %d up to date · %d need attention · %d review pending · %d deferred · %d failed%s\n",
		counts[syncpkg.Updated], counts[syncpkg.UpToDate], counts[syncpkg.Attention],
		counts[syncpkg.ReviewPending], counts[syncpkg.Deferred], counts[syncpkg.Failed], mode)
}

func syncGlyph(o syncpkg.Outcome) string {
	switch o {
	case syncpkg.Updated:
		return "⏸"
	case syncpkg.Attention:
		return "⚠"
	case syncpkg.ReviewPending:
		return "⚑"
	case syncpkg.Deferred:
		return "⋯"
	case syncpkg.Failed:
		return "✗"
	default:
		return "✓"
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
