// Package config loads the registry from a path of TOML fragments
// (REPO_REGISTRY_PATH), merges them, and resolves the effective repositories.
//
// Configuration attaches to named directory roots ([root.<name>] with a `dir`),
// not tags: settings inherit down the directory tree by `dir` prefix, and a
// repo's members nest under it (a `repos` array of bare ids for the common case,
// [[root.<name>.repo]] tables for exceptions). See docs/DESIGN.md §3.
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

// Settings are the inheritable fields, shared by [defaults], [root.*], and a
// [[root.*.repo]] entry. Pointers/slices distinguish "unset" (inherit) from a set
// value. `home_root` is gone — a root's `dir` is its home (DESIGN §3.4).
type Settings struct {
	Layout    *string      `toml:"layout"`
	Worktrees *bool        `toml:"worktrees"`
	Branches  []string     `toml:"branches"`
	OnRewrite *string      `toml:"on_rewrite"`
	Prune     *string      `toml:"prune"`
	Host      *string      `toml:"host"`
	Workflow  *string      `toml:"workflow"`
	ForkOwner *string      `toml:"fork_owner"`
	Pin       *string      `toml:"pin"`
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

// RepoEntry is a declared repo: a bare id (from a root's `repos` array) or a
// [[root.*.repo]] table carrying overrides.
type RepoEntry struct {
	ID   string `toml:"id"`
	Fork string `toml:"fork"`
	Settings
}

// Root is a [root.<name>] directory node: a `dir`, an inheritable settings
// bundle, and its declared members (DESIGN §3.4).
type Root struct {
	Dir string `toml:"dir"`
	Settings
	Repos    []string    `toml:"repos"` // bare host:owner/name ids
	RepoTabs []RepoEntry `toml:"repo"`  // [[root.<name>.repo]] exceptions
}

// file is one decoded TOML fragment.
type file struct {
	Defaults Settings        `toml:"defaults"`
	Hosts    map[string]Host `toml:"hosts"`
	Roots    map[string]Root `toml:"root"`
	Resolve  *Resolve        `toml:"resolve"`
}

// Registry is the merged, ready-to-use configuration.
type Registry struct {
	Hosts    map[string]Host
	Resolve  *Resolve
	defaults Settings
	roots    map[string]Root
}

// builtinDefaults apply when a field is set nowhere.
var builtinDefaults = model.Repo{
	Workflow:  model.UpstreamPush,
	Layout:    model.LayoutFlat,
	Worktrees: false,
	Branches:  []string{"main"},
	OnRewrite: "stop",
	Prune:     "auto",
}

// member pairs a declared repo entry with the root it is nested under.
type member struct {
	entry RepoEntry
	root  string
}

// Load reads and merges every fragment named on the path list, rejecting any
// unknown keys (so an out-of-date or mistyped config fails loudly rather than
// silently ignoring the setting). A path entry may be a file or a directory (in
// which case its *.toml files are read, sorted).
func Load(paths []string) (*Registry, error) {
	reg := &Registry{
		Hosts: map[string]Host{},
		roots: map[string]Root{},
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
			md, err := toml.DecodeFile(fp, &f)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", fp, err)
			}
			if un := md.Undecoded(); len(un) > 0 {
				return nil, fmt.Errorf("%s: unknown key(s): %s "+
					"(the config schema changed to named [root.*] nodes — see DESIGN §3.4)",
					fp, joinKeys(un))
			}
			reg.merge(f)
		}
	}
	return reg, nil
}

func joinKeys(keys []toml.Key) string {
	ss := make([]string, len(keys))
	for i, k := range keys {
		ss[i] = k.String()
	}
	sort.Strings(ss)
	return strings.Join(ss, ", ")
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

// merge folds one fragment into the registry: defaults are field-level last-wins;
// hosts union with later keys winning; roots merge by name (settings overlay,
// later `dir` wins, members append); resolve merges (via/apply_to last-wins,
// overrides unioned).
func (reg *Registry) merge(f file) {
	reg.defaults = overlay(reg.defaults, f.Defaults)
	for k, v := range f.Hosts {
		reg.Hosts[k] = v
	}
	for name, r := range f.Roots {
		reg.roots[name] = mergeRoot(reg.roots[name], r)
	}
	if f.Resolve != nil {
		reg.mergeResolve(f.Resolve)
	}
}

func mergeRoot(base, over Root) Root {
	base.Settings = overlay(base.Settings, over.Settings)
	if over.Dir != "" {
		base.Dir = over.Dir
	}
	base.Repos = append(base.Repos, over.Repos...)
	base.RepoTabs = append(base.RepoTabs, over.RepoTabs...)
	return base
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

// members returns every declared repo across all roots, with the root each is
// nested under, in a stable order (root name, then declaration order).
func (reg *Registry) members() []member {
	names := make([]string, 0, len(reg.roots))
	for n := range reg.roots {
		names = append(names, n)
	}
	sort.Strings(names)
	var out []member
	for _, n := range names {
		r := reg.roots[n]
		for _, id := range r.Repos {
			out = append(out, member{entry: RepoEntry{ID: id}, root: n})
		}
		for _, e := range r.RepoTabs {
			out = append(out, member{entry: e, root: n})
		}
	}
	return out
}

// Repos returns the effective declared repositories with inheritance applied.
func (reg *Registry) Repos() ([]model.Repo, error) {
	ms := reg.members()
	out := make([]model.Repo, 0, len(ms))
	for _, m := range ms {
		r, err := reg.effective(m)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// chain returns the inheritance chain for a repo nested under root `name`: every
// root whose `dir` is a path-prefix of that root's `dir` (its dir-ancestors,
// including itself), ordered shallowest → deepest. Longest prefix therefore wins.
func (reg *Registry) chain(name string) []string {
	self, ok := reg.roots[name]
	if !ok {
		return nil
	}
	target := expandHome(self.Dir)
	var names []string
	for n, r := range reg.roots {
		if pathHasPrefix(target, expandHome(r.Dir)) {
			names = append(names, n)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		return len(expandHome(reg.roots[names[i]].Dir)) < len(expandHome(reg.roots[names[j]].Dir))
	})
	return names
}

func (reg *Registry) effective(m member) (model.Repo, error) {
	id, err := ident.Parse(m.entry.ID)
	if err != nil {
		return model.Repo{}, err
	}

	chain := reg.chain(m.root)
	s := reg.defaults
	for _, n := range chain {
		s = overlay(s, reg.roots[n].Settings)
	}
	s = overlay(s, m.entry.Settings)

	r := model.Repo{
		ID:        id,
		Roots:     chain,
		HomeRoot:  reg.roots[m.root].Dir,
		Layout:    strOr(s.Layout, builtinDefaults.Layout),
		Worktrees: boolOr(s.Worktrees, builtinDefaults.Worktrees),
		Branches:  sliceOr(s.Branches, builtinDefaults.Branches),
		OnRewrite: strOr(s.OnRewrite, builtinDefaults.OnRewrite),
		Prune:     strOr(s.Prune, builtinDefaults.Prune),
		Pin:       strOr(s.Pin, ""),
		Hooks:     s.Hooks,
	}

	// Workflow is resolved first and independently of fork_owner: explicit
	// (repo/root/defaults) wins; else an *explicit* per-repo fork implies fork-pr;
	// else the builtin default. An ambient fork_owner never implies fork-pr.
	switch {
	case s.Workflow != nil:
		r.Workflow = *s.Workflow
	case m.entry.Fork != "":
		r.Workflow = model.ForkPR
	default:
		r.Workflow = builtinDefaults.Workflow
	}

	// Fork is resolved only after the workflow is known, and only when the
	// workflow needs one: explicit `fork` → derive from `fork_owner` → error.
	if err := reg.resolveFork(&r, m.entry, id, s); err != nil {
		return model.Repo{}, err
	}
	return r, nil
}

func (reg *Registry) resolveFork(r *model.Repo, e RepoEntry, id ident.ID, s Settings) error {
	if e.Fork != "" {
		fid, err := ident.Parse(e.Fork)
		if err != nil {
			return fmt.Errorf("%s: fork: %w", e.ID, err)
		}
		r.Fork = &fid
		return nil
	}
	if !workflowNeedsFork(r.Workflow) {
		return nil
	}
	if s.ForkOwner == nil {
		return fmt.Errorf("%s: workflow %q needs a fork but no `fork` or `fork_owner` is set",
			e.ID, r.Workflow)
	}
	fid, err := ident.Parse(*s.ForkOwner + "/" + id.Name)
	if err != nil {
		return fmt.Errorf("%s: fork_owner %q: %w (want host:owner)", e.ID, *s.ForkOwner, err)
	}
	r.Fork = &fid
	return nil
}

func workflowNeedsFork(w string) bool {
	return w == model.ForkPR || w == model.SupplyChainMirror
}

// Physical resolves a repo's clone URL for this machine (DESIGN §3.7).
func (reg *Registry) Physical(r model.Repo) (string, error) {
	return reg.PhysicalID(r.ID, r.Roots)
}

// PhysicalID resolves an identity's clone URL: an explicit override wins, else
// the `via` fold when apply_to matches the repo's roots, else the identity's own
// host. The resulting host key must exist in [hosts.*].
func (reg *Registry) PhysicalID(id ident.ID, roots []string) (string, error) {
	host, path := id.Host, id.OwnerRepo()
	if reg.Resolve != nil {
		if ov, ok := reg.Resolve.Overrides[id.String()]; ok {
			host, path = splitHostPath(ov)
		} else if reg.Resolve.applies(roots) {
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

func (r *Resolve) applies(roots []string) bool {
	for _, a := range r.ApplyTo {
		if a == "*" {
			return true
		}
		for _, n := range roots {
			if a == n {
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
	if over.Layout != nil {
		base.Layout = over.Layout
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
	if over.ForkOwner != nil {
		base.ForkOwner = over.ForkOwner
	}
	if over.Pin != nil {
		base.Pin = over.Pin
	}
	if over.Hooks != nil {
		base.Hooks = over.Hooks
	}
	return base
}

// pathHasPrefix reports whether path p sits at or below prefix, on path-component
// boundaries (so ~/contrib is a prefix of ~/contrib/x but not ~/contribX).
func pathHasPrefix(p, prefix string) bool {
	p, prefix = filepath.Clean(p), filepath.Clean(prefix)
	if p == prefix {
		return true
	}
	return strings.HasPrefix(p, prefix+string(filepath.Separator))
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
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
