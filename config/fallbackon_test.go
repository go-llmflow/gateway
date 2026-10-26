package config

import (
	"strings"
	"testing"
)

// #58: fallback_on and saturation_waiting are validated by check-config.
func TestFallbackOn(t *testing.T) {
	with := func(extra string) string {
		return strings.Replace(valid, `model "llama-batch" {`, "model \"llama-batch\" {\n  fallbacks = [\"llama\"]\n"+extra, 1)
	}
	c, err := Parse([]byte(with(`  fallback_on = ["unavailable", "saturated"]`+"\n  saturation_waiting = 3\n")), "t.hcl")
	if err != nil {
		t.Fatal(err)
	}
	var g ModelGroup
	for _, m := range c.ModelGroups {
		if m.Name == "llama-batch" {
			g = m
		}
	}
	if !g.FallsBackOn("saturated") || !g.FallsBackOn("unavailable") || g.SaturationWaiting != 3 {
		t.Fatalf("%+v", g)
	}
	// The default keeps v0.10 behaviour.
	c, err = Parse([]byte(with("")), "t.hcl")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range c.ModelGroups {
		if m.Name == "llama-batch" && (!m.FallsBackOn("unavailable") || m.FallsBackOn("saturated")) {
			t.Fatalf("default: %+v", m)
		}
	}
	for name, tc := range map[string]struct{ src, want string }{
		"unknown reason":        {with(`  fallback_on = ["overloaded"]` + "\n"), "Invalid fallback_on"},
		"repeated reason":       {with(`  fallback_on = ["saturated", "saturated"]` + "\n"), "Invalid fallback_on"},
		"empty":                 {with(`  fallback_on = []` + "\n"), "Invalid fallback_on"},
		"waiting w/o saturated": {with(`  saturation_waiting = 2` + "\n"), "Invalid saturation_waiting"},
		"negative waiting":      {with(`  fallback_on = ["saturated"]` + "\n  saturation_waiting = -1\n"), "Invalid saturation_waiting"},
		"no fallbacks": {strings.Replace(valid, `model "llama-batch" {`, "model \"llama-batch\" {\n  fallback_on = [\"saturated\"]\n", 1),
			"fallback_on without fallbacks"},
	} {
		if _, err := Parse([]byte(tc.src), "t.hcl"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}
