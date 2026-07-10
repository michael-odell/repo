package sync

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/michael-odell/repo/internal/gitx"
)

// relayout converts the container to its configured layout — the explicit
// sync --fix-layout action, run only after the normal sync has pushed history to
// the remote (DESIGN §4.1). kind is the current on-disk layout. All local refs
// and working-tree residue are preserved; the original is kept intact until the
// new layout validates.
func (x *run) relayout(kind gitx.LayoutKind) {
	if x.res.Err != nil {
		return
	}
	if x.opts.DryRun {
		x.add("would relayout %s → %s (sync --fix-layout)", layoutName(kind), layoutName(opp(kind)))
		x.updated("would relayout")
		return
	}
	switch kind {
	case gitx.LayoutSingle:
		x.relayoutToWorktree()
	default:
		// Reverse (worktree → single) lands in a follow-up; never touch data here.
		x.attention("worktree → single relayout not yet implemented")
		x.add("worktree → single conversion is pending; keep worktrees=true or convert manually")
	}
}

// relayoutToWorktree rebuilds a single clone as a bare+worktree container,
// preserving unpushed refs (the bare is cloned from the local .git) and the
// working-tree residue (copied onto the branch the clone was on). DESIGN §4.1.
func (x *run) relayoutToWorktree() {
	container := x.container
	aside := container + ".pre-worktree"
	if _, err := os.Stat(aside); err == nil {
		x.fail(fmt.Errorf("relayout staging path already exists: %s", shorten(aside)))
		return
	}
	prevBranch, _ := gitx.CurrentBranch(container) // branch the single clone was on
	origin, upstream, ok := x.resolveRemotes()
	if !ok {
		return
	}

	// 1. Move the intact clone aside; it is the recoverable fallback until the
	//    new layout validates.
	if err := os.Rename(container, aside); err != nil {
		x.fail(err)
		return
	}
	fail := func(err error) {
		// Restore the original if the half-built container isn't a usable clone.
		if gitx.ClassifyLayout(container) != gitx.LayoutSingle {
			_ = os.RemoveAll(container)
			_ = os.Rename(aside, container)
		}
		x.fail(err)
	}

	// 2. Bare object store from the LOCAL clone — preserves every ref (including
	//    unpushed branches) and needs no network.
	bare := filepath.Join(container, ".bare")
	if err := os.MkdirAll(container, 0o755); err != nil {
		fail(err)
		return
	}
	if err := gitx.CloneBare(aside, bare); err != nil {
		fail(err)
		return
	}
	if err := writeGitFile(container); err != nil {
		fail(err)
		return
	}

	// 3. Remotes + best-effort fetch so remote-tracking refs exist for any
	//    important branch that was not yet local.
	_, _ = gitx.EnsureRemote(container, "origin", origin)
	if upstream != "" {
		_, _ = gitx.EnsureRemote(container, "upstream", upstream)
	}
	_ = gitx.Fetch(container, "origin")
	if upstream != "" {
		_ = gitx.Fetch(container, "upstream")
	}

	// 4. A worktree per important branch, plus the previously-checked-out branch
	//    if it is not important (so ad-hoc work is not stranded).
	targets := append([]string{}, x.r.Branches...)
	if prevBranch != "" && !contains(targets, prevBranch) {
		targets = append(targets, prevBranch)
	}
	for _, b := range targets {
		if err := gitx.WorktreeAdd(container, filepath.Join(container, b), b); err != nil {
			fail(fmt.Errorf("add worktree %s: %w", b, err))
			return
		}
	}

	// 5. Carry the working-tree residue (modified, untracked, ignored) onto the
	//    branch the clone was on; a detached clone lands it on the primary branch.
	residueBranch := prevBranch
	if residueBranch == "" {
		residueBranch = branch0(x.r)
	}
	if err := copyTree(aside, filepath.Join(container, residueBranch), ".git"); err != nil {
		fail(err)
		return
	}

	// 6. Validate before discarding the original.
	if gitx.ClassifyLayout(container) != gitx.LayoutWorktree {
		fail(fmt.Errorf("relayout produced an unexpected layout"))
		return
	}
	for _, b := range x.r.Branches {
		if !gitx.IsRepo(filepath.Join(container, b)) {
			fail(fmt.Errorf("worktree %s missing after relayout", b))
			return
		}
	}

	// 7. Original fully superseded — remove it, then reconcile the fresh
	//    worktrees so lagging branches fast-forward.
	if err := os.RemoveAll(aside); err != nil {
		x.add("relayout complete but could not remove %s: %v", shorten(aside), err)
	}
	x.add("relayout: single → worktree (residue on %s)", residueBranch)
	for _, b := range x.r.Branches {
		x.updateUnit(filepath.Join(container, b), b)
	}
	if rank(x.res.Outcome) < rank(Updated) {
		x.mark(Updated, "relayout: single → worktree")
	}
}

func opp(k gitx.LayoutKind) gitx.LayoutKind {
	if k == gitx.LayoutSingle {
		return gitx.LayoutWorktree
	}
	return gitx.LayoutSingle
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// copyTree recursively copies src into dst (created as needed), overlaying files
// and skipping a top-level entry named exclude (the source's .git). Symlinks are
// recreated; regular-file modes are preserved. Used to carry working-tree
// residue across a relayout.
func copyTree(src, dst, exclude string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if rel == exclude {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			link, err := os.Readlink(p)
			if err != nil {
				return err
			}
			_ = os.Remove(target)
			return os.Symlink(link, target)
		default:
			return copyFile(p, target)
		}
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, fi.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
