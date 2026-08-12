package gitx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ReverseApplies reports whether base already contains everything a branch
// changed, established by *reverse-applying* the branch's whole diff to base's
// tree rather than by comparing patch ids (DESIGN §5.3).
//
// It exists to be a second opinion, and a second opinion is only worth having
// if it comes from somewhere else. Tiers 2 and 3 both rest on `git cherry`, so
// their agreement shows one mechanism is consistent with itself and nothing
// more — and those are exactly the tiers whose deletions fall back to `git
// branch -D`, where git's own ancestry check has already declined to vouch.
// Reverse application shares no code with patch-id hashing: it asks whether the
// change can be *undone* against base, which only holds if it is there.
//
// The check is deliberately conservative. Base moving on top of the same files
// can make context fail to match even though the work did land, so a false
// answer means "could not corroborate", never "did not land". Callers must
// treat it as a reason to withhold a deletion and never as evidence for one.
//
// Nothing in the repo is touched: the patch is checked against a scratch index
// built from base, so neither the real index nor any working tree is involved,
// and no worktree needs to be on the branch (or to exist).
func ReverseApplies(dir, branch, base string) (bool, error) {
	mergeBase, err := run(dir, "merge-base", base, branch)
	if err != nil || mergeBase == "" {
		return false, fmt.Errorf("no merge base with %s: %w", base, err)
	}
	// --binary so a binary change is representable rather than summarised as
	// "Binary files differ", which would not apply in either direction.
	patch, err := runRaw(dir, "diff", "--binary", mergeBase, branch)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(patch) == "" {
		// The branch changes nothing its merge base doesn't already have, so
		// there is nothing for base to be missing.
		return true, nil
	}

	tmp, err := os.MkdirTemp("", "repo-crosscheck-")
	if err != nil {
		return false, err
	}
	defer os.RemoveAll(tmp)

	patchFile := filepath.Join(tmp, "branch.patch")
	if err := os.WriteFile(patchFile, []byte(patch), 0o600); err != nil {
		return false, err
	}

	// A scratch index, so `apply --cached` has base's tree to check against
	// without the repo's real index — or the user's working tree — taking part.
	env := append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(tmp, "index"))
	if _, err := runCmd(dir, env, "read-tree", base); err != nil {
		return false, err
	}
	if _, err := runCmd(dir, env, "apply", "--cached", "--check", "-R", patchFile); err != nil {
		// git says the patch does not reverse-apply. That is the corroboration
		// failing, which is all this reports — the error text is kept for
		// whoever wants to know why it could not be established.
		return false, nil
	}
	return true, nil
}
