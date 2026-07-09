// Command repo is a personal git repository manager. See docs/DESIGN.md.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"text/tabwriter"
)

// version is overridden at release time via -ldflags "-X main.version=...".
var version = "dev"

type command struct {
	name    string
	summary string
	run     func(ctx context.Context, args []string) error
}

// commands is the dispatch table. Most are stubs at Stage 0; each stage fills
// them in (see docs/PLAN.md).
var commands = []command{
	{"version", "print version information", cmdVersion},
	{"list", "enumerate known repos (for completion)", cmdList},
	{"resolve", "resolve a repo id to its physical URL (debug)", cmdResolve},
	{"status", "report drift across repos (read-only)", notImplemented},
	{"apply", "regenerate shell artifacts from the registry", notImplemented},
	{"clone", "clone a repo into its configured location", notImplemented},
	{"scan", "discover on-disk repos", notImplemented},
	{"sync", "reconcile repos toward the registry", notImplemented},
	{"prune", "prune stale local branches", notImplemented},
	{"home", "print the home path of a repo", notImplemented},
	{"path", "print a path relative to a repo's home", notImplemented},
	{"review", "review pending supply-chain-mirror updates", notImplemented},
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "repo:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no command given")
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(os.Stdout)
		return nil
	}
	for _, c := range commands {
		if c.name == args[0] {
			return c.run(context.Background(), args[1:])
		}
	}
	usage(os.Stderr)
	return fmt.Errorf("unknown command %q", args[0])
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "repo — a personal git repository manager")
	fmt.Fprintln(w, "\nusage: repo <command> [arguments]\n\ncommands:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	tw.Flush()
}

func cmdVersion(_ context.Context, _ []string) error {
	fmt.Printf("repo %s", version)
	if rev := vcsRevision(); rev != "" {
		fmt.Printf(" (%s)", rev)
	}
	fmt.Println()
	return nil
}

func vcsRevision() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" {
			if len(s.Value) > 12 {
				return s.Value[:12]
			}
			return s.Value
		}
	}
	return ""
}

func notImplemented(_ context.Context, _ []string) error {
	return errors.New("not implemented yet")
}
