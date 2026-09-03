// Package artifact generates the shell-sourced files (DESIGN §6): prjpath.zsh,
// homes.zsh, and plugins.zsh. `repo` is the sole writer; the shell only reads.
package artifact

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/michael-odell/repo/internal/config"
	"github.com/michael-odell/repo/internal/ident"
	"github.com/michael-odell/repo/internal/model"
)

// pluginRoot is the root name whose members are emitted as zsh plugins into
// plugins.zsh. TODO(design): make plugin emission an explicit root/repo attribute
// rather than a hardcoded root name.
const pluginRoot = "plugins"

// entry is a unified view of a repo (declared or discovered) for emission.
type entry struct {
	id        ident.ID
	primary   string            // primary working tree / worktree
	worktrees map[string]string // branch -> path (worktree repos only)
	pluginURL string            // resolved origin URL when zsh-plugin
}

// Generate writes the three artifacts into outDir and returns their paths.
// repos is the declared ∪ discovered union (DESIGN §3.2) — callers pass the
// same set status/sync/list act on, so a repo apply emits for is exactly one
// a repo home/cs entry can navigate to.
func Generate(outDir string, reg *config.Registry, repos []model.Repo) ([]string, error) {
	entries := build(reg, repos)
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	files := map[string]string{
		"prjpath.zsh": prjpath(entries),
		"homes.zsh":   homes(entries),
		"plugins.zsh": plugins(entries),
	}
	var written []string
	for name, body := range files {
		p := filepath.Join(outDir, name)
		if err := writeAtomic(p, []byte(header(body)+body)); err != nil {
			return nil, err
		}
		written = append(written, p)
	}
	sort.Strings(written)
	return written, nil
}

// writeAtomic replaces path in one step: a temp file in the same directory,
// then a rename over the target.
//
// These files are sourced by every new shell, and `repo sync` (which now
// regenerates them, DESIGN §5.5) is itself invoked *from* shell startup, in the
// background — so the moment they are rewritten is exactly the moment another
// shell is most likely to be reading them. A plain write truncates first, which
// gives that reader a real chance at half a file: a `prjpath=(` with no closing
// paren is a syntax error in the shell's own startup, and an empty homes.zsh is
// a `cs` that silently forgets every repo. A rename is atomic and leaves an
// already-open reader on the intact old file, so the worst case becomes "one
// shell started with the previous generation", which is what the staleness
// header exists to handle anyway.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // no-op once the rename succeeds
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil { // CreateTemp makes it 0600
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// build converts the union into emission entries. repos is already deduped by
// identity and by directory (unionRepos), so no merge logic is needed here — a
// discovered-only repo (no id) is skipped since it isn't addressable by name.
func build(reg *config.Registry, repos []model.Repo) []entry {
	entries := make([]entry, 0, len(repos))
	for _, r := range repos {
		if r.ID.Zero() {
			continue
		}
		e := entry{id: r.ID, primary: r.PrimaryTree()}
		if r.Worktrees {
			e.worktrees = map[string]string{}
			for _, b := range r.Branches {
				e.worktrees[b] = r.WorktreePath(b)
			}
		}
		if hasRoot(r.Roots, pluginRoot) {
			// Plugins clone from origin: the fork when present (supply-chain
			// mirror / fork-pr), otherwise the definitive identity.
			origin := r
			if r.Fork != nil {
				origin.ID = *r.Fork
			}
			if url, err := reg.Physical(origin); err == nil {
				e.pluginURL = url
			} else if r.OriginURL != "" {
				e.pluginURL = r.OriginURL // discovered, undeclared: use the real origin verbatim
			}
		}
		entries = append(entries, e)
	}
	return entries
}

func prjpath(entries []entry) string {
	var paths []string
	for _, e := range entries {
		if len(e.worktrees) > 0 {
			for _, p := range e.worktrees {
				paths = append(paths, p)
			}
		} else if e.primary != "" {
			paths = append(paths, e.primary)
		}
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString("prjpath=(\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "  %s\n", quote(p))
	}
	b.WriteString(")\n")
	return b.String()
}

func homes(entries []entry) string {
	// Short-name ambiguity: only emit the bare key when it is unique.
	shortCount := map[string]int{}
	for _, e := range entries {
		shortCount[e.id.Short()]++
	}
	type kv struct{ k, v string }
	var kvs []kv
	for _, e := range entries {
		if shortCount[e.id.Short()] == 1 {
			kvs = append(kvs, kv{e.id.Short(), e.primary})
		}
		kvs = append(kvs, kv{e.id.OwnerRepo(), e.primary})
		for b, p := range e.worktrees {
			kvs = append(kvs, kv{e.id.Short() + "@" + b, p})
		}
	}
	sort.Slice(kvs, func(i, j int) bool { return kvs[i].k < kvs[j].k })

	var b strings.Builder
	b.WriteString("typeset -gA REPO_HOME\nREPO_HOME=(\n")
	for _, e := range kvs {
		fmt.Fprintf(&b, "  %s %s\n", quote(e.k), quote(e.v))
	}
	b.WriteString(")\n")
	return b.String()
}

func plugins(entries []entry) string {
	var lines []string
	for _, e := range entries {
		if e.pluginURL != "" {
			lines = append(lines, "plugin-def "+e.pluginURL)
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n"
}

func header(body string) string {
	sum := sha256.Sum256([]byte(body))
	return fmt.Sprintf("# GENERATED by `repo apply` — do not edit. source-hash:%s\n",
		hex.EncodeToString(sum[:])[:12])
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func hasRoot(roots []string, want string) bool {
	for _, r := range roots {
		if r == want {
			return true
		}
	}
	return false
}
