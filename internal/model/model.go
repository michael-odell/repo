// Package model holds the effective (post-inheritance) repository model that the
// rest of the tool operates on. See docs/DESIGN.md §3.
package model

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/michael-odell/repo/internal/ident"
)

// Workflow values (DESIGN §3.6).
const (
	UpstreamPush      = "upstream-push"
	ForkPR            = "fork-pr"
	SupplyChainMirror = "supply-chain-mirror"
	Vendor            = "vendor"
)

// Layouts (DESIGN §3.5).
const (
	LayoutFlat  = "flat"
	LayoutOwner = "owner"
)

// Hook is a maintenance command run at a lifecycle point (DESIGN §3.8).
type Hook struct {
	After string // e.g. "fetch"
	Run   string
}

// Repo is a fully-resolved repository: all root/default inheritance applied.
type Repo struct {
	ID        ident.ID
	Fork      *ident.ID // nil when there is no separate fork
	Roots     []string  // inheritance chain of root names (shallowest → deepest)
	Workflow  string
	HomeRoot  string // the owning root's dir; where the container lives
	Layout    string // LayoutFlat | LayoutOwner
	Worktrees bool
	Branches  []string

	// BranchesStated distinguishes a `branches` value config actually states —
	// at [defaults], a root, or the repo's own entry — from the builtin nobody
	// wrote. Both land in Branches; only a stated one outranks what the clone
	// says its own default branch is (see resolveBranches).
	BranchesStated bool

	Push         string   // "auto" | "manual" | "never" (DESIGN §3.6)
	TaskBranches string   // "auto" | "report" | "pull-only" (DESIGN §3.6)
	ShowBranches string   // "none" | "notable" | "unmerged" | "landed" | "all" (DESIGN §5.6)
	ForcePush    []string // glob patterns: branches sync may force-push (DESIGN §5.2)
	ForcePull    []string // glob patterns: branches sync may force-pull/reset (DESIGN §5.2)

	// FetchSkip names remotes, by glob against the remote name, that sync
	// should never fetch (DESIGN §3.6). It has no effect on a workflow's
	// managed remotes (origin, and upstream/untrusted when Fork is set) — those
	// are always fetched regardless, since branch reconciliation and the
	// review gate depend on them.
	FetchSkip []string

	// Tags is which tags to fetch at all, and ForceTags which of those may be
	// overwritten when upstream moves one (DESIGN §3.6/§5.2). They are separate
	// because scope and permission-to-destroy are separate questions: narrowing
	// what arrives is a bandwidth and ref-count decision, while following a
	// moved tag discards the object that tag used to name, with no reflog to
	// get it back. Empty Tags means fetch no tags; nil means the builtin ["*"].
	Tags      []string
	ForceTags []string

	// Path globs for local residue that is expected rather than notable: files
	// something regenerates, that you have no intention of committing (DESIGN
	// §3.6). They suppress the report, never the data-safety rules.
	ExpectedUntracked   []string
	ExpectedUncommitted []string
	// MergeScanLimit bounds the expensive half of merge detection: -1 unlimited,
	// 0 off (ancestry only), N commits of divergence (DESIGN §5.3). A pointer
	// because 0 is a meaningful value here — leaving the field zero must mean
	// "nobody said", not "switch the tiers off", or a Repo built anywhere but
	// config would quietly stop detecting squash merges.
	MergeScanLimit *int
	Prune          string // "auto" | "report" | "interactive" | "manual"

	// PruneKeep names branches prune must never remove, whatever the merge
	// tiers concluded, and PruneMinAge how long a ref must have sat still to be
	// removable at all (DESIGN §5.3). Both are the person's judgement rather
	// than the evidence's: a tier answers "has this landed", and neither of
	// these disputes the answer — they say the branch stays regardless. Zero
	// PruneMinAge is no age gate, which is why it needs no pointer: "nobody
	// said" and "no minimum" are the same instruction here.
	PruneKeep   []string
	PruneMinAge time.Duration

	// SyncFrequency is how long `sync --if-due` leaves this repo alone (DESIGN
	// §5.5). Nil is "nobody said", and the caller applies the built-in
	// interval; a pointer because "0" is a value someone can mean — always
	// due, every run — and the two must not collapse the way they harmlessly
	// do for PruneMinAge above.
	SyncFrequency *time.Duration

	Pin   string // vendor only
	Hooks []Hook

	// Discovered-only: a repo found on disk that the registry does not declare
	// (DESIGN §3.2). Dir is its actual container (authoritative over the computed
	// path); OriginURL is its existing origin remote (used verbatim instead of
	// re-resolving through [hosts.*], which need not know a discovered host).
	Dir       string
	OriginURL string
}

// Container is the on-disk directory that holds the repo (a single working tree
// when Worktrees is false, otherwise the bare repo + per-branch worktrees). A
// discovered repo carries its actual location in Dir, which is authoritative.
func (r Repo) Container() string {
	if r.Dir != "" {
		return r.Dir
	}
	root := expandHome(r.HomeRoot)
	if r.Layout == LayoutOwner {
		return filepath.Join(root, r.ID.Owner, r.ID.Name)
	}
	return filepath.Join(root, r.ID.Name)
}

// PrimaryTree is where cs/`repo home` lands with no branch specified: the
// worktree of the first important branch, or the container itself for a single
// working tree.
func (r Repo) PrimaryTree() string {
	if r.Worktrees && len(r.Branches) > 0 {
		return filepath.Join(r.Container(), r.Branches[0])
	}
	return r.Container()
}

// WorktreePath returns the path of a named branch's worktree.
func (r Repo) WorktreePath(branch string) string {
	if r.Worktrees {
		return filepath.Join(r.Container(), branch)
	}
	return r.Container()
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
