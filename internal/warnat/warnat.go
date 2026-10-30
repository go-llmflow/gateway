// Package warnat is the one definition of a soft quota's warning thresholds
// (#96): which lists are valid, how a form or a flag spells one, which level
// of the configuration wins, and when a threshold is armed again.
//
// A list is "warn at these percentages of a window": 1 to 5 integers from 1
// to 99, strictly increasing. nil means "not set at this level" (the level
// above applies); a non-nil empty list means "no warnings".
package warnat

import (
	"fmt"
	"strconv"
	"strings"
)

// Bounds of a list.
const (
	MaxLevels = 5
	Min       = 1
	Max       = 99
	// MaxInput bounds a spelled list (form field, flag, API string).
	MaxInput = 64
)

// Default is Anthropic's pair: Claude Code warns once utilization passes 75%
// and again past 95% (claude-apps-gateway-spend-limits, "Usage warnings in
// Claude Code"). A copy is returned: callers may not alter the default.
func Default() []int { return []int{75, 95} }

// Validate checks a list. nil and the empty list are valid.
func Validate(a []int) error {
	if len(a) > MaxLevels {
		return fmt.Errorf("warn_at: at most %d thresholds, got %d", MaxLevels, len(a))
	}
	prev := 0
	for i, v := range a {
		if v < Min || v > Max {
			return fmt.Errorf("warn_at[%d] = %d: want an integer from %d to %d", i, v, Min, Max)
		}
		if v <= prev {
			return fmt.Errorf("warn_at[%d] = %d: want a strictly increasing list (after %d)", i, v, prev)
		}
		prev = v
	}
	return nil
}

// Parse reads a spelled list: "" inherits (nil, set false); "off", "none" or
// "[]" turns warnings off (empty, set true); otherwise integers separated by
// commas or spaces, optionally in brackets: "80,95", "[50 75 95]".
func Parse(s string) (levels []int, set bool, err error) {
	if len(s) > MaxInput {
		return nil, false, fmt.Errorf("warn_at: at most %d characters", MaxInput)
	}
	t := strings.TrimSpace(s)
	switch strings.ToLower(t) {
	case "":
		return nil, false, nil
	case "off", "none", "[]":
		return []int{}, true, nil
	}
	t = strings.TrimSuffix(strings.TrimPrefix(t, "["), "]")
	fields := strings.FieldsFunc(t, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	if len(fields) == 0 {
		return nil, false, fmt.Errorf("warn_at %q: want integers such as 75,95, or off", s)
	}
	if len(fields) > MaxLevels {
		return nil, false, fmt.Errorf("warn_at: at most %d thresholds, got %d", MaxLevels, len(fields))
	}
	out := make([]int, 0, len(fields))
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil {
			return nil, false, fmt.Errorf("warn_at %q: %q is not an integer", s, f)
		}
		out = append(out, n)
	}
	if err := Validate(out); err != nil {
		return nil, false, err
	}
	return out, true, nil
}

// Format spells a list as Parse reads it back ("off" for the empty list).
func Format(a []int) string {
	if a == nil {
		return ""
	}
	if len(a) == 0 {
		return "off"
	}
	parts := make([]string, len(a))
	for i, v := range a {
		parts[i] = strconv.Itoa(v)
	}
	return strings.Join(parts, ",")
}

// Resolve picks the level in force. Pass the levels from the most specific
// to the most general (budget, profile, organisation, gateway): the first
// that is set wins, and the default applies when none is.
func Resolve(mostSpecificFirst ...[]int) []int {
	for _, l := range mostSpecificFirst {
		if l != nil {
			return l
		}
	}
	return Default()
}

// Hysteresis is how far below a threshold a rolling window's usage must fall
// before the threshold is armed again: 5 points, or half the threshold when
// that is smaller (so a threshold of 1 re-arms below 1).
func Hysteresis(th int) int { return min(5, th/2) }

// RearmBelow is the usage under which a fired threshold of a rolling window
// is armed again.
func RearmBelow(th int) int { return th - Hysteresis(th) }

// Crossed returns the thresholds of levels that pct has reached, ascending.
func Crossed(levels []int, pct float64) []int {
	var out []int
	for _, l := range levels {
		if pct >= float64(l) {
			out = append(out, l)
		}
	}
	return out
}
