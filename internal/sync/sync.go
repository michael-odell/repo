// Package sync reconciles repositories toward the registry (DESIGN §5). Stage 4
// implements single-tree upstream-push and supply-chain-mirror workflows; other
// workflows and worktrees are deferred (reported, not attempted).
package sync

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// Outcome classifies a repo's sync result for the concise report.
type Outcome int

const (
	UpToDate Outcome = iota
	Updated
	Attention
	ReviewPending
	Deferred
	Failed
)

// Result is the per-repo outcome plus an ordered reasoning trace (--verbose).
type Result struct {
	Name    string
	Cloned  bool
	Outcome Outcome
	Detail  string
	Actions []string
	Err     error
}

// Options controls a sync run.
type Options struct {
	DryRun    bool
	Verbose   bool
	Force     bool
	IfDue     bool
	FixLayout bool // convert a mismatched container to its configured layout
	Frequency time.Duration
	StateDir  string
}

// Run reconciles the selected repos in a bounded, isolated sweep.
func Run(reg *config.Registry, repos []model.Repo, opts Options) []Result {
	results := make([]Result, len(repos))
	var g errgroup.Group
	g.SetLimit(6)
	for i, r := range repos {
		i, r := i, r
		g.Go(func() error {
			results[i] = syncRepo(reg, r, opts)
			return nil
		})
	}
	_ = g.Wait()
	return results
}

type run struct {
	reg       *config.Registry
	r         model.Repo
	opts      Options
	container string
	branch    string // primary important branch (branches[0])

	// The unit currently being reconciled: a single working tree, or one
	// worktree of a worktree-layout repo. dir is its working tree, ub its
	// branch, and unit its report label ("" for a single tree).
	dir  string
	ub   string
	unit string

	res *Result
}

func syncRepo(reg *config.Registry, r model.Repo, opts Options) Result {
	res := &Result{Name: repoName(r)}
	x := &run{reg: reg, r: r, opts: opts, container: r.Container(), branch: branch0(r), res: res}

	if reason := deferredReason(r); reason != "" {
		res.Outcome, res.Detail = Deferred, reason
		x.add("deferred: %s", reason)
		return *res
	}
	if opts.IfDue && !opts.Force && !x.due() {
		res.Outcome, res.Detail = UpToDate, "not due"
		x.add("skipped: not due (last sync within %s)", opts.Frequency)
		return *res
	}

	x.provisionAndUpdate()
	x.hooks()
	if !opts.DryRun && res.Err == nil {
		x.writeTimestamp()
	}
	return *res
}

// provisionAndUpdate reconciles the repo in whichever layout it actually has on
// disk — provisioning fresh in the configured layout when absent — then updates
// it (DESIGN §4.1, §5.1). A worktree repo reconciles one worktree per important
// branch; a single-tree repo reconciles the container itself. A layout that
// disagrees with config is reconciled as far as the on-disk shape allows and
// surfaced, but never reorganized here: conversion is the explicit
// sync --fix-layout path.
func (x *run) provisionAndUpdate() {
	kind := gitx.ClassifyLayout(x.container)
	mismatch := kind != gitx.LayoutAbsent && (kind == gitx.LayoutWorktree) != x.r.Worktrees

	switch {
	case kind == gitx.LayoutWorktree:
		x.syncWorktree()
	case kind == gitx.LayoutSingle:
		x.syncSingle()
	case x.r.Worktrees: // absent → provision in the configured layout
		x.syncWorktree()
	default:
		x.syncSingle()
	}

	if mismatch {
		if x.opts.FixLayout {
			x.relayout(kind) // data is already synced above; now convert
		} else {
			x.add("on-disk layout is %s but config wants worktrees=%v — run: sync --fix-layout",
				layoutName(kind), x.r.Worktrees)
			x.attention("layout mismatch — run: sync --fix-layout")
		}
	}
}

func layoutName(k gitx.LayoutKind) string {
	if k == gitx.LayoutWorktree {
		return "worktree"
	}
	return "single"
}

// syncSingle provisions and updates a single working tree (worktrees = false).
func (x *run) syncSingle() {
	if !x.provision() {
		return
	}
	x.dir, x.ub = x.container, x.branch
	switch x.r.Workflow {
	case model.Vendor:
		x.updateVendor()
	case model.ForkPR:
		if x.onImportantBranch() {
			x.updateForkPR()
		}
	default: // upstream-push, supply-chain-mirror (both track origin)
		if x.onImportantBranch() {
			x.updateTracking("origin/" + x.ub)
			x.mirrorReview()
		}
	}
}

// syncWorktree provisions a bare+worktree container when absent, then reconciles
// each important branch's worktree, adding any that a newly-declared branch
// still lacks (DESIGN §4, §5.3).
func (x *run) syncWorktree() {
	if gitx.ClassifyLayout(x.container) == gitx.LayoutAbsent {
		if !x.provisionWorktree() {
			return
		}
	}
	for _, b := range x.r.Branches {
		wt := filepath.Join(x.container, b)
		if !gitx.IsRepo(wt) {
			if x.opts.DryRun {
				x.add("would add worktree %s", b)
				continue
			}
			if err := gitx.WorktreeAdd(x.container, wt, b); err != nil {
				x.attention("worktree add failed")
				x.add("add worktree %s failed: %v", b, err)
				continue
			}
			x.add("added worktree %s", b)
		}
		x.updateUnit(wt, b)
	}
}

// updateUnit reconciles one working tree (a worktree, or the single tree) to its
// branch per workflow.
func (x *run) updateUnit(dir, branch string) {
	x.dir, x.ub, x.unit = dir, branch, branch
	defer func() { x.unit = "" }()
	switch x.r.Workflow {
	case model.Vendor:
		x.updateVendor()
	case model.ForkPR:
		x.updateForkPR()
	default:
		x.updateTracking("origin/" + branch)
		x.mirrorReview()
	}
}

// provisionWorktree creates the bare repo, its .git pointer, remotes, and a
// worktree per important branch (DESIGN §4). Worktrees are added by syncWorktree
// after this returns.
func (x *run) provisionWorktree() bool {
	origin, upstream, ok := x.resolveRemotes()
	if !ok {
		return false
	}
	bare := filepath.Join(x.container, ".bare")
	if x.opts.DryRun {
		x.add("would clone --bare %s and add worktrees %v", origin, x.r.Branches)
		x.res.Outcome, x.res.Detail = Updated, "would clone (worktree)"
		return false
	}
	x.add("cloning bare %s → %s", origin, shorten(bare))
	if err := os.MkdirAll(x.container, 0o755); err != nil {
		x.fail(err)
		return false
	}
	if err := gitx.CloneBare(origin, bare); err != nil {
		x.fail(err)
		return false
	}
	if err := writeGitFile(x.container); err != nil {
		x.fail(err)
		return false
	}
	_, _ = gitx.EnsureRemote(x.container, "origin", origin)
	if upstream != "" {
		_, _ = gitx.EnsureRemote(x.container, "upstream", upstream)
	}
	if err := gitx.Fetch(x.container, "origin"); err != nil {
		x.fail(err)
		return false
	}
	if upstream != "" {
		_ = gitx.Fetch(x.container, "upstream")
	}
	x.res.Cloned = true
	return true
}

// resolveRemotes returns the origin and (when a fork exists) upstream clone URLs
// for a declared repo, or the discovered origin verbatim.
func (x *run) resolveRemotes() (origin, upstream string, ok bool) {
	if x.r.OriginURL != "" {
		return x.r.OriginURL, "", true
	}
	if x.r.Dir != "" {
		x.attention("no remote")
		x.add("discovered repo has no origin remote: nothing to sync")
		return "", "", false
	}
	originID := x.r.ID
	if x.r.Fork != nil {
		originID = *x.r.Fork
	}
	u, err := x.reg.PhysicalID(originID, x.r.Tags)
	if err != nil {
		x.fail(err)
		return "", "", false
	}
	if x.r.Fork != nil {
		if up, err := x.reg.PhysicalID(x.r.ID, x.r.Tags); err == nil {
			upstream = up
		}
	}
	return u, upstream, true
}

// writeGitFile writes the container's `.git` file pointing at the bare repo, so
// git commands run from the container root resolve to it (DESIGN §4).
func writeGitFile(container string) error {
	return os.WriteFile(filepath.Join(container, ".git"), []byte("gitdir: ./.bare\n"), 0o644)
}

// provision resolves the origin, clones when absent, ensures remotes, fetches,
// and applies the dirty guard. It returns false — having recorded the outcome —
// when the caller must not proceed to a workflow update (dry-run clone,
// discovered no-remote, dirty tree, or a failure).
func (x *run) provision() bool {
	// A discovered repo (found on disk) carries its own origin; act on that
	// rather than re-resolving through [hosts.*], which need not know its host.
	// A declared repo resolves its origin (or fork) from the registry.
	originURL := x.r.OriginURL
	if originURL == "" {
		if x.r.Dir != "" {
			x.attention("no remote")
			x.add("discovered repo has no origin remote: nothing to sync")
			return false
		}
		originID := x.r.ID
		if x.r.Fork != nil {
			originID = *x.r.Fork
		}
		u, err := x.reg.PhysicalID(originID, x.r.Tags)
		if err != nil {
			x.fail(err)
			return false
		}
		originURL = u
	}

	// Provision.
	if !gitx.IsRepo(x.container) {
		if x.opts.DryRun {
			x.add("would clone %s → %s", originURL, shorten(x.container))
			x.res.Outcome, x.res.Detail = Updated, "would clone"
			return false
		}
		x.add("cloning %s", originURL)
		if err := os.MkdirAll(filepath.Dir(x.container), 0o755); err != nil {
			x.fail(err)
			return false
		}
		if err := gitx.Clone(originURL, x.container); err != nil {
			x.fail(err)
			return false
		}
		x.res.Cloned = true
	}

	// Ensure remotes: origin, plus upstream when a fork exists.
	if !x.opts.DryRun {
		if changed, _ := gitx.EnsureRemote(x.container, "origin", originURL); changed {
			x.add("set origin = %s", originURL)
		}
		if x.r.Fork != nil {
			if up, err := x.reg.PhysicalID(x.r.ID, x.r.Tags); err == nil {
				if changed, _ := gitx.EnsureRemote(x.container, "upstream", up); changed {
					x.add("set upstream = %s", up)
				}
			}
		}
	}

	// Fetch (dry-run assesses against existing refs without touching the network).
	if x.opts.DryRun {
		x.add("would fetch origin%s", ifFork(x.r, " and upstream"))
	} else {
		if err := gitx.Fetch(x.container, "origin"); err != nil {
			x.fail(err)
			return false
		}
		x.add("fetched origin")
		if x.r.Fork != nil {
			if err := gitx.Fetch(x.container, "upstream"); err == nil {
				x.add("fetched upstream")
			}
		}
	}

	// Dirty guard.
	if dirty, _ := gitx.IsDirty(x.container); dirty {
		x.attention("dirty — updates skipped")
		x.add("dirty working tree: skipping updates")
		return false
	}
	return true
}

// onImportantBranch reports whether the checked-out branch is the primary
// important branch (the single-tree precondition for a branch-tracking update).
func (x *run) onImportantBranch() bool {
	cur, _ := gitx.CurrentBranch(x.container)
	switch {
	case cur == "":
		x.attention("detached HEAD")
		x.add("detached HEAD: skipping update")
		return false
	case cur != x.branch:
		x.attention(fmt.Sprintf("on %s, expected %s", cur, x.branch))
		x.add("on branch %s, expected %s: skipping update", cur, x.branch)
		return false
	}
	return true
}

// updateTracking fast-forwards the checked-out important branch to ref: the
// shared body for upstream-push and supply-chain-mirror (which track origin).
// fork-pr reuses the same fast-forward logic against upstream, then layers a
// fork push on top (updateForkPR).
func (x *run) updateTracking(ref string) {
	if _, ok := gitx.RevParse(x.dir, ref); !ok {
		x.attention(ref + " missing")
		x.add("no %s", ref)
		return
	}
	ahead, behind, _ := gitx.AheadBehind(x.dir, ref)
	switch {
	case ahead == 0 && behind == 0:
		x.add("%s up to date with %s", x.ub, ref)
		x.ok()
	case ahead == 0 && behind > 0:
		if x.opts.DryRun {
			x.add("would fast-forward %s +%d", x.ub, behind)
			x.updated(fmt.Sprintf("+%d (dry-run)", behind))
			return
		}
		if err := gitx.FastForwardCurrent(x.dir, ref); err != nil {
			x.applyRewrite(ref) // behind-only but non-FF ⇒ upstream rewrite
			return
		}
		x.add("fast-forwarded %s +%d", x.ub, behind)
		x.updated(fmt.Sprintf("+%d", behind))
	case ahead > 0 && behind == 0:
		x.attention(fmt.Sprintf("%d unpushed", ahead))
		x.add("%d unpushed commit(s) on %s", ahead, x.ub)
	default:
		x.applyRewrite(ref)
	}
}

// applyRewrite handles a non-fast-forward on an important branch per on_rewrite.
func (x *run) applyRewrite(ref string) {
	ahead, behind, _ := gitx.AheadBehind(x.dir, ref)
	if x.r.OnRewrite == "follow" {
		if ahead > 0 { // rail: never clobber local commits
			x.attention(fmt.Sprintf("rewrite with %d local commit(s) — stopped", ahead))
			x.add("rewrite on %s but %d local commit(s) present: escalated to stop", ref, ahead)
			return
		}
		if x.opts.DryRun {
			x.add("would follow rewrite: reset %s to %s", x.ub, ref)
			x.updated("would follow rewrite")
			return
		}
		if err := gitx.ResetHardCurrent(x.dir, ref); err != nil {
			x.fail(err)
			return
		}
		x.add("followed rewrite: reset %s to %s", x.ub, ref)
		x.updated("followed rewrite")
		return
	}
	x.attention(fmt.Sprintf("rewritten/diverged (+%d/-%d) — stopped", ahead, behind))
	x.add("non-fast-forward on %s (on_rewrite=stop): stopped", ref)
}

// mirrorReview flags a supply-chain-mirror whose upstream is ahead of the
// reviewed fork, without advancing it (DESIGN §5.4).
func (x *run) mirrorReview() {
	if x.r.Workflow != model.SupplyChainMirror || x.r.Fork == nil {
		return
	}
	upRef := "upstream/" + x.ub
	if _, ok := gitx.RevParse(x.dir, upRef); !ok {
		return
	}
	n, err := gitx.CountBetween(x.dir, "origin/"+x.ub, upRef)
	if err != nil || n == 0 {
		return
	}
	x.add("upstream is %d commit(s) ahead of the reviewed mirror — review pending", n)
	x.mark(ReviewPending, fmt.Sprintf("upstream +%d — review pending (repo review %s)", n, x.res.Name))
}

func (x *run) hooks() {
	if x.res.Err != nil {
		return
	}
	for _, h := range x.r.Hooks {
		if h.After != "fetch" {
			continue
		}
		if x.opts.DryRun {
			x.add("would run hook: %s", h.Run)
			continue
		}
		cmd := exec.Command("sh", "-c", h.Run)
		cmd.Dir = x.r.PrimaryTree()
		if out, err := cmd.CombinedOutput(); err != nil {
			x.add("hook failed (%s): %s", h.Run, strings.TrimSpace(string(out)))
			x.attention("hook failed")
		} else {
			x.add("ran hook: %s", h.Run)
		}
	}
}

// --- outcome helpers -------------------------------------------------------

// add records a reasoning-trace line, prefixed with the current worktree's
// branch when reconciling a multi-worktree repo.
func (x *run) add(format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	if x.unit != "" {
		msg = x.unit + ": " + msg
	}
	x.res.Actions = append(x.res.Actions, msg)
}

// mark raises the repo outcome to o (with detail) when o is at least as severe
// as the current outcome, so the most-notable of several worktrees governs the
// summary line while every worktree still contributes to the trace.
func (x *run) mark(o Outcome, detail string) {
	if rank(o) >= rank(x.res.Outcome) {
		x.res.Outcome, x.res.Detail = o, detail
	}
}
func rank(o Outcome) int {
	switch o {
	case Failed:
		return 6
	case Attention:
		return 5
	case ReviewPending:
		return 4
	case Updated:
		return 3
	case Deferred:
		return 2
	default: // UpToDate
		return 1
	}
}
func (x *run) ok() {
	if x.res.Cloned {
		x.mark(Updated, "cloned")
		return
	}
	x.mark(UpToDate, "up to date")
}
func (x *run) updated(detail string) {
	if x.res.Cloned {
		detail = "cloned · " + detail
	}
	x.mark(Updated, detail)
}
func (x *run) attention(detail string) { x.mark(Attention, detail) }
func (x *run) fail(err error) {
	x.res.Err = err
	x.mark(Failed, err.Error())
}

// --- cadence ---------------------------------------------------------------

func (x *run) timestampPath() string {
	name := strings.NewReplacer("/", "_", ":", "_").Replace(x.r.ID.String())
	return filepath.Join(x.opts.StateDir, name)
}
func (x *run) due() bool {
	info, err := os.Stat(x.timestampPath())
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) >= x.opts.Frequency
}
func (x *run) writeTimestamp() {
	_ = os.MkdirAll(x.opts.StateDir, 0o755)
	_ = os.WriteFile(x.timestampPath(), []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644)
}

// --- misc ------------------------------------------------------------------

func branch0(r model.Repo) string {
	if len(r.Branches) > 0 {
		return r.Branches[0]
	}
	return "main"
}

// repoName falls back to the directory leaf for a discovered repo with no id.
func repoName(r model.Repo) string {
	if r.ID.Zero() {
		return filepath.Base(r.Container())
	}
	return r.ID.Short()
}

func deferredReason(r model.Repo) string {
	return "" // every workflow and layout is now reconciled
}

func ifFork(r model.Repo, s string) string {
	if r.Fork != nil {
		return s
	}
	return ""
}

func shorten(p string) string {
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, p); err == nil && !strings.HasPrefix(rel, "..") {
			return "~/" + rel
		}
	}
	return p
}
