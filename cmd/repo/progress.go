package main

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	syncpkg "github.com/michael-odell/repo/internal/sync"
)

// A sweep is quiet by design — nothing is printed until every repo has been
// reconciled, because the report is a table and a table can't be written out of
// order. On a fleet where a fetch takes a while, that silence is
// indistinguishable from a hang, which is exactly the wrong impression for the
// one command that mostly waits on the network.
//
// So the sweep gets a status line: a spinner (something is moving), a count
// (how much is left), and the repo that has been in flight longest (what it is
// waiting on — the question anyone watching a slow sweep actually has). It is
// one line, rewritten in place, on stderr, and only when stderr is a terminal:
// piping or redirecting the report must not collect animation frames.
type progress struct {
	w     io.Writer
	total int

	mu      sync.Mutex
	done    int
	started map[string]time.Time
	frame   int
	painted bool

	stop chan struct{}
	gone chan struct{}
}

// spinnerFrames advance a quarter-second apart; braille dots match the report's
// existing glyph vocabulary and stay one cell wide in every terminal font.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

const (
	paintInterval = 100 * time.Millisecond
	// A repo is only named once it has been in flight long enough that naming it
	// means something. Below this, the name would change faster than it reads.
	nameAfter = 2 * time.Second
	// Terminal width is not queried: COLUMNS is not exported to children by most
	// shells, and asking properly means an ioctl dependency for one line of
	// cosmetics. 80 is the floor every terminal clears, and the line is built to
	// fit it rather than to fill whatever is available.
	lineWidth = 80
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
		p.done++
		return
	}
	p.started[ev.Name] = time.Now()
}

// stopAndClear ends the animation and wipes the line, leaving the cursor at
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
	if p.painted {
		fmt.Fprintf(p.w, "\r%s\r", strings.Repeat(" ", lineWidth))
	}
}

func (p *progress) paint() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.frame++
	p.painted = true
	fmt.Fprintf(p.w, "\r%-*s", lineWidth, p.line())
}

// line renders the current state, truncated to fit. Held under p.mu.
func (p *progress) line() string {
	spin := spinnerFrames[p.frame%len(spinnerFrames)]
	s := fmt.Sprintf("%s  %d/%d", spin, p.done, p.total)
	if n := len(p.started); n > 0 {
		s += fmt.Sprintf("  ·  %d active", n)
	}
	if name, since := p.longestRunning(); name != "" {
		s += fmt.Sprintf("  ·  %s %s", name, roundSeconds(since))
	}
	if len(s) > lineWidth {
		s = s[:lineWidth-1] + "…"
	}
	return s
}

// longestRunning names the repo that has been in flight longest, once it has
// been there long enough to be worth naming. On a healthy sweep this is just
// whatever is slowest; on a stalled one it is the answer to "what is it stuck
// on", without anyone having to go digging through `ps`. Held under p.mu.
func (p *progress) longestRunning() (string, time.Duration) {
	var name string
	var oldest time.Time
	for n, t := range p.started {
		// Ties broken by name so the line doesn't flicker between two repos that
		// started in the same millisecond.
		if name == "" || t.Before(oldest) || (t.Equal(oldest) && n < name) {
			name, oldest = n, t
		}
	}
	if name == "" {
		return "", 0
	}
	since := time.Since(oldest)
	if since < nameAfter {
		return "", 0
	}
	return name, since
}

func roundSeconds(d time.Duration) string {
	return d.Round(time.Second).String()
}

// isTerminal reports whether f is attached to a terminal rather than a pipe or
// file, the same test the report's color uses.
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
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
