package ident

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in                 string
		host, owner, name  string
		wantErr            bool
	}{
		{in: "github:romkatv/powerlevel10k", host: "github", owner: "romkatv", name: "powerlevel10k"},
		{in: "ghe:cban-ops/pt-helm", host: "ghe", owner: "cban-ops", name: "pt-helm"},
		{in: "ghe:group/sub/repo", host: "ghe", owner: "group/sub", name: "repo"},
		{in: "no-colon/repo", wantErr: true},
		{in: "github:owner", wantErr: true},
		{in: "github:owner/", wantErr: true},
		{in: ":owner/repo", wantErr: true},
		{in: "github:/repo", wantErr: true},
	}
	for _, c := range cases {
		id, err := Parse(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("Parse(%q): want error, got %+v", c.in, id)
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q): unexpected error: %v", c.in, err)
			continue
		}
		if id.Host != c.host || id.Owner != c.owner || id.Name != c.name {
			t.Errorf("Parse(%q) = %+v, want {%s %s %s}", c.in, id, c.host, c.owner, c.name)
		}
		if got := id.String(); got != c.in {
			t.Errorf("String() round-trip = %q, want %q", got, c.in)
		}
	}
}
