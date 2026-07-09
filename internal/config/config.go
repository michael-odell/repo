// Package config loads the registry from a path of TOML fragments
// (REPO_REGISTRY_PATH), merges them, applies defaults→tag→repo inheritance, and
// resolves logical identities to physical URLs. See docs/DESIGN.md §3.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/michael-odell/repo/internal/ident"
	"github.com/michael-odell/repo/internal/model"
)

// Settings are the inheritable fields, shared by [defaults], [tag.*], and
// [[repo]]. Pointers/slices distinguish "unset" (inherit) from a set value.
type Settings struct {
	HomeRoot  *string      `toml:"home_root"`
	Path      *string      `toml:"path"`
	Worktrees *bool        `toml:"worktrees"`
	Branches  []string     `toml:"branches"`
	OnRewrite *string      `toml:"on_rewrite"`
	Prune     *string      `toml:"prune"`
	Host      *string      `toml:"host"`
	Workflow  *string      `toml:"workflow"`
	Hooks     []model.Hook `toml:"hooks"`
}

// Host is a [hosts.*] entry.
type Host struct {
	Base string `toml:"base"`
}

// Resolve is the per-machine [resolve] overlay (DESIGN §3.7).
type Resolve struct {
	Via       string            `toml:"via"`
	ApplyTo   StringList        `toml:"apply_to"`
	Overrides map[string]string `toml:"overrides"`
}

// RepoEntry is a [[repo]] table.
type RepoEntry struct {
	ID   string   `toml:"id"`
	Fork string   `toml:"fork"`
	Tags []string `toml:"tags"`
	Pin  string   `toml:"pin"`
	Settings
}

// Root is a [[root]] discovery location (DESIGN §3.2).
type Root struct {
	Path string `toml:"path"`
	Tag  string `toml:"tag"`
}

// file is one decoded TOML fragment.
type file struct {
	Defaults Settings            `toml:"defaults"`
	Hosts    map[string]Host     `toml:"hosts"`
	Tags     map[string]Settings `toml:"tag"`
	Repos    []RepoEntry         `toml:"repo"`
	Roots    []Root              `toml:"root"`
	Resolve  *Resolve            `toml:"resolve"`
}

// Registry is the merged, ready-to-use configuration.
type Registry struct {
	Hosts    map[string]Host
	Resolve  *Resolve
	defaults Settings
	tags     map[string]Settings
	entries  []RepoEntry
	roots    []Root
}

// builtinDefaults apply when a field is set nowhere.
var builtinDefaults = model.Repo{
	Workflow:  model.UpstreamPush,
	HomeRoot:  "~/src",
	Path:      model.PathFlat,
	Worktrees: false,
	Branches:  []string{"main"},
	OnRewrite: "stop",
	Prune:     "auto",
}

// Load reads and merges every fragment named on the path list. A path entry may
// be a file or a directory (in which case its *.toml files are read, sorted).
func Load(paths []string) (*Registry, error) {
	reg := &Registry{
		Hosts: map[string]Host{},
		tags:  map[string]Settings{},
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		files, err := fragmentFiles(p)
		if err != nil {
			return nil, err
		}
		for _, fp := range files {
			var f file
			if _, err := toml.DecodeFile(fp, &f); err != nil {
				return nil, fmt.Errorf("%s: %w", fp, err)
			}
			reg.merge(f)
		}
	}
	return reg, nil
}

func fragmentFiles(p string) ([]string, error) {
	info, err := os.Stat(p)
	if err != nil {
		return nil, fmt.Errorf("registry path %s: %w", p, err)
	}
	if !info.IsDir() {
		return []string{p}, nil
	}
	matches, err := filepath.Glob(filepath.Join(p, "*.toml"))
	if err != nil {
		return nil, err
	}
	sort.Strings(matches)
	return matches, nil
}

// merge folds one fragment into the registry: defaults are field-level
// last-wins; hosts and tags union with later keys winning; repos append;
// resolve merges (via/apply_to last-wins, overrides unioned).
func (reg *Registry) merge(f file) {
	reg.defaults = overlay(reg.defaults, f.Defaults)
	for k, v := range f.Hosts {
		reg.Hosts[k] = v
	}
	for k, v := range f.Tags {
		reg.tags[k] = overlay(reg.tags[k], v)
	}
	reg.entries = append(reg.entries, f.Repos...)
	reg.roots = append(reg.roots, f.Roots...)
	if f.Resolve != nil {
		reg.mergeResolve(f.Resolve)
	}
}

func (reg *Registry) mergeResolve(r *Resolve) {
	if reg.Resolve == nil {
		reg.Resolve = &Resolve{Overrides: map[string]string{}}
	}
	if r.Via != "" {
		reg.Resolve.Via = r.Via
	}
	if len(r.ApplyTo) > 0 {
		reg.Resolve.ApplyTo = r.ApplyTo
	}
	for k, v := range r.Overrides {
		reg.Resolve.Overrides[k] = v
	}
}

// Repos returns the effective repositories with inheritance applied.
func (reg *Registry) Repos() ([]model.Repo, error) {
	out := make([]model.Repo, 0, len(reg.entries))
	for _, e := range reg.entries {
		r, err := reg.effective(e)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (reg *Registry) effective(e RepoEntry) (model.Repo, error) {
	id, err := ident.Parse(e.ID)
	if err != nil {
		return model.Repo{}, err
	}

	// Compose settings: defaults → each tag (in order) → the repo entry.
	s := reg.defaults
	for _, t := range e.Tags {
		s = overlay(s, reg.tags[t])
	}
	s = overlay(s, e.Settings)

	r := model.Repo{
		ID:        id,
		Tags:      e.Tags,
		Pin:       e.Pin,
		HomeRoot:  strOr(s.HomeRoot, builtinDefaults.HomeRoot),
		Path:      strOr(s.Path, builtinDefaults.Path),
		Worktrees: boolOr(s.Worktrees, builtinDefaults.Worktrees),
		Branches:  sliceOr(s.Branches, builtinDefaults.Branches),
		OnRewrite: strOr(s.OnRewrite, builtinDefaults.OnRewrite),
		Prune:     strOr(s.Prune, builtinDefaults.Prune),
		Hooks:     s.Hooks,
	}

	if e.Fork != "" {
		fid, err := ident.Parse(e.Fork)
		if err != nil {
			return model.Repo{}, fmt.Errorf("%s: fork: %w", e.ID, err)
		}
		r.Fork = &fid
	}

	// Workflow: explicit wins; otherwise infer fork-pr when a fork is present.
	switch {
	case s.Workflow != nil:
		r.Workflow = *s.Workflow
	case r.Fork != nil:
		r.Workflow = model.ForkPR
	default:
		r.Workflow = builtinDefaults.Workflow
	}
	return r, nil
}

// Physical resolves a repo's clone URL for this machine (DESIGN §3.7).
func (reg *Registry) Physical(r model.Repo) (string, error) {
	return reg.PhysicalID(r.ID, r.Tags)
}

// PhysicalID resolves an identity's clone URL: an explicit override wins, else
// the `via` fold when apply_to matches the tags, else the identity's own host.
// The resulting host key must exist in [hosts.*].
func (reg *Registry) PhysicalID(id ident.ID, tags []string) (string, error) {
	host, path := id.Host, id.OwnerRepo()
	if reg.Resolve != nil {
		if ov, ok := reg.Resolve.Overrides[id.String()]; ok {
			host, path = splitHostPath(ov)
		} else if reg.Resolve.applies(tags) {
			vh, vprefix := splitHostPath(reg.Resolve.Via)
			host, path = vh, vprefix+id.OwnerRepo()
		}
	}
	h, ok := reg.Hosts[host]
	if !ok {
		return "", fmt.Errorf("%s: unknown host %q (define [hosts.%s])", id, host, host)
	}
	return h.Base + path, nil
}

func (r *Resolve) applies(tags []string) bool {
	for _, a := range r.ApplyTo {
		if a == "*" {
			return true
		}
		for _, t := range tags {
			if a == t {
				return true
			}
		}
	}
	return false
}

// splitHostPath splits "host:path" (e.g. "gogsprod:mirrors/") into its parts.
func splitHostPath(s string) (host, path string) {
	host, path, _ = strings.Cut(s, ":")
	return host, path
}

// overlay returns base with every field that is set in over replaced.
func overlay(base, over Settings) Settings {
	if over.HomeRoot != nil {
		base.HomeRoot = over.HomeRoot
	}
	if over.Path != nil {
		base.Path = over.Path
	}
	if over.Worktrees != nil {
		base.Worktrees = over.Worktrees
	}
	if over.Branches != nil {
		base.Branches = over.Branches
	}
	if over.OnRewrite != nil {
		base.OnRewrite = over.OnRewrite
	}
	if over.Prune != nil {
		base.Prune = over.Prune
	}
	if over.Host != nil {
		base.Host = over.Host
	}
	if over.Workflow != nil {
		base.Workflow = over.Workflow
	}
	if over.Hooks != nil {
		base.Hooks = over.Hooks
	}
	return base
}

func strOr(p *string, def string) string {
	if p != nil {
		return *p
	}
	return def
}

func boolOr(p *bool, def bool) bool {
	if p != nil {
		return *p
	}
	return def
}

func sliceOr(s, def []string) []string {
	if s != nil {
		return s
	}
	return def
}
