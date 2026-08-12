// Package xdg resolves the XDG base directories `repo` keeps machine-local
// state under.
//
// State is deliberately not $REPO_OUT (DESIGN §6). That directory's contract is
// "generated, disposable, rewritten on every apply" — a record whose whole value
// is that it survives has no business in a directory documented as safe to
// delete and regenerate.
package xdg

import (
	"os"
	"path/filepath"
)

// appDir is the per-application subdirectory under whichever base applies.
const appDir = "repo"

// StateDir is where durable, machine-local records live: $XDG_STATE_HOME/repo,
// else ~/.local/state/repo.
//
// State rather than data because the spec puts "logs" and "history" here, and
// that is what these records are — an account of actions taken, read after the
// fact by whoever is asking what happened.
func StateDir() string {
	return filepath.Join(base("XDG_STATE_HOME", ".local", "state"), appDir)
}

// base returns the directory the named XDG variable points at, else the
// spec's default beneath the home directory.
//
// A relative value is ignored rather than resolved against the working
// directory, which is what the spec requires: honouring it would scatter state
// into whichever directory a sweep happened to start from.
func base(env string, def ...string) string {
	if v := os.Getenv(env); filepath.IsAbs(v) {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Nothing useful is left to resolve against; the caller's MkdirAll will
		// fail and say so with a path in hand, which beats guessing at "/".
		return filepath.Join(def...)
	}
	return filepath.Join(append([]string{home}, def...)...)
}
