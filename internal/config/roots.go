package config

import (
	"sort"
	"strings"
)

// ScanRoot is a discovery location: a directory to walk and the root name whose
// settings its repos inherit.
type ScanRoot struct {
	Name string
	Dir  string
}

// ScanRoots returns the directories worth walking for discovery: every declared
// [root.*]'s `dir`, sorted by name. These are the scan set (DESIGN §3.2); a
// discovered repo inherits from whichever root's `dir` is the deepest prefix of
// its path.
func (reg *Registry) ScanRoots() []ScanRoot {
	names := make([]string, 0, len(reg.roots))
	for n := range reg.roots {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]ScanRoot, 0, len(names))
	for _, n := range names {
		if dir := reg.roots[n].Dir; dir != "" {
			out = append(out, ScanRoot{Name: n, Dir: dir})
		}
	}
	return out
}

// RootFor returns the name of the root whose `dir` is the deepest path-prefix of
// dir, and the ordered inheritance chain (shallowest → deepest) for a repo found
// there — used to resolve a discovered repo's settings. The name is "" when no
// root covers the path.
func (reg *Registry) RootFor(dir string) (name string, chain []string) {
	target := expandHome(dir)
	for n, r := range reg.roots {
		if r.Dir != "" && pathHasPrefix(target, expandHome(r.Dir)) {
			chain = append(chain, n)
		}
	}
	sort.Slice(chain, func(i, j int) bool {
		return len(expandHome(reg.roots[chain[i]].Dir)) < len(expandHome(reg.roots[chain[j]].Dir))
	})
	if len(chain) > 0 {
		name = chain[len(chain)-1] // deepest prefix
	}
	return name, chain
}

// HostKeyForURL returns the [hosts.*] key whose base contains the given remote
// hostname, or "" if none match (a discovered repo on an unknown host).
func (reg *Registry) HostKeyForURL(hostname string) string {
	for key, h := range reg.Hosts {
		if hostname != "" && strings.Contains(h.Base, hostname) {
			return key
		}
	}
	return ""
}
