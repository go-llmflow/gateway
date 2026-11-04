package config

import (
	"strings"
	"testing"
)

func fixedCostSrc(fluids string) []byte {
	return []byte(`
master_key_file = "/unused"
cache_salt_key_file = "/unused"
org "a" {
  share = 1
  team "a" {}
}
pool "b300" {
  capacity = 4
}
model "m" {
  pool     = "b300"
  key_file = "/unused"
  upstream "u" {
    base_url = "http://x"
  }
}
` + fluids)
}

// fc wraps a fixed_cost body in a fluids block, one attribute per line (the
// HCL single-line block form takes one argument only).
func fc(extra, body string) string {
	return "fluids {\n  currency = \"EUR\"\n" + extra + "\n  fixed_cost \"x\" {\n" + body + "\n  }\n}\n"
}

func TestFixedCostValidation(t *testing.T) {
	ok := `fluids {
  currency = "EUR"
  fixed_cost "racks-b300" {
    monthly = 1800
    pool    = "b300"
    period "2026-07" {
      monthly = 1900.5
    }
  }
  fixed_cost "network" {
    monthly = 200
  }
}`
	c, err := Parse(fixedCostSrc(ok), "ok.hcl")
	if err != nil {
		t.Fatal(err)
	}
	got := c.Fluids.FixedCosts
	if len(got) != 2 || got[0].Pool != "b300" || got[0].MonthlyFor("2026-07") != 1900.5 || got[0].MonthlyFor("2026-08") != 1800 || got[1].Pool != "" {
		t.Fatalf("%+v", got)
	}
	for name, tc := range map[string]struct{ src, want string }{
		"no currency":  {"fluids {\n  fixed_cost \"x\" {\n    monthly = 1\n  }\n}\n", "currency"},
		"negative":     {fc("", "    monthly = -1"), "monthly"},
		"huge":         {fc("", "    monthly = 1e12"), "monthly"},
		"decimals":     {fc("", "    monthly = 1.001"), "decimals"},
		"unknown pool": {fc("", "    monthly = 1\n    pool = \"nope\""), "not declared"},
		"duplicate":    {fc("  fixed_cost \"x\" {\n    monthly = 1\n  }", "    monthly = 2"), "twice"},
		"bad month":    {fc("", "    monthly = 1\n    period \"2026-13\" {\n      monthly = 1\n    }"), "YYYY-MM"},
		"dup month":    {fc("", "    monthly = 1\n    period \"2026-07\" {\n      monthly = 1\n    }\n    period \"2026-07\" {\n      monthly = 2\n    }"), "twice"},
	} {
		if _, err := Parse(fixedCostSrc(tc.src), "bad.hcl"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}
}
