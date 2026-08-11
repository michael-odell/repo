// Package gitx is a thin wrapper over the git CLI (DESIGN §2: shell out to git,
// never go-git). All functions are read-only at this stage.
package gitx

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// run executes `git -C dir args...` and returns trimmed stdout.
func run(dir string, args ...string) (string, error) {
	out, err := runRaw(dir, args...)
	return strings.TrimSpace(out), err
}

// runRaw is run without the trim, for output whose leading or trailing bytes are
// significant — `status --porcelain`, whose first column is a space for a
// modified-but-unstaged file, and `-z` output, whose NUL terminators matter.
func runRaw(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), oneLine(ee.Stderr))
		}
		return "", err
	}
	return string(out), nil
}

// runAsProbe is run for a command that writes a throwaway object and therefore
// needs a committer identity git would otherwise have to auto-detect. Auto-
// detection fails outright wherever the hostname has no domain (CI runners,
// containers) or `user.useConfigOnly` is set, so a command left to fend for
// itself works on a developer's laptop and dies everywhere else. The identity is
// supplied here rather than read from config because the object is never
// referenced, never pushed, and gc reclaims it: whose name is on it is
// meaningless, and borrowing the user's would be a small lie in the object
// database.
func runAsProbe(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=repo", "GIT_AUTHOR_EMAIL=repo@localhost",
		"GIT_COMMITTER_NAME=repo", "GIT_COMMITTER_EMAIL=repo@localhost")
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), oneLine(ee.Stderr))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// oneLine collapses git's (often multi-line, blank-line-padded) stderr into a
// single line, so it reads as part of a Result's one-line Detail/Summary
// instead of breaking out of the tabular or verbose report layout.
func oneLine(stderr []byte) string {
	return strings.Join(strings.Fields(string(stderr)), " ")
}

// IsRepo reports whether dir contains a git repository (a .git dir or file, as
// used by worktree containers).
func IsRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// LayoutKind classifies a container's on-disk shape (DESIGN §4.1).
type LayoutKind int

const (
	LayoutAbsent   LayoutKind = iota // not a git repo (or the path is empty)
	LayoutSingle                     // a normal working-tree clone at the container
	LayoutWorktree                   // a bare repo (+ linked worktree siblings)
)

// ClassifyLayout reports whether the container is a single working-tree clone or
// a bare+worktree parent, discriminated by whether git sees a working tree at the
// container root: a normal clone reports is-bare=false, a bare+worktree parent
// (whose .git file points at .bare) reports is-bare=true.
func ClassifyLayout(dir string) LayoutKind {
	if !IsRepo(dir) {
		return LayoutAbsent
	}
	bare, err := run(dir, "rev-parse", "--is-bare-repository")
	if err != nil {
		return LayoutAbsent
	}
	if bare == "true" {
		return LayoutWorktree
	}
	return LayoutSingle
}

// Remotes returns a name→fetch-URL map.
func Remotes(dir string) (map[string]string, error) {
	names, err := run(dir, "remote")
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, n := range strings.Fields(names) {
		url, err := run(dir, "remote", "get-url", n)
		if err != nil {
			return nil, err
		}
		out[n] = url
	}
	return out, nil
}

// CurrentBranch returns the checked-out branch, or "" when detached.
func CurrentBranch(dir string) (string, error) {
	b, err := run(dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if b == "HEAD" {
		return "", nil
	}
	return b, nil
}

// DefaultBranch returns remote's default branch (e.g. "main"), read from the
// local refs/remotes/<remote>/HEAD symbolic ref that `git clone` sets up
// automatically — no network round trip, so this is safe to call for every
// repo during discovery. ok is false when that ref isn't present (pruned, or
// the clone predates automatic HEAD tracking): the caller falls back to a
// known mainline name, never to whatever happens to be checked out.
func DefaultBranch(dir, remote string) (string, bool) {
	out, err := run(dir, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD")
	if err != nil || out == "" {
		return "", false
	}
	return strings.TrimPrefix(out, remote+"/"), true
}

// InferDefaultBranch is the clone's own answer to which branch is the important
// one, in descending order of authority: origin's recorded HEAD symref; then,
// for a bare repo only, its own HEAD (`git clone --bare` records no
// remote-tracking refs, and a bare repo has no checkout that could have moved
// HEAD — its worktrees carry their own); then a conventional mainline name among
// its local branches. ok is false when none of the three resolves, and the
// caller must not paper over that with the checked-out branch: a task branch
// parked in a working tree is not evidence about which branch matters.
func InferDefaultBranch(dir string) (string, bool) {
	if b, ok := DefaultBranch(dir, "origin"); ok {
		return b, true
	}
	if ClassifyLayout(dir) == LayoutWorktree {
		if b, err := CurrentBranch(dir); err == nil && b != "" {
			return b, true
		}
	}
	if b := mainlineBranch(dir); b != "" {
		return b, true
	}
	return "", false
}

// mainlineBranch returns whichever of a small set of conventional names
// (checked in order of how common each is) exists as a local branch, or "" when
// none do — the weakest signal, a guess from convention rather than anything the
// repo states, and so the last one tried.
func mainlineBranch(dir string) string {
	locals, err := LocalBranches(dir)
	if err != nil {
		return ""
	}
	have := map[string]bool{}
	for _, b := range locals {
		have[b] = true
	}
	for _, name := range []string{"main", "master", "develop"} {
		if have[name] {
			return name
		}
	}
	return ""
}

// IsDirty reports whether the working tree has uncommitted changes (ignoring
// untracked files, matching the plugins-update dirty guard).
func IsDirty(dir string) (bool, error) {
	out, err := run(dir, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// Upstream returns the configured upstream tracking ref for branch (e.g.
// "origin/main"), or "" when none is set.
func Upstream(dir, branch string) string {
	out, err := run(dir, "rev-parse", "--abbrev-ref", branch+"@{upstream}")
	if err != nil {
		return ""
	}
	return out
}

// AheadBehind returns how many commits HEAD is ahead of and behind the given
// ref, computed via `rev-list --left-right --count ref...HEAD`.
func AheadBehind(dir, ref string) (ahead, behind int, err error) {
	return AheadBehindRefs(dir, ref, "HEAD")
}

// AheadBehindRefs returns how many commits branch is ahead of and behind base,
// computed via `rev-list --left-right --count base...branch`. Unlike
// AheadBehind, neither side needs to be checked out — used for task branches,
// which aren't the current HEAD (DESIGN §3.6).
func AheadBehindRefs(dir, base, branch string) (ahead, behind int, err error) {
	out, err := run(dir, "rev-list", "--left-right", "--count", base+"..."+branch)
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", out)
	}
	behind, _ = strconv.Atoi(fields[0]) // left = base-only
	ahead, _ = strconv.Atoi(fields[1])  // right = branch-only
	return ahead, behind, nil
}
