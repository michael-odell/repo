package gitx

import (
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

// RemoteURL returns the fetch URL of a remote and whether it exists.
func RemoteURL(dir, name string) (string, bool) {
	u, err := run(dir, "remote", "get-url", name)
	if err != nil {
		return "", false
	}
	return u, true
}

// EnsureRemote adds the remote or updates its URL; reports whether it changed.
func EnsureRemote(dir, name, url string) (changed bool, err error) {
	cur, ok := RemoteURL(dir, name)
	switch {
	case !ok:
		_, err = run(dir, "remote", "add", name, url)
		return true, err
	case cur != url:
		_, err = run(dir, "remote", "set-url", name, url)
		return true, err
	}
	return false, nil
}

// Fetch fetches a remote with prune and tags.
func Fetch(dir, remote string) error {
	_, err := run(dir, "fetch", "--prune", "--tags", remote)
	return err
}

// FastForwardCurrent fast-forwards the checked-out branch to ref, failing (not
// merging) when the move would not be a fast-forward.
func FastForwardCurrent(dir, ref string) error {
	_, err := run(dir, "merge", "--ff-only", ref)
	return err
}

// ResetHardCurrent resets the checked-out branch to ref (used for on_rewrite=follow).
func ResetHardCurrent(dir, ref string) error {
	_, err := run(dir, "reset", "--hard", ref)
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
// on_rewrite=follow adopts a moved vendor tag.
func ForceFetchTag(dir, remote, tag string) error {
	_, err := run(dir, "fetch", "--force", remote, "refs/tags/"+tag+":refs/tags/"+tag)
	return err
}
