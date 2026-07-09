package artifact

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/michael-odell/repo/internal/config"
)

func TestPluginsUseFork(t *testing.T) {
	reg, err := config.Load([]string{filepath.Join("..", "config", "testdata", "base.toml")})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := reg.Repos()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := Generate(dir, reg, repos, nil); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "plugins.zsh"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(body)
	// supply-chain-mirror plugin clones from the fork, not upstream.
	if !strings.Contains(got, "git@github.com:michael-odell/powerlevel10k") {
		t.Errorf("plugins.zsh missing fork URL:\n%s", got)
	}
	if strings.Contains(got, "romkatv/powerlevel10k") {
		t.Errorf("plugins.zsh should not reference upstream:\n%s", got)
	}
	// my own plugin (no fork) clones from its identity.
	if !strings.Contains(got, "git@github.com:michael-odell/zsh-history") {
		t.Errorf("plugins.zsh missing own-plugin URL:\n%s", got)
	}
}
