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
