package config

import (
	"strings"
	"testing"
	"time"
)

func TestParseFluids(t *testing.T) {
	c, err := parse(valid)
	if err != nil || c.Fluids != nil {
		t.Fatalf("no block: %v %v", c.Fluids, err)
	}
	src := strings.Replace(valid, `upstream "gpu01" { base_url = "http://10.0.0.1:8000" }`,
		`upstream "gpu01" {
    base_url = "http://10.0.0.1:8000"
    dcgm_url = "http://10.0.0.1:9400/metrics"
  }`, 1) + `
fluids {
  pue           = 1.4
  price_per_kwh = 0.18
  currency      = "EUR"
  idle_energy   = "by_usage"
  w_decode      = 3
  idle_watts    = 60
  interval      = "30s"
  period "2026-07" { price_per_kwh = 0.21 }
}
`
	c, err = parse(src)
	if err != nil {
		t.Fatal(err)
	}
	f := c.Fluids
	if f == nil || f.PUE != 1.4 || f.IdleEnergy != "by_usage" || f.WPrefill != 1 || f.WDecode != 3 || f.Interval != 30*time.Second || f.Settle != time.Minute {
		t.Fatalf("%+v", f)
	}
	if p := f.Params("2026-07"); p.PricePerKWh != 0.21 || p.PUE != 1.4 || p.Currency != "EUR" {
		t.Fatalf("%+v", p)
	}
	if p := f.Params("2026-08"); p.PricePerKWh != 0.18 {
		t.Fatalf("%+v", p)
	}
	if got := c.ModelGroups[0].Upstreams[0].DcgmURL; got != "http://10.0.0.1:9400/metrics" {
		t.Fatalf("dcgm_url %q", got)
	}
}

func TestParseFluidsErrors(t *testing.T) {
	for _, c := range []struct{ name, src, want string }{
		{"pue below one", valid + `fluids { pue = 0.9 }`, "Invalid pue"},
		{"pue huge", valid + `fluids { pue = 50 }`, "Invalid pue"},
		{"price negative", valid + `fluids { price_per_kwh = -1, currency = "EUR" }`, "Invalid price_per_kwh"},
		{"price without currency", valid + `fluids { price_per_kwh = 0.2 }`, "Missing currency"},
		{"currency is a report unit", valid + `fluids { currency = "gpu_hours" }`, "Invalid currency"},
		{"currency garbage", valid + `fluids { currency = "E U R" }`, "Invalid currency"},
		{"idle mode", valid + `fluids { idle_energy = "by_luck" }`, "Invalid idle_energy"},
		{"weights zero", valid + `fluids { w_prefill = 0, w_decode = 0 }`, "Invalid weights"},
		{"interval", valid + `fluids { interval = "0s" }`, "Invalid interval"},
		{"period label", valid + `fluids { period "July" { pue = 1.2 } }`, "Invalid period"},
		{"period twice", valid + `fluids { period "2026-07" { pue = 1.2 }
 period "2026-07" { pue = 1.3 } }`, "Duplicate period"},
		{"dcgm credentials", strings.Replace(valid, `upstream "gpu01" { base_url = "http://10.0.0.1:8000" }`,
			`upstream "gpu01" { base_url = "http://10.0.0.1:8000"
 dcgm_url = "http://u:p@10.0.0.1:9400/metrics" }`, 1), "Invalid dcgm_url"},
		{"dcgm scheme", strings.Replace(valid, `upstream "gpu01" { base_url = "http://10.0.0.1:8000" }`,
			`upstream "gpu01" { base_url = "http://10.0.0.1:8000"
 dcgm_url = "file:///etc/passwd" }`, 1), "Invalid dcgm_url"},
	} {
		_, err := parse(c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", c.name, err, c.want)
		}
	}
}

func FuzzParseFluids(f *testing.F) {
	f.Add(`fluids { pue = 1.4 price_per_kwh = 0.18 currency = "EUR" period "2026-07" { pue = 1.1 } }`)
	f.Add(`fluids { idle_energy = "by_share" interval = "5s" }`)
	f.Fuzz(func(t *testing.T, tail string) {
		c, err := parse(valid + "\n" + tail)
		if err == nil && c.Fluids != nil {
			p := c.Fluids.Params("2026-07")
			if p.PUE < 0 || p.PricePerKWh < 0 {
				t.Fatalf("negative terms %+v", p)
			}
		}
	})
}
