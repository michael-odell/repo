package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadTOML(t *testing.T, body string) *Registry {
	t.Helper()
	p := filepath.Join(t.TempDir(), "r.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := Load([]string{p})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestTagDefaultsAreUniform: `tags`/`force_tags` deliberately do *not* vary by
// workflow. Every workflow fetches every tag and forces none, because nothing
// about how a repo is used predicts whether its upstream rewrites tags — that's
// a property of the upstream, and only an explicit opt-in can state it.
func TestTagDefaultsAreUniform(t *testing.T) {
	reg := loadTOML(t, `
[hosts.gh]
base = "git@gh:"
[root.r]
dir = "~/src"
[[root.r.repo]]
id = "gh:a/up"
[[root.r.repo]]
id = "gh:a/fork"
workflow = "fork-pr"
fork = "gh:me/fork"
[[root.r.repo]]
id = "gh:a/vend"
workflow = "vendor"
[[root.r.repo]]
id = "gh:a/mirror"
workflow = "supply-chain-mirror"
fork = "gh:me/mirror"
`)
	repos, err := reg.Repos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 4 {
		t.Fatalf("got %d repos, want 4", len(repos))
	}
	for _, r := range repos {
		if len(r.Tags) != 1 || r.Tags[0] != "*" {
			t.Errorf("%s (%s): tags = %v, want [*]", r.ID, r.Workflow, r.Tags)
		}
		if len(r.ForceTags) != 0 {
			t.Errorf("%s (%s): force_tags = %v, want empty — no forced overwrite without an opt-in",
				r.ID, r.Workflow, r.ForceTags)
		}
	}
}

// TestEmptyTagsIsADecision: `tags = []` must survive inheritance as "fetch no
// tags" rather than being mistaken for "unset" and falling back to every tag.
func TestEmptyTagsIsADecision(t *testing.T) {
	reg := loadTOML(t, `
[hosts.gh]
base = "git@gh:"
[root.r]
dir = "~/src"
tags = []
repos = ["gh:a/b"]
`)
	repos, err := reg.Repos()
	if err != nil {
		t.Fatal(err)
	}
	if repos[0].Tags == nil || len(repos[0].Tags) != 0 {
		t.Errorf("tags = %v, want an empty (not absent) list", repos[0].Tags)
	}
}

// TestTagsInheritAndOverride: the two lists inherit down the root chain and are
// overridable per repo, like every other setting.
func TestTagsInheritAndOverride(t *testing.T) {
	reg := loadTOML(t, `
[hosts.gh]
base = "git@gh:"
[defaults]
force_tags = ["*.latest"]
[root.r]
dir = "~/src"
tags = ["v*"]
repos = ["gh:a/inherits"]
[[root.r.repo]]
id = "gh:a/overrides"
tags = ["release/*"]
force_tags = []
`)
	repos, err := reg.Repos()
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string][]string{}
	force := map[string][]string{}
	for _, r := range repos {
		byName[r.ID.Name] = r.Tags
		force[r.ID.Name] = r.ForceTags
	}
	if got := byName["inherits"]; len(got) != 1 || got[0] != "v*" {
		t.Errorf("inherits: tags = %v, want [v*] from the root", got)
	}
	if got := force["inherits"]; len(got) != 1 || got[0] != "*.latest" {
		t.Errorf("inherits: force_tags = %v, want [*.latest] from [defaults]", got)
	}
	if got := byName["overrides"]; len(got) != 1 || got[0] != "release/*" {
		t.Errorf("overrides: tags = %v, want [release/*]", got)
	}
	if got := force["overrides"]; len(got) != 0 {
		t.Errorf("overrides: force_tags = %v, want the root's list overridden to empty", got)
	}
}

// TestLatestTagPinNeedsEveryTag guards the one combination that is silently
// wrong rather than loudly broken: latest-tag re-resolves against whichever tags
// were fetched, so a narrowed list changes which version gets pinned with no
// error and a plausible-looking result.
func TestLatestTagPinNeedsEveryTag(t *testing.T) {
	for _, c := range []struct {
		name, tags string
		wantErr    bool
	}{
		{"narrowed list is rejected", `tags = ["v*"]`, true},
		{"empty list is rejected", `tags = []`, true},
		{"every tag is fine", `tags = ["*"]`, false},
		{"silence is fine", ``, false},
	} {
		reg := loadTOML(t, `
[hosts.gh]
base = "git@gh:"
[root.r]
dir = "~/src"
[[root.r.repo]]
id = "gh:a/vend"
workflow = "vendor"
pin = "latest-tag"
`+c.tags+"\n")
		err := reg.Validate()
		if c.wantErr && err == nil {
			t.Errorf("%s: Validate = nil, want an error", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s: Validate = %v, want nil", c.name, err)
		}
		if c.wantErr && err != nil && !strings.Contains(err.Error(), "latest-tag") {
			t.Errorf("%s: error %q should name the pin", c.name, err)
		}
	}
}

// TestTagGlobsMustBeRefspecSafe: these two lists become git refspecs, where the
// glob dialect is narrower than path.Match. A pattern that means one thing here
// and another to git would fetch a different set than it reads like.
func TestTagGlobsMustBeRefspecSafe(t *testing.T) {
	for _, pat := range []string{`"v*.*"`, `"v?.0"`, `"v[0-9]*"`} {
		reg := loadTOML(t, `
[hosts.gh]
base = "git@gh:"
[root.r]
dir = "~/src"
tags = [`+pat+`]
repos = ["gh:a/b"]
`)
		if err := reg.Validate(); err == nil {
			t.Errorf("tags = [%s]: Validate = nil, want a refspec-dialect error", pat)
		}
	}
	// The forms git does support must keep passing.
	for _, pat := range []string{`"*"`, `"v*"`, `"*.latest"`, `"release/1.0"`} {
		reg := loadTOML(t, `
[hosts.gh]
base = "git@gh:"
[root.r]
dir = "~/src"
tags = [`+pat+`]
repos = ["gh:a/b"]
`)
		if err := reg.Validate(); err != nil {
			t.Errorf("tags = [%s]: Validate = %v, want nil", pat, err)
		}
	}
}
