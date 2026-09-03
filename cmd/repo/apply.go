package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/michael-odell/repo/internal/artifact"
	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/model"
)

// outDir is where generated artifacts live: $REPO_OUT, else ~/.local/repo
// (uncommitted; DESIGN §6).
func outDir() string {
	if v := os.Getenv("REPO_OUT"); v != "" {
		return v
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "repo")
}

func cmdApply(_ context.Context, _ []string) error {
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	repos, err := unionRepos(reg)
	if err != nil {
		return err
	}
	written, err := artifact.Generate(outDir(), reg, repos)
	if err != nil {
		return err
	}
	for _, p := range written {
		fmt.Println("wrote", p)
	}
	return nil
}

// refreshArtifacts regenerates the shell artifacts after a sync — DESIGN §5.5:
// "after syncing, `repo` regenerates the artifacts so drift between 'registry
// changed' and 'shell sees it' heals on the same beat". Only `repo apply` wrote
// them before, so a sync that cloned a new repo left the shell navigating by a
// map that didn't contain it until someone remembered a command that nothing
// prompts for — and the artifacts are the *only* thing the shell reads (§2
// principle 2), so what they omit is simply not there.
//
// It always emits the whole union, never the sweep's selection: the artifacts
// describe the machine, so regenerating them from `repo sync work` would shrink
// prjpath to the work root and drop every other repo out of `cs`.
//
// A failure is reported and does not fail the sync: the repositories are
// already reconciled by this point, and the exit status answers "did a repo
// fail", not "did the shell's map get rewritten". Stale artifacts are what the
// staleness header is for.
func refreshArtifacts(w io.Writer, reg *config.Registry, repos []model.Repo, dryRun, verbose bool) {
	if dryRun {
		if verbose {
			fmt.Fprintln(w, "would regenerate shell artifacts in", outDir())
		}
		return
	}
	written, err := artifact.Generate(outDir(), reg, repos)
	if err != nil {
		fmt.Fprintf(w, "warning: could not regenerate shell artifacts in %s: %v\n", outDir(), err)
		return
	}
	if verbose {
		fmt.Fprintf(w, "regenerated %d shell artifact(s) in %s\n", len(written), outDir())
	}
}
