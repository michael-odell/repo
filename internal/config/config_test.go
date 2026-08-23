package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-odell/repo/internal/model"
)

func load(t *testing.T, fragments ...string) *Registry {
	t.Helper()
	paths := make([]string, len(fragments))
	for i, f := range fragments {
		paths[i] = filepath.Join("testdata", f)
	}
	reg, err := Load(paths)
	if err != nil {
		t.Fatalf("Load(%v): %v", fragments, err)
	}
	return reg
}

func repoByShort(t *testing.T, reg *Registry, short string) model.Repo {
	t.Helper()
	repos, err := reg.Repos()
	if err != nil {
		t.Fatalf("Repos(): %v", err)
	}
	for _, r := range repos {
		if r.ID.Short() == short {
			return r
		}
	}
	t.Fatalf("repo %q not found", short)
	return model.Repo{}
}

func TestInheritance(t *testing.T) {
	reg := load(t, "base.toml")

	pt := repoByShort(t, reg, "pt-helm")
	if pt.HomeRoot != "~/wd" || pt.Layout != model.LayoutOwner || !pt.Worktrees {
		t.Errorf("pt-helm inherited wrong from root work: %+v", pt)
	}
	// fork was derived from the work root's fork_owner, not stated on the repo.
	if pt.Fork == nil || pt.Fork.String() != "ghe:michael-odell/pt-helm" {
		t.Errorf("pt-helm derived fork = %v, want ghe:michael-odell/pt-helm", pt.Fork)
	}
	if pt.Workflow != model.ForkPR {
		t.Errorf("pt-helm workflow = %q, want fork-pr", pt.Workflow)
	}
	if pt.Push != "auto" || pt.TaskBranches != "auto" {
		t.Errorf("pt-helm push/task_branches = %q/%q, want auto/auto (fork-pr default)", pt.Push, pt.TaskBranches)
	}
	if len(pt.Branches) != 2 || pt.Branches[0] != "main" || pt.Branches[1] != "release" {
		t.Errorf("pt-helm branches = %v, want [main release]", pt.Branches)
	}
	if pt.Fork == nil || pt.Fork.Short() != "pt-helm" {
		t.Errorf("pt-helm fork = %v", pt.Fork)
	}
	if len(pt.Hooks) != 1 || pt.Hooks[0].After != "fetch" {
		t.Errorf("pt-helm hooks = %v", pt.Hooks)
	}

	home, _ := os.UserHomeDir()
	if got, want := pt.Container(), filepath.Join(home, "wd/cban-ops/pt-helm"); got != want {
		t.Errorf("Container() = %q, want %q", got, want)
	}
	if got, want := pt.PrimaryTree(), filepath.Join(home, "wd/cban-ops/pt-helm/main"); got != want {
		t.Errorf("PrimaryTree() = %q, want %q", got, want)
	}

	// A repo with no fork and no explicit workflow keeps the default.
	zh := repoByShort(t, reg, "zsh-history")
	if zh.Workflow != model.UpstreamPush {
		t.Errorf("zsh-history workflow = %q, want upstream-push", zh.Workflow)
	}
	if zh.HomeRoot != "~/.zsh/plugins" {
		t.Errorf("zsh-history home_root = %q, want ~/.zsh/plugins", zh.HomeRoot)
	}
	if zh.Push != "manual" || zh.TaskBranches != "report" {
		t.Errorf("zsh-history push/task_branches = %q/%q, want manual/report (upstream-push default)", zh.Push, zh.TaskBranches)
	}

	// supply-chain-mirror defaults to never/pull-only, but every setting stays
	// overridable (DESIGN §3.6) — it's a default, not a hard rule.
	p10k := repoByShort(t, reg, "powerlevel10k")
	if p10k.Push != "never" || p10k.TaskBranches != "pull-only" {
		t.Errorf("powerlevel10k push/task_branches = %q/%q, want never/pull-only (mirror default)", p10k.Push, p10k.TaskBranches)
	}
}

// TestWorkflowDefaults locks in the per-workflow push/task_branches/show_branches
// defaults (DESIGN §3.6): a workflow is a named bundle of defaults over one
// shared settings surface, not a set of per-workflow-only switches.
//
// show_branches is "notable" — never "none" — for the read-only workflows: a
// vendored or mirrored repo has no parked work to nudge you about, but it is
// exactly where a finding most needs to surface.
func TestWorkflowDefaults(t *testing.T) {
	cases := []struct {
		workflow, push, task, show string
	}{
		{model.UpstreamPush, "manual", "report", "unmerged"},
		{model.ForkPR, "auto", "auto", "unmerged"},
		{model.SupplyChainMirror, "never", "pull-only", "notable"},
		{model.Vendor, "never", "pull-only", "notable"},
	}
	for _, c := range cases {
		push, task, show := WorkflowDefaults(c.workflow)
		if push != c.push || task != c.task || show != c.show {
			t.Errorf("WorkflowDefaults(%q) = %q, %q, %q; want %q, %q, %q",
				c.workflow, push, task, show, c.push, c.task, c.show)
		}
	}
}

// TestPushTaskBranchesOverridable confirms a repo can override its workflow's
// push/task_branches default in either direction (DESIGN §3.6) — including
// overriding a mirror's "never"/"pull-only" default to "auto", which only
// affects local↔fork traffic and can't itself bypass the review gate (§5.4).
func TestPushTaskBranchesOverridable(t *testing.T) {
	reg := load(t, "base.toml", "push_override.toml")
	p10k := repoByShort(t, reg, "powerlevel10k")
	if p10k.Push != "auto" || p10k.TaskBranches != "auto" {
		t.Errorf("powerlevel10k push/task_branches = %q/%q, want auto/auto (explicit override)", p10k.Push, p10k.TaskBranches)
	}
	if len(p10k.ForcePush) != 1 || p10k.ForcePush[0] != "wip/*" {
		t.Errorf("powerlevel10k force_push = %v, want [wip/*]", p10k.ForcePush)
	}
}

func TestPhysicalNoOverlay(t *testing.T) {
	reg := load(t, "base.toml")
	p10k := repoByShort(t, reg, "powerlevel10k")
	got, err := reg.Physical(p10k)
	if err != nil {
		t.Fatal(err)
	}
	if want := "git@github.com:romkatv/powerlevel10k"; got != want {
		t.Errorf("Physical = %q, want %q", got, want)
	}
}

func TestPhysicalWithFold(t *testing.T) {
	reg := load(t, "base.toml", "overlay.toml")

	// apply_to = "*" folds the plugin through the private host, owner preserved.
	p10k := repoByShort(t, reg, "powerlevel10k")
	got, err := reg.Physical(p10k)
	if err != nil {
		t.Fatal(err)
	}
	if want := "git@gogsprod.example.com:mirrors/romkatv/powerlevel10k"; got != want {
		t.Errorf("folded Physical = %q, want %q", got, want)
	}

	// An explicit override wins over the fold.
	pt := repoByShort(t, reg, "pt-helm")
	got, err = reg.Physical(pt)
	if err != nil {
		t.Fatal(err)
	}
	if want := "git@gogsprod.example.com:team/pt-helm"; got != want {
		t.Errorf("override Physical = %q, want %q", got, want)
	}
}

func TestUnknownKeysRejected(t *testing.T) {
	dir := t.TempDir()
	// The pre-pivot schema: home_root/tags/[[repo]] no longer exist.
	writeTOML(t, dir, `
[defaults]
home_root = "~/src"
[[repo]]
id = "github:me/x"
tags = ["work"]
`)
	_, err := Load([]string{dir})
	if err == nil || !strings.Contains(err.Error(), "unknown key") {
		t.Fatalf("want unknown-key error, got %v", err)
	}
	for _, want := range []string{"defaults.home_root", "repo", "repo.tags"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should name %q; got %v", want, err)
		}
	}
}

func TestValidateSemantics(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[hosts.github]
base = "git@github.com:"

[root.bad]
dir           = "~/bad"
workflow      = "teleport"
push          = "sometimes"
task_branches = "ignore"
force_push    = ["["]

[[root.bad.repo]]
id = "github:me/x"

[root.nodir]
layout = "flat"
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = reg.Validate()
	if err == nil {
		t.Fatal("want validation error, got nil")
	}
	for _, want := range []string{
		`workflow = "teleport"`, `root "nodir": missing`,
		`push = "sometimes"`, `task_branches = "ignore"`,
		`force_push: "["`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validation error should mention %q; got:\n%v", want, err)
		}
	}
}

func TestForkOwnerRequiredWhenWorkflowNeedsFork(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[hosts.github]
base = "git@github.com:"
[root.r]
dir = "~/r"
[[root.r.repo]]
id = "github:me/x"
workflow = "fork-pr"
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if _, err := reg.Repos(); err == nil || !strings.Contains(err.Error(), "fork_owner") {
		t.Fatalf("want fork_owner error, got %v", err)
	}
}

func TestInheritedForDiscovered(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[root.contrib]
dir      = "~/contrib"
workflow = "vendor"
pin      = "latest-tag"

[root.src]
dir = "~/src"
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// A repo under the contrib root inherits vendor + pin, overriding whatever a
	// discovered repo's remotes would infer.
	if got := reg.InheritedFor([]string{"contrib"}); got.Workflow != "vendor" || got.Pin != "latest-tag" {
		t.Errorf("contrib inherited = %+v, want workflow=vendor pin=latest-tag", got)
	}
	// A root that sets no workflow leaves it unset, so the caller keeps its
	// remote-inferred value rather than being forced to a default.
	if got := reg.InheritedFor([]string{"src"}); got.Workflow != "" {
		t.Errorf("src inherited workflow = %q, want empty (caller infers)", got.Workflow)
	}
}

// TestFragmentReadOnce covers a path list that reaches one fragment twice — a
// directory plus a file inside it, and a symlink into a checkout the path also
// names directly. Settings would overlay themselves harmlessly, but declared
// members append, so a second read would double every repo the fragment
// declares.
func TestFragmentReadOnce(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[hosts.github]
base = "git@github.com:"
[root.r]
dir = "~/r"
[[root.r.repo]]
id = "github:me/x"
`)
	frag := filepath.Join(dir, "f.toml")
	link := filepath.Join(t.TempDir(), "linked.toml")
	if err := os.Symlink(frag, link); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{
		{dir, frag},       // the directory and a file inside it
		{dir, dir},        // the same path entry twice
		{dir, link},       // two routes to one file, one a symlink
		{frag, dir, link}, // all three, in one path list
	} {
		reg, err := Load(paths)
		if err != nil {
			t.Fatalf("Load(%v): %v", paths, err)
		}
		repos, err := reg.Repos()
		if err != nil {
			t.Fatalf("Repos(): %v", err)
		}
		if len(repos) != 1 {
			t.Errorf("Load(%v) declared %d repos, want 1: %v", paths, len(repos), repos)
		}
	}
}

// TestDuplicateDeclarationRejected locks in that a repo declared twice for one
// container is a config error rather than a silently doubled row. The same id
// under two roots is two clones in two places and stays legal.
func TestDuplicateDeclarationRejected(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[hosts.github]
base = "git@github.com:"

[root.wd]
dir    = "~/wd"
layout = "owner"
repos  = ["github:acme/thing"]

[[root.wd.repo]]
id = "github:acme/thing"

[root.other]
dir    = "~/other"
layout = "owner"
repos  = ["github:acme/thing"]
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = reg.Validate()
	if err == nil {
		t.Fatal("want validation error for the doubled declaration, got nil")
	}
	for _, want := range []string{"wd/acme/thing", "declared 2 times", `github:acme/thing in root "wd"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q; got:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "other/acme/thing") {
		t.Errorf("same id under a second root is a separate clone, not a collision; got:\n%v", err)
	}
}

// TestMergeScanLimitInherits: the expensive half of merge detection is switched
// off per repo, so the setting has to reach a repo the same way every other
// setting does — stated at the root, overridden on the entry it doesn't suit.
func TestMergeScanLimitInherits(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[hosts.gh]
base = "git@github.com:"

[root.wd]
dir              = "~/wd"
merge_scan_limit = 250

[[root.wd.repo]]
id = "gh:acme/normal"

[[root.wd.repo]]
id               = "gh:acme/ancient"
merge_scan_limit = 0    # patch tiers off: this one is a graveyard of old branches

[[root.wd.repo]]
id               = "gh:acme/monorepo"
merge_scan_limit = -1   # no limit: worth the cost here
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, c := range []struct {
		short string
		want  int
	}{
		{"normal", 250},  // from the root
		{"ancient", 0},   // off
		{"monorepo", -1}, // unlimited
	} {
		got := repoByShort(t, reg, c.short).MergeScanLimit
		if got == nil || *got != c.want {
			t.Errorf("%s merge_scan_limit = %v, want %d", c.short, got, c.want)
		}
	}
}

// TestMergeScanLimitRejectsNonsense: -1 and 0 both mean something, and they mean
// opposite things, so anything below them is a typo rather than an intention.
func TestMergeScanLimitRejectsNonsense(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[root.wd]
dir              = "~/wd"
merge_scan_limit = -5
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = reg.Validate()
	if err == nil || !strings.Contains(err.Error(), "merge_scan_limit = -5") {
		t.Fatalf("want a merge_scan_limit error, got %v", err)
	}
}

func writeTOML(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "f.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUnknownHost(t *testing.T) {
	reg := load(t, "base.toml")
	// Point a repo at a host with no [hosts.*] entry.
	repos, _ := reg.Repos()
	r := repos[0]
	r.ID.Host = "nowhere"
	_, err := reg.Physical(r)
	if err == nil || !strings.Contains(err.Error(), "unknown host") {
		t.Errorf("want unknown-host error, got %v", err)
	}
}

// TestDirOverlayOverridesJustItsSubtree (DESIGN §3.9): a [dir.*] node scoped to
// one owner under an owner-layout root overrides settings for repos declared
// there, while a repo under the same root but a different owner still gets the
// root's own settings.
func TestDirOverlayOverridesJustItsSubtree(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[hosts.github]
base = "git@github.com:"

[root.contrib]
dir      = "~/contrib"
layout   = "owner"
workflow = "upstream-push"
repos    = ["github:acme/other"]

[dir.contrib-prometheus]
dir      = "~/contrib/prometheus"
workflow = "vendor"
pin      = "latest-tag"
repos    = ["github:prometheus/prometheus"]
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	prom := repoByShort(t, reg, "prometheus")
	if prom.Workflow != "vendor" || prom.Pin != "latest-tag" {
		t.Errorf("prometheus workflow/pin = %q/%q, want vendor/latest-tag (from the dir overlay)",
			prom.Workflow, prom.Pin)
	}
	// The chain still reaches the covering root — a bare root name selects
	// repos under a nested dir too (cmd/repo's `sync <root>`).
	if !contains(prom.Roots, "contrib") || !contains(prom.Roots, "dir.contrib-prometheus") {
		t.Errorf("prometheus chain = %v, want both contrib and dir.contrib-prometheus", prom.Roots)
	}

	other := repoByShort(t, reg, "other")
	if other.Workflow != "upstream-push" || other.Pin != "" {
		t.Errorf("other workflow/pin = %q/%q, want upstream-push/\"\" (untouched by the overlay)",
			other.Workflow, other.Pin)
	}

	// The critical placement check: HomeRoot must stay the covering ROOT's dir,
	// never the dir node's own (deeper) dir, or Container() double-nests the
	// owner segment (~/contrib/prometheus/prometheus/prometheus instead of
	// ~/contrib/prometheus/prometheus).
	home, _ := os.UserHomeDir()
	if got, want := prom.Container(), filepath.Join(home, "contrib/prometheus/prometheus"); got != want {
		t.Errorf("prometheus Container() = %q, want %q", got, want)
	}
}

// TestDirCannotSetLayout (DESIGN §3.9): layout decides where a repo's container
// lives, computed from its root alone — a [dir.*] node overriding it would make
// --fix's placement decision depend on an override only reachable once the
// repo is already there. Validate rejects it outright rather than resolving it
// silently one way or the other.
func TestDirCannotSetLayout(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[root.contrib]
dir = "~/contrib"

[dir.sub]
dir    = "~/contrib/sub"
layout = "flat"
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = reg.Validate()
	if err == nil || !strings.Contains(err.Error(), "`layout` is root-only") {
		t.Fatalf("want a layout-is-root-only error, got %v", err)
	}
}

// TestDirWorktreesIsFine: unlike layout, worktrees reshapes a container in
// place rather than relocating it, so a [dir.*] node may set it freely.
func TestDirWorktreesIsFine(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[hosts.github]
base = "git@github.com:"

[root.contrib]
dir = "~/contrib"

[dir.sub]
dir       = "~/contrib/sub"
worktrees = true
repos     = ["github:acme/thing"]
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if r := repoByShort(t, reg, "thing"); !r.Worktrees {
		t.Errorf("thing.Worktrees = false, want true (from the dir overlay)")
	}
}

// TestOrphanDirRejected (DESIGN §3.9): a [dir.*] node overlays part of a root's
// tree; one that covers ground no root ever walks would never apply to
// anything a sweep could find, so it's a config error rather than a silent
// no-op.
func TestOrphanDirRejected(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[root.contrib]
dir = "~/contrib"

[dir.orphan]
dir = "~/elsewhere/sub"
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = reg.Validate()
	if err == nil || !strings.Contains(err.Error(), `dir "orphan": dir "~/elsewhere/sub" is not under any root`) {
		t.Fatalf("want an orphan-dir error, got %v", err)
	}
}

// TestDirAndRootNamespacesAreSeparate (DESIGN §3.9): a [dir.*] and a [root.*]
// may share a name without colliding — the geometry (each node's own `dir`)
// decides everything, never the label.
func TestDirAndRootNamespacesAreSeparate(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[hosts.github]
base = "git@github.com:"

[root.contrib]
dir      = "~/contrib"
workflow = "upstream-push"

[dir.contrib]
dir      = "~/contrib/sub"
workflow = "vendor"
repos    = ["github:acme/thing"]
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if r := repoByShort(t, reg, "thing"); r.Workflow != "vendor" {
		t.Errorf("thing.Workflow = %q, want vendor (the dir node, not the same-named root)", r.Workflow)
	}
}

// TestExplainNamesEachLink (DESIGN §7): repo config --explain's per-field
// provenance — a field set at [defaults], overridden at the root, overridden
// again at the dir overlay, and overridden a final time on the repo's own
// entry, with a field nobody touched absent entirely.
func TestExplainNamesEachLink(t *testing.T) {
	dir := t.TempDir()
	writeTOML(t, dir, `
[hosts.github]
base = "git@github.com:"

[defaults]
prune = "auto"

[root.contrib]
dir   = "~/contrib"
push  = "manual"

[dir.contrib-sub]
dir      = "~/contrib/sub"
workflow = "vendor"

[[dir.contrib-sub.repo]]
id       = "github:acme/thing"
workflow = "upstream-push"
`)
	reg, err := Load([]string{dir})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := reg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	r := repoByShort(t, reg, "thing")
	got := reg.Explain(r.ID.String(), r.Roots)
	want := map[string]string{
		"prune":    "defaults",
		"push":     "contrib",
		"workflow": "repo", // the entry's own override outranks the dir overlay
	}
	for field, wantSrc := range want {
		if got[field] != wantSrc {
			t.Errorf("Explain()[%q] = %q, want %q", field, got[field], wantSrc)
		}
	}
	if _, ok := got["pin"]; ok {
		t.Errorf(`Explain()["pin"] = %q, want absent (nothing set it)`, got["pin"])
	}
}
