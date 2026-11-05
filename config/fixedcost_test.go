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

// charge and basis of a fixed cost, the network and storage prices and an
// organisation's markup.
func TestBillingTermsValidation(t *testing.T) {
	ok := `fluids {
  currency           = "EUR"
  price_per_gb       = 0.02
  price_per_gb_month = 0.05
  period "2026-12" {
    price_per_gb = 0.03
  }
  fixed_cost "hardware" {
    monthly = 4200
    pool    = "b300"
    charge  = "non_shareholders"
  }
  fixed_cost "storage-array" {
    monthly = 900
    basis   = "storage_gb_month"
  }
}`
	src := string(fixedCostSrc(ok))
	src = strings.Replace(src, "org \"a\" {\n  share = 1\n  team \"a\" {}\n}", "org \"a\" {\n  share = 1\n  team \"a\" {}\n}\norg \"tiers\" {\n  share = 0\n  team \"t\" {}\n  billing {\n    markup = 0.15\n  }\n}", 1)
	c, err := Parse([]byte(src), "ok.hcl")
	if err != nil {
		t.Fatal(err)
	}
	costs := c.Fluids.FixedCosts
	if len(costs) != 2 || costs[0].Charge != ChargeNonShareholders || costs[0].Basis != BasisEnergy || costs[1].Charge != ChargeAll || costs[1].Basis != BasisStorageGBMonth {
		t.Fatalf("%+v", costs)
	}
	gb, gbm := c.Fluids.NetworkStoragePrices("2026-11")
	gb12, gbm12 := c.Fluids.NetworkStoragePrices("2026-12")
	if gb != 0.02 || gbm != 0.05 || gb12 != 0.03 || gbm12 != 0.05 {
		t.Fatalf("prices %v %v / %v %v", gb, gbm, gb12, gbm12)
	}
	if c.MarkupOf("tiers") != 0.15 || c.MarkupOf("a") != 0 || c.MarkupOf("nobody") != 0 {
		t.Fatalf("markups %v %v", c.MarkupOf("tiers"), c.MarkupOf("a"))
	}
	for name, tc := range map[string]struct{ src, want string }{
		"charge":       {fc("", "    monthly = 1\n    charge = \"nobody\""), "Invalid charge"},
		"basis":        {fc("", "    monthly = 1\n    basis = \"vibes\""), "Invalid basis"},
		"gb no cur":    {"fluids {\n  price_per_gb = 1\n}\n", "currency"},
		"gb negative":  {"fluids {\n  currency = \"EUR\"\n  price_per_gb = -1\n}\n", "price_per_gb"},
		"gbm too much": {"fluids {\n  currency = \"EUR\"\n  price_per_gb_month = 1e9\n}\n", "price_per_gb_month"},
	} {
		if _, err := Parse(fixedCostSrc(tc.src), "bad.hcl"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}
	for name, m := range map[string]string{"negative": "-0.1", "huge": "11"} {
		bad := strings.Replace(string(fixedCostSrc("")), "org \"a\" {\n  share = 1\n  team \"a\" {}\n}", "org \"a\" {\n  share = 1\n  team \"a\" {}\n  billing {\n    markup = "+m+"\n  }\n}", 1)
		if _, err := Parse([]byte(bad), "bad.hcl"); err == nil || !strings.Contains(err.Error(), "Invalid markup") {
			t.Errorf("markup %s: %v", name, err)
		}
	}
}
