package sync

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// squashThenRevert leaves `feature` squash-merged into main and then backed out
// again: the patch tiers still call it merged, and the branch is the last copy
// of the content. Corroboration is the only thing standing between that branch
// and a -D.
func squashThenRevert(t *testing.T, clone string) {
	t.Helper()
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "h", "one\ntwo\n", "work")
	git(t, clone, "checkout", "-q", "main")
	merge(t, clone, "feature", true)
	if err := os.Remove(clone + "/h"); err != nil {
		t.Fatal(err)
	}
	git(t, clone, "commit", "-qam", "back that out")
}

// TestClassifyWithholdsAnUncorroboratedBranch: with a corroborator, "prunable"
// stops being the tiers' word alone. This branch passes every tier and is still
// the only copy of its content, so the label has to say so.
func TestClassifyWithholdsAnUncorroboratedBranch(t *testing.T) {
	_, clone, _ := setupUpstreamRepo(t, "")
	squashThenRevert(t, clone)

	cor := OpenCorroborations().Unbounded()
	defer func() { _ = cor.Close() }()
	verdicts, err := Classify(clone, mainRepo(), cor)
	must(t, err)

	var v Verdict
	for _, got := range verdicts {
		if got.Name == "feature" {
			v = got
		}
	}
	if !v.State.Merged() {
		t.Fatalf("setup is wrong: the tiers should still call feature merged, got %v", v.State)
	}
	if v.Prunable {
		t.Error("a branch holding the last copy of its work was reported prunable")
	}
	if v.Blocker != "could not corroborate" {
		t.Errorf("Blocker = %q, want %q", v.Blocker, "could not corroborate")
	}
	// Without a corroborator the tiers' word stands alone — which is what makes
	// passing nil a decision rather than a default.
	plain, err := Classify(clone, mainRepo(), nil)
	must(t, err)
	for _, got := range plain {
		if got.Name == "feature" && !got.Prunable {
			t.Error("without a corroborator the label should be the tiers' word alone")
		}
	}
}

// TestCorroborationIsCachedOnTheShaPair: the answer is a pure function of the
// two shas, so a second ask must not re-run git. A sweep repeats; the expensive
// branches are exactly the ones that would otherwise pay again every time.
func TestCorroborationIsCachedOnTheShaPair(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	_, clone, _ := setupUpstreamRepo(t, "")
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "h", "one\ntwo\n", "work")
	git(t, clone, "checkout", "-q", "main")
	merge(t, clone, "feature", true)

	branchSHA, _ := gitx.RevParse(clone, "feature")
	baseSHA, _ := gitx.RevParse(clone, "main")

	cor := OpenCorroborations()
	first := cor.Corroborate(clone, "feature", "main", branchSHA, baseSHA, 0)
	if !first.OK {
		t.Fatalf("a squash merge was not corroborated: %v", first.Tried)
	}
	second := cor.Corroborate(clone, "feature", "main", branchSHA, baseSHA, 0)
	if !second.OK {
		t.Fatal("the cached answer disagreed with the one it cached")
	}
	if !cached(second.Tried) {
		t.Errorf("second ask re-ran the routes: %v", second.Tried)
	}
	// The reasons come back with it, so --explain can still say why on a hit.
	if !strings.Contains(strings.Join(second.Tried, " "), "reverse-apply:") {
		t.Errorf("the cached answer forgot how it was reached: %v", second.Tried)
	}

	// And it survives the process: Close writes it, a fresh cache reads it.
	must(t, cor.Close())
	again := OpenCorroborations().Corroborate(clone, "feature", "main", branchSHA, baseSHA, 0)
	if !again.OK || !cached(again.Tried) {
		t.Errorf("the cache did not survive being written and reloaded: %v", again.Tried)
	}
}

// cached reports whether an answer came back from the cache rather than the
// routes. The marker is appended after the remembered reasons, so it is the
// last line that says so.
func cached(tried []string) bool {
	return len(tried) > 0 && strings.HasPrefix(tried[len(tried)-1], "cached:")
}

// TestABudgetThatRanOutIsNotCached is the distinction the cache turns on. A
// route that answered says something about the two trees and is true forever;
// a route the clock stopped says how busy this machine was, and remembering it
// would strand the branch on every future sweep for a reason that has nothing
// to do with it.
func TestABudgetThatRanOutIsNotCached(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	_, clone, _ := setupUpstreamRepo(t, "")
	git(t, clone, "checkout", "-q", "-b", "feature")
	writeCommit(t, clone, "h", "one\ntwo\n", "work")
	git(t, clone, "checkout", "-q", "main")
	merge(t, clone, "feature", true)

	branchSHA, _ := gitx.RevParse(clone, "feature")
	baseSHA, _ := gitx.RevParse(clone, "main")

	cor := OpenCorroborations()
	got := cor.Corroborate(clone, "feature", "main", branchSHA, baseSHA, time.Nanosecond)
	if got.OK {
		t.Fatal("a budget of one nanosecond corroborated something")
	}
	if got.Complete {
		t.Error("a check the clock stopped was reported as a completed outcome")
	}
	must(t, cor.Close())

	// Nothing was written, so an unhurried ask still gets the real answer.
	body, err := os.ReadFile(cache + "/repo/corroborations.json")
	if err == nil {
		var entries map[string]cacheEntry
		must(t, json.Unmarshal(body, &entries))
		if len(entries) != 0 {
			t.Errorf("an incomplete check was cached: %v", entries)
		}
	}
	real := OpenCorroborations().Corroborate(clone, "feature", "main", branchSHA, baseSHA, 0)
	if !real.OK {
		t.Errorf("the branch stayed stranded after the budget lifted: %v", real.Tried)
	}
}

// TestCorroborateBudgetOf: unset means the ambient default, and zero means off.
// Conflating them would make "I said nothing" and "I said don't" the same
// instruction, which is why the setting is a pointer.
func TestCorroborateBudgetOf(t *testing.T) {
	if got := corroborateBudgetOf(model.Repo{}); got != gitx.DefaultCorroborateBudget() {
		t.Errorf("unset = %v, want the ambient default %v", got, gitx.DefaultCorroborateBudget())
	}
	off := time.Duration(0)
	if got := corroborateBudgetOf(model.Repo{CorroborateBudget: &off}); got != 0 {
		t.Errorf("explicit 0 = %v, want 0 (off)", got)
	}
	set := 5 * time.Second
	if got := corroborateBudgetOf(model.Repo{CorroborateBudget: &set}); got != set {
		t.Errorf("explicit 5s = %v, want 5s", got)
	}
}
