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
	// help is the long-form help shown by `repo <name> --help`: a usage line,
	// then any arguments/flags. Empty falls back to the summary alone.
	help string
	run  func(ctx context.Context, args []string) error
}

// commands is the dispatch table. Most are stubs at Stage 0; each stage fills
// them in (see docs/PLAN.md).
var commands = []command{
	{name: "version", summary: "print version information", run: cmdVersion,
		help: "usage: repo version"},
	{name: "list", summary: "enumerate known repos (for completion)", run: cmdList,
		help: "usage: repo list\n\nPrint every declared repo with its id, workflow, and resolved physical URL."},
	{name: "resolve", summary: "resolve a repo id to its physical URL (debug)", run: cmdResolve,
		help: "usage: repo resolve <id|name>\n\nResolve a logical id (github:owner/repo) or short name to the clone URL this\nmachine would use, applying any [resolve] overlay."},
	{name: "status", summary: "report drift across repos (read-only)", run: cmdStatus,
		help: "usage: repo status\n\nDiscover repos under the roots and report per-repo drift (branch, ahead/behind,\ndirty, not-cloned). Read-only."},
	{name: "apply", summary: "regenerate shell artifacts from the registry", run: cmdApply,
		help: "usage: repo apply\n\nRegenerate the shell navigation/completion artifacts into $REPO_OUT."},
	{name: "clone", summary: "clone a repo into its configured location", run: notImplemented,
		help: "usage: repo clone <url|id>"},
	{name: "scan", summary: "discover on-disk repos", run: cmdScan,
		help: "usage: repo scan\n\nWalk the discovery roots and list every git repo found, with its inferred id,\nworkflow, and tag."},
	{name: "sync", summary: "reconcile repos toward the registry", run: cmdSync,
		help: "usage: repo sync [flags] [repo...]\n\nReconcile the selected repos (all, or by --tag or name) toward the registry.\n\nflags:\n  --tag <tag>    limit to repos with this tag\n  --if-due       only sync repos whose cadence is due\n  --force        ignore cadence\n  -n, --dry-run  show planned actions without changing anything\n  -v, --verbose  explain the decision for every repo"},
	{name: "prune", summary: "prune stale local branches", run: notImplemented,
		help: "usage: repo prune [repo...]"},
	{name: "home", summary: "print the home path of a repo", run: notImplemented,
		help: "usage: repo home <id|name>"},
	{name: "path", summary: "print a path relative to a repo's home", run: notImplemented,
		help: "usage: repo path <id|name> [subpath]"},
	{name: "review", summary: "review pending supply-chain-mirror updates", run: notImplemented,
		help: "usage: repo review"},
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
			if wantsHelp(args[1:]) {
				commandHelp(os.Stdout, c)
				return nil
			}
			return c.run(context.Background(), args[1:])
		}
	}
	usage(os.Stderr)
	return fmt.Errorf("unknown command %q", args[0])
}

// wantsHelp reports whether -h/--help is the first argument to a subcommand.
func wantsHelp(args []string) bool {
	return len(args) > 0 && (args[0] == "-h" || args[0] == "--help")
}

func commandHelp(w io.Writer, c command) {
	fmt.Fprintf(w, "repo %s — %s\n", c.name, c.summary)
	if c.help != "" {
		fmt.Fprintf(w, "\n%s\n", c.help)
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "repo — a personal git repository manager")
	fmt.Fprintln(w, "\nusage: repo <command> [arguments]\n\ncommands:")
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	for _, c := range commands {
		fmt.Fprintf(tw, "  %s\t%s\n", c.name, c.summary)
	}
	tw.Flush()
	fmt.Fprint(w, environmentHelp)
	fmt.Fprintln(w, "\nRun 'repo <command> --help' for command-specific help.")
}

// environmentHelp documents the configuration environment variables. All are
// path-style lists (colon-separated) except REPO_OUT.
const environmentHelp = `
environment:
  REPO_REGISTRY_PATH  registry fragment files/dirs to merge (default ~/.config/repo)
  REPO_ROOTS          directories to scan for repos (default: the registry's home_roots)
  REPO_OUT            where generated shell artifacts are written (default ~/.local/repo)
`

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
