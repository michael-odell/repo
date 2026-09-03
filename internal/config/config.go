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
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

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
	// FetchSkip names remotes (by glob against the remote name) sync should
	// never fetch (DESIGN §3.6) — the exclusion side of "fetch every remote by
	// default." It never applies to a workflow's managed remotes.
	FetchSkip []string `toml:"fetch_skip"`
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
	MergeScanLimit *int `toml:"merge_scan_limit"`
	// How long `sync --if-due` leaves this repo alone (DESIGN §5.5), in
	// prune_min_age's syntax: "7d", "12h", "2w". "0" means always due.
	SyncFrequency *string `toml:"sync_frequency"`
	Prune         *string `toml:"prune"`
	// The two dials that belong to the person rather than to the evidence
	// (DESIGN §5.3): branch-name globs prune must never remove whatever any
	// tier concluded, and how long a ref must have sat still to be removable.
	// A name-based veto outranks inference, which is why prune_keep is a list
	// like force_push and not a flag.
	PruneKeep   []string     `toml:"prune_keep"`
	PruneMinAge *string      `toml:"prune_min_age"`
	Host        *string      `toml:"host"`
	Workflow    *string      `toml:"workflow"`
	ForkOwner   *string      `toml:"fork_owner"`
	Pin         *string      `toml:"pin"`
	Hooks       []model.Hook `toml:"hooks"`
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
	Dirs     map[string]Root `toml:"dir"`
	Resolve  *Resolve        `toml:"resolve"`
}

// Registry is the merged, ready-to-use configuration.
type Registry struct {
	Hosts    map[string]Host
	Resolve  *Resolve
	defaults Settings
	roots    map[string]Root
	// dirs are [dir.<name>] nodes (DESIGN §3.9): a settings overlay on part of a
	// root's tree, shaped exactly like a Root (same fields, same merge), but
	// never a scan location (ScanRoots ignores it) and never nested per parent
	// root — its name is a label, unrelated to any directory or root name. Kept
	// in a separate map, not folded into roots, because the two are separate
	// namespaces: a dir and a root may share a name without colliding.
	dirs map[string]Root
}

// builtinDefaults apply when a field is set nowhere.
var builtinDefaults = model.Repo{
	Workflow:  model.UpstreamPush,
	Layout:    model.LayoutFlat,
	Worktrees: false,
	Branches:  []string{"main"},
	// Report rather than auto. The setting predates anything reading it, and
	// wiring it up on the old default would have had every repo on the machine
	// deleting branches on the next sweep — asking for trust the classification
	// has not yet been watched earning (DESIGN §5.3, the confidence path).
	Prune: "report",
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
		dirs:  map[string]Root{},
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
// hosts union with later keys winning; roots and dirs each merge by name within
// their own namespace (settings overlay, later `dir` wins, members append);
// resolve merges (via/apply_to last-wins, overrides unioned).
func (reg *Registry) merge(f file) {
	reg.defaults = overlay(reg.defaults, f.Defaults)
	maps.Copy(reg.Hosts, f.Hosts)
	for name, r := range f.Roots {
		reg.roots[name] = mergeRoot(reg.roots[name], r)
	}
	for name, d := range f.Dirs {
		reg.dirs[name] = mergeRoot(reg.dirs[name], d)
	}
	if f.Resolve != nil {
		reg.mergeResolve(f.Resolve)
	}
}

// dirChainPrefix qualifies a [dir.*] name for use as a chain-name — everywhere a
// repo's inheritance chain is recorded (model.Repo.Roots, RootFor's chain, the
// name checkCollisions/Explain report). A bare TOML key can never contain a
// literal dot, so this can never collide with a declared root name (DESIGN §3.9).
const dirChainPrefix = "dir."

// node resolves a chain-name to its settings-bearing entry — a bare root name,
// or a dirChainPrefix-qualified [dir.*] name.
func (reg *Registry) node(name string) (Root, bool) {
	if n, ok := strings.CutPrefix(name, dirChainPrefix); ok {
		r, ok := reg.dirs[n]
		return r, ok
	}
	r, ok := reg.roots[name]
	return r, ok
}

// allNodes returns every settings-bearing node — roots and dirs together — keyed
// by chain-name, for the prefix-matching chain/RootFor share. Dirs are excluded
// from ScanRoots (they are never a scan location) but participate here exactly
// like roots (DESIGN §3.9).
func (reg *Registry) allNodes() map[string]Root {
	out := make(map[string]Root, len(reg.roots)+len(reg.dirs))
	maps.Copy(out, reg.roots)
	for n, d := range reg.dirs {
		out[dirChainPrefix+n] = d
	}
	return out
}

// homeRootDir returns the dir of the deepest *actual* root (never a [dir.*]
// node) in a chain — the dir a repo's container path is computed from. A
// [dir.*] node never contributes to placement, only to settings (DESIGN §3.9),
// so this deliberately skips past any in the chain rather than taking the
// deepest entry outright: taking a [dir.*] node's own dir here would double up
// the owner segment Container() already appends for a `layout = owner` root.
func (reg *Registry) homeRootDir(chain []string) string {
	for _, n := range slices.Backward(chain) {
		if r, ok := reg.roots[n]; ok {
			return r.Dir
		}
	}
	return ""
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
	maps.Copy(reg.Resolve.Overrides, r.Overrides)
}

// members returns every declared repo across all roots and dirs, with the
// chain-name each is nested under, in a stable order (roots before dirs, each
// alphabetically by name, then declaration order). A [dir.*] node holds members
// the same way a root does — `repos`/`[[dir.*.repo]]` — for provisioning a repo
// whose settings should come from that overlay (DESIGN §3.9).
func (reg *Registry) members() []member {
	var out []member
	rootNames := make([]string, 0, len(reg.roots))
	for n := range reg.roots {
		rootNames = append(rootNames, n)
	}
	sort.Strings(rootNames)
	for _, n := range rootNames {
		out = append(out, membersOf(reg.roots[n], n)...)
	}
	dirNames := make([]string, 0, len(reg.dirs))
	for n := range reg.dirs {
		dirNames = append(dirNames, n)
	}
	sort.Strings(dirNames)
	for _, n := range dirNames {
		out = append(out, membersOf(reg.dirs[n], dirChainPrefix+n)...)
	}
	return out
}

func membersOf(r Root, chainName string) []member {
	var out []member
	for _, id := range r.Repos {
		out = append(out, member{entry: RepoEntry{ID: id}, root: chainName})
	}
	for _, e := range r.RepoTabs {
		out = append(out, member{entry: e, root: chainName})
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

// chain returns the inheritance chain for a repo nested under node `name` (a
// root, or a dirChainPrefix-qualified dir): every node whose `dir` is a
// path-prefix of that node's own `dir` (its dir-ancestors, including itself),
// ordered shallowest → deepest. Longest prefix therefore wins. Roots and dirs
// share this one chain (DESIGN §3.9) — only ScanRoots and homeRootDir tell them
// apart.
func (reg *Registry) chain(name string) []string {
	nodes := reg.allNodes()
	self, ok := nodes[name]
	if !ok {
		return nil
	}
	target := expandHome(self.Dir)
	var names []string
	for n, r := range nodes {
		if pathHasPrefix(target, expandHome(r.Dir)) {
			names = append(names, n)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		return len(expandHome(nodes[names[i]].Dir)) < len(expandHome(nodes[names[j]].Dir))
	})
	return names
}

// Inherited resolves the settings a discovered repo in the given root chain
// should adopt (DESIGN §3.2: config overrides remote inference). Fields the
// config leaves unset are returned zero/"" so the caller falls back to what it
// reads from disk and remotes.
type Inherited struct {
	Workflow            string         // "" when unset by config → caller keeps its inference
	Layout              string         // "" when unset
	Worktrees           *bool          // nil when unset
	Push                string         // "" when unset by config → caller applies WorkflowDefaults
	TaskBranches        string         // "" when unset by config → caller applies WorkflowDefaults
	ShowBranches        string         // "" when unset by config → caller applies WorkflowDefaults
	ForcePush           []string       // nil when unset
	ForcePull           []string       // nil when unset
	FetchSkip           []string       // nil when unset
	Tags                []string       // nil when unset; empty means "fetch no tags"
	ForceTags           []string       // nil when unset
	ExpectedUntracked   []string       // nil when unset
	ExpectedUncommitted []string       // nil when unset
	MergeScanLimit      *int           // nil when unset
	SyncFrequency       *time.Duration // nil when unset: the built-in interval
	Prune               string         // "" when unset
	PruneKeep           []string       // nil when unset
	PruneMinAge         time.Duration  // 0 when unset: no age gate
	Pin                 string         // "" when unset
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
		r, _ := reg.node(n)
		s = overlay(s, r.Settings)
	}
	return overlay(s, entry).Branches
}

// InheritedFor overlays [defaults] with each root in the chain and returns the
// result for a discovered repo.
func (reg *Registry) InheritedFor(chain []string) Inherited {
	s := reg.defaults
	for _, n := range chain {
		r, _ := reg.node(n)
		s = overlay(s, r.Settings)
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
		FetchSkip:           s.FetchSkip,
		Tags:                s.Tags,
		ForceTags:           s.ForceTags,
		ExpectedUntracked:   s.ExpectedUntracked,
		ExpectedUncommitted: s.ExpectedUncommitted,
		MergeScanLimit:      s.MergeScanLimit,
		Prune:               strOr(s.Prune, ""),
		PruneKeep:           s.PruneKeep,
		// Validate has already rejected a value ParseAge can't read, so the
		// error here is unreachable for any registry that got this far; a
		// discovered repo falls back to "no age gate" rather than failing a
		// sweep over a setting that was reported at load time.
		PruneMinAge:   mustAge(s.PruneMinAge),
		SyncFrequency: mustFrequency(s.SyncFrequency),
		Pin:           strOr(s.Pin, ""),
		Hooks:         s.Hooks,
	}
}

// mustFrequency parses a validated sync_frequency for a discovered repo. Unset
// stays nil — the built-in interval — and so does a value ParseAge can't read,
// which Validate has already reported: falling back to the built-in is the same
// thing every other unset setting does, and is better than failing a sweep over
// a cadence that was flagged at load time. Zero is preserved rather than
// flattened into nil, since "0" means always due (DESIGN §5.5).
func mustFrequency(s *string) *time.Duration {
	if s == nil {
		return nil
	}
	d, err := ParseAge(*s)
	if err != nil {
		return nil
	}
	return &d
}

// mustAge parses a validated prune_min_age, treating unset and unreadable
// alike as "no age gate" — see the call site.
func mustAge(s *string) time.Duration {
	if s == nil {
		return 0
	}
	d, _ := ParseAge(*s)
	return d
}

func (reg *Registry) effective(m member) (model.Repo, error) {
	id, err := ident.Parse(m.entry.ID)
	if err != nil {
		return model.Repo{}, err
	}

	chain := reg.chain(m.root)
	s := reg.defaults
	for _, n := range chain {
		r, _ := reg.node(n)
		s = overlay(s, r.Settings)
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
		HomeRoot:            reg.homeRootDir(chain),
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
		FetchSkip:           s.FetchSkip,
		Tags:                sliceOr(s.Tags, builtinDefaults.Tags),
		ForceTags:           s.ForceTags,
		ExpectedUntracked:   s.ExpectedUntracked,
		ExpectedUncommitted: s.ExpectedUncommitted,
		MergeScanLimit:      s.MergeScanLimit,
		Prune:               strOr(s.Prune, builtinDefaults.Prune),
		PruneKeep:           s.PruneKeep,
		Pin:                 strOr(s.Pin, ""),
		Hooks:               s.Hooks,
	}
	if s.PruneMinAge != nil {
		age, err := ParseAge(*s.PruneMinAge)
		if err != nil {
			return model.Repo{}, fmt.Errorf("%s: prune_min_age: %w", id, err)
		}
		r.PruneMinAge = age
	}
	if s.SyncFrequency != nil {
		freq, err := ParseAge(*s.SyncFrequency)
		if err != nil {
			return model.Repo{}, fmt.Errorf("%s: sync_frequency: %w", id, err)
		}
		r.SyncFrequency = &freq
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

// settingsField probes one Settings field for the walk Explain does — named for
// display, and tested for "did this link set it" without reflection, matching
// overlay's own explicit field-by-field style.
type settingsField struct {
	name string
	set  func(Settings) bool
}

// settingsFields lists every inheritable field Explain reports provenance for —
// the same fields overlay folds, in the same order as the README settings
// reference table. `host` and `fork_owner` are deliberately absent: neither
// survives onto a resolved model.Repo (host only matters for resolving a bare
// clone name; fork_owner only supplies a fork's derivation, and that derivation
// — explicit fork vs. fork_owner vs. error, DESIGN §3.6 — isn't a single
// last-writer-wins fold the way every other field here is, so it's out of scope
// for this per-field walk).
var settingsFields = []settingsField{
	{"layout", func(s Settings) bool { return s.Layout != nil }},
	{"worktrees", func(s Settings) bool { return s.Worktrees != nil }},
	{"branches", func(s Settings) bool { return s.Branches != nil }},
	{"workflow", func(s Settings) bool { return s.Workflow != nil }},
	{"push", func(s Settings) bool { return s.Push != nil }},
	{"task_branches", func(s Settings) bool { return s.TaskBranches != nil }},
	{"show_branches", func(s Settings) bool { return s.ShowBranches != nil }},
	{"force_push", func(s Settings) bool { return s.ForcePush != nil }},
	{"force_pull", func(s Settings) bool { return s.ForcePull != nil }},
	{"fetch_skip", func(s Settings) bool { return s.FetchSkip != nil }},
	{"tags", func(s Settings) bool { return s.Tags != nil }},
	{"force_tags", func(s Settings) bool { return s.ForceTags != nil }},
	{"expected_untracked", func(s Settings) bool { return s.ExpectedUntracked != nil }},
	{"expected_uncommitted", func(s Settings) bool { return s.ExpectedUncommitted != nil }},
	{"merge_scan_limit", func(s Settings) bool { return s.MergeScanLimit != nil }},
	{"prune", func(s Settings) bool { return s.Prune != nil }},
	{"prune_keep", func(s Settings) bool { return s.PruneKeep != nil }},
	{"prune_min_age", func(s Settings) bool { return s.PruneMinAge != nil }},
	{"sync_frequency", func(s Settings) bool { return s.SyncFrequency != nil }},
	{"pin", func(s Settings) bool { return s.Pin != nil }},
	{"hooks", func(s Settings) bool { return s.Hooks != nil }},
}

// Explain resolves, for one repo's chain (model.Repo.Roots — shallowest →
// deepest, declared or discovered alike), which link last set each field: the
// per-field detail behind `repo config --explain` (DESIGN §7, §3.9), moved
// behind a flag rather than shown by default because it's one line per field
// rather than one line per repo. The walk is [defaults] → chain → the repo's
// own declared entry (looked up by id; the zero Settings, contributing nothing,
// for a discovered repo or a bare `repos = [...]` line), matching effective's
// own walk exactly. A field neither the chain nor [defaults] set is absent from
// the result — the caller's builtin default applies, same as everywhere else.
func (reg *Registry) Explain(id string, chain []string) map[string]string {
	type link struct {
		name string
		s    Settings
	}
	links := make([]link, 0, len(chain)+2)
	links = append(links, link{"defaults", reg.defaults})
	for _, n := range chain {
		r, _ := reg.node(n)
		links = append(links, link{n, r.Settings})
	}
	links = append(links, link{"repo", reg.entrySettings(id)})

	out := map[string]string{}
	for _, f := range settingsFields {
		for _, l := range links {
			if f.set(l.s) {
				out[f.name] = l.name
			}
		}
	}
	return out
}

// entrySettings returns the per-repo override Settings a declared repo carries
// — a [[root.*.repo]]/[[dir.*.repo]] table's fields, or the zero value for a
// bare `repos` entry or an id with no declaration at all (a discovered repo).
func (reg *Registry) entrySettings(id string) Settings {
	for _, m := range reg.members() {
		if m.entry.ID == id {
			return m.entry.Settings
		}
	}
	return Settings{}
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
		if a == "*" || slices.Contains(roots, a) {
			return true
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
	if over.FetchSkip != nil {
		base.FetchSkip = over.FetchSkip
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
	if over.PruneKeep != nil {
		base.PruneKeep = over.PruneKeep
	}
	if over.PruneMinAge != nil {
		base.PruneMinAge = over.PruneMinAge
	}
	if over.SyncFrequency != nil {
		base.SyncFrequency = over.SyncFrequency
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
