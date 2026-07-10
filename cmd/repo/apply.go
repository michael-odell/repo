package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/michael-odell/repo/internal/artifact"
	"github.com/michael-odell/repo/internal/discover"
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
	repos, err := reg.Repos()
	if err != nil {
		return err
	}
	found, err := discover.Discover(resolveRoots(reg), reg)
	if err != nil {
		return err
	}
	written, err := artifact.Generate(outDir(), reg, repos, found)
	if err != nil {
		return err
	}
	for _, p := range written {
		fmt.Println("wrote", p)
	}
	return nil
}
