package gitx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Corroboration is what the cross-check established, and what each route it
// tried had to say. Tried is kept even on success, so a caller that wants to
// show its work can, and so a withheld branch can report what was attempted
// rather than a bare refusal.
type Corroboration struct {
	OK    bool
	Via   string   // the route that established presence, when OK
	Tried []string // one line per route attempted, in order
}

// Corroborate reports whether base already contains everything a branch
// changed, established by routes that share no code with patch-id hashing
// (DESIGN §5.3).
//
// It exists to be a second opinion, and a second opinion is only worth having
// if it comes from somewhere else. Tiers 2 and 3 both rest on `git cherry`, so
// their agreement shows one mechanism is consistent with itself and nothing
// more — and those are exactly the tiers whose deletions fall back to `git
// branch -D`, where git's own ancestry check has already declined to vouch.
//
// It asks something deliberately stronger than the tiers do, which is where the
// two disagree: `git cherry` asks whether the patch is anywhere in base's
// history, this asks whether the change is in base's tree *now*. Work that
// landed and was then reverted satisfies the first and fails the second, and
// deleting that branch would take the last copy of the content.
//
// Two routes are tried, in order of strictness, and either one is enough:
//
//  1. reverse-apply — undo the branch's whole diff against base's tree. Exact
//     and cheap, and it proves presence outright, but it is a textual
//     application: base editing within the diff's context window since the fork
//     defeats it whether or not the work landed.
//  2. merge-tree — ask whether merging the branch into base would change base.
//     It compares content rather than context, so drift cannot defeat it.
//
// Both failing means "could not corroborate", never "did not land". Callers must
// treat that as a reason to withhold a deletion and never as evidence for one.
//
// Nothing in the repo is touched: route 1 works against a scratch index built
// from base, and route 2 touches neither the index nor any working tree, so no
// worktree needs to be on the branch (or to exist).
func Corroborate(dir, branch, base string) (Corroboration, error) {
	var c Corroboration

	mergeBase, err := run(dir, "merge-base", base, branch)
	if err != nil || mergeBase == "" {
		return c, fmt.Errorf("no merge base with %s: %w", base, err)
	}

	ok, note, err := reverseApplies(dir, branch, base, mergeBase)
	if err != nil {
		return c, err
	}
	c.Tried = append(c.Tried, "reverse-apply: "+note)
	if ok {
		c.OK, c.Via = true, "reverse-apply"
		return c, nil
	}

	// Only reached once route 1 has already failed, so the ordinary case pays
	// for one route.
	ok, note = mergeAddsNothing(dir, branch, base)
	c.Tried = append(c.Tried, "merge-tree: "+note)
	if ok {
		c.OK, c.Via = true, "merge-tree"
	}
	return c, nil
}

// reverseApplies is route 1: take the branch's whole diff against the merge
// base and undo it against a scratch index built from the base tip. A patch
// only reverse-applies where its post-image is actually sitting, so success is
// proof the content is there.
//
// The error return is for "the check could not be run at all" (a diff that
// would not generate, a base that would not read into an index). A patch that
// simply does not apply is not an error — it is this route answering no.
func reverseApplies(dir, branch, base, mergeBase string) (bool, string, error) {
	// --binary so a binary change is representable rather than summarised as
	// "Binary files differ", which would not apply in either direction.
	patch, err := runRaw(dir, "diff", "--binary", mergeBase, branch)
	if err != nil {
		return false, "", err
	}
	if strings.TrimSpace(patch) == "" {
		// The branch changes nothing its merge base doesn't already have, so
		// there is nothing for base to be missing.
		return true, "the branch changes nothing its merge base does not already have", nil
	}

	tmp, err := os.MkdirTemp("", "repo-crosscheck-")
	if err != nil {
		return false, "", err
	}
	defer os.RemoveAll(tmp)

	patchFile := filepath.Join(tmp, "branch.patch")
	if err := os.WriteFile(patchFile, []byte(patch), 0o600); err != nil {
		return false, "", err
	}

	// A scratch index, so `apply --cached` has base's tree to check against
	// without the repo's real index — or the user's working tree — taking part.
	env := append(os.Environ(), "GIT_INDEX_FILE="+filepath.Join(tmp, "index"))
	if _, err := runCmd(dir, env, "read-tree", base); err != nil {
		return false, "", err
	}
	if _, err := runCmd(dir, env, "apply", "--cached", "--check", "-R", patchFile); err != nil {
		// git says the patch does not reverse-apply. Not an error: this route
		// declining is exactly what it is here to be able to do. The wording
		// stays about the diff's context, because base drifting over a hunk is
		// the common reason and it is not a statement about the branch.
		return false, "base's tree no longer matches the diff's context", nil
	}
	return true, "base holds the branch's whole diff", nil
}

// mergeAddsNothing is route 2: merge the branch into base and see whether the
// result is base's own tree. If it is, byte for byte, the branch contributes
// nothing base does not already have.
//
// This compares content rather than context, which is the whole point — a
// branch far enough behind will have had base edit inside its diff's context
// window since the fork, and that is not evidence about whether the work
// landed. Being a three-way merge it still shares no code with patch-id
// hashing, so the independence the second opinion rests on survives.
//
// It gives up nothing route 1 protected. Where work landed and was then
// reverted, the base side is back at the merge base while the branch side still
// carries the change, so the merge re-introduces it and the result differs from
// base. A conflict is a withhold, and so is a git too old for `--write-tree`:
// every failure mode here declines.
//
// `git apply --3way` is the obvious alternative and is unsafe: it counts
// "already un-applied" as success, so it passes both a base that never had the
// work and a base that reverted it (DESIGN §5.3).
func mergeAddsNothing(dir, branch, base string) (bool, string) {
	baseTree, err := run(dir, "rev-parse", base+"^{tree}")
	if err != nil || baseTree == "" {
		return false, "could not read base's tree"
	}
	// --write-tree is the modern, worktree-free form. It writes the merge
	// result into the object store, but in the case that matters it writes
	// nothing new: a result equal to base's tree is a tree that already exists.
	out, code, err := runCmdCode(dir, nil, "merge-tree", "--write-tree", base, branch)
	switch {
	case err != nil && code == 1:
		// Exit 1 is a conflicted merge — and also how an unmergeable argument
		// reports itself. Both decline, so they need not be told apart.
		return false, "the branch does not merge cleanly into base"
	case err != nil:
		// Anything else — unrelated histories, a git without --write-tree.
		return false, fmt.Sprintf("could not run (%v)", err)
	}
	if strings.TrimSpace(out) != baseTree {
		return false, "merging the branch into base would change base"
	}
	return true, "merging the branch into base would not change base"
}
