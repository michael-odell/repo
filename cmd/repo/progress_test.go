package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/michael-odell/repo/internal/sync"
)

// testProgress builds a display writing to a buffer, bypassing the terminal
// check newProgress makes — the rendering is what's under test, not the
// decision to render.
func testProgress(total int) (*progress, *bytes.Buffer) {
	var buf bytes.Buffer
	return &progress{
		w:       &buf,
		total:   total,
		started: map[string]time.Time{},
		stop:    make(chan struct{}),
		gone:    make(chan struct{}),
	}, &buf
}

func TestProgressCountsFinishedRepos(t *testing.T) {
	p, _ := testProgress(3)
	p.update(syncpkg.Progress{Name: "a"})
	p.update(syncpkg.Progress{Name: "b"})
	if got := p.sortedNames(); len(got) != 2 {
		t.Errorf("in flight = %v, want both", got)
	}
	if !strings.Contains(p.line(), "0/3") {
		t.Errorf("line = %q, want 0 of 3 done", p.line())
	}

	p.update(syncpkg.Progress{Name: "a", Finished: true, Outcome: syncpkg.UpToDate})
	if got := p.sortedNames(); len(got) != 1 || got[0] != "b" {
		t.Errorf("in flight = %v, want only b", got)
	}
	if !strings.Contains(p.line(), "1/3") {
		t.Errorf("line = %q, want 1 of 3 done", p.line())
	}
	if !strings.Contains(p.line(), "1 active") {
		t.Errorf("line = %q, want the active count", p.line())
	}
}

// TestProgressNamesTheLongestRunning: on a stalled sweep this is the whole
// point — the name of the repo everything is waiting on, without going to `ps`.
func TestProgressNamesTheLongestRunning(t *testing.T) {
	p, _ := testProgress(2)
	p.update(syncpkg.Progress{Name: "acme/quick"})
	p.update(syncpkg.Progress{Name: "acme/stuck"})

	// Backdate the stuck one past the naming threshold.
	p.mu.Lock()
	p.started["acme/stuck"] = time.Now().Add(-90 * time.Second)
	p.mu.Unlock()

	line := p.line()
	if !strings.Contains(line, "acme/stuck") {
		t.Errorf("line = %q, want the longest-running repo named", line)
	}
	if !strings.Contains(line, "1m30s") {
		t.Errorf("line = %q, want how long it has been running", line)
	}
	if strings.Contains(line, "acme/quick") {
		t.Errorf("line = %q, should name only the longest-running repo", line)
	}
}

// TestProgressStaysQuietUntilARepoIsSlow: a name that changes faster than it
// can be read is noise, so repos are only named once they've been in flight
// long enough for the name to mean something.
func TestProgressStaysQuietUntilARepoIsSlow(t *testing.T) {
	p, _ := testProgress(2)
	p.update(syncpkg.Progress{Name: "acme/fresh"})
	if strings.Contains(p.line(), "acme/fresh") {
		t.Errorf("line = %q, should not name a repo that just started", p.line())
	}
}

// TestProgressFitsOneLine: the status line is rewritten in place, so anything
// wider than the terminal wraps and leaves a trail of half-lines behind it.
func TestProgressFitsOneLine(t *testing.T) {
	p, _ := testProgress(4000)
	long := "some-very-long-owner-name/an-equally-long-repository-name-that-keeps-going-and-going"
	p.update(syncpkg.Progress{Name: long})
	p.mu.Lock()
	p.started[long] = time.Now().Add(-time.Hour)
	p.mu.Unlock()

	if got := len([]rune(p.line())); got > lineWidth {
		t.Errorf("line is %d columns, want at most %d: %q", got, lineWidth, p.line())
	}
}

// TestProgressClearsItself: the report is printed after the sweep, and a
// spinner frozen mid-spin above it would read as work still in progress.
func TestProgressClearsItself(t *testing.T) {
	p, buf := testProgress(1)
	go p.loop()
	p.update(syncpkg.Progress{Name: "acme/one"})
	time.Sleep(3 * paintInterval)
	p.stopAndClear()

	out := buf.String()
	if !strings.Contains(out, "\r") {
		t.Fatalf("nothing was painted: %q", out)
	}
	if !strings.HasSuffix(out, "\r") {
		t.Errorf("output should end having wiped the line, got %q", out[max(0, len(out)-40):])
	}
	// Nothing may be left on screen: the last thing written is blanks then \r.
	if last := out[strings.LastIndex(out[:len(out)-1], "\r")+1:]; strings.TrimSpace(strings.TrimSuffix(last, "\r")) != "" {
		t.Errorf("line was not cleared, left %q", last)
	}
}

// TestProgressIsNilSafe: with no terminal there is no display, and every call
// site would otherwise need to know that.
func TestProgressIsNilSafe(t *testing.T) {
	var p *progress
	p.update(syncpkg.Progress{Name: "x"})
	p.stopAndClear()
}
