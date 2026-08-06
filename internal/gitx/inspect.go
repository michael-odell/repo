package gitx

import "strings"

// UntrackedFiles lists untracked, non-ignored files in a working tree (the work
// a layout collapse must not silently discard).
func UntrackedFiles(dir string) ([]string, error) {
	out, err := run(dir, "ls-files", "--others", "--exclude-standard")
	return splitLines(out), err
}

// DirtyFiles lists the tracked files a working tree has modified — the detail
// behind IsDirty's boolean, needed to test them against `expected_uncommitted`
// (DESIGN §3.6). It runs the same command IsDirty does, so the two can never
// disagree about whether a tree is dirty.
//
// `-z` is what makes this parseable: it emits raw NUL-terminated paths instead
// of quoting the awkward ones. A rename or copy entry carries a second path (the
// original), which is consumed and reported too — a pattern naming either side
// should match.
func DirtyFiles(dir string) ([]string, error) {
	out, err := runRaw(dir, "status", "--porcelain", "-z", "--untracked-files=no")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	fields := strings.Split(strings.TrimRight(out, "\x00"), "\x00")
	var files []string
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if len(f) < 4 { // "XY path" — anything shorter is padding, not an entry
			continue
		}
		status, path := f[:2], f[3:]
		files = append(files, path)
		if status[0] == 'R' || status[0] == 'C' {
			if i+1 < len(fields) {
				i++
				files = append(files, fields[i])
			}
		}
	}
	return files, nil
}

// IgnoredFiles lists ignored files present in a working tree (discardable only
// with consent: DESIGN §4.1).
func IgnoredFiles(dir string) ([]string, error) {
	out, err := run(dir, "ls-files", "--others", "--ignored", "--exclude-standard")
	return splitLines(out), err
}

// LocalBranches lists local branch names (short form). Used to find task
// branches — any local branch not among a repo's configured important
// `branches` (DESIGN §3.6, §5.1).
func LocalBranches(dir string) ([]string, error) {
	out, err := run(dir, "for-each-ref", "--format=%(refname:short)", "refs/heads")
	if err != nil {
		return nil, err
	}
	return splitLines(out), nil
}

// RemoteBranches lists a remote's branch names (short form), excluding its HEAD
// symref. Used to recreate local branches when collapsing a bare into a clone.
func RemoteBranches(dir, remote string) ([]string, error) {
	out, err := run(dir, "for-each-ref", "--format=%(refname:short)", "refs/remotes/"+remote)
	if err != nil {
		return nil, err
	}
	var branches []string
	for _, b := range splitLines(out) {
		if b == remote+"/HEAD" {
			continue
		}
		branches = append(branches, strings.TrimPrefix(b, remote+"/"))
	}
	return branches, nil
}

// Worktree is one linked worktree of a container.
type Worktree struct {
	Path   string
	Branch string // "" when detached or bare
	Bare   bool
}

// Worktrees lists the container's worktrees via `worktree list --porcelain`.
func Worktrees(container string) ([]Worktree, error) {
	out, err := run(container, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var wts []Worktree
	var cur *Worktree
	for _, line := range splitLines(out) {
		switch {
		case strings.HasPrefix(line, "worktree "):
			wts = append(wts, Worktree{Path: strings.TrimPrefix(line, "worktree ")})
			cur = &wts[len(wts)-1]
		case cur == nil:
			// ignore stray lines before the first worktree entry
		case line == "bare":
			cur.Bare = true
		case strings.HasPrefix(line, "branch "):
			cur.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		}
	}
	return wts, nil
}

// WorktreeFor returns the path of the working tree that currently has branch
// checked out, or "" when no tree does. dir may be any working tree or the
// container of a bare+worktree repo — `git worktree list` reports every tree of
// the repo either way, so this answers the same question in both layouts
// (DESIGN §5.1): a single clone is simply a repo with exactly one worktree.
func WorktreeFor(dir, branch string) string {
	wts, err := Worktrees(dir)
	if err != nil {
		return ""
	}
	for _, w := range wts {
		if w.Branch == branch {
			return w.Path
		}
	}
	return ""
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}
