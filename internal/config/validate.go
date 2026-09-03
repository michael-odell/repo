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

// enumValue is one permitted value of a setting, and whether any code acts on
// it (DESIGN §3.4).
//
// The zero value is *not* implemented, which is the whole point: a value added
// to one of the sets below without someone claiming it is rejected at load
// rather than accepted and ignored. `prune = "auto"` sat in a live registry for
// months that way — valid key, valid value, no consumer, no warning — and the
// only reason anyone found out was going looking.
type enumValue struct {
	name        string
	implemented bool
}

// done marks a value some consumer honours. It is a claim about the code, so
// each consumer package holds it to the code by test: see
// TestEveryPruneModeIsImplemented, which asserts not merely that every mode is
// handled but that no two of them behave the same.
func done(name string) enumValue { return enumValue{name: name, implemented: true} }

// planned marks a value the design has settled and the code has not caught up
// with. It parses, and is then refused with a message saying so — which is the
// difference between "you typed this wrong" and "this doesn't work yet", and
// the one a reader of a rejected config actually needs.
func planned(name string) enumValue { return enumValue{name: name} }

// enum sets for validated fields.
var (
	validLayouts      = []enumValue{done(model.LayoutFlat), done(model.LayoutOwner)}
	validWorkflows    = []enumValue{done(model.UpstreamPush), done(model.ForkPR), done(model.SupplyChainMirror), done(model.Vendor)}
	validPush         = []enumValue{done("auto"), done("manual"), done("never")}
	validTaskBranches = []enumValue{done("auto"), done("report"), done("pull-only")}
	validShowBranches = []enumValue{done("none"), done("notable"), done("unmerged"), done("landed"), done("all")}
	validPrune        = []enumValue{done("manual"), done("report"), done("interactive"), done("auto")}

	// validHookEvents is what a hook's `after` may name. Only "fetch" runs
	// today (DESIGN §5's per-repo order: provision → fetch → update branches →
	// hooks → drift); the same machinery guards it so a second event cannot
	// reach a registry before something runs it.
	validHookEvents = []enumValue{done("fetch")}
)

// PruneModes lists the prune modes the sweep actually implements, in the order
// DESIGN §5.3 ranks them. Exported for the consumer's own test to check itself
// against — the declaration above is a claim, and this is how it is held to
// account rather than trusted.
func PruneModes() []string { return usable(validPrune) }

// HookEvents lists the hook lifecycle points the sync engine implements, on the
// same terms as PruneModes: the declaration above is a claim, and the consumer's
// own test (TestEveryHookEventRuns) is what holds it to the code.
func HookEvents() []string { return usable(validHookEvents) }

// usable is the implemented values of an enum, which is also what an error
// message should offer: naming a value that parses but does nothing as an
// option would be advice to hit the same wall again.
func usable(values []enumValue) []string {
	var out []string
	for _, v := range values {
		if v.implemented {
			out = append(out, v.name)
		}
	}
	return out
}

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
		checkAges(add, fmt.Sprintf("root %q", n), r.Settings)
		checkHooks(add, fmt.Sprintf("root %q", n), r.Settings.Hooks)
	}
	dirNames := make([]string, 0, len(reg.dirs))
	for n := range reg.dirs {
		dirNames = append(dirNames, n)
	}
	sort.Strings(dirNames)
	for _, n := range dirNames {
		d := reg.dirs[n]
		where := fmt.Sprintf("dir %q", n)
		switch {
		case strings.TrimSpace(d.Dir) == "":
			add("%s: missing `dir`", where)
		case !reg.dirNestsUnderARoot(d.Dir):
			add("%s: dir %q is not under any root's `dir` — a [dir.*] node overlays "+
				"part of a root's tree (DESIGN §3.9), not a tree of its own", where, d.Dir)
		}
		if d.Layout != nil {
			add("%s: `layout` is root-only — a [dir.*] node can't change where a repo "+
				"lives, only how it's treated once it's there (DESIGN §3.9)", where)
		}
		checkEnums(add, where, d.Settings)
		checkGlobs(add, where, globsOfSettings(d.Settings))
		checkScanLimit(add, where, d.Settings)
		checkAges(add, where, d.Settings)
		checkHooks(add, where, d.Settings.Hooks)
	}

	checkEnums(add, "defaults", reg.defaults)
	checkGlobs(add, "defaults", globsOfSettings(reg.defaults))
	checkScanLimit(add, "defaults", reg.defaults)
	checkAges(add, "defaults", reg.defaults)
	checkHooks(add, "defaults", reg.defaults.Hooks)

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
			checkHooks(add, r.ID.String(), r.Hooks)
			checkPinReachesItsTag(add, r)
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

// dirNestsUnderARoot reports whether dir sits at or below some root's `dir` —
// the requirement a [dir.*] node's own `dir` must meet (DESIGN §3.9): it
// overlays part of a root's tree, so a dir claiming ground no root's scan ever
// reaches would silently never apply to anything a sweep could find.
func (reg *Registry) dirNestsUnderARoot(dir string) bool {
	target := expandHome(dir)
	for _, r := range reg.roots {
		if r.Dir != "" && pathHasPrefix(target, expandHome(r.Dir)) {
			return true
		}
	}
	return false
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

// checkAges rejects a duration nothing can read. Both settings spelled in the
// age syntax are checked here, at every tier rather than only where a repo
// resolves, because a root that currently declares no repos still holds the
// setting for whatever lands under it later, and a typo that waits for its
// first repo to surface is a typo that surfaces during someone else's task.
//
// A bad sync_frequency is the quieter of the two: it only shows up under
// --if-due, which is the run nobody watches — the backgrounded one from shell
// startup.
func checkAges(add func(string, ...any), where string, s Settings) {
	for _, f := range []struct {
		field string
		value *string
	}{
		{"prune_min_age", s.PruneMinAge},
		{"sync_frequency", s.SyncFrequency},
	} {
		if f.value == nil {
			continue
		}
		if _, err := ParseAge(*f.value); err != nil {
			add("%s: %s: %v", where, f.field, err)
		}
	}
}

// enumChecker returns the per-field check, split out from checkEnums so the
// three outcomes it has to keep apart — honoured, not implemented, misspelled —
// can be tested without a whole registry around them.
func enumChecker(add func(string, ...any), where string) func(string, *string, []enumValue) {
	return func(field string, v *string, allowed []enumValue) {
		if v == nil {
			return
		}
		for _, a := range allowed {
			if a.name != *v {
				continue
			}
			// Spelled right, and nothing reads it. Rejected rather than
			// ignored: a setting that would do nothing reads exactly like a
			// working one and behaves exactly like an absent one, and the
			// parser cannot tell them apart on its own (DESIGN §3.4).
			if !a.implemented {
				add("%s: %s = %q is not implemented yet (want one of %s)",
					where, field, *v, strings.Join(usable(allowed), ", "))
			}
			return
		}
		add("%s: %s = %q (want one of %s)", where, field, *v, strings.Join(usable(allowed), ", "))
	}
}

func checkEnums(add func(string, ...any), where string, s Settings) {
	check := enumChecker(add, where)
	check("layout", s.Layout, validLayouts)
	check("workflow", s.Workflow, validWorkflows)
	check("push", s.Push, validPush)
	check("task_branches", s.TaskBranches, validTaskBranches)
	check("show_branches", s.ShowBranches, validShowBranches)
	check("prune", s.Prune, validPrune)
}

// checkHooks holds hooks to the rule every other setting already obeys (DESIGN
// §3.4): a value nothing acts on is refused at load, not accepted and ignored.
// The engine matches `after` exactly and skips whatever it doesn't recognise,
// so `after = "pull"` — or a typo'd "fetchh", or an entry that omits `after`
// altogether — is a maintenance command that never runs, in a registry that
// reads like it does, with nothing said on any sweep. That is the same shape as
// the `prune = "auto"` gap, and it hides better: a hook is usually written for
// one repo, so nobody else's run is odd enough to prompt a second look.
func checkHooks(add func(string, ...any), where string, hooks []model.Hook) {
	check := enumChecker(add, where)
	for i, h := range hooks {
		switch {
		case strings.TrimSpace(h.After) == "":
			add("%s: hooks[%d]: missing `after` (want one of %s)",
				where, i, strings.Join(usable(validHookEvents), ", "))
		default:
			after := h.After
			check(fmt.Sprintf("hooks[%d].after", i), &after, validHookEvents)
		}
		if strings.TrimSpace(h.Run) == "" {
			add("%s: hooks[%d]: missing `run`", where, i)
		}
	}
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
	check("fetch_skip", s.FetchSkip)
	check("prune_keep", s.PruneKeep)
	check("expected_untracked", s.ExpectedUntracked)
	check("expected_uncommitted", s.ExpectedUncommitted)
	checkRefspecGlobs(add, where, "tags", s.Tags)
	checkRefspecGlobs(add, where, "force_tags", s.ForceTags)
}

// checkRefspecGlobs holds the tag lists to the narrower dialect git accepts.
// Every other glob in the registry is matched here by path.Match, but these two
// are handed to git as refspecs (`+refs/tags/<p>:refs/tags/<p>`), where the
// rules differ: git allows at most one "*", and treats "?" and "[...]" as
// ordinary characters rather than wildcards. A pattern that means one thing to
// path.Match and another to git would silently fetch — or overwrite — a
// different set of tags than it reads like, so the overlap is enforced rather
// than documented.
func checkRefspecGlobs(add func(string, ...any), where, field string, patterns []string) {
	for _, p := range patterns {
		switch {
		case p == "":
			add("%s: %s: empty pattern", where, field)
		case strings.Count(p, "*") > 1:
			add("%s: %s: %q: git refspecs allow at most one \"*\"", where, field, p)
		case strings.ContainsAny(p, "?["):
			add("%s: %s: %q: git refspecs have no %q or %q wildcard — only \"*\"",
				where, field, p, "?", "[...]")
		}
	}
}

// checkPinReachesItsTag catches the one combination that would otherwise be
// silently wrong rather than loudly broken. `pin = "latest-tag"` re-resolves the
// highest semver among the tags that are *local* (DESIGN §5.1), so narrowing
// `tags` changes which version the repo pins to — with no error, and a
// plausible-looking answer. A literal pin excluded by `tags` fails visibly
// instead ("tag X missing"), so only latest-tag needs the guard.
func checkPinReachesItsTag(add func(string, ...any), r model.Repo) {
	if r.Pin != "latest-tag" || len(r.Tags) == 0 {
		if r.Pin == "latest-tag" && len(r.Tags) == 0 {
			add("%s: pin = \"latest-tag\" but tags = [] — there would be no tags to pin to",
				r.ID)
		}
		return
	}
	for _, p := range r.Tags {
		if p == "*" {
			return
		}
	}
	add("%s: pin = \"latest-tag\" with tags = %v: the pin resolves against whichever "+
		"tags were fetched, so a narrowed list silently changes which version is pinned "+
		"(use tags = [\"*\"], or pin the tag by name)", r.ID, r.Tags)
}

// globSettings is the set of glob-bearing fields checkGlobs validates, so adding
// a new pattern list is one field here rather than another parameter at every
// call site.
type globSettings struct {
	ForcePush           []string
	ForcePull           []string
	FetchSkip           []string
	PruneKeep           []string
	Tags                []string
	ForceTags           []string
	ExpectedUntracked   []string
	ExpectedUncommitted []string
}

func globsOfSettings(s Settings) globSettings {
	return globSettings{
		ForcePush:           s.ForcePush,
		ForcePull:           s.ForcePull,
		FetchSkip:           s.FetchSkip,
		PruneKeep:           s.PruneKeep,
		Tags:                s.Tags,
		ForceTags:           s.ForceTags,
		ExpectedUntracked:   s.ExpectedUntracked,
		ExpectedUncommitted: s.ExpectedUncommitted,
	}
}

func globsOfRepo(r model.Repo) globSettings {
	return globSettings{
		ForcePush:           r.ForcePush,
		ForcePull:           r.ForcePull,
		FetchSkip:           r.FetchSkip,
		PruneKeep:           r.PruneKeep,
		Tags:                r.Tags,
		ForceTags:           r.ForceTags,
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
