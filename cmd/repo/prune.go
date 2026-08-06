package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/michael-odell/repo/internal/gitx"
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

	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', tabwriter.StripEscape)
	total, pruned := 0, 0
	for _, r := range selected {
		container := r.Container()
		if !gitx.IsRepo(container) {
			continue
		}
		verdicts, err := syncpkg.Classify(container, r)
		if err != nil || len(verdicts) == 0 {
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

		if !*del || len(prunable) == 0 {
			continue
		}
		tw.Flush()
		if !*yes && !confirmPrune(repoName(r), prunable) {
			fmt.Fprintf(os.Stdout, "    skipped %s\n", repoName(r))
			continue
		}
		for _, v := range prunable {
			// `-d` wherever git's own ancestry check can confirm the merge, so
			// two independent judgements have to agree before a branch goes;
			// `-D` only for the tiers that check cannot see (DESIGN §5.3).
			if err := gitx.DeleteBranch(container, v.Name, syncpkg.NeedsForceDelete(v)); err != nil {
				fmt.Fprintf(os.Stdout, "    ✗ %s: %v\n", v.Name, err)
				continue
			}
			fmt.Fprintf(os.Stdout, "    ✂ deleted %s\n", v.Name)
			pruned++
		}
	}
	tw.Flush()

	switch {
	case total == 0:
		fmt.Println("\nnothing prunable")
	case !*del:
		fmt.Printf("\n%d branch(es) prunable — re-run with --delete to remove them\n", total)
	default:
		fmt.Printf("\n%d branch(es) deleted\n", pruned)
	}
	return nil
}

// confirmPrune asks before removing branches from one repo. With no TTY it
// returns false rather than proceeding: a non-interactive run that meant to
// delete says so with --yes, and one that didn't must not delete by accident
// (the same contract --lose-ignored uses for a relayout).
func confirmPrune(name string, vs []syncpkg.Verdict) bool {
	if !isTTY() {
		fmt.Printf("    (not a terminal — re-run with --yes to delete these)\n")
		return false
	}
	names := make([]string, 0, len(vs))
	for _, v := range vs {
		names = append(names, v.Name)
	}
	fmt.Printf("  delete %d branch(es) from %s (%s)? [y/N] ", len(vs), name, strings.Join(names, ", "))
	var resp string
	_, _ = fmt.Scanln(&resp)
	resp = strings.ToLower(strings.TrimSpace(resp))
	return resp == "y" || resp == "yes"
}

func isTTY() bool {
	fi, err := os.Stdin.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// primaryBranch is the repo's first important branch — the reference every
// verdict is measured against.
func primaryBranch(r model.Repo) string {
	if len(r.Branches) > 0 {
		return r.Branches[0]
	}
	return "the important branch"
}
