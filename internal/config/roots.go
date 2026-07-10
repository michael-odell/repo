package config

import "strings"

// DeclaredRoots returns the [[root]] entries from the registry fragments.
func (reg *Registry) DeclaredRoots() []Root { return reg.roots }

// HomeRoots returns the unique home_root values worth scanning, used as a
// discovery fallback when no [[root]] entries are configured. It unions the
// effective default home_root (so ~/src is scanned even when every declared
// repo overrides it via a tag), every tag's home_root, and every declared
// repo's home_root — so a location is discovered even before it holds a repo
// the registry names.
func (reg *Registry) HomeRoots() []string {
	seen := map[string]bool{}
	var out []string
	add := func(root string) {
		if root != "" && !seen[root] {
			seen[root] = true
			out = append(out, root)
		}
	}

	add(strOr(reg.defaults.HomeRoot, builtinDefaults.HomeRoot))
	for _, t := range reg.tags {
		if t.HomeRoot != nil {
			add(*t.HomeRoot)
		}
	}
	repos, err := reg.Repos()
	if err != nil {
		return out
	}
	for _, r := range repos {
		add(r.HomeRoot)
	}
	return out
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
