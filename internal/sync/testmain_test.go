package sync

import (
	"os"
	"testing"
)

// TestMain keeps the corroboration cache out of the real one. Its entries are
// keyed on pairs of shas, so a stray one is harmless in principle — but a test
// suite that writes into somebody's home directory is not a thing to leave to
// principle, and a cache seeded by a previous run is exactly how a test starts
// passing for the wrong reason (DESIGN §5.3).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "repo-test-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
