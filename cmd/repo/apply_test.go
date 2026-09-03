package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/model"
)

func baseRegistry(t *testing.T) (*config.Registry, []model.Repo) {
	t.Helper()
	reg, err := config.Load([]string{filepath.Join("..", "..", "internal", "config", "testdata", "base.toml")})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := reg.Repos()
	if err != nil {
		t.Fatal(err)
	}
	return reg, repos
}

// TestSyncRefreshesArtifacts: DESIGN §5.5 has the artifacts regenerated on the
// same beat as the sync, because the shell reads nothing else (§2 principle 2)
// — a repo cloned by a sweep is not navigable until they are rewritten.
func TestSyncRefreshesArtifacts(t *testing.T) {
	out := t.TempDir()
	t.Setenv("REPO_OUT", out)
	reg, repos := baseRegistry(t)

	var buf bytes.Buffer
	refreshArtifacts(&buf, reg, repos, false, false)
	for _, name := range []string{"prjpath.zsh", "homes.zsh", "plugins.zsh"} {
		if _, err := os.Stat(filepath.Join(out, name)); err != nil {
			t.Errorf("%s not regenerated: %v", name, err)
		}
	}
	if buf.Len() != 0 {
		t.Errorf("a successful refresh should say nothing; got %q", buf.String())
	}
}

// TestDryRunWritesNoArtifacts: -n is answerable without writing anything a
// shell reads (DESIGN §5.7).
func TestDryRunWritesNoArtifacts(t *testing.T) {
	out := t.TempDir()
	t.Setenv("REPO_OUT", out)
	reg, repos := baseRegistry(t)

	var buf bytes.Buffer
	refreshArtifacts(&buf, reg, repos, true, true)
	ents, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 0 {
		t.Errorf("--dry-run wrote %d file(s) into %s", len(ents), out)
	}
	if !strings.Contains(buf.String(), "would regenerate") {
		t.Errorf("verbose dry run should say what it skipped; got %q", buf.String())
	}
}

// TestArtifactFailureDoesNotFailTheSync: by the time the artifacts are written
// the repos are already reconciled, so an unwritable $REPO_OUT is a warning on
// the run, not a failed sync — but it must not pass silently either.
func TestArtifactFailureDoesNotFailTheSync(t *testing.T) {
	out := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(out, []byte("in the way\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REPO_OUT", out)
	reg, repos := baseRegistry(t)

	var buf bytes.Buffer
	refreshArtifacts(&buf, reg, repos, false, false)
	if !strings.Contains(buf.String(), "could not regenerate") {
		t.Errorf("a failed refresh must be reported; got %q", buf.String())
	}
}
