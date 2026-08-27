package sync

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	stdsync "sync" // this package is also called sync
	"time"

	"github.com/michael-odell/repo/internal/gitx"
	"github.com/michael-odell/repo/internal/model"
	"github.com/michael-odell/repo/internal/xdg"
)

// Corroborator is what Classify consults before letting a rewritten-tier branch
// be reported prunable (DESIGN §5.3).
//
// A nil Corroborator means "don't ask": the label is then the patch tiers' word
// alone, which is what it was before corroboration moved into classification.
// That is the right shape for tests and for a caller that only wants the tiers'
// view, and the wrong one for anything a person will act on.
type Corroborator interface {
	Corroborate(dir, branch, base, branchSHA, baseSHA string, budget time.Duration) gitx.Corroboration
	// HonoursBudget reports whether a repo's corroborate_budget applies. It
	// bounds the *sweep*, so a corroborator that answers false — one behind a
	// command someone typed — corroborates even a repo that switched the
	// sweep's check off, and its label and its actions stay in agreement.
	HonoursBudget() bool
}

// Corroborations is a budgeted, cached Corroborator, safe for the sweep's
// parallel workers.
//
// The cache is keyed on the pair of shas the answer is a pure function of, so
// it cannot go stale: a ref that moves is a different key rather than an
// invalidation, and there is nothing to expire. Entries carry the time they
// were written only so the file can be bounded.
type Corroborations struct {
	unbounded bool
	path      string

	mu      stdsync.Mutex
	entries map[string]cacheEntry
	dirty   bool
}

type cacheEntry struct {
	OK  bool   `json:"ok"`
	Via string `json:"via,omitempty"`
	// Tried is kept so a cache hit can still answer "why not?". `--explain`
	// exists to show the mechanism, and a remembered verdict that had forgotten
	// its reasons would be exactly the conclusion-with-its-working-erased that
	// section refuses (DESIGN §5.3).
	Tried []string  `json:"tried,omitempty"`
	Seen  time.Time `json:"seen"`
}

// maxCacheEntries bounds the file. A machine with a few dozen repos writes a
// handful of entries a year, so this is not a limit anyone reaches — it is
// there so a pathological case cannot grow the file without end. The oldest go
// first, which for a permanently-valid entry means the least recently useful.
const maxCacheEntries = 5000

// OpenCorroborations loads the cache, or starts an empty one. A cache that
// cannot be read is not an error: the answers in it are all rederivable, which
// is the entire reason it lives under XDG_CACHE_HOME rather than beside the
// journal (DESIGN §5.3).
func OpenCorroborations() *Corroborations {
	c := &Corroborations{
		path:    filepath.Join(xdg.CacheDir(), "corroborations.json"),
		entries: map[string]cacheEntry{},
	}
	if body, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(body, &c.entries)
	}
	return c
}

// Unbounded returns the same cache with per-repo budgets ignored, for a command
// someone typed on purpose. A sweep must stay fast; `repo prune` is a deliberate
// act on a named repo and should finish rather than report a branch it declined
// to think about.
func (c *Corroborations) Unbounded() *Corroborations {
	c.unbounded = true
	return c
}

// HonoursBudget is false once Unbounded has been called.
func (c *Corroborations) HonoursBudget() bool { return !c.unbounded }

// Corroborate answers from the cache when it can, and records what it learns.
func (c *Corroborations) Corroborate(dir, branch, base, branchSHA, baseSHA string, budget time.Duration) (got gitx.Corroboration) {
	start := time.Now()
	defer func() { got.Took = time.Since(start) }()

	key := branchSHA + ":" + baseSHA
	if branchSHA != "" && baseSHA != "" {
		c.mu.Lock()
		e, ok := c.entries[key]
		c.mu.Unlock()
		if ok {
			tried := append(append([]string{}, e.Tried...),
				"cached: answered when these two commits were last compared")
			return gitx.Corroboration{OK: e.OK, Via: e.Via, Complete: true, Tried: tried}
		}
	}

	if c.unbounded {
		budget = 0
	}
	got, err := gitx.Corroborate(dir, branch, base, budget)
	if err != nil {
		// The check could not be run at all, which is neither a yes nor a
		// remembered no: an unreadable repo is not a fact about these trees.
		return gitx.Corroboration{Tried: []string{"cross-check could not run: " + err.Error()}}
	}
	// Only a completed outcome is remembered, in either direction. A budget
	// that ran out says how busy this machine was, not whether the work landed.
	if got.Complete && branchSHA != "" && baseSHA != "" {
		c.mu.Lock()
		c.entries[key] = cacheEntry{OK: got.OK, Via: got.Via, Tried: got.Tried, Seen: time.Now()}
		c.dirty = true
		c.mu.Unlock()
	}
	return got
}

// Close writes the cache back, atomically, if anything was learned. Every
// failure here is silent by design: the run's real work is already done, and a
// cache that could not be saved costs a recomputation next time and nothing
// else — reporting it would be noise about an optimisation.
func (c *Corroborations) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.dirty {
		return nil
	}
	c.trim()
	body, err := json.Marshal(c.entries)
	if err != nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return nil
	}
	// Written aside and renamed, so a sweep interrupted mid-write leaves the
	// previous cache intact rather than a truncated file the next run cannot
	// parse.
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return nil
	}
	_ = os.Rename(tmp, c.path)
	return nil
}

// trim drops the oldest entries once the file would exceed its bound.
func (c *Corroborations) trim() {
	if len(c.entries) <= maxCacheEntries {
		return
	}
	keys := make([]string, 0, len(c.entries))
	for k := range c.entries {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return c.entries[keys[i]].Seen.After(c.entries[keys[j]].Seen)
	})
	for _, k := range keys[maxCacheEntries:] {
		delete(c.entries, k)
	}
}

// corroborated reports how many verdicts were corroborated this pass and what
// it cost. A verdict nothing asked about — an ancestry-tier merge, or one a
// cheaper blocker settled — carries no corroboration and is not counted.
func corroborated(vs []Verdict) (n int, spent time.Duration) {
	for _, v := range vs {
		if len(v.Corroboration.Tried) == 0 {
			continue
		}
		n++
		spent += v.Corroboration.Took
	}
	return n, spent
}

// wantsCorroboration reports whether this repo's label should be corroborated.
//
// A budget of zero is "off", not "unlimited" — the one place those two readings
// of the same number had to be told apart. It switches off the *sweep's* check,
// which is the thing whose cost anyone was worried about, so a corroborator that
// does not honour budgets asks anyway: `repo prune` reporting a branch its own
// delete gate would then refuse is the disagreement this all exists to end.
func wantsCorroboration(r model.Repo, cor Corroborator) bool {
	return corroborateBudgetOf(r) > 0 || !cor.HonoursBudget()
}

// corroborateBudgetOf resolves how long corroboration may take for this repo:
// what config states, else the ambient default. Unset is deliberately not
// "off" — see model.Repo.CorroborateBudget.
func corroborateBudgetOf(r model.Repo) time.Duration {
	if r.CorroborateBudget != nil {
		return *r.CorroborateBudget
	}
	return gitx.DefaultCorroborateBudget()
}
