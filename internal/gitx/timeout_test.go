package gitx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeGit puts a stand-in `git` first on PATH. The script hangs forever and
// leaves a background child holding the inherited stdout pipe open — the shape
// of a stalled `fetch`, whose ssh grandchild is what actually blocks and what
// keeps the pipe from closing after the parent is signalled.
func fakeGit(t *testing.T, pidFile string) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nsleep 300 &\necho $! > " + pidFile + "\nsleep 300\n"
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestRunTimesOut: without a deadline a stalled git holds its worker forever,
// and a sweep of six of them stops dead with nothing to report. The invocation
// must come back on its own, as an ordinary error naming what happened.
func TestRunTimesOut(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	fakeGit(t, pidFile)
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
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	fakeGit(t, pidFile)
	// Generous on purpose: the fake has to start a shell and fork a child before
	// the deadline fires, and on a machine already running the rest of the suite
	// that has taken longer than a few hundred milliseconds. The subject here is
	// which processes survive cancellation, not how quickly it happens —
	// TestRunTimesOut covers the timing, and needs nothing to have started.
	t.Setenv(timeoutEnv, "3s")

	if _, err := run(t.TempDir(), "status"); err == nil {
		t.Fatal("want a timeout error, got nil")
	}

	pid, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("fake git never recorded its child: %v", err)
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(pid)), "%d", &n); err != nil {
		t.Fatalf("unreadable child pid %q: %v", pid, err)
	}
	// Signal 0 tests for existence without delivering anything.
	if err := syscall.Kill(n, 0); err == nil {
		t.Errorf("child %d survived the timeout; the process group was not killed", n)
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
