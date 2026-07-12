package config

import (
	"fmt"
	"sort"
	"strings"

	"github.com/michael-odell/repo/internal/model"
)

// enum sets for validated fields.
var (
	validLayouts   = []string{model.LayoutFlat, model.LayoutOwner}
	validWorkflows = []string{model.UpstreamPush, model.ForkPR, model.SupplyChainMirror, model.Vendor}
	validRewrite   = []string{"stop", "follow"}
	validPrune     = []string{"auto", "report", "manual"}
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
	}
	checkEnums(add, "defaults", reg.defaults)

	// effective() surfaces id/fork parse errors and undervable forks; resolving
	// Physical additionally catches unknown hosts. Report the first structural
	// failure precisely, then per-repo issues.
	repos, err := reg.Repos()
	if err != nil {
		add("%v", err)
	} else {
		for _, r := range repos {
			checkEnums(add, r.ID.String(), settingsOf(r))
			if _, err := reg.Physical(r); err != nil {
				add("%v", err)
			}
		}
	}

	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("invalid registry:\n  - %s", strings.Join(errs, "\n  - "))
}

// settingsOf lifts the enum-bearing fields of a resolved repo back into a
// Settings so checkEnums can validate the effective values too.
func settingsOf(r model.Repo) Settings {
	return Settings{
		Layout:    &r.Layout,
		OnRewrite: &r.OnRewrite,
		Prune:     &r.Prune,
		Workflow:  &r.Workflow,
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
	check("on_rewrite", s.OnRewrite, validRewrite)
	check("prune", s.Prune, validPrune)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
