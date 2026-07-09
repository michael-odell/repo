package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/model"
)

// registryPaths returns the REPO_REGISTRY_PATH fragment list, defaulting to
// ~/.config/repos when unset (DESIGN §3.7).
func registryPaths() []string {
	if v := os.Getenv("REPO_REGISTRY_PATH"); v != "" {
		return filepath.SplitList(v)
	}
	if home, err := os.UserHomeDir(); err == nil {
		return []string{filepath.Join(home, ".config", "repos")}
	}
	return nil
}

func loadRegistry() (*config.Registry, error) {
	paths := registryPaths()
	if len(paths) == 0 {
		return nil, fmt.Errorf("no registry configured (set REPO_REGISTRY_PATH)")
	}
	return config.Load(paths)
}

func cmdList(_ context.Context, _ []string) error {
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	repos, err := reg.Repos()
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tWORKFLOW\tURL")
	for _, r := range repos {
		url, err := reg.Physical(r)
		if err != nil {
			url = "!" + err.Error()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.ID.Short(), r.ID, r.Workflow, url)
	}
	return tw.Flush()
}

func cmdResolve(_ context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: repo resolve <id|name>")
	}
	reg, err := loadRegistry()
	if err != nil {
		return err
	}
	repos, err := reg.Repos()
	if err != nil {
		return err
	}
	r, ok := find(repos, args[0])
	if !ok {
		return fmt.Errorf("no repo matching %q", args[0])
	}
	url, err := reg.Physical(r)
	if err != nil {
		return err
	}
	fmt.Println(url)
	return nil
}

// find matches a query against the full id or, when unambiguous, the short name.
func find(repos []model.Repo, q string) (model.Repo, bool) {
	for _, r := range repos {
		if r.ID.String() == q {
			return r, true
		}
	}
	var match model.Repo
	n := 0
	for _, r := range repos {
		if r.ID.Short() == q || strings.EqualFold(r.ID.OwnerRepo(), q) {
			match, n = r, n+1
		}
	}
	if n == 1 {
		return match, true
	}
	return model.Repo{}, false
}
