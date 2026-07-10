package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/michael-odell/repo/internal/model"
	syncpkg "github.com/michael-odell/repo/internal/sync"
)

func cmdSync(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("sync", flag.ContinueOnError)
	tag := fs.String("tag", "", "limit to repos with this tag")
	ifDue := fs.Bool("if-due", false, "only sync repos whose cadence is due")
	force := fs.Bool("force", false, "ignore cadence")
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
	selected, err := selectRepos(repos, *tag, fs.Args())
	if err != nil {
		return err
	}
	if len(selected) == 0 {
		return fmt.Errorf("no repos selected")
	}

	opts := syncpkg.Options{
		DryRun:    dryRun || dryRunN,
		Verbose:   verbose || verboseV,
		Force:     *force,
		IfDue:     *ifDue,
		Frequency: 7 * 24 * time.Hour,
		StateDir:  filepath.Join(outDir(), "last-sync"),
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

func selectRepos(repos []model.Repo, tag string, names []string) ([]model.Repo, error) {
	if len(names) > 0 {
		var out []model.Repo
		for _, n := range names {
			r, ok := find(repos, n)
			if !ok {
				return nil, fmt.Errorf("no repo matching %q", n)
			}
			out = append(out, r)
		}
		return out, nil
	}
	if tag == "" {
		return repos, nil
	}
	var out []model.Repo
	for _, r := range repos {
		for _, t := range r.Tags {
			if t == tag {
				out = append(out, r)
				break
			}
		}
	}
	return out, nil
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
