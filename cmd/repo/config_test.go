package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

// TestConfigOutputRoundTripsAsToml (DESIGN §7): the default output is meant to
// be pasteable straight back as a [[root.*.repo]]/[[dir.*.repo]] override —
// config in, config out — so it has to decode as valid TOML carrying the same
// values, not just look like TOML.
func TestConfigOutputRoundTripsAsToml(t *testing.T) {
	reg := loadReg(t, `
[hosts.github]
base = "git@github.com:"

[root.contrib]
dir      = "~/contrib"
layout   = "owner"
workflow = "vendor"
pin      = "latest-tag"
repos    = ["github:prometheus/prometheus"]
`)
	repos, err := unionRepos(reg)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := find(repos, "prometheus/prometheus")
	if !ok {
		t.Fatal("prometheus/prometheus not found")
	}

	var buf bytes.Buffer
	if err := renderConfig(&buf, r); err != nil {
		t.Fatalf("renderConfig: %v", err)
	}

	var out configOut
	if _, err := toml.Decode(buf.String(), &out); err != nil {
		t.Fatalf("output does not parse as TOML: %v\n%s", err, buf.String())
	}
	if out.ID != "github:prometheus/prometheus" || out.Workflow != "vendor" || out.Pin != "latest-tag" {
		t.Errorf("round-tripped id/workflow/pin = %q/%q/%q, want github:prometheus/prometheus/vendor/latest-tag",
			out.ID, out.Workflow, out.Pin)
	}
	if out.Layout != "owner" || out.Worktrees {
		t.Errorf("round-tripped layout/worktrees = %q/%v, want owner/false", out.Layout, out.Worktrees)
	}
}

// TestConfigOmitsUsuallyEmptyFields: the fields nearly every repo leaves unset
// (force_push, prune_keep, hooks, ...) are the ones a first glance shouldn't
// have to scroll past — the verbosity complaint --explain doesn't try to solve
// (that's its job), but the default view does.
func TestConfigOmitsUsuallyEmptyFields(t *testing.T) {
	reg := loadReg(t, `
[hosts.github]
base = "git@github.com:"

[root.contrib]
dir   = "~/contrib"
repos = ["github:acme/thing"]
`)
	repos, err := unionRepos(reg)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := find(repos, "acme/thing")
	if !ok {
		t.Fatal("acme/thing not found")
	}
	var buf bytes.Buffer
	if err := renderConfig(&buf, r); err != nil {
		t.Fatalf("renderConfig: %v", err)
	}
	for _, absent := range []string{"force_push", "force_pull", "prune_keep", "prune_min_age",
		"merge_scan_limit", "expected_untracked", "expected_uncommitted", "hooks", "fork", "pin"} {
		if strings.Contains(buf.String(), absent) {
			t.Errorf("output should omit unset %q; got:\n%s", absent, buf.String())
		}
	}
}

// TestExplainNamesTheOverridingDirNode: the whole point of --explain is
// catching an override that silently isn't landing, so its source for a field
// a [dir.*] node set has to name that node, not just "somewhere in config".
func TestExplainNamesTheOverridingDirNode(t *testing.T) {
	reg := loadReg(t, `
[hosts.github]
base = "git@github.com:"

[root.contrib]
dir      = "~/contrib"
layout   = "owner"
workflow = "upstream-push"

[dir.contrib-prometheus]
dir      = "~/contrib/prometheus"
workflow = "vendor"
repos    = ["github:prometheus/prometheus"]
`)
	repos, err := unionRepos(reg)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := find(repos, "prometheus/prometheus")
	if !ok {
		t.Fatal("prometheus/prometheus not found")
	}
	got := reg.Explain(r.ID.String(), r.Roots)
	if got["workflow"] != "dir.contrib-prometheus" {
		t.Errorf(`Explain()["workflow"] = %q, want "dir.contrib-prometheus"`, got["workflow"])
	}
}

// TestExplainLabelsWorkflowDefaultedFields: push/task_branches/show_branches
// fall back to WorkflowDefaults, not one fixed builtin, when nothing sets
// them — mislabeling that "builtin" would show a value (vendor's push =
// "never") that looks like it contradicts the source given for it (the fixed
// builtin default is "manual").
func TestExplainLabelsWorkflowDefaultedFields(t *testing.T) {
	reg := loadReg(t, `
[hosts.github]
base = "git@github.com:"

[root.contrib]
dir      = "~/contrib"
workflow = "vendor"
repos    = ["github:acme/thing"]
`)
	repos, err := unionRepos(reg)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := find(repos, "acme/thing")
	if !ok {
		t.Fatal("acme/thing not found")
	}
	var buf bytes.Buffer
	if err := renderExplain(&buf, reg.Explain(r.ID.String(), r.Roots), r); err != nil {
		t.Fatalf("renderExplain: %v", err)
	}
	line := lineWith(t, strings.Split(buf.String(), "\n"), "push")
	if !strings.Contains(line, "never") || !strings.Contains(line, "workflow default") {
		t.Errorf("push line = %q, want value never and source \"workflow default\"", line)
	}
}

// TestFindOneByExactPath: config is single-repo, so unlike sync's selectors a
// path here must land on exactly one repo's own container.
func TestFindOneByExactPath(t *testing.T) {
	reg := loadReg(t, `
[hosts.github]
base = "git@github.com:"

[root.contrib]
dir    = "~/contrib"
layout = "flat"
repos  = ["github:acme/thing"]
`)
	repos, err := unionRepos(reg)
	if err != nil {
		t.Fatal(err)
	}
	r, ok := findOne(repos, "~/contrib/thing")
	if !ok || r.ID.String() != "github:acme/thing" {
		t.Fatalf("findOne(~/contrib/thing) = %v, %v", r, ok)
	}
	if _, ok := findOne(repos, "~/contrib"); ok {
		t.Errorf("findOne(~/contrib) should not match the root's own dir, only a repo's container")
	}
}
