package ident

import (
	"fmt"
	"strings"
)

// ParseRemoteURL extracts the hostname and owner/repo path from a git remote
// URL, handling scp-like (git@host:owner/repo.git), ssh:// and https:// forms.
// This is the inverse of resolution and mirrors the ghlink parser.
func ParseRemoteURL(raw string) (hostname, ownerRepo string, err error) {
	s := strings.TrimSuffix(strings.TrimSpace(raw), ".git")
	switch {
	case strings.HasPrefix(s, "git@"), !strings.Contains(s, "://") && strings.Contains(s, ":"):
		// scp-like: [user@]host:owner/repo
		if at := strings.Index(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		host, path, ok := strings.Cut(s, ":")
		if !ok || host == "" || path == "" {
			return "", "", fmt.Errorf("unrecognised remote %q", raw)
		}
		return host, strings.TrimPrefix(path, "/"), nil
	case strings.Contains(s, "://"):
		// scheme://[user@]host[:port]/owner/repo
		s = s[strings.Index(s, "://")+3:]
		if at := strings.Index(s, "@"); at >= 0 {
			s = s[at+1:]
		}
		host, path, ok := strings.Cut(s, "/")
		if !ok || host == "" || path == "" {
			return "", "", fmt.Errorf("unrecognised remote %q", raw)
		}
		if colon := strings.Index(host, ":"); colon >= 0 {
			host = host[:colon] // strip port
		}
		return host, path, nil
	default:
		return "", "", fmt.Errorf("unrecognised remote %q", raw)
	}
}
