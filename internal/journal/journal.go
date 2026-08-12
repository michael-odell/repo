// Package journal records the branches prune removed, so a deletion can be
// answered for after the fact (DESIGN §5.3).
//
// Reflog recovery is real but it expires with gc, needs the repo in hand, and
// requires already knowing which branch to ask about. The question someone
// actually has is "what did it take from me, and when" — across every repo at
// once — and no reflog answers that. This does, and it is the only record that
// would exist at all if tag pruning ever lands, since refs/tags has no reflog.
package journal

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/michael-odell/repo/internal/xdg"
)

// Entry is one removed branch.
type Entry struct {
	When    time.Time
	Repo    string // owner/repo, as the report names it
	Branch  string
	SHA     string // what the ref held, read before the deletion
	Verdict string // the merge state that justified it
	Mode    string // what did it: "--delete", "interactive", "auto"
}

// Restore is the command that puts the branch back. It is written into the
// record rather than left to be reconstructed, because the moment someone needs
// it is the moment they are least inclined to work out the syntax — and because
// the record then survives being read somewhere `repo` isn't installed.
func (e Entry) Restore() string {
	return fmt.Sprintf("git branch %s %s", e.Branch, e.SHA)
}

// line is the entry as one tab-separated record. Tabs rather than aligned
// columns because every other field can contain a space — "merged (rewritten)"
// does — and a log that will be read back by machine has to be unambiguous
// about where a field ends. Git forbids tabs and newlines in ref names, so no
// field here can carry a separator.
func (e Entry) line() string {
	return strings.Join([]string{
		e.When.UTC().Format(time.RFC3339),
		e.Repo,
		e.Branch,
		e.SHA,
		e.Verdict,
		e.Mode,
		e.Restore(),
	}, "\t") + "\n"
}

// Log is an open journal, held for the duration of a run.
type Log struct {
	f    *os.File
	path string
}

// Open opens the journal for appending, creating it and its directory.
//
// Callers open it *before* the first deletion and treat failure as a reason not
// to delete: the record is what makes pruning answerable, so a run that cannot
// write one has lost the property it was relying on, and finding that out after
// the branches are gone is exactly the wrong order. Opening once per run also
// keeps a sweep from reopening the file per branch.
func Open() (*Log, error) {
	dir := xdg.StateDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	p := filepath.Join(dir, "prune.log")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	return &Log{f: f, path: p}, nil
}

// Append writes one entry, flushed to the file before it returns — nothing is
// buffered across a deletion, so an interrupted sweep still accounts for every
// branch it actually removed.
func (l *Log) Append(e Entry) error {
	if e.When.IsZero() {
		e.When = time.Now()
	}
	_, err := l.f.WriteString(e.line())
	return err
}

// Path is the journal's location, for reporting it to whoever will want to read
// it later.
func (l *Log) Path() string { return l.path }

func (l *Log) Close() error { return l.f.Close() }
