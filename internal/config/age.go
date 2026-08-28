package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseAge reads a duration in the units branch ages are actually discussed in.
//
// Go's own syntax stops at hours, which is the wrong scale for this question
// entirely: nobody protects a branch for "336h", they protect it for two weeks.
// So `d` and `w` are added on top of what time.ParseDuration accepts, and the
// Go forms keep working for anyone who wants them (and for the tests, where
// seconds are the practical unit).
//
// Days here are 24 hours flat. Wall-clock days vary with daylight saving, but
// this measures "how long since this ref moved" against a threshold someone
// picked by feel — a branch is not meaningfully more or less prunable for the
// hour the clocks shifted, and pretending to that precision would be a lie
// about what the number means.
func ParseAge(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if n, unit, ok := splitAge(s); ok {
		if n < 0 {
			return 0, fmt.Errorf("%q: a negative age has nothing to mean here", s)
		}
		switch unit {
		case "d":
			return time.Duration(n) * 24 * time.Hour, nil
		case "w":
			return time.Duration(n) * 7 * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q: want a duration like \"14d\", \"2w\", or \"48h\"", s)
	}
	if d < 0 {
		return 0, fmt.Errorf("%q: a negative age has nothing to mean here", s)
	}
	return d, nil
}

// splitAge peels a whole-number count off a "14d"/"2w" form. Fractions are left
// to time.ParseDuration to reject: "1.5d" is a way of saying something this
// setting has no use for.
func splitAge(s string) (n int, unit string, ok bool) {
	if len(s) < 2 {
		return 0, "", false
	}
	unit = s[len(s)-1:]
	if unit != "d" && unit != "w" {
		return 0, "", false
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	return n, unit, err == nil
}
