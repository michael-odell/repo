// Package sync reconciles repositories toward the registry (DESIGN §5). Stage 4
// implements single-tree upstream-push and supply-chain-mirror workflows; other
// workflows and worktrees are deferred (reported, not attempted).
package sync

import (
	"fmt"
	"os"
	"os/exec"
	"path"
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
	Name     string
	Cloned   bool
	Outcome  Outcome
	Detail   string
	Branches []BranchNote // task-branch findings, rendered as sub-bullets (DESIGN §5.6)
	Actions  []string
	Err      error

	// migrate, when set, defers a --fix-layout conversion to the serial phase
	// after every repo's network sync has finished (DESIGN §4.1).
	migrate *pendingMigration
}

// BranchNote is one branch finding — important or task — shown as an indented
// sub-bullet under its repo (DESIGN §5.6) in both compact and --verbose
// output, and regardless of dry-run vs a real sync, whenever there is more
// than one notable branch to report (a lone notable branch folds onto the
// repo's own row instead; see finalizeBranches). Recorded via branchMark,
// which also elevates the repo's own Outcome to match — a notable branch is
// exactly as capable of flagging the repo as any other Outcome source.
type BranchNote struct {
	Name    string
	Summary string
	Outcome Outcome
}

type pendingMigration struct {
	kind gitx.LayoutKind // the container's current on-disk layout
}

// Options controls a sync run.
type Options struct {
	DryRun      bool
	Verbose     bool
	Force       bool
	IfDue       bool
	FixLayout   bool // convert a mismatched container to its configured layout
	LoseIgnored bool // pre-approve discarding .gitignore'd files during a relayout
	Frequency   time.Duration
	StateDir    string
}

// Run reconciles the selected repos in a bounded, isolated sweep, then performs
// any --fix-layout conversions serially. Migrations run only after every repo's
// network work is done, so their prompts are orderly and never interleave with
// concurrent output (DESIGN §4.1).
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

	for i := range results {
		if results[i].migrate == nil || results[i].Err != nil {
			continue // nothing to convert, or the repo's sync already failed
		}
		r := repos[i]
		// The conversion owns the headline outcome; the sync trace is preserved
		// in Actions, but the pre-migration dirty/attention state should not mask
		// a successful relayout.
		results[i].Outcome, results[i].Detail = UpToDate, ""
		x := &run{reg: reg, r: r, opts: opts, container: r.Container(), branch: branch0(r), res: &results[i]}
		x.relayout(results[i].migrate.kind)
	}
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

	// totalBranches is the repo's local branch count (important + task),
	// fetched once before any branch is reconciled, so finalizeBranches can
	// decide whether naming a lone notable branch (or counting the boring
	// ones) adds information or just noise on a single-branch repo.
	totalBranches int

	// detailIsBranch is true when the last successful mark() call came from
	// branchMark — i.e. a branch, not some other repo-level fact, currently
	// owns res.Outcome/Detail. finalizeBranches uses it to avoid overwriting
	// a non-branch fact that ties a branch's rank (see finalizeBranches).
	detailIsBranch bool

	res *Result
}

func syncRepo(reg *config.Registry, r model.Repo, opts Options) Result {
	res := &Result{Name: repoName(r)}
	x := &run{reg: reg, r: r, opts: opts, container: r.Container(), branch: branch0(r), res: res}

	// A discovered repo whose important branch couldn't be confidently inferred
	// (no origin default-branch symref, no known mainline name — see
	// discoveredRepo) is flagged rather than guessed from whatever's checked
	// out: silently treating an arbitrary branch as important would get its
	// task-branch findings (and push policy) wrong.
	if r.Dir != "" && x.branch == "" {
		res.Outcome, res.Detail = Attention, "can't tell which branch is important — add `branches = [...]`"
		x.add("no origin default branch and no known mainline name (main/master/develop) among local branches")
		return *res
	}

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
	x.finalizeBranches()
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
			// Defer the conversion to Run's serial phase, after all network work.
			x.res.migrate = &pendingMigration{kind: kind}
			x.add("layout mismatch — will convert to %s layout after sync",
				layoutName(opp(kind)))
		} else {
			x.add("on-disk layout is %s but config wants worktrees=%v — run: sync --fix-layout",
				layoutName(kind), x.r.Worktrees)
			x.attention("layout mismatch — run: sync --fix-layout")
		}
	}
}

// hookDir picks an existing working tree to run hooks in: the configured primary
// tree when it exists, else the container (e.g. a mismatched single clone that
// has not yet been converted to worktrees).
func (x *run) hookDir() string {
	if pt := x.r.PrimaryTree(); gitx.IsRepo(pt) {
		return pt
	}
	return x.container
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
	x.totalBranches = countLocalBranches(x.container)
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
	x.taskBranches()
}

// syncWorktree provisions a bare+worktree container when absent, then reconciles
// each important branch's worktree, adding any that a newly-declared branch
// still lacks (DESIGN §4, §5.3).
func (x *run) syncWorktree() {
	if gitx.ClassifyLayout(x.container) == gitx.LayoutAbsent {
		if !x.provisionWorktree() {
			return
		}
	} else if !x.fetchWorktreeRemotes() {
		// provisionWorktree's own clone+fetch already leaves an absent
		// container current; an already-provisioned one still needs this
		// network step every sync, the same way provision() does for a
		// single tree — without it, a worktree repo would only ever see the
		// state it had at its first clone.
		return
	}
	x.totalBranches = countLocalBranches(x.container)
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
	// Once per repo, not per worktree — task branches live in the shared bare
	// repo, not any one worktree.
	x.taskBranches()
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

// fetchWorktreeRemotes ensures origin (and upstream, for a fork) are set and
// fetched for an already-provisioned worktree container — the network step
// every sync needs, mirroring provision()'s fetch for a single tree (dry-run
// only reports intent). provisionWorktree covers this on an initial clone;
// this covers every sync after.
func (x *run) fetchWorktreeRemotes() bool {
	origin, upstream, ok := x.resolveRemotes()
	if !ok {
		return false
	}
	if x.opts.DryRun {
		x.add("would fetch origin%s", ifFork(x.r, " and upstream"))
		return true
	}
	if changed, _ := gitx.EnsureRemote(x.container, "origin", origin); changed {
		x.add("set origin = %s", origin)
	}
	if err := gitx.Fetch(x.container, "origin"); err != nil {
		x.fail(err)
		return false
	}
	x.add("fetched origin")
	if upstream != "" {
		if changed, _ := gitx.EnsureRemote(x.container, "upstream", upstream); changed {
			x.add("set upstream = %s", upstream)
		}
		if err := gitx.Fetch(x.container, "upstream"); err == nil {
			x.add("fetched upstream")
		}
	}
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
	u, err := x.reg.PhysicalID(originID, x.r.Roots)
	if err != nil {
		x.fail(err)
		return "", "", false
	}
	if x.r.Fork != nil {
		if up, err := x.reg.PhysicalID(x.r.ID, x.r.Roots); err == nil {
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
		u, err := x.reg.PhysicalID(originID, x.r.Roots)
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
			if up, err := x.reg.PhysicalID(x.r.ID, x.r.Roots); err == nil {
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

	// Dirty guard: uncommitted tracked changes block updates (a merge could
	// conflict with them). Untracked files are reported the same way but never
	// block — a plain fast-forward doesn't touch them (DESIGN §5.1, principle 3).
	dirty, _ := gitx.IsDirty(x.container)
	untracked, _ := gitx.UntrackedFiles(x.container)
	if len(untracked) > 0 {
		x.attention(fmt.Sprintf("%d untracked file(s)", len(untracked)))
		x.add("%d untracked file(s) present", len(untracked))
	}
	if dirty {
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
// shared body for upstream-push and supply-chain-mirror (which track origin),
// and a vendor branch pin. fork-pr reuses the same fast-forward logic against
// upstream, then layers a fork push on top (updateForkPR).
func (x *run) updateTracking(ref string) {
	if _, ok := gitx.RevParse(x.dir, ref); !ok {
		x.branchMark(x.ub, Attention, "missing on origin")
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
			x.branchMark(x.ub, Updated, fmt.Sprintf("would fast-forward +%d", behind))
			return
		}
		if err := gitx.FastForwardCurrent(x.dir, ref); err != nil {
			x.applyRewrite(ref) // behind-only but non-FF ⇒ upstream rewrite
			return
		}
		x.add("fast-forwarded %s +%d", x.ub, behind)
		x.branchMark(x.ub, Updated, fmt.Sprintf("fast-forwarded +%d", behind))
	case ahead > 0 && behind == 0:
		x.pushOrReport(ahead, "origin")
	default:
		x.applyRewrite(ref)
	}
}

// pushOrReport handles the important branch's ahead-only (clean fast-forward)
// commits per the `push` setting (DESIGN §3.6): every workflow shares this
// setting and this code path, only the default differs.
func (x *run) pushOrReport(ahead int, remote string) {
	switch x.r.Push {
	case "auto":
		x.pushAhead(ahead, remote)
	case "never":
		x.branchMark(x.ub, Attention, fmt.Sprintf("%d unpushed (unexpected)", ahead))
		x.add("%d local commit(s) on %s — consider changing workflow (push=never)", ahead, x.ub)
	default: // "manual"
		x.branchMark(x.ub, Attention, fmt.Sprintf("%d unpushed", ahead))
		x.add("%d unpushed commit(s) on %s", ahead, x.ub)
	}
}

// pushAhead fast-forward-pushes local commits straight to remote. Only ever
// called when behind == 0 (relative to remote), so this is always a clean,
// non-force push (DESIGN §3.6 push=auto).
func (x *run) pushAhead(ahead int, remote string) {
	if x.opts.DryRun {
		x.add("would push %s +%d to %s", x.ub, ahead, remote)
		x.branchMark(x.ub, Updated, fmt.Sprintf("would push +%d", ahead))
		return
	}
	if err := gitx.Push(x.dir, remote, x.ub); err != nil {
		x.branchMark(x.ub, Attention, fmt.Sprintf("%d unpushed — push failed", ahead))
		x.add("push %s to %s failed: %v", x.ub, remote, err)
		return
	}
	x.add("pushed %s +%d to %s", x.ub, ahead, remote)
	x.branchMark(x.ub, Updated, fmt.Sprintf("pushed +%d", ahead))
}

// applyRewrite handles a non-fast-forward on an important branch per
// force_pull (DESIGN §5.2): the remote rewrote; a listed branch follows
// automatically, an unmatched one stops and reports.
func (x *run) applyRewrite(ref string) {
	ahead, behind, _ := gitx.AheadBehind(x.dir, ref)
	if matchesAny(x.r.ForcePull, x.ub) {
		if ahead > 0 { // rail: never clobber local commits
			x.branchMark(x.ub, Attention, fmt.Sprintf("rewrite with %d local commit(s) — stopped", ahead))
			x.add("rewrite on %s but %d local commit(s) present: escalated to stop", ref, ahead)
			return
		}
		if x.opts.DryRun {
			x.add("would follow rewrite: reset %s to %s", x.ub, ref)
			x.branchMark(x.ub, Updated, "would follow rewrite")
			return
		}
		if err := gitx.ResetHardCurrent(x.dir, ref); err != nil {
			x.fail(err)
			return
		}
		x.add("followed rewrite: reset %s to %s", x.ub, ref)
		x.branchMark(x.ub, Updated, "followed rewrite")
		return
	}
	x.branchMark(x.ub, Attention, fmt.Sprintf("rewritten/diverged (+%d/-%d) — stopped", ahead, behind))
	x.add("non-fast-forward on %s (no force_pull match): stopped", ref)
}

// matchesAny reports whether name matches any of the glob patterns (DESIGN
// §3.6/§5.2). "*" alone is special-cased to mean every branch — plain
// path.Match doesn't cross "/" boundaries, which would otherwise surprise
// anyone naming a branch like "wip/foo".
func matchesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if p == "*" {
			return true
		}
		if ok, _ := path.Match(p, name); ok {
			return true
		}
	}
	return false
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
	x.branchMark(x.ub, ReviewPending, fmt.Sprintf("upstream +%d — review pending (repo review %s)", n, x.res.Name))
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
		cmd.Dir = x.hookDir()
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

// branchMark records a per-branch finding — important or task — as a
// candidate sub-bullet (DESIGN §5.6) and elevates the repo's own Outcome to
// match, via mark()'s usual rank-max rule. Whether the finding actually
// renders as a bullet or folds onto the repo's own row is decided later, once
// every branch has reported in, by finalizeBranches. When this call wins the
// row (mark reports true), detailIsBranch records that a branch — not some
// other repo-level fact — is the current reason for the row, which is what
// lets finalizeBranches tell the two apart on a rank tie.
//
// A branch already holding a finding gets it replaced, not duplicated: a
// fork-pr branch that fast-forwards and then pushes, for instance, calls this
// twice for the same name, and the second call is simply the fuller
// description of where that branch ended up — same as mark() always let the
// last call for a repo win.
func (x *run) branchMark(name string, o Outcome, detail string) {
	if o == Updated && x.res.Cloned {
		detail = "cloned · " + detail
	}
	if x.mark(o, detail) {
		x.detailIsBranch = true
	}
	if o == UpToDate {
		return
	}
	for i, b := range x.res.Branches {
		if b.Name == name {
			x.res.Branches[i] = BranchNote{Name: name, Summary: detail, Outcome: o}
			return
		}
	}
	x.res.Branches = append(x.res.Branches, BranchNote{Name: name, Summary: detail, Outcome: o})
}

// finalizeBranches decides, once every important and task branch has
// reported in, how the repo's own row represents them (DESIGN §5.6): a
// single notable branch folds onto the row alone — no accompanying bullet —
// named when the repo tracks more than one branch, so the reader isn't left
// guessing which one; two or more roll up into a count, with every notable
// branch still broken out as its own sub-bullet so nothing is lost; and a
// repo with no notable branches at all reports how many were checked, when
// that number is worth knowing.
//
// In both the one- and many-branch cases, a non-branch fact (a dirty tree, a
// failed hook, a layout mismatch) that currently owns the row is left alone —
// guarded by detailIsBranch rather than a rank comparison, since a same-rank
// non-branch fact (hooks run after every branch, so it can tie) must still
// win: branch findings are never lost either way, since the bullets list them
// regardless of what the row says, but a non-branch fact has nowhere else to
// go.
func (x *run) finalizeBranches() {
	switch len(x.res.Branches) {
	case 0:
		if x.res.Outcome == UpToDate && x.totalBranches > 1 {
			x.res.Detail = fmt.Sprintf("%d branches up to date", x.totalBranches)
		}
	case 1:
		if x.totalBranches > 1 && x.detailIsBranch {
			b := x.res.Branches[0]
			x.res.Detail = b.Name + ": " + b.Summary
		}
		x.res.Branches = nil
	default:
		if x.detailIsBranch {
			x.res.Detail = fmt.Sprintf("%d branches %s", len(x.res.Branches), rollupWord(x.res.Outcome))
		}
	}
}

// rollupWord phrases finalizeBranches' multi-branch rollup by the worst
// Outcome among the notable branches. Failed never reaches here — branch-
// level failures stay repo-level (see fail()), not routed through branchMark.
func rollupWord(o Outcome) string {
	switch o {
	case Attention:
		return "need attention"
	case ReviewPending:
		return "pending review"
	default: // Updated
		return "updated"
	}
}

// countLocalBranches is LocalBranches with a lenient zero on error — used
// only to decide whether naming/counting branches adds information, so a
// failure to list them should silently skip that polish rather than fail.
func countLocalBranches(dir string) int {
	branches, err := gitx.LocalBranches(dir)
	if err != nil {
		return 0
	}
	return len(branches)
}

// mark raises the repo outcome to o (with detail) when o is at least as severe
// as the current outcome, so the most-notable of several worktrees governs the
// summary line while every worktree still contributes to the trace.
func (x *run) mark(o Outcome, detail string) bool {
	if rank(o) >= rank(x.res.Outcome) {
		x.res.Outcome, x.res.Detail = o, detail
		x.detailIsBranch = false
		return true
	}
	return false
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
	if r.Dir != "" {
		return "" // discovered repo: no confident important-branch signal (see discoveredRepo) — flagged by syncRepo, never guessed
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
