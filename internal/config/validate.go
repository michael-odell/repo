package config

import (
	"fmt"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// enum sets for validated fields.
var (
	validLayouts      = []string{model.LayoutFlat, model.LayoutOwner}
	validWorkflows    = []string{model.UpstreamPush, model.ForkPR, model.SupplyChainMirror, model.Vendor}
	validPush         = []string{"auto", "manual", "never"}
	validTaskBranches = []string{"auto", "report", "pull-only"}
	validShowBranches = []string{"none", "notable", "unmerged", "all"}
	validPrune        = []string{"auto", "report", "manual"}
)

// Validate checks the loaded registry semantically and returns a single error
// aggregating every problem found, so a broken config surfaces all its faults at
// once rather than one per run. Load already rejects unknown keys; Validate
// covers structure (roots need a `dir`), enum values, identity/fork parsing, and
// host resolvability.
func (reg *Registry) Validate() error {
	var errs []string
	add := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	names := make([]string, 0, len(reg.roots))
	for n := range reg.roots {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := reg.roots[n]
		if strings.TrimSpace(r.Dir) == "" {
			add("root %q: missing `dir`", n)
		}
		checkEnums(add, fmt.Sprintf("root %q", n), r.Settings)
		checkGlobs(add, fmt.Sprintf("root %q", n), globsOfSettings(r.Settings))
		checkScanLimit(add, fmt.Sprintf("root %q", n), r.Settings)
	}
	checkEnums(add, "defaults", reg.defaults)
	checkGlobs(add, "defaults", globsOfSettings(reg.defaults))
	checkScanLimit(add, "defaults", reg.defaults)

	// effective() surfaces id/fork parse errors and undervable forks; resolving
	// Physical additionally catches unknown hosts. Report the first structural
	// failure precisely, then per-repo issues.
	repos, err := reg.Repos()
	if err != nil {
		add("%v", err)
	} else {
		for _, r := range repos {
			checkEnums(add, r.ID.String(), settingsOf(r))
			checkGlobs(add, r.ID.String(), globsOfRepo(r))
			if _, err := reg.Physical(r); err != nil {
				add("%v", err)
			}
		}
	}

	reg.checkCollisions(add)

	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("invalid registry:\n  - %s", strings.Join(errs, "\n  - "))
}

// checkCollisions reports declarations that resolve to the same container.
// Two entries for one directory are always a mistake and never a pair of repos:
// only one of them can own that clone, so the other's settings vanish silently,
// while every command over the declared ∪ discovered union lists the repo twice
// and sync does its work on it twice. The key is the container rather than the
// identity because one id declared under two roots is two distinct clones in two
// places, which stays legal.
func (reg *Registry) checkCollisions(add func(string, ...any)) {
	byDir := map[string][]string{}
	var dirs []string
	for _, m := range reg.members() {
		r, err := reg.effective(m)
		if err != nil {
			continue // already reported, with its own parse error
		}
		dir := filepath.Clean(r.Container())
		if len(byDir[dir]) == 0 {
			dirs = append(dirs, dir)
		}
		byDir[dir] = append(byDir[dir], fmt.Sprintf("%s in root %q", r.ID, m.root))
	}
	for _, dir := range dirs {
		if decls := byDir[dir]; len(decls) > 1 {
			add("%s: declared %d times (%s) — one container holds one repo, so all but one declaration is ignored",
				dir, len(decls), strings.Join(distinct(decls), ", "))
		}
	}
}

// settingsOf lifts the enum-bearing fields of a resolved repo back into a
// Settings so checkEnums can validate the effective values too.
func settingsOf(r model.Repo) Settings {
	return Settings{
		Layout:       &r.Layout,
		Push:         &r.Push,
		TaskBranches: &r.TaskBranches,
		ShowBranches: &r.ShowBranches,
		Prune:        &r.Prune,
		Workflow:     &r.Workflow,
	}
}

// checkScanLimit rejects a merge_scan_limit below -1. The two negative-adjacent
// values that mean something are spelled out, because "-1 disables the limit"
// and "0 disables the tiers" are easy to swap and the failure would be silent:
// one makes a sweep slower than it should be, the other quietly stops finding
// squash merges.
func checkScanLimit(add func(string, ...any), where string, s Settings) {
	if s.MergeScanLimit != nil && *s.MergeScanLimit < gitx.ScanUnlimited {
		add("%s: merge_scan_limit = %d (want -1 for no limit, 0 to skip the patch tiers, or a commit count)",
			where, *s.MergeScanLimit)
	}
}

func checkEnums(add func(string, ...any), where string, s Settings) {
	check := func(field string, v *string, allowed []string) {
		if v != nil && !contains(allowed, *v) {
			add("%s: %s = %q (want one of %s)", where, field, *v, strings.Join(allowed, ", "))
		}
	}
	check("layout", s.Layout, validLayouts)
	check("workflow", s.Workflow, validWorkflows)
	check("push", s.Push, validPush)
	check("task_branches", s.TaskBranches, validTaskBranches)
	check("show_branches", s.ShowBranches, validShowBranches)
	check("prune", s.Prune, validPrune)
}

// checkGlobs validates force_push/force_pull entries compile as glob patterns
// (DESIGN §5.2) — an "*" is special-cased elsewhere to mean every branch, but
// still must round-trip through path.Match without error here.
func checkGlobs(add func(string, ...any), where string, s globSettings) {
	check := func(field string, patterns []string) {
		for _, p := range patterns {
			if _, err := path.Match(p, "x"); err != nil {
				add("%s: %s: %q: %v", where, field, p, err)
			}
		}
	}
	check("force_push", s.ForcePush)
	check("force_pull", s.ForcePull)
	check("expected_untracked", s.ExpectedUntracked)
	check("expected_uncommitted", s.ExpectedUncommitted)
}

// globSettings is the set of glob-bearing fields checkGlobs validates, so adding
// a new pattern list is one field here rather than another parameter at every
// call site.
type globSettings struct {
	ForcePush           []string
	ForcePull           []string
	ExpectedUntracked   []string
	ExpectedUncommitted []string
}

func globsOfSettings(s Settings) globSettings {
	return globSettings{
		ForcePush:           s.ForcePush,
		ForcePull:           s.ForcePull,
		ExpectedUntracked:   s.ExpectedUntracked,
		ExpectedUncommitted: s.ExpectedUncommitted,
	}
}

func globsOfRepo(r model.Repo) globSettings {
	return globSettings{
		ForcePush:           r.ForcePush,
		ForcePull:           r.ForcePull,
		ExpectedUntracked:   r.ExpectedUntracked,
		ExpectedUncommitted: r.ExpectedUncommitted,
	}
}

// distinct drops repeats while keeping order, so a repo declared twice under one
// root names itself once in the message rather than echoing.
func distinct(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
