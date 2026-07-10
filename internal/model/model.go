// Package model holds the effective (post-inheritance) repository model that the
// rest of the tool operates on. See docs/DESIGN.md §3.
package model

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/michael-odell/repo/internal/ident"
)

// Workflow values (DESIGN §3.6).
const (
	UpstreamPush       = "upstream-push"
	ForkPR             = "fork-pr"
	SupplyChainMirror  = "supply-chain-mirror"
	Vendor             = "vendor"
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

// Repo is a fully-resolved repository: all tag/default inheritance applied.
type Repo struct {
	ID        ident.ID
	Fork      *ident.ID // nil when there is no separate fork
	Tags      []string
	Workflow  string
	HomeRoot  string
	Layout    string // LayoutFlat | LayoutOwner
	Worktrees bool
	Branches  []string
	OnRewrite string // "stop" | "follow"
	Prune     string // "auto" | "report" | "manual"
	Pin       string // vendor only
	Hooks     []Hook

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
