package gitx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGit puts a stand-in `git` first on PATH. The script hangs forever and
// leaves a background child holding the inherited stdout pipe open — the shape
// of a stalled `fetch`, whose ssh grandchild is what actually blocks and what
// keeps the pipe from closing after the parent is signalled. The child appends
// to tickFile while it lives, which is how the test tells whether it is still
// running: a pid is not enough, because a killed child that has not yet been
// reaped is a zombie, and signalling a zombie succeeds.
func fakeGit(t *testing.T, tickFile string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\n" +
		"( while : ; do echo tick >> " + tickFile + "; sleep 0.05; done ) &\n" +
		"sleep 300\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// TestRunTimesOut: without a deadline a stalled git holds its worker forever,
// and a sweep of six of them stops dead with nothing to report. The invocation
// must come back on its own, as an ordinary error naming what happened.
func TestRunTimesOut(t *testing.T) {
	fakeGit(t, filepath.Join(t.TempDir(), "tick"))
	t.Setenv(timeoutEnv, "300ms")

	start := time.Now()
	_, err := run(t.TempDir(), "status")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error = %q, want it to say it timed out", err)
	}
	if !strings.Contains(err.Error(), timeoutEnv) {
		t.Errorf("error = %q, want it to name %s so the limit can be raised", err, timeoutEnv)
	}
	// The deadline plus WaitDelay, with room for a slow machine — the point is
	// that it returns at all rather than blocking on the child's pipe.
	if elapsed > 20*time.Second {
		t.Errorf("took %s to give up on a 300ms timeout", elapsed)
	}
}

// TestRunKillsTheProcessGroup: git's children are what block, and they do not
// die with it. Cancelling only the parent would leave the ssh (or index-pack)
// behind, still holding the pipe and still doing whatever stalled.
func TestRunKillsTheProcessGroup(t *testing.T) {
	tick := filepath.Join(t.TempDir(), "tick")
	fakeGit(t, tick)
	// Generous on purpose: the fake has to start a shell and fork a child before
	// the deadline fires, and on a machine already running the rest of the suite
	// that has taken longer than a few hundred milliseconds. The subject here is
	// which processes survive cancellation, not how quickly it happens —
	// TestRunTimesOut covers the timing, and needs nothing to have started.
	t.Setenv(timeoutEnv, "3s")

	if _, err := run(t.TempDir(), "status"); err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	if fileSize(t, tick) == 0 {
		t.Fatal("fake git's child never ran; the fixture proves nothing")
	}

	// Whether the child is *working* is the question, and the only one a pid
	// can't answer. Wait for the ticking to stop rather than assuming how
	// quickly it should: a fixed sleep here is the same class of timing
	// assumption that made the previous version of this test flaky, and the
	// contract is that the group dies, not that it dies within some number of
	// milliseconds of a loaded runner's scheduler getting round to it.
	//
	// A child that genuinely survived never settles, so it runs this loop out
	// and fails the check below — tolerating a slow death costs nothing.
	settled := fileSize(t, tick)
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
		if n := fileSize(t, tick); n == settled {
			break
		} else {
			settled = n
		}
	}

	before := fileSize(t, tick)
	time.Sleep(600 * time.Millisecond)
	if after := fileSize(t, tick); after != before {
		t.Errorf("child was still running after the timeout (%d → %d bytes); "+
			"the process group was not killed", before, after)
	}
}

// TestNetworkTimeoutIsSeparate: a first clone of a large repo can legitimately
// run for minutes, so the commands that cross the network get their own, longer
// budget — a local query taking that long means something else is wrong.
func TestNetworkTimeoutIsSeparate(t *testing.T) {
	if got, want := timeoutFor([]string{"fetch"}), defaultNetworkTimeout; got != want {
		t.Errorf("fetch timeout = %s, want %s", got, want)
	}
	if got, want := timeoutFor([]string{"rev-list"}), defaultTimeout; got != want {
		t.Errorf("rev-list timeout = %s, want %s", got, want)
	}
	if got, want := timeoutEnvFor([]string{"clone"}), networkTimeoutEnv; got != want {
		t.Errorf("clone timeout env = %s, want %s", got, want)
	}

	t.Setenv(networkTimeoutEnv, "45s")
	if got, want := timeoutFor([]string{"fetch"}), 45*time.Second; got != want {
		t.Errorf("overridden fetch timeout = %s, want %s", got, want)
	}
	// A typo must not read as "no timeout".
	t.Setenv(networkTimeoutEnv, "banana")
	if got, want := timeoutFor([]string{"fetch"}), defaultNetworkTimeout; got != want {
		t.Errorf("unparseable override gave %s, want the default %s", got, want)
	}
}
