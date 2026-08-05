package main

import (
	"bytes"
	"strings"
	"testing"
	"text/tabwriter"
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

// TestColorCellRequiresStripEscapeFlag: colorCell's escape bytes are only
// exempted from tabwriter's *width* math by default — tabwriter.StripEscape
// must also be passed to NewWriter, or those \xff bytes get written to
// output verbatim (invalid standalone UTF-8, renders as garbage in a real
// terminal). This pins the actual sync.go/status.go construction, not just
// colorCell in isolation, since that's the wiring that was missing.
func TestColorCellRequiresStripEscapeFlag(t *testing.T) {
	old := colorEnabled
	colorEnabled = true
	defer func() { colorEnabled = old }()

	cell := colorCell(ansiBoldRed, "✗")

	var withFlag bytes.Buffer
	tw := tabwriter.NewWriter(&withFlag, 0, 4, 2, ' ', tabwriter.StripEscape)
	fprintCell(t, tw, cell)

	if bytes.IndexByte(withFlag.Bytes(), 0xff) >= 0 {
		t.Errorf("output with StripEscape still contains raw \\xff bytes: %q", withFlag.String())
	}
	if !bytes.Contains(withFlag.Bytes(), []byte(ansiBoldRed)) {
		t.Errorf("output with StripEscape lost the ANSI color code: %q", withFlag.String())
	}

	var withoutFlag bytes.Buffer
	tw2 := tabwriter.NewWriter(&withoutFlag, 0, 4, 2, ' ', 0)
	fprintCell(t, tw2, cell)
	if bytes.IndexByte(withoutFlag.Bytes(), 0xff) < 0 {
		t.Errorf("expected raw \\xff bytes without StripEscape (sanity check that the flag is what matters)")
	}
}

func fprintCell(t *testing.T, tw *tabwriter.Writer, cell string) {
	t.Helper()
	if _, err := tw.Write([]byte(cell + "\tname\tdetail\n")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
}
