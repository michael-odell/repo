// Package discover finds git repositories on disk under the configured roots and
// infers their identity, tag, and workflow (DESIGN §3.2). A cloned repo already
// carries most of what the registry would state, so discovery reads its remotes
// rather than requiring config.
package discover

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/ident"
	"github.com/michael-odell/repo/internal/model"
)

// maxDepth is how far below a root a repo may sit: flat (root/repo, depth 1) or
// owner-nested (root/owner/repo, depth 2).
const maxDepth = 2

// Found is a repository discovered on disk.
type Found struct {
	ID       ident.ID // zero when the repo has no usable remote
	Dir      string
	Remotes  map[string]string
	Workflow string
	Root     string   // name of the deepest root whose dir contains it ("" if none)
	Roots    []string // inheritance chain of root names
	Note     string   // e.g. "no remote"
}

// Discover walks the scan roots and returns the repos found beneath them.
func Discover(roots []config.ScanRoot, reg *config.Registry) ([]Found, error) {
	var found []Found
	for _, root := range roots {
		base := expandHome(root.Dir)
		if _, err := os.Stat(base); err != nil {
			continue // a configured root need not exist on every machine
		}
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if path != base && depth(base, path) > maxDepth {
				return fs.SkipDir
			}
			if d.Name() == ".git" {
				return fs.SkipDir
			}
			if gitx.IsRepo(path) {
				found = append(found, inspect(path, reg))
				return fs.SkipDir // don't descend into a repo
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return found, nil
}

func inspect(dir string, reg *config.Registry) Found {
	name, chain := reg.RootFor(dir)
	f := Found{Dir: dir, Root: name, Roots: chain, Workflow: model.UpstreamPush}
	remotes, err := gitx.Remotes(dir)
	if err != nil || len(remotes) == 0 {
		f.Note = "no remote"
		return f
	}
	f.Remotes = remotes
	origin := remotes["origin"]
	if origin == "" {
		for _, u := range remotes {
			origin = u
			break
		}
	}
	// Remote-inferred workflow: origin+upstream → fork-pr; origin+untrusted →
	// supply-chain-mirror (its self-documenting remote name, DESIGN §3.6).
	if _, ok := remotes["untrusted"]; ok {
		f.Workflow = model.SupplyChainMirror
	} else if _, ok := remotes["upstream"]; ok {
		f.Workflow = model.ForkPR
	}
	if host, ownerRepo, err := ident.ParseRemoteURL(origin); err == nil {
		key := reg.HostKeyForURL(host)
		if key == "" {
			key = host // fall back to the literal hostname
		}
		if id, err := ident.Parse(key + ":" + ownerRepo); err == nil {
			f.ID = id
		} else {
			f.Note = "unparseable identity"
		}
	} else {
		f.Note = "unparseable remote"
	}
	return f
}

// depth returns the number of path segments of p below base.
func depth(base, p string) int {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return maxDepth + 1
	}
	return len(strings.Split(rel, string(filepath.Separator)))
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}
