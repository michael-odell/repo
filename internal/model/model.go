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

// Path layouts (DESIGN §3.5).
const (
	PathFlat  = "flat"
	PathOwner = "owner"
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
	Path      string // PathFlat | PathOwner
	Worktrees bool
	Branches  []string
	OnRewrite string // "stop" | "follow"
	Prune     string // "auto" | "report" | "manual"
	Pin       string // vendor only
	Hooks     []Hook
}

// Container is the on-disk directory that holds the repo (a single working tree
// when Worktrees is false, otherwise the bare repo + per-branch worktrees).
func (r Repo) Container() string {
	root := expandHome(r.HomeRoot)
	if r.Path == PathOwner {
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
