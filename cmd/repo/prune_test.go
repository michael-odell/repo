package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
)

// cloneWithLandedBranch builds a clone whose `landed` branch is an ancestor of
// main — the plainest prunable case, and the only tier `git branch -d` will
// also vouch for. main stays checked out, so nothing blocks the deletion.
func cloneWithLandedBranch(t *testing.T, wd, name string) string {
	t.Helper()
	origin := filepath.Join(t.TempDir(), name+".git")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "init", "-q", "-b", "main", ".")
	if err := os.WriteFile(filepath.Join(origin, "f"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "add", "f")
	git(t, origin, "commit", "-q", "-m", "one")

	dir := filepath.Join(wd, name)
	git(t, wd, "clone", "-q", origin, dir)
	git(t, dir, "checkout", "-q", "-b", "landed")
	if err := os.WriteFile(filepath.Join(dir, "g"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "g")
	git(t, dir, "commit", "-q", "-m", "two")
	git(t, dir, "checkout", "-q", "main")
	git(t, dir, "merge", "-q", "--ff-only", "landed")
	return dir
}

func selectedRepo(t *testing.T, wd, dir string) []model.Repo {
	t.Helper()
	return []model.Repo{resolved(t, testRegistry(t, wd), dir)}
}

func hasBranch(t *testing.T, dir, branch string) bool {
	t.Helper()
	branches, err := gitx.LocalBranches(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range branches {
		if b == branch {
			return true
		}
	}
	return false
}

// TestDryRunDeletesNothing is the guarantee the flag exists for: the deletion
// path runs in full — selection, classification, the confirmation it would ask
// — and the repo is left exactly as it was found.
func TestDryRunDeletesNothing(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")

	var out bytes.Buffer
	if err := runPrune(&out, selectedRepo(t, wd, dir), pruneOpts{DryRun: true}); err != nil {
		t.Fatal(err)
	}

	if !hasBranch(t, dir, "landed") {
		t.Error("a dry run deleted the branch")
	}
	if _, err := os.Stat(filepath.Join(state, "repo", "prune.log")); !os.IsNotExist(err) {
		t.Error("a dry run wrote to the journal")
	}
	if !strings.Contains(out.String(), "would delete landed") {
		t.Errorf("dry run did not say what it would do:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "re-run without --dry-run") {
		t.Errorf("dry run did not say how to do it for real:\n%s", out.String())
	}
}

// TestDryRunAsksNothing: waiting for an answer that changes nothing would only
// teach the habit of typing y, so the question is printed rather than asked —
// which is also what lets a dry run be useful with no terminal at all.
func TestDryRunAsksNothing(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")

	var out bytes.Buffer
	if err := runPrune(&out, selectedRepo(t, wd, dir), pruneOpts{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "would ask:") {
		t.Errorf("dry run did not show the question it would ask:\n%s", out.String())
	}
	if strings.Contains(out.String(), "not a terminal") {
		t.Errorf("dry run declined for want of a terminal it never needed:\n%s", out.String())
	}
}

// TestDeleteRecordsWhatItRemoved: the record is the reason a deletion is
// answerable for later, so it carries the full SHA and the command that undoes
// it — and prune says where to find it rather than leaving it to be discovered.
func TestDeleteRecordsWhatItRemoved(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")
	sha, _ := gitx.RevParse(dir, "landed")

	var out bytes.Buffer
	if err := runPrune(&out, selectedRepo(t, wd, dir), pruneOpts{Delete: true, Yes: true}); err != nil {
		t.Fatal(err)
	}

	if hasBranch(t, dir, "landed") {
		t.Fatal("the landed branch survived --delete")
	}
	body, err := os.ReadFile(filepath.Join(state, "repo", "prune.log"))
	if err != nil {
		t.Fatalf("no journal after a deletion: %v", err)
	}
	record := string(body)
	if !strings.Contains(record, "git branch landed "+sha) {
		t.Errorf("the record cannot restore the branch it removed:\n%s", record)
	}
	if !strings.Contains(record, "merged") {
		t.Errorf("the record does not say what justified the deletion:\n%s", record)
	}
	if !strings.Contains(out.String(), "recorded in "+filepath.Join(state, "repo", "prune.log")) {
		t.Errorf("prune did not say where the record went:\n%s", out.String())
	}
}

// TestNothingIsDeletedWithoutAJournal: the record is what makes pruning
// answerable for, so losing it is a reason to stop — discovering it after the
// branches are gone is exactly the wrong order.
func TestNothingIsDeletedWithoutAJournal(t *testing.T) {
	// A regular file where the state directory belongs: MkdirAll cannot
	// proceed, which is the same failure a read-only or full disk produces.
	blocked := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(blocked, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("XDG_STATE_HOME", blocked)
	wd := t.TempDir()
	dir := cloneWithLandedBranch(t, wd, "proj")

	var out bytes.Buffer
	err := runPrune(&out, selectedRepo(t, wd, dir), pruneOpts{Delete: true, Yes: true})
	if err == nil {
		t.Fatal("prune deleted branches it could not record")
	}
	if !strings.Contains(err.Error(), "nothing was deleted") {
		t.Errorf("the error does not say the repo was left alone: %v", err)
	}
	if !hasBranch(t, dir, "landed") {
		t.Error("the branch went despite the journal failing")
	}
}
