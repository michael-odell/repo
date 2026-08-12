package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseAgeUnits(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"14d", 14 * 24 * time.Hour},
		{"2w", 14 * 24 * time.Hour},
		{"48h", 48 * time.Hour},
		{"90s", 90 * time.Second},
		{"0d", 0},
	}
	for _, c := range cases {
		got, err := ParseAge(c.in)
		if err != nil {
			t.Errorf("ParseAge(%q): %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseAge(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestParseAgeRejectsWhatItCannotMean: a setting that silently read "2 weeks"
// as zero would switch off the protection it was written to add, which is the
// one failure mode worth being loud about.
func TestParseAgeRejectsWhatItCannotMean(t *testing.T) {
	for _, in := range []string{"14", "two weeks", "-3d", "-5m", "1.5d", "d"} {
		if got, err := ParseAge(in); err == nil {
			t.Errorf("ParseAge(%q) = %v, want an error", in, got)
		}
	}
}

// TestBadAgeIsReportedAtLoad: the message has to name the setting and show a
// form that works, since the whole point of validating here is that nobody
// finds out during a sweep.
func TestBadAgeIsReportedAtLoad(t *testing.T) {
	dir := t.TempDir()
	frag := filepath.Join(dir, "registry.toml")
	body := "[defaults]\nprune_min_age = \"two weeks\"\n"
	if err := os.WriteFile(frag, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := Load([]string{frag})
	if err != nil {
		t.Fatal(err)
	}
	err = reg.Validate()
	if err == nil {
		t.Fatal("a prune_min_age nothing can read was accepted")
	}
	for _, want := range []string{"prune_min_age", "14d"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
}
