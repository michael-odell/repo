package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	syncpkg "github.com/michael-odell/repo/internal/sync"
)

// explainVerdict prints the evidence behind one verdict (DESIGN §5.3).
//
// The verdict itself stays terse — `merged (rewritten)` is a claim about what
// is true — and this is where the mechanism is honest to show, because it was
// asked for. The tiers are printed in the order they were tried, including the
// ones that found nothing: "we looked for a replay and there wasn't one" is
// most of what makes the answer believable, and a list of only the successful
// step reads like a conclusion with its working erased.
func explainVerdict(w io.Writer, v syncpkg.Verdict, base string, indent string) {
	e := v.Evidence
	fmt.Fprintf(w, "%s%s vs %s: %d ahead, %d behind\n", indent, v.Name, base, e.Ahead, e.Behind)
	for _, s := range e.Steps {
		mark := " "
		if s.Answered {
			mark = "→"
		}
		fmt.Fprintf(w, "%s  %s %-11s %s\n", indent, mark, s.Tier, s.Found)
	}
	if len(e.Steps) == 0 {
		fmt.Fprintf(w, "%s    (no tier ran: %s)\n", indent, firstNonEmpty(v.Blocker, "no evidence recorded"))
	}

	// The conclusions share the tiers' label column: what was tried and what it
	// came to are one list, and ragging them apart makes the reader find the
	// alignment before they can read either.
	fmt.Fprintf(w, "%s    verdict     %s\n", indent, v.Summary(base))
	if !v.Updated.IsZero() {
		fmt.Fprintf(w, "%s    ref         %s, last moved %s ago\n",
			indent, shortSHA(v.SHA), roughAge(time.Since(v.Updated)))
	}
	// How it would be removed is part of the evidence, not a detail: `-D` is
	// exactly the case where git's own check does not stand behind the decision,
	// and someone endorsing a deletion should be told which of the two they are
	// endorsing (DESIGN §5.3).
	if v.Prunable {
		if syncpkg.NeedsForceDelete(v) {
			fmt.Fprintf(w, "%s    removal     git branch -D (git's own -d check cannot see this merge)\n", indent)
		} else {
			fmt.Fprintf(w, "%s    removal     git branch -d (git agrees independently)\n", indent)
		}
	}
}

// roughAge renders a duration at the resolution these settings are written in.
// It matches internal/sync's rendering deliberately: the age in an explanation
// and the age in a blocker are the same fact and must read the same way.
func roughAge(d time.Duration) string {
	switch {
	case d >= 14*24*time.Hour:
		return fmt.Sprintf("%dw", int(d/(7*24*time.Hour)))
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	default:
		return "moments"
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}
