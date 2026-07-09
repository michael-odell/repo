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
	if pt.HomeRoot != "~/wd" || pt.Path != model.PathOwner || !pt.Worktrees {
		t.Errorf("pt-helm inherited wrong from tag work: %+v", pt)
	}
	if pt.Workflow != model.ForkPR {
		t.Errorf("pt-helm workflow = %q, want fork-pr", pt.Workflow)
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
