package main

import (
	"os"
	"text/tabwriter"
)

// colorEnabled decides once, at startup, whether glyphs get colored: only
// when stdout is an actual terminal (not piped/redirected), and the user
// hasn't opted out via NO_COLOR or TERM=dumb (both conventional signals).
var colorEnabled = shouldColor()

func shouldColor() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	fi, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&os.ModeCharDevice != 0
}

const (
	ansiReset   = "\x1b[0m"
	ansiGreen   = "\x1b[32m"
	ansiCyan    = "\x1b[36m"
	ansiYellow  = "\x1b[33m"
	ansiMagenta = "\x1b[35m"
	ansiGray    = "\x1b[90m"
	ansiBoldRed = "\x1b[1;31m"
)

// color wraps s in an ANSI color code for a plain (non-tabwriter) line — the
// --verbose report, where fmt.Fprintf writes straight to the terminal.
func color(code, s string) string {
	if !colorEnabled {
		return s
	}
	return code + s + ansiReset
}

// colorCell is color for a cell inside a tabwriter table. The ANSI bytes are
// bracketed in tabwriter.Escape (\xff) so the writer excludes them from its
// column-width calculation — otherwise the invisible escape codes would count
// as visible characters and throw off alignment. The glyph itself stays
// outside the escape brackets, so its width still counts exactly as it did
// uncolored.
//
// The tabwriter.Writer this feeds MUST be constructed with the StripEscape
// flag — without it, tabwriter excludes the escape bytes from width math but
// still writes them to output verbatim, which renders as garbage (the raw
// \xff bytes aren't valid standalone UTF-8).
func colorCell(code, s string) string {
	if !colorEnabled {
		return s
	}
	// string(tabwriter.Escape) would UTF-8-encode the rune (0xC3 0xBF), not
	// give the single raw 0xFF byte tabwriter actually scans for — go through
	// a []byte to get that byte literally.
	esc := string([]byte{tabwriter.Escape})
	return esc + code + esc + s + esc + ansiReset + esc
}
