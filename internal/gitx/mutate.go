package gitx

import (
	"fmt"
	"strconv"
	"strings"
)

// The clones go through runCmd like every other invocation, which gets them the
// network deadline — a first clone is the single likeliest thing in a sweep to
// hang, so exempting it would have exempted the main case. It also stops them
// writing git's transfer progress straight to the terminal: six concurrent
// clones interleaving "Receiving objects" with each other, and with the sweep's
// own status line, is noise rather than feedback. What is happening, and to
// which repo, is the status line's job (see cmd/repo/progress.go); a clone that
// fails still reports git's stderr as the error.

// Clone clones url into dir (parent created by caller).
func Clone(url, dir string) error {
	_, err := runCmd("", nil, "clone", url, dir)
	return err
}

// CloneBare clones url into dir as a bare repository — the object store for a
// worktree-layout container (DESIGN §4).
func CloneBare(url, dir string) error {
	_, err := runCmd("", nil, "clone", "--bare", url, dir)
	return err
}

// CloneLocal clones a local repository into dir as a normal working clone,
// hardlinking objects where possible — used to collapse a bare worktree
// container back into a single tree (DESIGN §4.1).
func CloneLocal(src, dir string) error {
	_, err := runCmd("", nil, "clone", "--local", src, dir)
	return err
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

// MovedTagsError reports a fetch that failed *only* because the remote moved
// tags this clone already has. `--tags` asks for refs/tags/*:refs/tags/* with no
// leading "+", so git refuses to overwrite an existing tag: it rejects those
// refs individually, updates every other ref in the same run, and then exits 1
// because something didn't update. The distinction matters because that exit
// status is otherwise indistinguishable from a fetch that achieved nothing —
// and treating the two alike abandons a repo whose branches are, in fact,
// current (DESIGN §5.2).
//
// It carries the tags rather than a message so the caller can report them in
// its own voice, and says nothing about *why* upstream moved them: a rewritten
// tag is what git observed, and whether that was a release process or something
// worth alarm is not a thing a fetch can establish.
type MovedTagsError struct {
	Remote string
	Tags   []string // local tag names left at their old value
}

func (e *MovedTagsError) Error() string {
	return fmt.Sprintf("%s moved %d existing tag(s), not followed: %s",
		e.Remote, len(e.Tags), strings.Join(e.Tags, ", "))
}

// TagPolicy is which tags a fetch should ask for and which of those it may
// overwrite (DESIGN §3.6). The zero value fetches every tag and overwrites
// none, which is what every caller outside sync wants.
type TagPolicy struct {
	Fetch []string // glob patterns; nil means every tag, empty means none
	Force []string // glob patterns; those the fetch may overwrite in place
}

// fetchArgs turns a policy into git's arguments for fetching from remote. The
// whole feature is which refspecs are asked for and which carry a leading "+",
// because that plus is exactly git's "you may overwrite this ref":
//
//	--tags                                     every tag, none overwritable
//	--no-tags +refs/tags/v*:refs/tags/v*       only v*, and v* may be overwritten
//
// A forced pattern is listed *in addition to* the unforced scope rather than
// instead of it: git applies the more specific forced refspec to the tags it
// names and the plain one to the rest, so one invocation both follows the
// blessed tags and keeps refusing every other. --no-tags is what makes a
// narrowed scope real — without it git auto-follows any tag reachable from the
// branches it fetched, and the list would quietly not be a list.
//
// The branch refspec is restated here whenever any tag refspec is passed,
// because **naming a refspec on the command line replaces the remote's
// configured one entirely** rather than adding to it. Without this, asking to
// follow one tag would silently stop fetching branches altogether — the repo
// would keep syncing, report no error, and quietly never see upstream again.
// It matches what EnsureRemote writes into remote.<name>.fetch, so the explicit
// and configured forms stay the same fetch.
func (p TagPolicy) fetchArgs(remote string) []string {
	all := p.Fetch == nil || matchesGlob(p.Fetch, "*")
	if all && len(p.Force) == 0 {
		return []string{"--tags"} // configured refspec untouched: no need to restate it
	}

	args := []string{"+refs/heads/*:refs/remotes/" + remote + "/*"}
	if all {
		args = append(args, "--tags")
	} else {
		args = append(args, "--no-tags")
		for _, g := range p.Fetch {
			args = append(args, "refs/tags/"+g+":refs/tags/"+g)
		}
	}
	// A forced pattern outside the fetched scope is not an error: it simply
	// names nothing, the same way force_pull may name a branch that doesn't
	// exist. Skipping them all when no tags are fetched keeps git from being
	// handed a refspec that contradicts --no-tags.
	if len(p.Fetch) != 0 || all {
		for _, g := range p.Force {
			args = append(args, "+refs/tags/"+g+":refs/tags/"+g)
		}
	}
	return args
}

func matchesGlob(patterns []string, want string) bool {
	for _, p := range patterns {
		if p == want {
			return true
		}
	}
	return false
}

// TagMove is one tag this fetch overwrote in place, because force_tags named
// it. From is what the tag pointed at before — the only record of it that will
// exist anywhere, since refs/tags has no reflog, which is why it is carried out
// of the fetch rather than merely counted.
type TagMove struct {
	Tag  string
	From string
	To   string
}

func (m TagMove) String() string {
	return fmt.Sprintf("%s moved %s → %s", m.Tag, shortSHA(m.From), shortSHA(m.To))
}

func shortSHA(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// FetchResult is what a fetch did that a caller may want to report, beyond
// success or failure.
type FetchResult struct {
	// Followed are the tags force_tags allowed this fetch to overwrite. A
	// followed move is never silent: it is the one thing here that destroys
	// something, so it is reported whether or not anything else went wrong.
	Followed []TagMove
}

// Fetch fetches a remote with prune, and with tags per policy. A fetch that
// fails only because the remote moved tags the clone already has — and that the
// policy did not bless — returns *MovedTagsError; every other failure returns
// git's error unchanged. The result is meaningful on both paths, since one
// fetch can follow a blessed tag and refuse an unblessed one at the same time.
func Fetch(dir, remote string, policy TagPolicy) (FetchResult, error) {
	args := append([]string{"fetch", "--porcelain", "--prune", remote}, policy.fetchArgs(remote)...)
	out, code, err := runCmdCode(dir, nil, args...)
	res := FetchResult{Followed: followedTags(out)}
	if err == nil {
		return res, nil
	}
	// Exit 1 is git's "some ref did not update", the only status that can mean a
	// per-ref refusal; anything else (128 for a dead connection or a bad remote)
	// failed before or beyond ref negotiation and is nobody's tag problem.
	if code != 1 {
		return res, err
	}
	tags, onlyTags := rejectedTags(out)
	if !onlyTags || len(tags) == 0 {
		return res, err
	}
	return res, &MovedTagsError{Remote: remote, Tags: tags}
}

// followedTags reads the tags a fetch overwrote in place. Git's porcelain flag
// for an updated existing tag is "t"; a brand-new tag is "*" and is not a move,
// having replaced nothing.
func followedTags(porcelain string) []TagMove {
	var out []TagMove
	for _, line := range splitLines(porcelain) {
		f := strings.Fields(line)
		if len(f) < 4 || f[0] != "t" || !strings.HasPrefix(f[3], "refs/tags/") {
			continue
		}
		out = append(out, TagMove{Tag: strings.TrimPrefix(f[3], "refs/tags/"), From: f[1], To: f[2]})
	}
	return out
}

// rejectedTags reads `fetch --porcelain` output — one "<flag> <old> <new>
// <ref>" line per ref, with "!" marking a ref git declined to update — and
// returns the rejected tags, plus whether *every* rejection was a tag. A
// rejected branch means something else went wrong, and the caller must keep
// treating that as a failure rather than shrugging it off with the tags.
func rejectedTags(porcelain string) (tags []string, onlyTags bool) {
	for _, line := range splitLines(porcelain) {
		if !strings.HasPrefix(line, "!") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			return nil, false // unparseable: assume the worst and stay fatal
		}
		ref := f[3]
		if !strings.HasPrefix(ref, "refs/tags/") {
			return nil, false
		}
		tags = append(tags, strings.TrimPrefix(ref, "refs/tags/"))
	}
	return tags, true
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
