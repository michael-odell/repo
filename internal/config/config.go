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
	Layout       *string  `toml:"layout"`
	Worktrees    *bool    `toml:"worktrees"`
	Branches     []string `toml:"branches"`
	Push         *string  `toml:"push"`
	TaskBranches *string  `toml:"task_branches"`
	ShowBranches *string  `toml:"show_branches"`
	ForcePush    []string `toml:"force_push"`
	ForcePull    []string `toml:"force_pull"`
	// Which tags to fetch, and which of those may be overwritten when upstream
	// moves one (DESIGN §3.6). Separate lists because scope and
	// permission-to-destroy are separate questions — see model.Repo.
	Tags      []string `toml:"tags"`
	ForceTags []string `toml:"force_tags"`
	// Path globs naming local residue that is expected rather than notable
	// (DESIGN §3.6). They change what sync calls to your attention; they never
	// change what it protects.
	ExpectedUntracked   []string `toml:"expected_untracked"`
	ExpectedUncommitted []string `toml:"expected_uncommitted"`
	// How far apart a branch and its base may be before merge detection gives up
	// on the expensive patch-id tiers: -1 unlimited, 0 off, N commits (DESIGN
	// §5.3). Per repo because the cost is per repo.
	MergeScanLimit *int         `toml:"merge_scan_limit"`
	Prune          *string      `toml:"prune"`
	Host           *string      `toml:"host"`
	Workflow       *string      `toml:"workflow"`
	ForkOwner      *string      `toml:"fork_owner"`
	Pin            *string      `toml:"pin"`
	Hooks          []model.Hook `toml:"hooks"`
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
	Prune:     "auto",
	// Every tag, and none of them forced. Tags are not narrowed by default
	// because `repo` itself reads them in exactly one place — a vendor `pin`
	// resolving through refs/tags — and everywhere else they exist for whoever
	// uses the clone by hand (`git describe`, `checkout v1.2.3`); guessing a
	// narrower set per workflow would be guessing about the *upstream*, not
	// about how the repo is used. ForceTags stays empty for the same reason
	// force_push/force_pull do (§3.6): no forced overwrite without an explicit
	// opt-in, and a tag has no reflog to undo one.
	Tags: []string{"*"},
}

// WorkflowDefaults returns the push/task_branches defaults for a workflow
// (DESIGN §3.6): every workflow shares the same settings surface, and every
// value remains overridable — only the default varies, chosen so the common
// case per workflow needs zero configuration.
func WorkflowDefaults(workflow string) (push, taskBranches, showBranches string) {
	switch workflow {
	case model.ForkPR:
		return "auto", "auto", "unmerged"
	case model.SupplyChainMirror, model.Vendor:
		// A vendored or mirrored repo's local branches are scaffolding, not
		// parked work, so there is nothing there to nudge you about — but a
		// finding still has to surface.
		return "never", "pull-only", "notable"
	default: // upstream-push
		return "manual", "report", "unmerged"
	}
}

// member pairs a declared repo entry with the root it is nested under.
type member struct {
	entry RepoEntry
	root  string
}

// Load reads and merges every fragment named on the path list, rejecting any
// unknown keys (so an out-of-date or mistyped config fails loudly rather than
// silently ignoring the setting). A path entry may be a file or a directory (in
// which case its *.toml files are read, sorted). Each distinct fragment is read
// once however many times the path names it.
func Load(paths []string) (*Registry, error) {
	reg := &Registry{
		Hosts: map[string]Host{},
		roots: map[string]Root{},
	}
	read := map[string]bool{}
	for _, p := range paths {
		if p == "" {
			continue
		}
		files, err := fragmentFiles(p)
		if err != nil {
			return nil, err
		}
		for _, fp := range files {
			key := fragmentKey(fp)
			if read[key] {
				continue // already merged, under this path entry or an earlier one
			}
			read[key] = true
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

// fragmentKey identifies a fragment by its real location, so one file reached
// two ways — a path entry naming a directory *and* a file inside it, or two
// routes to one file through a symlinked dotfiles checkout — is merged once.
// Re-merging a fragment is never meaningful and is actively wrong: settings
// would overlay themselves harmlessly, but declared members append, so a second
// read silently doubles every repo the fragment declares (and with it every row
// those repos produce, and the work sync does on them).
func fragmentKey(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		p = real
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return filepath.Clean(p)
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

// Inherited resolves the settings a discovered repo in the given root chain
// should adopt (DESIGN §3.2: config overrides remote inference). Fields the
// config leaves unset are returned zero/"" so the caller falls back to what it
// reads from disk and remotes.
type Inherited struct {
	Workflow            string   // "" when unset by config → caller keeps its inference
	Layout              string   // "" when unset
	Worktrees           *bool    // nil when unset
	Push                string   // "" when unset by config → caller applies WorkflowDefaults
	TaskBranches        string   // "" when unset by config → caller applies WorkflowDefaults
	ShowBranches        string   // "" when unset by config → caller applies WorkflowDefaults
	ForcePush           []string // nil when unset
	ForcePull           []string // nil when unset
	Tags                []string // nil when unset; empty means "fetch no tags"
	ForceTags           []string // nil when unset
	ExpectedUntracked   []string // nil when unset
	ExpectedUncommitted []string // nil when unset
	MergeScanLimit      *int     // nil when unset
	Prune               string   // "" when unset
	Pin                 string   // "" when unset
	Hooks               []model.Hook
}

// StatedBranches returns `branches` as config states it for a repo — from
// [defaults], any root it sits under, or its own entry (pass its Settings; the
// zero value for a discovered repo, which has none), innermost winning as
// everywhere else — and nil when config is silent at every tier.
//
// Config is honored wherever it is written, including [defaults]: a value there
// applies clear down the tree, and a repo whose trunk disagrees with it is a
// config bug worth reporting as one. What is *not* config is builtinDefaults —
// nobody wrote it, so it cannot outrank what a clone says about itself; if it
// did, a discovered repo would never get to answer for itself at all.
func (reg *Registry) StatedBranches(chain []string, entry Settings) []string {
	s := reg.defaults
	for _, n := range chain {
		s = overlay(s, reg.roots[n].Settings)
	}
	return overlay(s, entry).Branches
}

// InheritedFor overlays [defaults] with each root in the chain and returns the
// result for a discovered repo.
func (reg *Registry) InheritedFor(chain []string) Inherited {
	s := reg.defaults
	for _, n := range chain {
		s = overlay(s, reg.roots[n].Settings)
	}
	return Inherited{
		Workflow:            strOr(s.Workflow, ""),
		Layout:              strOr(s.Layout, ""),
		Worktrees:           s.Worktrees,
		Push:                strOr(s.Push, ""),
		TaskBranches:        strOr(s.TaskBranches, ""),
		ShowBranches:        strOr(s.ShowBranches, ""),
		ForcePush:           s.ForcePush,
		ForcePull:           s.ForcePull,
		Tags:                s.Tags,
		ForceTags:           s.ForceTags,
		ExpectedUntracked:   s.ExpectedUntracked,
		ExpectedUncommitted: s.ExpectedUncommitted,
		MergeScanLimit:      s.MergeScanLimit,
		Prune:               strOr(s.Prune, ""),
		Pin:                 strOr(s.Pin, ""),
		Hooks:               s.Hooks,
	}
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

	// Workflow is resolved first and independently of fork_owner: explicit
	// (repo/root/defaults) wins; else an *explicit* per-repo fork implies fork-pr;
	// else the builtin default. An ambient fork_owner never implies fork-pr.
	var workflow string
	switch {
	case s.Workflow != nil:
		workflow = *s.Workflow
	case m.entry.Fork != "":
		workflow = model.ForkPR
	default:
		workflow = builtinDefaults.Workflow
	}

	// push/task_branches/show_branches defaults are workflow-dependent (DESIGN
	// §3.6), so they need the resolved workflow above, unlike every other field
	// here.
	pushDefault, taskDefault, showDefault := WorkflowDefaults(workflow)
	r := model.Repo{
		ID:                  id,
		Roots:               chain,
		HomeRoot:            reg.roots[m.root].Dir,
		Workflow:            workflow,
		Layout:              strOr(s.Layout, builtinDefaults.Layout),
		Worktrees:           boolOr(s.Worktrees, builtinDefaults.Worktrees),
		Branches:            sliceOr(s.Branches, builtinDefaults.Branches),
		BranchesStated:      s.Branches != nil, // == StatedBranches(chain, entry): the same overlay
		Push:                strOr(s.Push, pushDefault),
		TaskBranches:        strOr(s.TaskBranches, taskDefault),
		ShowBranches:        strOr(s.ShowBranches, showDefault),
		ForcePush:           s.ForcePush,
		ForcePull:           s.ForcePull,
		Tags:                sliceOr(s.Tags, builtinDefaults.Tags),
		ForceTags:           s.ForceTags,
		ExpectedUntracked:   s.ExpectedUntracked,
		ExpectedUncommitted: s.ExpectedUncommitted,
		MergeScanLimit:      s.MergeScanLimit,
		Prune:               strOr(s.Prune, builtinDefaults.Prune),
		Pin:                 strOr(s.Pin, ""),
		Hooks:               s.Hooks,
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
	if over.Push != nil {
		base.Push = over.Push
	}
	if over.TaskBranches != nil {
		base.TaskBranches = over.TaskBranches
	}
	if over.ShowBranches != nil {
		base.ShowBranches = over.ShowBranches
	}
	if over.ForcePush != nil {
		base.ForcePush = over.ForcePush
	}
	if over.ForcePull != nil {
		base.ForcePull = over.ForcePull
	}
	if over.Tags != nil {
		base.Tags = over.Tags
	}
	if over.ForceTags != nil {
		base.ForceTags = over.ForceTags
	}
	if over.ExpectedUntracked != nil {
		base.ExpectedUntracked = over.ExpectedUntracked
	}
	if over.ExpectedUncommitted != nil {
		base.ExpectedUncommitted = over.ExpectedUncommitted
	}
	if over.MergeScanLimit != nil {
		base.MergeScanLimit = over.MergeScanLimit
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
