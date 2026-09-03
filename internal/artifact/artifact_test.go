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
	if _, err := Generate(dir, reg, repos); err != nil {
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

// TestGenerateReplacesRatherThanRewrites: these files are sourced by every new
// shell, and the run that rewrites them (`repo sync`, DESIGN §5.5) is launched
// from shell startup itself — so a reader is most likely to be mid-source at
// exactly the moment of the write. A truncate-and-write can hand that reader
// half a file; a rename cannot.
//
// The check is a hard link, which makes the distinction observable without
// racing anything: after a rename, the link still names the *old* inode and so
// still holds the old content, exactly as an already-open reader would. A
// rewrite in place would show the new content through the link instead.
func TestGenerateReplacesRatherThanRewrites(t *testing.T) {
	reg, err := config.Load([]string{filepath.Join("..", "config", "testdata", "base.toml")})
	if err != nil {
		t.Fatal(err)
	}
	repos, err := reg.Repos()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if _, err := Generate(dir, reg, repos); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "homes.zsh")
	before, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "reader-holds-this")
	if err := os.Link(target, link); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}

	// Regenerate from a smaller union, so the new content genuinely differs.
	if _, err := Generate(dir, reg, repos[:1]); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) == string(before) {
		t.Fatal("fixture problem: regeneration produced identical content, so this proves nothing")
	}
	held, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}
	if string(held) != string(before) {
		t.Error("the file was rewritten in place: a shell already reading it would have seen the write")
	}

	// And nothing is left lying around in the output directory.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}
