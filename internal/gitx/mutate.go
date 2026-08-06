package gitx

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// Clone clones url into dir (parent created by caller).
func Clone(url, dir string) error {
	cmd := exec.Command("git", "clone", url, dir)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// CloneBare clones url into dir as a bare repository — the object store for a
// worktree-layout container (DESIGN §4).
func CloneBare(url, dir string) error {
	cmd := exec.Command("git", "clone", "--bare", url, dir)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// CloneLocal clones a local repository into dir as a normal working clone,
// hardlinking objects where possible — used to collapse a bare worktree
// container back into a single tree (DESIGN §4.1).
func CloneLocal(src, dir string) error {
	cmd := exec.Command("git", "clone", "--local", src, dir)
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// CreateBranch creates a local branch at start (no checkout), used to preserve
// every ref when rebuilding a clone from a bare repo.
func CreateBranch(dir, name, start string) error {
	_, err := run(dir, "branch", name, start)
	return err
}

// WorktreeAdd checks out branch into a new worktree at path. It prefers an
// existing local branch, else creates a tracking branch from origin/upstream so
// a newly-declared important branch gets a worktree without manual setup.
func WorktreeAdd(container, path, branch string) error {
	if _, err := run(container, "worktree", "add", path, branch); err == nil {
		return nil
	}
	for _, start := range []string{"origin/" + branch, "upstream/" + branch} {
		if _, ok := RevParse(container, start); ok {
			_, err := run(container, "worktree", "add", "-b", branch, path, start)
			return err
		}
	}
	return fmt.Errorf("no local or remote branch %q to add a worktree for", branch)
}

// RemoteURL returns the fetch URL of a remote and whether it exists.
func RemoteURL(dir, name string) (string, bool) {
	u, err := run(dir, "remote", "get-url", name)
	if err != nil {
		return "", false
	}
	return u, true
}

// EnsureRemote adds the remote or updates its URL, and normalizes its fetch
// refspec to the standard remote-tracking mapping so a fetch always populates
// refs/remotes/<name>/* — including in a bare worktree container, whose clone
// may otherwise carry a non-standard refspec. Reports whether the URL changed.
func EnsureRemote(dir, name, url string) (changed bool, err error) {
	cur, ok := RemoteURL(dir, name)
	switch {
	case !ok:
		if _, err = run(dir, "remote", "add", name, url); err != nil {
			return true, err
		}
		changed = true
	case cur != url:
		if _, err = run(dir, "remote", "set-url", name, url); err != nil {
			return true, err
		}
		changed = true
	}
	_, err = run(dir, "config", "remote."+name+".fetch", "+refs/heads/*:refs/remotes/"+name+"/*")
	return changed, err
}

// Fetch fetches a remote with prune and tags.
func Fetch(dir, remote string) error {
	_, err := run(dir, "fetch", "--prune", "--tags", remote)
	return err
}

// FastForwardCurrent fast-forwards the checked-out branch to ref, failing (not
// merging) when the move would not be a fast-forward. Use this only for a
// branch that is HEAD of dir — it moves the working tree along with the branch.
// For any other branch, use FastForwardRef.
func FastForwardCurrent(dir, ref string) error {
	_, err := run(dir, "merge", "--ff-only", ref)
	return err
}

// FastForwardRef fast-forwards a branch that is *not* checked out anywhere to
// ref, without touching any working tree — the counterpart to
// FastForwardCurrent (DESIGN §5.1). `git fetch . <ref>:<branch>` is the whole
// implementation, and git supplies both rails for free: it rejects a
// non-fast-forward (no plus in the refspec), and it refuses outright if branch
// is checked out in *any* worktree of the repo, which is why callers can pick
// between the two functions on a simple "is it checked out" test without
// racing a worktree they didn't know about.
func FastForwardRef(dir, branch, ref string) error {
	_, err := run(dir, "fetch", ".", ref+":"+branch)
	return err
}

// ResetHardCurrent resets the checked-out branch to ref (used when force_pull
// matches, DESIGN §5.2). Like FastForwardCurrent, this is the HEAD-of-dir
// variant: it discards working-tree state along with the ref, so it is only
// ever called for a branch that is actually checked out there.
func ResetHardCurrent(dir, ref string) error {
	_, err := run(dir, "reset", "--hard", ref)
	return err
}

// ForceSetRef moves a branch that is not checked out anywhere to ref even when
// the move is not a fast-forward — the no-working-tree counterpart to
// ResetHardCurrent (DESIGN §5.2, force_pull). The leading "+" is the only
// difference from FastForwardRef; git still refuses if branch is checked out.
func ForceSetRef(dir, branch, ref string) error {
	_, err := run(dir, "fetch", ".", "+"+ref+":"+branch)
	return err
}

// RevParse resolves a ref to a SHA and whether it exists.
func RevParse(dir, ref string) (string, bool) {
	sha, err := run(dir, "rev-parse", "--verify", "--quiet", ref)
	if err != nil || sha == "" {
		return "", false
	}
	return sha, true
}

// CountBetween returns the number of commits in `from..to` (reachable from to,
// not from).
func CountBetween(dir, from, to string) (int, error) {
	out, err := run(dir, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}

// Push pushes a single branch to a remote without force, so a non-fast-forward
// fails rather than clobbering the remote (fork-pr: DESIGN §5.1).
func Push(dir, remote, branch string) error {
	_, err := run(dir, "push", remote, branch)
	return err
}

// ForcePush force-pushes a single branch to a remote with --force-with-lease
// (rejects if the remote moved since the last fetch, so it can't clobber a
// concurrent push it hasn't seen) — used only for a branch explicitly listed in
// `force_push` (DESIGN §5.2).
func ForcePush(dir, remote, branch string) error {
	_, err := run(dir, "push", "--force-with-lease", remote, branch)
	return err
}

// Checkout switches the working tree to ref (a branch or, for a vendor tag pin,
// a detached tag).
func Checkout(dir, ref string) error {
	_, err := run(dir, "checkout", "--quiet", ref)
	return err
}

// Tags lists local tag names.
func Tags(dir string) ([]string, error) {
	out, err := run(dir, "tag", "--list")
	if err != nil {
		return nil, err
	}
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// TagAtHead returns the tag pointing exactly at HEAD, or "" when HEAD is not on a
// tag. Used to report a vendor pin bump (old → new).
func TagAtHead(dir string) string {
	out, err := run(dir, "describe", "--tags", "--exact-match", "HEAD")
	if err != nil {
		return ""
	}
	return out
}

// RemoteTagSHA returns the object id a remote advertises for a tag, and whether
// the remote has it. Compared against the local tag to detect a moved tag
// (a rewrite: DESIGN §5.2).
func RemoteTagSHA(dir, remote, tag string) (string, bool) {
	out, err := run(dir, "ls-remote", "--tags", remote, "refs/tags/"+tag)
	if err != nil || out == "" {
		return "", false
	}
	return strings.Fields(out)[0], true
}

// ForceFetchTag overwrites a local tag with the remote's, used only when
// force_pull matches a moved vendor tag (DESIGN §5.2).
func ForceFetchTag(dir, remote, tag string) error {
	_, err := run(dir, "fetch", "--force", remote, "refs/tags/"+tag+":refs/tags/"+tag)
	return err
}
