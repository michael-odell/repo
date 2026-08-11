package main

import (
	"bytes"
	"strings"
	"testing"

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
	if l := lineWith(t, verbose, "romkatv/powerlevel10k"); !strings.Contains(l, "("+model.SupplyChainMirror+")") {
		t.Errorf("verbose row %q should name its workflow", l)
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
