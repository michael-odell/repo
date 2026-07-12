package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"github.com/michael-odell/repo/internal/discover"
	"github.com/michael-odell/repo/internal/gitx"
)

// layoutLabel reports a discovered container's on-disk shape (DESIGN §4.1) so a
// scan shows which repos are single working trees vs bare+worktree parents.
func layoutLabel(dir string) string {
	switch gitx.ClassifyLayout(dir) {
	case gitx.LayoutWorktree:
		return "worktree"
	case gitx.LayoutSingle:
		return "single"
	default:
		return "—"
	}
}

func cmdScan(_ context.Context, _ []string) error {
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	found, err := discover.Discover(resolveRoots(reg), reg)
	if err != nil {
		return err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Dir < found[j].Dir })
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tWORKFLOW\tROOT\tLAYOUT\tDIR\tNOTE")
	for _, f := range found {
		id := "—"
		if !f.ID.Zero() {
			id = f.ID.String()
		}
		root := f.Root
		if root == "" {
			root = "—"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", id, f.Workflow, root, layoutLabel(f.Dir), shorten(f.Dir), f.Note)
	}
	return tw.Flush()
}

// shorten replaces the home prefix with ~ for readability.
func shorten(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && rel != ".." && !filepath.IsAbs(rel) {
			return "~/" + rel
		}
	}
	return p
}
