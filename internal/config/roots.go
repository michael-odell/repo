package config

import "strings"

// DeclaredRoots returns the [[root]] entries from the registry fragments.
func (reg *Registry) DeclaredRoots() []Root { return reg.roots }

// HomeRoots returns the unique home_root values across the effective repos, used
// as a discovery fallback when no roots are otherwise configured.
func (reg *Registry) HomeRoots() []string {
	seen := map[string]bool{}
	var out []string
	repos, err := reg.Repos()
	if err != nil {
		return nil
	}
	for _, r := range repos {
		if !seen[r.HomeRoot] {
			seen[r.HomeRoot] = true
			out = append(out, r.HomeRoot)
		}
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
