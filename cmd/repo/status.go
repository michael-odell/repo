package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"text/tabwriter"

	"golang.org/x/sync/errgroup"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/discover"
	"github.com/michael-odell/repo/internal/gitx"
)

// resolveRoots picks discovery roots: REPO_ROOTS env, else [[root]] entries,
// else the registry's home_roots (DESIGN §3.2).
func resolveRoots(reg *config.Registry) []config.Root {
	if v := os.Getenv("REPO_ROOTS"); v != "" {
		var rs []config.Root
		for _, p := range filepath.SplitList(v) {
			if p != "" {
				rs = append(rs, config.Root{Path: p})
			}
		}
		return rs
	}
	if dr := reg.DeclaredRoots(); len(dr) > 0 {
		return dr
	}
	var rs []config.Root
	for _, h := range reg.HomeRoots() {
		rs = append(rs, config.Root{Path: h})
	}
	return rs
}

type observation struct {
	name          string
	branch        string
	upstream      string
	ahead, behind int
	dirty         bool
	cloned        bool
	note          string
	err           error
}

func cmdStatus(ctx context.Context, _ []string) error {
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	found, err := discover.Discover(resolveRoots(reg), reg)
	if err != nil {
		return err
	}

	// Observe drift per repo in a bounded, isolated sweep: one repo's failure
	// is captured, never aborting the others (DESIGN §2.5).
	obs := make([]observation, len(found))
	var g errgroup.Group
	g.SetLimit(8)
	for i, f := range found {
		i, f := i, f
		g.Go(func() error {
			obs[i] = observe(f)
			return nil
		})
	}
	_ = g.Wait()

	// Declared-but-not-cloned repos.
	onDisk := map[string]bool{}
	for _, f := range found {
		if !f.ID.Zero() {
			onDisk[f.ID.String()] = true
		}
	}
	repos, err := reg.Repos()
	if err != nil {
		return err
	}
	for _, r := range repos {
		if onDisk[r.ID.String()] || gitx.IsRepo(r.PrimaryTree()) {
			continue
		}
		obs = append(obs, observation{name: r.ID.Short(), note: "not cloned"})
	}

	render(os.Stdout, obs)
	return nil
}

func observe(f discover.Found) observation {
	o := observation{name: nameOf(f), cloned: true, note: f.Note}
	branch, err := gitx.CurrentBranch(f.Dir)
	if err != nil {
		o.err = err
		return o
	}
	o.branch = branch
	if dirty, err := gitx.IsDirty(f.Dir); err == nil {
		o.dirty = dirty
	}
	if branch != "" {
		if up := gitx.Upstream(f.Dir, branch); up != "" {
			o.upstream = up
			o.ahead, o.behind, _ = gitx.AheadBehind(f.Dir, up)
		} else {
			o.note = "no upstream"
		}
	}
	return o
}

func nameOf(f discover.Found) string {
	if f.ID.Zero() {
		return filepath.Base(f.Dir)
	}
	return f.ID.Short()
}

func render(w *os.File, obs []observation) {
	sort.Slice(obs, func(i, j int) bool { return obs[i].name < obs[j].name })
	clean, attention, absent := 0, 0, 0
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, o := range obs {
		switch {
		case !o.cloned:
			absent++
		case o.err != nil || o.dirty || o.ahead > 0 || o.behind > 0 || o.note != "":
			attention++
		default:
			clean++
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", glyph(o), o.name, detail(o))
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d clean · %d need attention · %d not cloned\n", clean, attention, absent)
}

func glyph(o observation) string {
	switch {
	case o.err != nil:
		return "✗"
	case !o.cloned:
		return "⋯"
	case o.dirty || o.ahead > 0 || o.behind > 0:
		return "⚠"
	case o.note != "":
		return "•"
	default:
		return "✓"
	}
}

func detail(o observation) string {
	if o.err != nil {
		return "error: " + o.err.Error()
	}
	if !o.cloned {
		return o.note
	}
	parts := []string{}
	if o.branch != "" {
		parts = append(parts, o.branch)
	}
	if o.ahead > 0 {
		parts = append(parts, fmt.Sprintf("+%d ahead", o.ahead))
	}
	if o.behind > 0 {
		parts = append(parts, fmt.Sprintf("-%d behind", o.behind))
	}
	if o.dirty {
		parts = append(parts, "dirty")
	}
	if o.note != "" {
		parts = append(parts, o.note)
	}
	if len(parts) <= 1 {
		if len(parts) == 1 {
			return parts[0] + "  up to date"
		}
		return "up to date"
	}
	return join(parts)
}

func join(parts []string) string {
	out := parts[0]
	for _, p := range parts[1:] {
		out += "  " + p
	}
	return out
}
