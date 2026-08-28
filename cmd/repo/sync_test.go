package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/model"
	syncpkg "github.com/michael-odell/repo/internal/sync"
)

func renderedLines(t *testing.T, opts syncpkg.Options) []string {
	t.Helper()
	results := []syncpkg.Result{
		{Name: "pex/space-flavor", Workflow: model.UpstreamPush, Outcome: syncpkg.UpToDate, Detail: "up to date"},
		{
			Name: "romkatv/powerlevel10k", Workflow: model.SupplyChainMirror,
			Outcome: syncpkg.Attention, Detail: "2 branches need attention",
			Branches: []syncpkg.BranchNote{
				{Name: "wip/experiment", Summary: "never pushed", Outcome: syncpkg.Attention},
			},
		},
	}
	var buf bytes.Buffer
	renderSync(&buf, results, opts)
	return strings.Split(buf.String(), "\n")
}

func lineWith(t *testing.T, lines []string, substr string) string {
	t.Helper()
	for _, l := range lines {
		if strings.Contains(l, substr) {
			return l
		}
	}
	t.Fatalf("no line containing %q in:\n%s", substr, strings.Join(lines, "\n"))
	return ""
}

// TestRenderSyncShowsWorkflow: the workflow decides how a repo is synced and is
// as often inferred from its remotes as configured, so it belongs on the row
// rather than being something to go and look up.
func TestRenderSyncShowsWorkflow(t *testing.T) {
	lines := renderedLines(t, syncpkg.Options{})
	if l := lineWith(t, lines, "pex/space-flavor"); !strings.Contains(l, model.UpstreamPush) {
		t.Errorf("repo row %q should name its workflow", l)
	}
	if l := lineWith(t, lines, "romkatv/powerlevel10k"); !strings.Contains(l, model.SupplyChainMirror) {
		t.Errorf("repo row %q should name its workflow", l)
	}

	verbose := renderedLines(t, syncpkg.Options{Verbose: true})
	if l := lineWith(t, verbose, "romkatv/powerlevel10k"); !strings.Contains(l, "("+model.SupplyChainMirror+", ") {
		t.Errorf("verbose row %q should name its workflow", l)
	}
}

// TestRenderSyncVerboseShowsWhyItFailed: a failure records its reason as the
// repo's outcome detail, not as a trace line, so a --verbose report that only
// printed the trace showed a red ✗ and a name — nothing about what went wrong,
// in the mode you run precisely because something went wrong.
func TestRenderSyncVerboseShowsWhyItFailed(t *testing.T) {
	results := []syncpkg.Result{{
		Name:     "acme/broken",
		Workflow: model.ForkPR,
		Outcome:  syncpkg.Failed,
		Detail:   "git fetch --prune --tags origin: fatal: repository not found",
		Err:      errors.New("git fetch --prune --tags origin: fatal: repository not found"),
		Elapsed:  2*time.Minute + 40*time.Second,
	}}
	var buf bytes.Buffer
	renderSync(&buf, results, syncpkg.Options{Verbose: true})

	line := lineWith(t, strings.Split(buf.String(), "\n"), "acme/broken")
	if !strings.Contains(line, "repository not found") {
		t.Errorf("verbose failure row = %q, want the reason it failed", line)
	}
	if !strings.Contains(line, "fork-pr") || !strings.Contains(line, "2m40s") {
		t.Errorf("verbose failure row = %q, should still carry workflow and duration", line)
	}
}

// TestRenderSyncTallyDoesNotUnderstateFailures: the ⚠ bucket used to be called
// "need attention", which beside a count of failures reads as a claim that the
// failures don't need any — and it sat last, after the good news.
func TestRenderSyncTallyDoesNotUnderstateFailures(t *testing.T) {
	results := []syncpkg.Result{
		{Name: "acme/one", Outcome: syncpkg.Failed, Detail: "fetch failed"},
		{Name: "acme/two", Outcome: syncpkg.Failed, Detail: "fetch failed"},
		{Name: "acme/three", Outcome: syncpkg.Attention, Detail: "1 branch needs attention"},
		{Name: "acme/four", Outcome: syncpkg.UpToDate, Detail: "up to date"},
	}
	var buf bytes.Buffer
	renderSync(&buf, results, syncpkg.Options{})
	tally := lineWith(t, strings.Split(buf.String(), "\n"), "failed ·")

	if strings.Contains(tally, "need attention") {
		t.Errorf("tally = %q, should not label one bucket as the set needing attention", tally)
	}
	if !strings.Contains(tally, "2 failed") || !strings.Contains(tally, "1 flagged") {
		t.Errorf("tally = %q, want 2 failed and 1 flagged", tally)
	}
	if strings.Index(tally, "failed") > strings.Index(tally, "up to date") {
		t.Errorf("tally = %q, want the failures before the good news", tally)
	}
}

// TestRenderSyncVerboseTimesEachStep: "this repo takes four minutes" is only
// actionable once you know which step spent them. A trace line is written after
// its work, so the gap from the previous line is that work's duration — which
// is what gets printed, next to the thing it measures.
func TestRenderSyncVerboseTimesEachStep(t *testing.T) {
	results := []syncpkg.Result{{
		Name: "acme/manifests", Workflow: model.UpstreamPush,
		Outcome: syncpkg.UpToDate, Detail: "up to date",
		Elapsed: 4*time.Minute + 2*time.Second,
		Actions: []syncpkg.Action{
			{Text: "fetched origin", At: 4 * time.Minute},
			{Text: "main up to date with origin/main", At: 4*time.Minute + 2*time.Second},
		},
	}}
	var buf bytes.Buffer
	renderSync(&buf, results, syncpkg.Options{Verbose: true})
	lines := strings.Split(buf.String(), "\n")

	// The fetch took the four minutes; the branch check took two seconds.
	if l := lineWith(t, lines, "fetched origin"); !strings.Contains(l, "4m0s") {
		t.Errorf("fetch line = %q, want the 4m it spent", l)
	}
	if l := lineWith(t, lines, "up to date with origin/main"); !strings.Contains(l, "2s") {
		t.Errorf("branch line = %q, want the 2s it spent", l)
	}
	if l := lineWith(t, lines, "up to date with origin/main"); strings.Contains(l, "4m2s") {
		t.Errorf("line = %q, should show its own step, not the running total", l)
	}
}

// TestRenderSyncNamesTheSlowestRepo: a report organised by outcome says nothing
// about where the time went, and the repo that ate the sweep is often a ✓ — so
// it has to be named separately or not at all. Only when it was slow enough to
// plausibly be what you were waiting on.
func TestRenderSyncNamesTheSlowestRepo(t *testing.T) {
	quick := []syncpkg.Result{
		{Name: "acme/one", Workflow: model.UpstreamPush, Detail: "up to date", Elapsed: 2 * time.Second},
		{Name: "acme/two", Workflow: model.UpstreamPush, Detail: "up to date", Elapsed: 3 * time.Second},
	}
	var buf bytes.Buffer
	renderSync(&buf, quick, syncpkg.Options{})
	if strings.Contains(buf.String(), "slowest") {
		t.Errorf("a sweep with nothing slow in it should not name a slowest repo:\n%s", buf.String())
	}

	slow := append([]syncpkg.Result{}, quick...)
	slow[1].Elapsed = 4 * time.Minute
	buf.Reset()
	renderSync(&buf, slow, syncpkg.Options{})
	if !strings.Contains(buf.String(), "slowest: acme/two 4m0s") {
		t.Errorf("want the slow repo named with its duration:\n%s", buf.String())
	}

	// One repo alone is the whole sweep; naming it says nothing you didn't watch.
	buf.Reset()
	renderSync(&buf, slow[1:], syncpkg.Options{})
	if strings.Contains(buf.String(), "slowest") {
		t.Errorf("a single-repo sweep should not name a slowest:\n%s", buf.String())
	}
}

// TestRenderSyncBranchRowsStayAligned: a branch's summary elaborates its repo's
// detail, so it must stay in the detail column — the empty workflow cell in the
// branch row is what holds that alignment.
func TestRenderSyncBranchRowsStayAligned(t *testing.T) {
	lines := renderedLines(t, syncpkg.Options{})
	repo := lineWith(t, lines, "romkatv/powerlevel10k")
	branch := lineWith(t, lines, "wip/experiment")

	detailCol := strings.Index(repo, "2 branches need attention")
	summaryCol := strings.Index(branch, "never pushed")
	if detailCol != summaryCol {
		t.Errorf("branch summary starts at column %d, repo detail at %d:\n%s\n%s",
			summaryCol, detailCol, repo, branch)
	}
	if strings.Contains(branch, model.SupplyChainMirror) {
		t.Errorf("branch row %q should not repeat the repo's workflow", branch)
	}
}

// TestRenderSyncNamesPrunableBranches: landed branches are observations, so
// they never lift a repo's glyph — and outside show_branches = "all" they get
// no line either. Without the footer, the only way to learn prune had anything
// to offer was to already know to ask it.
// nVerdicts is n placeholder verdicts, for the tally the footer renders — which
// only ever asks how many there are.
func nVerdicts(n int) []syncpkg.Verdict { return make([]syncpkg.Verdict, n) }

func TestRenderSyncNamesPrunableBranches(t *testing.T) {
	results := []syncpkg.Result{
		{Name: "acme/one", Workflow: model.UpstreamPush, Outcome: syncpkg.UpToDate, Prunable: nVerdicts(3)},
		{Name: "acme/two", Workflow: model.UpstreamPush, Outcome: syncpkg.UpToDate, Prunable: nVerdicts(9)},
		{Name: "acme/three", Workflow: model.UpstreamPush, Outcome: syncpkg.UpToDate},
	}
	var buf bytes.Buffer
	renderSync(&buf, results, syncpkg.Options{})
	out := buf.String()

	if !strings.Contains(out, "12 branch(es) prunable across 2 repo(s)") {
		t.Errorf("footer does not total the prunable branches:\n%s", out)
	}
	if !strings.Contains(out, "repo prune") {
		t.Errorf("footer does not name the command that acts on them:\n%s", out)
	}
}

// TestRenderSyncSaysNothingAboutPruningWhenThereIsNothing: a footer that
// appeared on every clean sweep would be noise, and would teach people to skip
// the line that matters when it isn't.
func TestRenderSyncSaysNothingAboutPruningWhenThereIsNothing(t *testing.T) {
	results := []syncpkg.Result{
		{Name: "acme/one", Workflow: model.UpstreamPush, Outcome: syncpkg.UpToDate},
	}
	var buf bytes.Buffer
	renderSync(&buf, results, syncpkg.Options{})
	if strings.Contains(buf.String(), "prunable") {
		t.Errorf("footer mentions pruning with nothing to prune:\n%s", buf.String())
	}
}
