package xdg

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStateDirHonoursTheEnvironment(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/var/lib/xdg-test")
	if got, want := StateDir(), filepath.Join("/var/lib/xdg-test", "repo"); got != want {
		t.Errorf("StateDir() = %s, want %s", got, want)
	}
}

func TestStateDirDefaultsBeneathHome(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "")
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory to resolve against")
	}
	if got, want := StateDir(), filepath.Join(home, ".local", "state", "repo"); got != want {
		t.Errorf("StateDir() = %s, want %s", got, want)
	}
}

// TestARelativeStateHomeIsIgnored: the spec says so, and the reason is real —
// honouring it would put the journal wherever a sweep happened to be started
// from, which is how a record becomes several records nobody can find.
func TestARelativeStateHomeIsIgnored(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "relative/state")
	if got := StateDir(); !filepath.IsAbs(got) {
		t.Errorf("StateDir() = %s, want an absolute path", got)
	}
}
