package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/term"

	syncpkg "github.com/michael-odell/repo/internal/sync"
)

// A sweep is quiet by design — nothing is printed until every repo has been
// reconciled, because the report is a table and a table can't be written out of
// order. On a fleet where a fetch takes a while, that silence is
// indistinguishable from a hang, which is exactly the wrong impression for the
// one command that mostly waits on the network.
//
// So the sweep gets a live block: a header carrying a spinner (something is
// moving) and a count (how much is left), then one row per repo in flight
// naming it and how long it has been there. The rows are the useful part — on a
// slow sweep they answer "what is it waiting on" without anyone going to `ps`,
// and giving each repo a whole line is what makes the name legible enough to
// answer it. The block is rewritten in place, on stderr, and only when stderr
// is a terminal: piping or redirecting the report must not collect animation
// frames.
type progress struct {
	w     io.Writer
	total int
	// size reports the terminal's width and height. It is re-queried on every
	// paint rather than cached, so a window resized mid-sweep is picked up
	// without a SIGWINCH handler, and it is a field so tests can render at a
	// size they control.
	size func() (int, int)

	mu      sync.Mutex
	done    int
	started map[string]time.Time
	// slots maps display row to the repo occupying it, "" for a row whose repo
	// has finished. See claim.
	slots   []string
	frame   int
	painted int // rows the last paint left on screen

	stop chan struct{}
	gone chan struct{}
}

// spinnerFrames advance a quarter-second apart; braille dots match the report's
// existing glyph vocabulary and stay one cell wide in every terminal font.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	paintInterval = 100 * time.Millisecond
	// rowIndent sets the rows under the header, so the block reads as one
	// thing rather than as a list that happens to start with a spinner.
	rowIndent = "  "
	// gap is the space between the longest name and the duration column. Two
	// columns is enough to separate the fields without the eye having to
	// travel to pair a name with its number.
	gap = 2
	// maxRows bounds the block's height. The sweep's concurrency limit is well
	// under this, so it only matters if that limit rises — a block taller than
	// the terminal would scroll, and a scrolled block can't be rewritten in
	// place.
	maxRows = 8
	// fallbackWidth/fallbackHeight are what a terminal that won't report its
	// size gets: 80×24 is the floor every terminal clears.
	fallbackWidth  = 80
	fallbackHeight = 24
)

// newProgress returns a live display, or nil when there is nothing to show it
// on. A nil *progress is usable — every method tolerates it — so callers don't
// branch on whether they're attached to a terminal.
func newProgress(total int) *progress {
	if total == 0 || !isTerminal(os.Stderr) || os.Getenv("TERM") == "dumb" {
		return nil
	}
	p := &progress{
		w:       os.Stderr,
		total:   total,
		size:    func() (int, int) { return terminalSize(os.Stderr) },
		started: map[string]time.Time{},
		stop:    make(chan struct{}),
		gone:    make(chan struct{}),
	}
	go p.loop()
	return p
}

func (p *progress) loop() {
	defer close(p.gone)
	t := time.NewTicker(paintInterval)
	defer t.Stop()
	for {
		select {
		case <-p.stop:
			return
		case <-t.C:
			p.paint()
		}
	}
}

// update folds one sweep event into the display's state.
func (p *progress) update(ev syncpkg.Progress) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if ev.Finished {
		delete(p.started, ev.Name)
		p.release(ev.Name)
		p.done++
		return
	}
	p.started[ev.Name] = time.Now()
	p.claim(ev.Name)
}

// claim gives a repo entering the sweep a display row, reusing the first row
// whose repo has finished. Rows are assigned and held rather than sorted on
// every paint: a repo finishing would otherwise shift every line below it up
// one, and on a fleet where most repos finish quickly that shuffling is
// unreadable. A fixed row lets the eye track one repo, and one climbing
// duration, in place. Held under p.mu.
func (p *progress) claim(name string) {
	for i, n := range p.slots {
		if n == "" {
			p.slots[i] = name
			return
		}
	}
	p.slots = append(p.slots, name)
}

// release frees a finished repo's row, trimming rows off the end so the block
// shrinks rather than trailing blanks. A freed row with a longer-running repo
// still below it is held open: the next repo to start claims it within
// milliseconds, and closing the gap instead would move that longer-running
// repo's line out from under the reader for a single frame. See rows for what
// happens to a row nothing is left to claim. Held under p.mu.
func (p *progress) release(name string) {
	for i, n := range p.slots {
		if n == name {
			p.slots[i] = ""
			break
		}
	}
	for len(p.slots) > 0 && p.slots[len(p.slots)-1] == "" {
		p.slots = p.slots[:len(p.slots)-1]
	}
}

// stopAndClear ends the animation and wipes the block, leaving the cursor at
// column zero for the report that follows. Clearing rather than leaving the
// last frame matters: a finished sweep should not leave a spinner frozen
// mid-spin above its own output, implying work still in progress.
func (p *progress) stopAndClear() {
	if p == nil {
		return
	}
	close(p.stop)
	<-p.gone
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clear()
}

func (p *progress) paint() {
	p.mu.Lock()
	defer p.mu.Unlock()
	// Once every repo is accounted for there is nothing left to animate, and
	// what comes next in the same sweep may be a question waiting on an answer
	// — a --fix relayout asks before discarding ignored files, and it asks
	// after the concurrent phase but before the caller stops this display.
	// Going quiet here keeps the block from repainting over that prompt, or
	// erasing it outright.
	if p.done >= p.total && len(p.started) == 0 {
		p.clear()
		return
	}
	p.frame++
	p.render(p.lines())
}

// render rewrites the block in place: back to its first row, then each row
// cleared and redrawn, then everything below the last one erased so a block
// that just got shorter doesn't leave its old tail on screen. Held under p.mu.
func (p *progress) render(lines []string) {
	var b strings.Builder
	b.WriteString(p.rewind())
	for i, l := range lines {
		if i > 0 {
			b.WriteString("\n")
		}
		// The carriage return is not redundant with the newline: a terminal
		// that isn't translating output leaves the column where it was.
		b.WriteString("\r\x1b[K")
		b.WriteString(l)
	}
	b.WriteString("\x1b[J")
	p.painted = len(lines)
	fmt.Fprint(p.w, b.String())
}

// rewind puts the cursor back at column zero of the block's first row. Held
// under p.mu.
func (p *progress) rewind() string {
	if p.painted <= 1 {
		return "\r"
	}
	return fmt.Sprintf("\r\x1b[%dA", p.painted-1)
}

// clear wipes the block and forgets it. Held under p.mu.
func (p *progress) clear() {
	if p.painted == 0 {
		return
	}
	fmt.Fprint(p.w, p.rewind()+"\x1b[J")
	p.painted = 0
}

// lines renders the current state, one string per screen row and none of them
// wider than the terminal — a row that wrapped would occupy two screen lines
// and throw off the cursor arithmetic that rewrites the block. Held under p.mu.
func (p *progress) lines() []string {
	width, height := p.size()
	out := []string{truncate(p.header(), width)}

	// Leave the header its line and one more besides, so the block never fills
	// the screen to the last row.
	rows := maxRows
	if h := height - 2; h < rows {
		rows = h
	}
	if rows < 1 {
		return out
	}

	active := p.rows()
	var hidden int
	if len(active) > rows {
		hidden = len(active) - (rows - 1)
		active = active[:rows-1]
	}
	nameW, durW := columns(active, width)
	for _, r := range active {
		out = append(out, truncate(r.render(nameW, durW), width))
	}
	if hidden > 0 {
		out = append(out, truncate(fmt.Sprintf("%s…and %d more", rowIndent, hidden), width))
	}
	return out
}

// header says how the sweep as a whole is going: what fraction is behind it,
// and how much is in flight right now. Held under p.mu.
func (p *progress) header() string {
	s := fmt.Sprintf("%s  %d/%d", spinnerFrames[p.frame%len(spinnerFrames)], p.done, p.total)
	if n := len(p.started); n > 0 {
		s += fmt.Sprintf("  ·  %d active", n)
	}
	return s
}

// a row is one line of the block: a repo in flight and how long it has been
// there, or the zero value for a row whose repo has finished while a
// longer-running one holds a row below it.
type row struct {
	name  string
	since time.Duration
}

func (r row) dur() string { return roundSeconds(r.since) }

func (r row) render(nameW, durW int) string {
	if r.name == "" {
		return ""
	}
	return fmt.Sprintf("%s%-*s%*s", rowIndent, nameW, truncate(r.name, nameW), gap+durW, r.dur())
}

// rows snapshots the block's rows in display order. Held under p.mu.
func (p *progress) rows() []row {
	now := time.Now()
	// Everything accounted for is everything there is: no repo is left to
	// start, so no freed row will ever be reclaimed. Closing the gaps now
	// costs the stability that holding them open was buying, and keeps the
	// last seconds of a sweep from being mostly blank lines.
	draining := p.done+len(p.started) >= p.total
	out := make([]row, 0, len(p.slots))
	for _, n := range p.slots {
		if n == "" {
			if !draining {
				out = append(out, row{})
			}
			continue
		}
		out = append(out, row{name: n, since: now.Sub(p.started[n])})
	}
	return out
}

// columns sizes the name and duration fields from what's actually in the
// block, so the durations line up in a column of their own that sits just
// past the longest name rather than out at the far edge of a wide terminal.
// Names are given whatever is left.
func columns(rows []row, width int) (nameW, durW int) {
	for _, r := range rows {
		if r.name == "" {
			continue
		}
		if n := len([]rune(r.name)); n > nameW {
			nameW = n
		}
		if d := len(r.dur()); d > durW {
			durW = d
		}
	}
	if avail := width - len(rowIndent) - gap - durW; nameW > avail {
		nameW = avail
	}
	if nameW < 1 {
		nameW = 1
	}
	return nameW, durW
}

// truncate cuts s to at most n columns, marking the cut with an ellipsis. It
// counts runes rather than bytes: a repo name is arbitrary text, and slicing a
// UTF-8 string by byte can land mid-rune and put a replacement character on
// screen.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	switch {
	case n < 1:
		return ""
	case n == 1:
		return "…"
	}
	return string(r[:n-1]) + "…"
}

func roundSeconds(d time.Duration) string {
	return d.Round(time.Second).String()
}

// terminalSize reports the terminal's dimensions, falling back to the size
// every terminal clears when it won't say. Width matters more than it did when
// this was a single line: the block's whole purpose is giving repo names room
// to be read, and clamping a 200-column terminal to 80 would throw away most
// of the room it has.
func terminalSize(f *os.File) (int, int) {
	w, h, err := term.GetSize(int(f.Fd()))
	if err != nil || w <= 0 || h <= 0 {
		return fallbackWidth, fallbackHeight
	}
	return w, h
}

// isTerminal reports whether f is attached to a terminal rather than a pipe or
// file, the same test the report's color uses.
func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}

// sortedNames is used by tests to assert on state deterministically.
func (p *progress) sortedNames() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]string, 0, len(p.started))
	for n := range p.started {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
