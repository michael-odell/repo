package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/michael-odell/repo/internal/model"
)

// cmdConfig prints one repo's fully resolved settings (DESIGN §7, §3.9) — the
// diagnostic counterpart to how easy inheritance makes it to bury a typo'd
// override. Default output is TOML: config in, config out, the same shape as a
// [[root.*.repo]] table you could paste to freeze this repo's behavior exactly
// as inherited. --explain trades that for one line per field naming which link
// in the root/dir chain last set it, at the cost of being far more to read.
func cmdConfig(_ context.Context, args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	explain := fs.Bool("explain", false, "print, per field, which link in the chain last set it")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: repo config [--explain] <id|name|path>")
	}

	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	repos, err := unionRepos(reg)
	if err != nil {
		return err
	}
	r, ok := findOne(repos, fs.Arg(0))
	if !ok {
		return fmt.Errorf("no repo matching %q", fs.Arg(0))
	}

	if *explain {
		return renderExplain(os.Stdout, reg.Explain(r.ID.String(), r.Roots), r)
	}
	return renderConfig(os.Stdout, r)
}

// findOne resolves a single repo by id/short-name (find, shared with resolve
// and sync's selectors) or, failing that, by an exact container path — config
// is inherently single-repo, so unlike sync's selectors a path here must name
// one repo's container precisely rather than a subtree that could hold several.
func findOne(repos []model.Repo, q string) (model.Repo, bool) {
	if r, ok := find(repos, q); ok {
		return r, true
	}
	if !strings.ContainsAny(q, "/~") {
		return model.Repo{}, false
	}
	target := filepath.Clean(expandPath(q))
	var match model.Repo
	n := 0
	for _, r := range repos {
		if filepath.Clean(r.Container()) == target {
			match, n = r, n+1
		}
	}
	if n == 1 {
		return match, true
	}
	return model.Repo{}, false
}

// configFields lists the settings repo config prints and explains, in the same
// order and under the same names as config.Explain's provenance map (DESIGN §7)
// — every field a resolved model.Repo carries except `fork`, which is shown in
// the default TOML output but whose derivation (explicit fork vs. fork_owner
// vs. error, DESIGN §3.6) isn't a per-field overlay the way the rest are, so it
// has no provenance entry to explain.
var configFields = []struct {
	name string
	val  func(model.Repo) string
}{
	{"layout", func(r model.Repo) string { return r.Layout }},
	{"worktrees", func(r model.Repo) string { return strconv.FormatBool(r.Worktrees) }},
	{"branches", func(r model.Repo) string { return strings.Join(r.Branches, ",") }},
	{"workflow", func(r model.Repo) string { return r.Workflow }},
	{"push", func(r model.Repo) string { return r.Push }},
	{"task_branches", func(r model.Repo) string { return r.TaskBranches }},
	{"show_branches", func(r model.Repo) string { return r.ShowBranches }},
	{"force_push", func(r model.Repo) string { return strings.Join(r.ForcePush, ",") }},
	{"force_pull", func(r model.Repo) string { return strings.Join(r.ForcePull, ",") }},
	{"tags", func(r model.Repo) string { return strings.Join(r.Tags, ",") }},
	{"force_tags", func(r model.Repo) string { return strings.Join(r.ForceTags, ",") }},
	{"expected_untracked", func(r model.Repo) string { return strings.Join(r.ExpectedUntracked, ",") }},
	{"expected_uncommitted", func(r model.Repo) string { return strings.Join(r.ExpectedUncommitted, ",") }},
	{"merge_scan_limit", func(r model.Repo) string {
		if r.MergeScanLimit == nil {
			return ""
		}
		return strconv.Itoa(*r.MergeScanLimit)
	}},
	{"prune", func(r model.Repo) string { return r.Prune }},
	{"prune_keep", func(r model.Repo) string { return strings.Join(r.PruneKeep, ",") }},
	{"prune_min_age", func(r model.Repo) string {
		if r.PruneMinAge == 0 {
			return ""
		}
		return r.PruneMinAge.String()
	}},
	{"pin", func(r model.Repo) string { return r.Pin }},
	{"hooks", func(r model.Repo) string {
		parts := make([]string, len(r.Hooks))
		for i, h := range r.Hooks {
			parts[i] = h.After + ":" + h.Run
		}
		return strings.Join(parts, ",")
	}},
}

// workflowDefaulted are the fields whose builtin fallback isn't one fixed value
// but WorkflowDefaults(workflow) (DESIGN §3.6) — so when nothing in the chain
// set one, the honest source is "workflow default", not the bare "builtin" that
// would read as a single hardcoded value regardless of workflow (and, worse,
// could be read as contradicting the value actually shown: vendor's push
// default is "never", not the fixed builtin "manual" every other unset field
// falls back to).
var workflowDefaulted = map[string]bool{
	"push": true, "task_branches": true, "show_branches": true,
}

// renderExplain prints one line per field: its resolved value, and which link
// in the chain — a root name, a "dir.<name>" overlay (DESIGN §3.9), "defaults",
// or "repo" for the repo's own declared entry — last set it. "builtin" means
// none of those did and the field has one fixed fallback; "workflow default"
// means none did but the fallback itself depends on the resolved workflow.
func renderExplain(w io.Writer, provenance map[string]string, r model.Repo) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "FIELD\tVALUE\tSOURCE")
	for _, f := range configFields {
		src := provenance[f.name]
		if src == "" {
			src = "builtin"
			if workflowDefaulted[f.name] {
				src = "workflow default"
			}
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", f.name, f.val(r), src)
	}
	return tw.Flush()
}

// configOut is the printable shape of the default `repo config` output: TOML,
// field order matching the README settings reference table, so it reads as
// something you could paste into a [[root.*.repo]]/[[dir.*.repo]] table
// (DESIGN §7). omitempty/omitzero on the fields that are usually unset keeps
// the common case short — the verbosity repo config --explain deliberately
// doesn't try to avoid.
type configOut struct {
	ID                  string        `toml:"id,omitempty"`
	Fork                string        `toml:"fork,omitempty"`
	Workflow            string        `toml:"workflow"`
	Layout              string        `toml:"layout"`
	Worktrees           bool          `toml:"worktrees"`
	Branches            []string      `toml:"branches"`
	Push                string        `toml:"push"`
	TaskBranches        string        `toml:"task_branches"`
	ShowBranches        string        `toml:"show_branches"`
	ForcePush           []string      `toml:"force_push,omitempty"`
	ForcePull           []string      `toml:"force_pull,omitempty"`
	Tags                []string      `toml:"tags"`
	ForceTags           []string      `toml:"force_tags,omitempty"`
	ExpectedUntracked   []string      `toml:"expected_untracked,omitempty"`
	ExpectedUncommitted []string      `toml:"expected_uncommitted,omitempty"`
	MergeScanLimit      *int          `toml:"merge_scan_limit,omitempty"`
	Prune               string        `toml:"prune"`
	PruneKeep           []string      `toml:"prune_keep,omitempty"`
	PruneMinAge         time.Duration `toml:"prune_min_age,omitzero"`
	Pin                 string        `toml:"pin,omitempty"`
	Hooks               []model.Hook  `toml:"hooks,omitempty"`
}

func renderConfig(w io.Writer, r model.Repo) error {
	fmt.Fprintf(w, "# %s — %s\n", repoName(r), shorten(r.Container()))
	out := configOut{
		Workflow:            r.Workflow,
		Layout:              r.Layout,
		Worktrees:           r.Worktrees,
		Branches:            r.Branches,
		Push:                r.Push,
		TaskBranches:        r.TaskBranches,
		ShowBranches:        r.ShowBranches,
		ForcePush:           r.ForcePush,
		ForcePull:           r.ForcePull,
		Tags:                r.Tags,
		ForceTags:           r.ForceTags,
		ExpectedUntracked:   r.ExpectedUntracked,
		ExpectedUncommitted: r.ExpectedUncommitted,
		MergeScanLimit:      r.MergeScanLimit,
		Prune:               r.Prune,
		PruneKeep:           r.PruneKeep,
		PruneMinAge:         r.PruneMinAge,
		Pin:                 r.Pin,
		Hooks:               r.Hooks,
	}
	if !r.ID.Zero() {
		out.ID = r.ID.String()
	}
	if r.Fork != nil {
		out.Fork = r.Fork.String()
	}
	return toml.NewEncoder(w).Encode(out)
}
