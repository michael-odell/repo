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

// TestWorkflowDefaults locks in the per-workflow push/task_branches defaults
// (DESIGN §3.6): a workflow is a named bundle of defaults over one shared
// settings surface, not a set of per-workflow-only switches.
func TestWorkflowDefaults(t *testing.T) {
	cases := []struct {
		workflow, push, task string
	}{
		{model.UpstreamPush, "manual", "report"},
		{model.ForkPR, "auto", "auto"},
		{model.SupplyChainMirror, "never", "pull-only"},
		{model.Vendor, "never", "pull-only"},
	}
	for _, c := range cases {
		push, task := WorkflowDefaults(c.workflow)
		if push != c.push || task != c.task {
			t.Errorf("WorkflowDefaults(%q) = %q, %q; want %q, %q", c.workflow, push, task, c.push, c.task)
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
