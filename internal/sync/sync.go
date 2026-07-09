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
	branch    string
	res       *Result
}

func syncRepo(reg *config.Registry, r model.Repo, opts Options) Result {
	res := &Result{Name: r.ID.Short()}
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
	x.mirrorReview()
	x.hooks()
	if !opts.DryRun && res.Err == nil {
		x.writeTimestamp()
	}
	return *res
}

func (x *run) provisionAndUpdate() {
	originID := x.r.ID
	if x.r.Fork != nil {
		originID = *x.r.Fork
	}
	originURL, err := x.reg.PhysicalID(originID, x.r.Tags)
	if err != nil {
		x.fail(err)
		return
	}

	// Provision.
	if !gitx.IsRepo(x.container) {
		if x.opts.DryRun {
			x.add("would clone %s → %s", originURL, shorten(x.container))
			x.res.Outcome, x.res.Detail = Updated, "would clone"
			return
		}
		x.add("cloning %s", originURL)
		if err := os.MkdirAll(filepath.Dir(x.container), 0o755); err != nil {
			x.fail(err)
			return
		}
		if err := gitx.Clone(originURL, x.container); err != nil {
			x.fail(err)
			return
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
			return
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
		return
	}

	// Update the primary important branch (must be the checked-out one at Stage 4).
	cur, _ := gitx.CurrentBranch(x.container)
	switch {
	case cur == "":
		x.attention("detached HEAD")
		x.add("detached HEAD: skipping update")
		return
	case cur != x.branch:
		x.attention(fmt.Sprintf("on %s, expected %s", cur, x.branch))
		x.add("on branch %s, expected %s: skipping update", cur, x.branch)
		return
	}

	originRef := "origin/" + x.branch
	if _, ok := gitx.RevParse(x.container, originRef); !ok {
		x.attention(originRef + " missing")
		x.add("no %s", originRef)
		return
	}

	ahead, behind, _ := gitx.AheadBehind(x.container, originRef)
	switch {
	case ahead == 0 && behind == 0:
		x.add("%s up to date with %s", x.branch, originRef)
		x.ok()
	case ahead == 0 && behind > 0:
		if x.opts.DryRun {
			x.add("would fast-forward %s +%d", x.branch, behind)
			x.updated(fmt.Sprintf("+%d (dry-run)", behind))
			return
		}
		if err := gitx.FastForwardCurrent(x.container, originRef); err != nil {
			x.applyRewrite(originRef) // behind-only but non-FF ⇒ upstream rewrite
			return
		}
		x.add("fast-forwarded %s +%d", x.branch, behind)
		x.updated(fmt.Sprintf("+%d", behind))
	case ahead > 0 && behind == 0:
		x.attention(fmt.Sprintf("%d unpushed", ahead))
		x.add("%d unpushed commit(s) on %s", ahead, x.branch)
	default:
		x.applyRewrite(originRef)
	}
}

// applyRewrite handles a non-fast-forward on an important branch per on_rewrite.
func (x *run) applyRewrite(ref string) {
	ahead, behind, _ := gitx.AheadBehind(x.container, ref)
	if x.r.OnRewrite == "follow" {
		if ahead > 0 { // rail: never clobber local commits
			x.attention(fmt.Sprintf("rewrite with %d local commit(s) — stopped", ahead))
			x.add("rewrite on %s but %d local commit(s) present: escalated to stop", ref, ahead)
			return
		}
		if x.opts.DryRun {
			x.add("would follow rewrite: reset %s to %s", x.branch, ref)
			x.updated("would follow rewrite")
			return
		}
		if err := gitx.ResetHardCurrent(x.container, ref); err != nil {
			x.fail(err)
			return
		}
		x.add("followed rewrite: reset %s to %s", x.branch, ref)
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
	upRef := "upstream/" + x.branch
	if _, ok := gitx.RevParse(x.container, upRef); !ok {
		return
	}
	n, err := gitx.CountBetween(x.container, "origin/"+x.branch, upRef)
	if err != nil || n == 0 {
		return
	}
	x.add("upstream is %d commit(s) ahead of the reviewed mirror — review pending", n)
	if x.res.Outcome != Attention && x.res.Outcome != Failed {
		x.res.Outcome = ReviewPending
		x.res.Detail = fmt.Sprintf("upstream +%d — review pending (repo review %s)", n, x.res.Name)
	}
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
		cmd.Dir = x.container
		if out, err := cmd.CombinedOutput(); err != nil {
			x.add("hook failed (%s): %s", h.Run, strings.TrimSpace(string(out)))
			x.attention("hook failed")
		} else {
			x.add("ran hook: %s", h.Run)
		}
	}
}

// --- outcome helpers -------------------------------------------------------

func (x *run) add(format string, a ...any) {
	x.res.Actions = append(x.res.Actions, fmt.Sprintf(format, a...))
}
func (x *run) ok() {
	if x.res.Cloned {
		x.res.Outcome, x.res.Detail = Updated, "cloned"
		return
	}
	x.res.Outcome, x.res.Detail = UpToDate, "up to date"
}
func (x *run) updated(detail string) {
	if x.res.Cloned {
		detail = "cloned · " + detail
	}
	x.res.Outcome, x.res.Detail = Updated, detail
}
func (x *run) attention(detail string) { x.res.Outcome, x.res.Detail = Attention, detail }
func (x *run) fail(err error)          { x.res.Err, x.res.Outcome, x.res.Detail = err, Failed, err.Error() }

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

func deferredReason(r model.Repo) string {
	if r.Worktrees {
		return "worktrees not yet supported"
	}
	if r.Workflow != model.UpstreamPush && r.Workflow != model.SupplyChainMirror {
		return "workflow " + r.Workflow + " not yet supported"
	}
	return ""
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
