package main

import (
	"strings"
	"testing"
)

// TestColorDisabledIsPassthrough: with colorEnabled false (the non-tty
// default, e.g. output piped to a file), both helpers must return s
// unchanged — no stray ANSI/escape bytes leaking into redirected output.
func TestColorDisabledIsPassthrough(t *testing.T) {
	old := colorEnabled
	colorEnabled = false
	defer func() { colorEnabled = old }()

	if got := color(ansiBoldRed, "✗"); got != "✗" {
		t.Errorf("color() = %q, want %q unchanged", got, "✗")
	}
	if got := colorCell(ansiBoldRed, "✗"); got != "✗" {
		t.Errorf("colorCell() = %q, want %q unchanged", got, "✗")
	}
}

// TestColorEnabledWrapsWithReset: with colorEnabled true, color() brackets s
// with the given code and a reset.
func TestColorEnabledWrapsWithReset(t *testing.T) {
	old := colorEnabled
	colorEnabled = true
	defer func() { colorEnabled = old }()

	got := color(ansiBoldRed, "✗")
	if want := ansiBoldRed + "✗" + ansiReset; got != want {
		t.Errorf("color() = %q, want %q", got, want)
	}
}

// TestColorCellEscapesANSIForTabwriter: colorCell must bracket only the ANSI
// bytes in tabwriter.Escape (so tabwriter excludes them from column-width
// math), leaving the glyph itself outside the escape markers so its width
// still counts normally.
func TestColorCellEscapesANSIForTabwriter(t *testing.T) {
	old := colorEnabled
	colorEnabled = true
	defer func() { colorEnabled = old }()

	got := colorCell(ansiBoldRed, "✗")
	const esc = "\xff"
	want := esc + ansiBoldRed + esc + "✗" + esc + ansiReset + esc
	if got != want {
		t.Errorf("colorCell() = %q, want %q", got, want)
	}
	// Exactly two escape-delimited spans, glyph itself unescaped.
	if n := strings.Count(got, esc); n != 4 {
		t.Errorf("colorCell() has %d escape bytes, want 4 (two open/close pairs)", n)
	}
}
