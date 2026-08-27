package gitx

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	defaultCorroborateBudget = 2 * time.Second
	corroborateBudgetEnv     = "REPO_CORROBORATE_BUDGET"
)

// DefaultCorroborateBudget is the ceiling for a repo whose config says nothing:
// the environment's value if it sets one, else 2s. It bounds a sweep, not a
// deliberate `repo prune --delete`, which passes no budget at all — the sweep
// is the thing that must stay fast, and the command someone typed on purpose is
// the thing that should finish (DESIGN §5.3).
func DefaultCorroborateBudget() time.Duration {
	if v := os.Getenv(corroborateBudgetEnv); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
	}
	return defaultCorroborateBudget
}

// errBudget marks a route cut short by the caller's budget rather than
// answered. It is not an error the caller reports: corroboration failing to
// finish means the same as corroboration failing — the branch is withheld —
// but it must not be *remembered*, because it says something about this machine
// under this load rather than about the two trees (DESIGN §5.3).
var errBudget = errors.New("budget exceeded")

// deadline bounds a whole corroboration. A zero deadline is no bound: the
// ambient per-command timeouts apply and the check takes as long as it takes,
// which is what an explicit `repo prune --delete` asks for.
type deadline struct{ at time.Time }

func newDeadline(budget time.Duration) deadline {
	if budget <= 0 {
		return deadline{}
	}
	return deadline{at: time.Now().Add(budget)}
}

// run executes one git command under whatever is left of the budget. Handing
// each call the *remaining* time rather than the whole budget is what keeps the
// total bounded by it, however many calls a route makes.
func (d deadline) run(dir string, env []string, args ...string) (string, int, error) {
	if d.at.IsZero() {
		return runCmdCode(dir, env, args...)
	}
	left := time.Until(d.at)
	if left <= 0 {
		return "", 0, errBudget
	}
	out, code, err := runCmdCodeWithin(dir, env, left, "", args...)
	if errors.Is(err, errTimedOut) {
		return out, code, errBudget
	}
	return out, code, err
}

func (d deadline) text(dir string, args ...string) (string, error) {
	out, _, err := d.run(dir, nil, args...)
	return strings.TrimSpace(out), err
}

// Corroboration is what the cross-check established, and what each route it
// tried had to say. Tried is kept even on success, so a caller that wants to
// show its work can, and so a withheld branch can report what was attempted
// rather than a bare refusal.
type Corroboration struct {
	OK    bool
	Via   string   // the route that established presence, when OK
	Tried []string // one line per route attempted, in order
	// Complete is false when a route was stopped by the budget instead of
	// answering. The verdict is the same either way — not corroborated, so
	// withhold — but an incomplete one must never be cached: it is a fact about
	// how busy the machine was, not about the branches (DESIGN §5.3).
	Complete bool
	// Took is how long establishing this took, so a caller can report the cost
	// without timing the call itself — and so a cached answer can say honestly
	// that it cost nothing.
	Took time.Duration
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
func Corroborate(dir, branch, base string, budget time.Duration) (c Corroboration, err error) {
	start := time.Now()
	defer func() { c.Took = time.Since(start) }()

	c = Corroboration{Complete: true}
	d := newDeadline(budget)

	mergeBase, err := d.text(dir, "merge-base", base, branch)
	if errors.Is(err, errBudget) {
		return budgetRanOut(c, budget, "merge-base"), nil
	}
	if err != nil || mergeBase == "" {
		return c, fmt.Errorf("no merge base with %s: %w", base, err)
	}

	ok, note, err := reverseApplies(d, dir, branch, base, mergeBase)
	if errors.Is(err, errBudget) {
		return budgetRanOut(c, budget, "reverse-apply"), nil
	}
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
	ok, note, err = mergeAddsNothing(d, dir, branch, base)
	if errors.Is(err, errBudget) {
		return budgetRanOut(c, budget, "merge-tree"), nil
	}
	c.Tried = append(c.Tried, "merge-tree: "+note)
	if ok {
		c.OK, c.Via = true, "merge-tree"
	}
	return c, nil
}

// budgetRanOut records where the clock stopped a check. The result is an
// ordinary "not corroborated" — nothing may be deleted on it — carrying the one
// extra bit that stops it being remembered as if it were an answer.
func budgetRanOut(c Corroboration, budget time.Duration, where string) Corroboration {
	c.OK, c.Complete = false, false
	c.Tried = append(c.Tried, fmt.Sprintf("%s: stopped at the %s budget", where, budget))
	return c
}

// reverseApplies is route 1: take the branch's whole diff against the merge
// base and undo it against a scratch index built from the base tip. A patch
// only reverse-applies where its post-image is actually sitting, so success is
// proof the content is there.
//
// The error return is for "the check could not be run at all" (a diff that
// would not generate, a base that would not read into an index). A patch that
// simply does not apply is not an error — it is this route answering no.
func reverseApplies(d deadline, dir, branch, base, mergeBase string) (bool, string, error) {
	// --binary so a binary change is representable rather than summarised as
	// "Binary files differ", which would not apply in either direction.
	patch, _, err := d.run(dir, nil, "diff", "--binary", mergeBase, branch)
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
	if _, _, err := d.run(dir, env, "read-tree", base); err != nil {
		return false, "", err
	}
	if _, _, err := d.run(dir, env, "apply", "--cached", "--check", "-R", patchFile); err != nil {
		if errors.Is(err, errBudget) {
			return false, "", err
		}
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
func mergeAddsNothing(d deadline, dir, branch, base string) (bool, string, error) {
	baseTree, err := d.text(dir, "rev-parse", base+"^{tree}")
	if errors.Is(err, errBudget) {
		return false, "", err
	}
	if err != nil || baseTree == "" {
		return false, "could not read base's tree", nil
	}
	// --write-tree is the modern, worktree-free form. It writes the merge
	// result into the object store, but in the case that matters it writes
	// nothing new: a result equal to base's tree is a tree that already exists.
	out, code, err := d.run(dir, nil, "merge-tree", "--write-tree", base, branch)
	switch {
	case errors.Is(err, errBudget):
		return false, "", err
	case err != nil && code == 1:
		// Exit 1 is a conflicted merge — and also how an unmergeable argument
		// reports itself. Both decline, so they need not be told apart.
		return false, "the branch does not merge cleanly into base", nil
	case err != nil:
		// Anything else — unrelated histories, a git without --write-tree.
		return false, fmt.Sprintf("could not run (%v)", err), nil
	}
	if strings.TrimSpace(out) != baseTree {
		return false, "merging the branch into base would change base", nil
	}
	return true, "merging the branch into base would not change base", nil
}
