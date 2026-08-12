package journal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAppendRecordsARestorableLine: the record's whole purpose is being read
// later by someone who needs the branch back, so every field they would need
// has to survive the trip — including the full SHA, not the abbreviation the
// report prints.
func TestAppendRecordsARestorableLine(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	log, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	err = log.Append(Entry{
		When:    time.Date(2026, 8, 12, 9, 14, 3, 0, time.UTC),
		Repo:    "acme/noodle",
		Branch:  "refactor-auth",
		SHA:     "9f3a1c2b8e4d5a6f7c8b9a0d1e2f3a4b5c6d7e8f",
		Verdict: "merged (rewritten)",
		Mode:    "--delete",
	})
	if err != nil {
		t.Fatal(err)
	}
	log.Close()

	body, err := os.ReadFile(log.Path())
	if err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(strings.TrimSuffix(string(body), "\n"), "\t")
	want := []string{
		"2026-08-12T09:14:03Z",
		"acme/noodle",
		"refactor-auth",
		"9f3a1c2b8e4d5a6f7c8b9a0d1e2f3a4b5c6d7e8f",
		"merged (rewritten)",
		"--delete",
		"git branch refactor-auth 9f3a1c2b8e4d5a6f7c8b9a0d1e2f3a4b5c6d7e8f",
	}
	if len(fields) != len(want) {
		t.Fatalf("got %d fields, want %d: %q", len(fields), len(want), fields)
	}
	for i := range want {
		if fields[i] != want[i] {
			t.Errorf("field %d = %q, want %q", i, fields[i], want[i])
		}
	}
}

// TestAVerdictWithASpaceStaysOneField: "merged (rewritten)" contains a space,
// which is why the record is tab-separated. If a reader ever has to guess where
// a field ends, the log stops being usable by anything but a human.
func TestAVerdictWithASpaceStaysOneField(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	log, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	if err := log.Append(Entry{Repo: "a/b", Branch: "x", SHA: "abc", Verdict: "merged (rewritten)"}); err != nil {
		t.Fatal(err)
	}
	log.Close()

	body, _ := os.ReadFile(log.Path())
	if got := strings.Count(string(body), "\t"); got != 6 {
		t.Fatalf("got %d tabs, want 6 — a field carried a separator: %q", got, body)
	}
}

// TestASecondRunAppends: a journal that truncated would answer "what did it
// take from me" for exactly one run, which is the run nobody asks about.
func TestASecondRunAppends(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())

	for _, branch := range []string{"first", "second"} {
		log, err := Open()
		if err != nil {
			t.Fatal(err)
		}
		if err := log.Append(Entry{Repo: "a/b", Branch: branch, SHA: "abc"}); err != nil {
			t.Fatal(err)
		}
		log.Close()
	}

	log, _ := Open()
	defer log.Close()
	body, _ := os.ReadFile(log.Path())
	if lines := strings.Count(string(body), "\n"); lines != 2 {
		t.Fatalf("got %d records, want 2:\n%s", lines, body)
	}
	if !strings.Contains(string(body), "first") {
		t.Errorf("the first run's record is gone:\n%s", body)
	}
}

// TestJournalLandsUnderXDGState: state, not $REPO_OUT — a record whose value is
// that it survives must not live in a directory documented as regenerable.
func TestJournalLandsUnderXDGState(t *testing.T) {
	state := t.TempDir()
	t.Setenv("XDG_STATE_HOME", state)

	log, err := Open()
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if want := filepath.Join(state, "repo", "prune.log"); log.Path() != want {
		t.Errorf("journal at %s, want %s", log.Path(), want)
	}
}
