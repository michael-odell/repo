// Package ident parses and represents a repository's definitive identity,
// "host:owner/repo" (see docs/DESIGN.md §3.3). The host is a key into the
// registry's [hosts.*] table; owner may contain slashes (nested groups); the
// short name is the final path segment.
package ident

import (
	"fmt"
	"strings"
)

// ID is a definitive repository identity: host:owner/repo.
type ID struct {
	Host  string // registry host key, e.g. "github", "ghe"
	Owner string // everything between "host:" and the last "/"
	Name  string // final path segment (short name)
}

// Parse reads a "host:owner/repo" string. Owner may itself contain slashes
// (e.g. "ghe:group/sub/repo" → Owner "group/sub", Name "repo").
func Parse(s string) (ID, error) {
	host, rest, ok := strings.Cut(s, ":")
	if !ok {
		return ID{}, fmt.Errorf("identity %q: missing host (want host:owner/repo)", s)
	}
	if host == "" {
		return ID{}, fmt.Errorf("identity %q: empty host", s)
	}
	i := strings.LastIndex(rest, "/")
	if i <= 0 || i == len(rest)-1 {
		return ID{}, fmt.Errorf("identity %q: want host:owner/repo", s)
	}
	return ID{Host: host, Owner: rest[:i], Name: rest[i+1:]}, nil
}

// String returns the canonical host:owner/repo form.
func (id ID) String() string { return id.Host + ":" + id.Owner + "/" + id.Name }

// OwnerRepo returns owner/repo (the path within a host).
func (id ID) OwnerRepo() string { return id.Owner + "/" + id.Name }

// Short returns the final path segment used for cs/completion.
func (id ID) Short() string { return id.Name }

// Zero reports whether the identity is unset.
func (id ID) Zero() bool { return id == ID{} }
