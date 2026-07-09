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
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// IsRepo reports whether dir contains a git repository (a .git dir or file, as
// used by worktree containers).
func IsRepo(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
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
	out, err := run(dir, "rev-list", "--left-right", "--count", ref+"...HEAD")
	if err != nil {
		return 0, 0, err
	}
	fields := strings.Fields(out)
	if len(fields) != 2 {
		return 0, 0, fmt.Errorf("unexpected rev-list output %q", out)
	}
	behind, _ = strconv.Atoi(fields[0]) // left = ref-only
	ahead, _ = strconv.Atoi(fields[1])  // right = HEAD-only
	return ahead, behind, nil
}
