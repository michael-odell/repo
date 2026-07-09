package ident

import "testing"

func TestParseRemoteURL(t *testing.T) {
	cases := []struct {
		in, host, path string
		wantErr        bool
	}{
		{in: "git@github.com:michael-odell/etc.git", host: "github.com", path: "michael-odell/etc"},
		{in: "git@github.com:michael-odell/etc", host: "github.com", path: "michael-odell/etc"},
		{in: "saascm-gogs.onbmc.com:modell/colo", host: "saascm-gogs.onbmc.com", path: "modell/colo"},
		{in: "https://github.com/owner/repo.git", host: "github.com", path: "owner/repo"},
		{in: "https://ghe.example.com:8443/group/sub/repo", host: "ghe.example.com", path: "group/sub/repo"},
		{in: "ssh://git@github.com/owner/repo", host: "github.com", path: "owner/repo"},
		{in: "not a url", wantErr: true},
	}
	for _, c := range cases {
		host, path, err := ParseRemoteURL(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseRemoteURL(%q): want error, got %s/%s", c.in, host, path)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseRemoteURL(%q): %v", c.in, err)
			continue
		}
		if host != c.host || path != c.path {
			t.Errorf("ParseRemoteURL(%q) = %q,%q; want %q,%q", c.in, host, path, c.host, c.path)
		}
	}
}
