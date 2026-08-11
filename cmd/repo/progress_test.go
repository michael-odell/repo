package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	syncpkg "github.com/michael-odell/repo/internal/sync"
)

// testProgress builds a display writing to a buffer at a fixed terminal size,
// bypassing the terminal check newProgress makes — the rendering is what's
// under test, not the decision to render.
func testProgress(total int) (*progress, *bytes.Buffer) {
	return testProgressSized(total, 80, 24)
}

func testProgressSized(total, width, height int) (*progress, *bytes.Buffer) {
	var buf bytes.Buffer
	return &progress{
		w:       &buf,
		total:   total,
		size:    func() (int, int) { return width, height },
		started: map[string]time.Time{},
		stop:    make(chan struct{}),
		gone:    make(chan struct{}),
	}, &buf
}

// block renders the display the way paint would, as one string per screen row.
func block(p *progress) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lines()
}

// backdate ages a repo that is already in flight, so a test can assert on a
// duration without waiting for one.
func backdate(p *progress, name string, d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.started[name] = time.Now().Add(-d)
}

func TestProgressCountsFinishedRepos(t *testing.T) {
	p, _ := testProgress(3)
	p.update(syncpkg.Progress{Name: "a"})
	p.update(syncpkg.Progress{Name: "b"})
	if got := p.sortedNames(); len(got) != 2 {
		t.Errorf("in flight = %v, want both", got)
	}
	if head := block(p)[0]; !strings.Contains(head, "0/3") {
		t.Errorf("header = %q, want 0 of 3 done", head)
	}

	p.update(syncpkg.Progress{Name: "a", Finished: true, Outcome: syncpkg.UpToDate})
	if got := p.sortedNames(); len(got) != 1 || got[0] != "b" {
		t.Errorf("in flight = %v, want only b", got)
	}
	head := block(p)[0]
	if !strings.Contains(head, "1/3") {
		t.Errorf("header = %q, want 1 of 3 done", head)
	}
	if !strings.Contains(head, "1 active") {
		t.Errorf("header = %q, want the active count", head)
	}
}

// TestProgressGivesEveryActiveRepoItsOwnRow: the whole point of the block. A
// single line has room to name one repo, and a fleet syncing six at a time
// spends the whole sweep with five of them unaccounted for.
func TestProgressGivesEveryActiveRepoItsOwnRow(t *testing.T) {
	p, _ := testProgress(20)
	names := []string{"acme/one", "acme/two", "acme/three", "acme/four", "acme/five", "acme/six"}
	for i, n := range names {
		p.update(syncpkg.Progress{Name: n})
		backdate(p, n, time.Duration(i+1)*time.Second)
	}

	rows := block(p)
	if len(rows) != len(names)+1 {
		t.Fatalf("block is %d rows, want a header plus %d: %q", len(rows), len(names), rows)
	}
	for i, n := range names {
		if !strings.Contains(rows[i+1], n) {
			t.Errorf("row %d = %q, want %s", i+1, rows[i+1], n)
		}
	}
	// Each row carries its own age: with six in flight, "which one is stuck"
	// is answered by reading down the column, not by picking one to name.
	if !strings.Contains(rows[6], "6s") {
		t.Errorf("row = %q, want how long that repo has been running", rows[6])
	}
}

// TestProgressHoldsARowWhileNeighboursFinish: a repo keeps the row it was
// given. Re-sorting on every paint would shift every line below whichever repo
// finished, and on a fleet where most finish in under a second nothing on
// screen would stay still long enough to read.
func TestProgressHoldsARowWhileNeighboursFinish(t *testing.T) {
	p, _ := testProgress(9)
	for _, n := range []string{"acme/first", "acme/slow", "acme/third"} {
		p.update(syncpkg.Progress{Name: n})
	}
	rowOf := func(name string) int {
		for i, l := range block(p) {
			if strings.Contains(l, name) {
				return i
			}
		}
		return -1
	}
	before := rowOf("acme/slow")

	// The repo above it finishes and is replaced by a new one.
	p.update(syncpkg.Progress{Name: "acme/first", Finished: true, Outcome: syncpkg.UpToDate})
	p.update(syncpkg.Progress{Name: "acme/fourth"})

	if after := rowOf("acme/slow"); after != before {
		t.Errorf("acme/slow moved from row %d to %d", before, after)
	}
	if got := rowOf("acme/fourth"); got != before-1 {
		t.Errorf("acme/fourth took row %d, want the freed row %d", got, before-1)
	}
}

// TestProgressShrinksAsTheSweepDrains: rows are trimmed off the end as the
// last repos finish, so the block doesn't sit there as a tail of blanks.
func TestProgressShrinksAsTheSweepDrains(t *testing.T) {
	p, _ := testProgress(3)
	for _, n := range []string{"acme/a", "acme/b", "acme/c"} {
		p.update(syncpkg.Progress{Name: n})
	}
	if got := len(block(p)); got != 4 {
		t.Fatalf("block is %d rows, want a header plus 3", got)
	}
	for _, n := range []string{"acme/b", "acme/c"} {
		p.update(syncpkg.Progress{Name: n, Finished: true, Outcome: syncpkg.UpToDate})
	}
	if got := len(block(p)); got != 2 {
		t.Errorf("block is %d rows, want a header plus the one still running: %q", got, block(p))
	}
}

// TestProgressClosesGapsOnlyOnceNothingIsLeftToStart: a row freed mid-sweep is
// held open for the repo about to claim it, but a row freed when the queue is
// empty never will be — and holding those open leaves the last seconds of a
// sweep as a header over a screen of blank lines.
func TestProgressClosesGapsOnlyOnceNothingIsLeftToStart(t *testing.T) {
	// Mid-sweep: 3 in flight of 9, so 6 are still queued.
	p, _ := testProgress(9)
	for _, n := range []string{"acme/a", "acme/slow", "acme/c"} {
		p.update(syncpkg.Progress{Name: n})
	}
	p.update(syncpkg.Progress{Name: "acme/a", Finished: true, Outcome: syncpkg.UpToDate})
	rows := block(p)
	if len(rows) != 4 || rows[1] != "" {
		t.Errorf("block = %q, want the freed row held open for the next repo", rows)
	}

	// Drain: the two still running are all that is left of the sweep.
	q, _ := testProgress(3)
	for _, n := range []string{"acme/a", "acme/slow", "acme/c"} {
		q.update(syncpkg.Progress{Name: n})
	}
	q.update(syncpkg.Progress{Name: "acme/a", Finished: true, Outcome: syncpkg.UpToDate})
	rows = block(q)
	if len(rows) != 3 {
		t.Fatalf("block = %q, want a header plus the two still running", rows)
	}
	if !strings.Contains(rows[1], "acme/slow") {
		t.Errorf("block = %q, want the gap closed once nothing is left to start", rows)
	}
}

// TestProgressRowsFitTheTerminal: the block is rewritten in place by counting
// rows back from the cursor, so a row wide enough to wrap takes two screen
// lines and every subsequent repaint lands in the wrong place.
func TestProgressRowsFitTheTerminal(t *testing.T) {
	for _, width := range []int{80, 40, 24} {
		p, _ := testProgressSized(4000, width, 24)
		long := "some-very-long-owner-name/an-equally-long-repository-name-that-keeps-going-and-going"
		p.update(syncpkg.Progress{Name: long})
		backdate(p, long, time.Hour)
		p.update(syncpkg.Progress{Name: "acme/short"})

		for _, l := range block(p) {
			if got := len([]rune(l)); got > width {
				t.Errorf("row is %d columns on a %d-column terminal: %q", got, width, l)
			}
		}
	}
}

// TestProgressUsesTheTerminalsWidth: a name is truncated to fit the terminal,
// not to a hardcoded 80 — the block exists to make names legible, and clamping
// a wide terminal would throw away the room it has to do that.
func TestProgressUsesTheTerminalsWidth(t *testing.T) {
	name := "some-very-long-owner-name/an-equally-long-repository-name-that-keeps-going"
	narrow, _ := testProgressSized(2, 60, 24)
	wide, _ := testProgressSized(2, 160, 24)
	for _, p := range []*progress{narrow, wide} {
		p.update(syncpkg.Progress{Name: name})
		backdate(p, name, time.Minute)
	}

	if row := block(narrow)[1]; !strings.Contains(row, "…") {
		t.Errorf("row = %q, want the name truncated at 60 columns", row)
	}
	if row := block(wide)[1]; !strings.Contains(row, name) {
		t.Errorf("row = %q, want the whole name on a 160-column terminal", row)
	}
}

// TestProgressTruncatesOnRuneBoundaries: repo names are arbitrary text, and
// cutting one by byte can land mid-rune and put a replacement character on
// screen.
func TestProgressTruncatesOnRuneBoundaries(t *testing.T) {
	p, _ := testProgressSized(2, 30, 24)
	name := "組織/" + strings.Repeat("日本語", 12)
	p.update(syncpkg.Progress{Name: name})
	backdate(p, name, time.Minute)

	row := block(p)[1]
	if strings.ContainsRune(row, '�') {
		t.Errorf("row = %q, want no replacement character", row)
	}
	if !strings.HasPrefix(row, rowIndent+"組織/") {
		t.Errorf("row = %q, want the name's leading runes intact", row)
	}
}

// TestProgressCapsItsHeight: a block taller than the terminal scrolls, and a
// scrolled block can't be rewritten in place.
func TestProgressCapsItsHeight(t *testing.T) {
	const height = 8
	p, _ := testProgressSized(40, 80, height)
	for i := range 12 {
		p.update(syncpkg.Progress{Name: string(rune('a'+i)) + "/repo"})
	}
	rows := block(p)
	if len(rows) > height-1 {
		t.Errorf("block is %d rows on a %d-row terminal: %q", len(rows), height, rows)
	}
	if last := rows[len(rows)-1]; !strings.Contains(last, "more") {
		t.Errorf("last row = %q, want the repos it couldn't show accounted for", last)
	}
}

// TestProgressRewindsExactlyAsFarAsItPainted: the block is rewritten by moving
// the cursor back over the rows it left behind. Off by one and it walks up the
// screen, smearing a copy of itself over the scrollback every 100ms.
func TestProgressRewindsExactlyAsFarAsItPainted(t *testing.T) {
	p, buf := testProgress(9)
	for _, n := range []string{"acme/a", "acme/b", "acme/c"} {
		p.update(syncpkg.Progress{Name: n})
	}
	p.paint() // 4 rows: header plus three repos
	buf.Reset()
	p.paint()

	if want := "\r\x1b[3A"; !strings.HasPrefix(buf.String(), want) {
		t.Errorf("repaint began %q, want it to rewind 3 rows with %q", buf.String(), want)
	}
}

// TestProgressClearsItself: the report is printed after the sweep, and a
// spinner frozen mid-spin above it would read as work still in progress.
func TestProgressClearsItself(t *testing.T) {
	p, buf := testProgress(4)
	go p.loop()
	p.update(syncpkg.Progress{Name: "acme/one"})
	p.update(syncpkg.Progress{Name: "acme/two"})
	time.Sleep(3 * paintInterval)
	p.stopAndClear()

	out := buf.String()
	if !strings.Contains(out, "acme/one") {
		t.Fatalf("nothing was painted: %q", out)
	}
	// Back to the top of the block, then erase it and everything below.
	if want := "\r\x1b[2A\x1b[J"; !strings.HasSuffix(out, want) {
		t.Errorf("output should end having wiped the block with %q, got %q", want, out[max(0, len(out)-20):])
	}
}

// TestProgressGoesQuietOnceEveryRepoIsAccountedFor: the sweep's --fix relayout
// prompts on stdin after the concurrent phase but before the caller stops this
// display. A block still repainting there would erase the question.
func TestProgressGoesQuietOnceEveryRepoIsAccountedFor(t *testing.T) {
	p, buf := testProgress(1)
	p.update(syncpkg.Progress{Name: "acme/only"})
	p.paint()
	p.update(syncpkg.Progress{Name: "acme/only", Finished: true, Outcome: syncpkg.UpToDate})

	buf.Reset()
	p.paint()
	if want := "\r\x1b[1A\x1b[J"; buf.String() != want {
		t.Errorf("paint after the sweep wrote %q, want it to wipe the block (%q) and stop", buf.String(), want)
	}
	buf.Reset()
	p.paint()
	if buf.String() != "" {
		t.Errorf("paint after the sweep wrote %q, want nothing", buf.String())
	}
}

// TestProgressIsNilSafe: with no terminal there is no display, and every call
// site would otherwise need to know that.
func TestProgressIsNilSafe(t *testing.T) {
	var p *progress
	p.update(syncpkg.Progress{Name: "x"})
	p.stopAndClear()
}
