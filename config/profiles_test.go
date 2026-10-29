package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// ml spreads the blocks written on one line over several: HCL allows one
// attribute only in a one-line block (the issue's example could not parse).
func ml(s string) string {
	return strings.NewReplacer("{ ", "{\n", " }", "\n}", "  ", "\n").Replace(s)
}

var profileBlocks = ml(`
workday { gpu_hours = 0.8 }
profile "q" { days = 0.25  session_share = 0.25 }
profile "h" { days = 0.5  session_share = 0.25 }
profile "1d" { label = "Un jour par semaine"  days = 1  session_share = 0.5  overflow_cap = 2 }
profile "2d" { days = 2  session_share = 0.5 }
profile "7d" { days = 7  session_share = 1 }
default_profile = "q"
session_window = "4h"
`)

func TestProfilesParse(t *testing.T) {
	// One attribute per line inside a block: HCL refuses several on a
	// one-line block.
	if _, err := Parse([]byte(valid+`profile "x" { days = 1  session_share = 0.5 }`), "p.hcl"); err == nil {
		t.Fatal("HCL accepted a one-line block with two attributes")
	}
	c, err := Parse([]byte(valid+profileBlocks), "p.hcl")
	if err != nil {
		t.Fatal(err)
	}
	p := c.Profiles
	if !p.Enabled() || len(p.List) != 5 || p.Default != "q" || p.SessionWindow != 4*time.Hour || p.Workday[""] != 0.8 {
		t.Fatalf("%+v", p)
	}
	for id, want := range map[string]string{"q": "¼ day / week", "h": "½ day / week", "1d": "Un jour par semaine", "2d": "2 days / week", "7d": "7 days / week"} {
		if got, _ := p.Get(id); got.Label != want {
			t.Errorf("%s: label %q, want %q", id, got.Label, want)
		}
	}
	if x, _ := p.Get("1d"); x.OverflowCap != 2 || x.SessionShare != 0.5 || x.Days != 1 {
		t.Fatalf("%+v", x)
	}
	// Without profiles: nothing, and the default window.
	c, err = Parse([]byte(valid), "p.hcl")
	if err != nil || c.Profiles.Enabled() || c.Profiles.SessionWindow != DefaultSessionWindow {
		t.Fatalf("%+v %v", c.Profiles, err)
	}
}

func TestDaysLabel(t *testing.T) {
	for d, want := range map[float64]string{0: "0 days / week", 0.25: "¼ day / week", 0.5: "½ day / week", 0.75: "¾ day / week",
		1: "1 day / week", 1.5: "1.5 days / week", 3: "3 days / week", 7: "7 days / week"} {
		if got := DaysLabel(d); got != want {
			t.Errorf("%v: %q, want %q", d, got, want)
		}
	}
}

func TestProfilesInvalid(t *testing.T) {
	var many strings.Builder
	for i := 0; i <= MaxProfiles; i++ {
		fmt.Fprintf(&many, "profile \"p%d\" { days = 1  session_share = 0.5 }\n", i)
	}
	for name, src := range map[string]string{
		"days above 7":         `profile "x" { days = 7.5  session_share = 0.5 }`,
		"negative days":        `profile "x" { days = -1  session_share = 0.5 }`,
		"session share 0":      `profile "x" { days = 1  session_share = 0 }`,
		"session share 2":      `profile "x" { days = 1  session_share = 2 }`,
		"overflow cap":         `profile "x" { days = 1  session_share = 0.5  overflow_cap = 1000 }`,
		"negative overflow":    `profile "x" { days = 1  session_share = 0.5  overflow_cap = -1 }`,
		"bad id":               `profile "X y" { days = 1  session_share = 0.5 }`,
		"duplicate":            `profile "x" { days = 1  session_share = 0.5 }` + "\n" + `profile "x" { days = 2  session_share = 0.5 }`,
		"label control":        `profile "x" { label = "a\u0007b"  days = 1  session_share = 0.5 }`,
		"label empty":          `profile "x" { label = " "  days = 1  session_share = 0.5 }`,
		"label too long":       `profile "x" { label = "` + strings.Repeat("a", 65) + `"  days = 1  session_share = 0.5 }`,
		"unknown default":      `profile "x" { days = 1  session_share = 0.5 }` + "\ndefault_profile = \"y\"",
		"window not 5m":        `profile "x" { days = 1  session_share = 0.5 }` + "\nsession_window = \"7m\"",
		"window too long":      `profile "x" { days = 1  session_share = 0.5 }` + "\nsession_window = \"25h\"",
		"workday unit unused":  `profile "x" { days = 1  session_share = 0.5 }` + "\nworkday { EUR = 5 }",
		"workday zero":         `profile "x" { days = 1  session_share = 0.5 }` + "\nworkday { gpu_hours = 0 }",
		"workday huge":         `profile "x" { days = 1  session_share = 0.5 }` + "\nworkday { gpu_hours = 1e10 }",
		"workday string":       `profile "x" { days = 1  session_share = 0.5 }` + "\nworkday { gpu_hours = \"1\" }",
		"workday function":     `profile "x" { days = 1  session_share = 0.5 }` + "\nworkday { gpu_hours = max(1, 2) }",
		"workday nested block": `profile "x" { days = 1  session_share = 0.5 }` + "\nworkday {\n gpu { x = 1 }\n}",
		"workday no profile":   "workday { gpu_hours = 1 }",
		"too many":             many.String(),
	} {
		if _, err := Parse([]byte(valid+ml(src)), "p.hcl"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// The control: the same shape with valid values parses, so each case
	// above fails for its value, not for its syntax.
	if _, err := Parse([]byte(valid+ml(`profile "x" { label = "ok"  days = 1  session_share = 0.5  overflow_cap = 2 }`+
		"\ndefault_profile = \"x\"\nsession_window = \"10m\"\nworkday { gpu_hours = 1 }")), "p.hcl"); err != nil {
		t.Fatal(err)
	}
}

// FuzzProfileHCL: whatever the profile blocks, a configuration that parses
// holds bounded profiles only.
func FuzzProfileHCL(f *testing.F) {
	f.Add(profileBlocks)
	f.Add(`profile "x" { days = 1e308  session_share = 1e-308 }`)
	f.Add(`workday { gpu_hours = 0.8  gpu_hours = 1 }` + "\nprofile \"x\" { days = 1  session_share = 0.5 }")
	f.Add(`profile "x" { days = 0  session_share = 1  overflow_cap = 217 }` + "\ndefault_profile = \"x\"\nsession_window = \"24h\"")
	f.Fuzz(func(t *testing.T, extra string) {
		c, err := Parse([]byte(valid+extra), "fuzz.hcl")
		if err != nil {
			return
		}
		p := c.Profiles
		if len(p.List) > MaxProfiles || p.SessionWindow < 5*time.Minute || p.SessionWindow > 24*time.Hour || p.SessionWindow%(5*time.Minute) != 0 {
			t.Fatalf("%+v", p)
		}
		if p.Default != "" {
			if _, ok := p.Get(p.Default); !ok {
				t.Fatalf("default %q unknown", p.Default)
			}
		}
		ids := map[string]bool{}
		for _, x := range p.List {
			if !validProfileID(x.ID) || ids[x.ID] || !validLabel(x.Label) || !(x.Days >= 0 && x.Days <= 7) ||
				!(x.SessionShare > 0 && x.SessionShare <= 1) || !(x.OverflowCap >= 0 && x.OverflowCap <= MaxOverflowCap) {
				t.Fatalf("unbounded profile %+v", x)
			}
			ids[x.ID] = true
		}
		for u, w := range p.Workday {
			if !(w > 0 && w <= MaxWorkday) {
				t.Fatalf("workday %q = %v", u, w)
			}
		}
	})
}
