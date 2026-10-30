package config

import (
	"math"
	"regexp"
	"sort"
	"time"

	"github.com/go-llmflow/gateway/energy"
	"github.com/hashicorp/hcl/v2"
)

// Fluids prices the electricity and cooling of the platform and says how
// measured energy is attributed (#97):
//
//	fluids {
//	  pue           = 1.4
//	  price_per_kwh = 0.18
//	  currency      = "EUR"
//	  idle_energy   = "by_share"   # or "by_usage"
//	  period "2026-07" { price_per_kwh = 0.21 }
//	}
//
// The energy itself is scraped from the DCGM exporters named by the
// upstreams' dcgm_url.
type Fluids struct {
	PUE         float64
	PricePerKWh float64
	Currency    string
	// CurrencyDecimals is the currency's minor unit in decimal digits (2 for
	// EUR and USD, 0 for JPY): the chargeback rounds to it, once per row, half
	// to even.
	CurrencyDecimals int
	// IdleEnergy is energy.IdleByShare (default: pro rata to the guaranteed
	// shares) or energy.IdleByUsage.
	IdleEnergy string
	// WPrefill and WDecode weigh a prefill and a decode token in the split of
	// active energy between requests.
	WPrefill, WDecode float64
	// IdleWatts is the power of a GPU that serves nothing; energy up to it is
	// idle, the rest active. 0: all the energy of an interval with work is
	// active.
	IdleWatts float64
	// Interval is the scrape period; Settle how long an interval waits before
	// its energy is attributed, so that the accounting of its requests has
	// reached the store.
	Interval, Settle time.Duration
	// Periods override PUE and price for one month ("2026-07"), whatever the
	// energy was measured: the tariff changes, the PUE is re-measured.
	Periods map[string]FluidsPeriod
}

// FluidsPeriod is a per-month override; a zero field keeps the block's value.
type FluidsPeriod struct {
	PUE, PricePerKWh float64
}

// Fluids defaults and bounds.
const (
	DefaultFluidsInterval = 15 * time.Second
	DefaultFluidsSettle   = time.Minute
	MaxPUE                = 5
	MaxPricePerKWh        = 100
	MaxWeight             = 1e6
	MaxIdleWatts          = 2000
)

// Params returns the price terms of the month "YYYY-MM".
func (f *Fluids) Params(month string) energy.Params {
	p := energy.Params{PUE: f.PUE, PricePerKWh: f.PricePerKWh, Currency: f.Currency}
	if o, ok := f.Periods[month]; ok {
		if o.PUE != 0 {
			p.PUE = o.PUE
		}
		if o.PricePerKWh != 0 {
			p.PricePerKWh = o.PricePerKWh
		}
	}
	return p
}

// Weights returns the token weights of the attribution.
func (f *Fluids) Weights() energy.Weights {
	return energy.Weights{Prefill: f.WPrefill, Decode: f.WDecode}
}

type fluidsHCL struct {
	PUE            *float64          `hcl:"pue,optional"`
	PUERange       hcl.Range         `hcl:"pue,attr_value_range"`
	Price          *float64          `hcl:"price_per_kwh,optional"`
	PriceRange     hcl.Range         `hcl:"price_per_kwh,attr_value_range"`
	Currency       *string           `hcl:"currency,optional"`
	CurrencyRange  hcl.Range         `hcl:"currency,attr_value_range"`
	Decimals       *int              `hcl:"currency_decimals,optional"`
	DecimalsRange  hcl.Range         `hcl:"currency_decimals,attr_value_range"`
	IdleEnergy     *string           `hcl:"idle_energy,optional"`
	IdleRange      hcl.Range         `hcl:"idle_energy,attr_value_range"`
	WPrefill       *float64          `hcl:"w_prefill,optional"`
	WPrefillRange  hcl.Range         `hcl:"w_prefill,attr_value_range"`
	WDecode        *float64          `hcl:"w_decode,optional"`
	WDecodeRange   hcl.Range         `hcl:"w_decode,attr_value_range"`
	IdleWatts      *float64          `hcl:"idle_watts,optional"`
	IdleWattsRange hcl.Range         `hcl:"idle_watts,attr_value_range"`
	Interval       *string           `hcl:"interval,optional"`
	IntervalRange  hcl.Range         `hcl:"interval,attr_value_range"`
	Settle         *string           `hcl:"settle,optional"`
	SettleRange    hcl.Range         `hcl:"settle,attr_value_range"`
	Periods        []fluidsPeriodHCL `hcl:"period,block"`
	DefRange       hcl.Range         `hcl:",def_range"`
}

type fluidsPeriodHCL struct {
	Month      string    `hcl:"month,label"`
	MonthRange hcl.Range `hcl:"month,label_range"`
	PUE        *float64  `hcl:"pue,optional"`
	PUERange   hcl.Range `hcl:"pue,attr_value_range"`
	Price      *float64  `hcl:"price_per_kwh,optional"`
	PriceRange hcl.Range `hcl:"price_per_kwh,attr_value_range"`
}

var (
	monthRE    = regexp.MustCompile(`^20[0-9]{2}-(0[1-9]|1[0-2])$`)
	currencyRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{1,15}$`)
)

// fluids validates the fluids block and the upstreams' dcgm_url.
func (v *validator) fluids(raw *fileHCL, c *Config) {
	// dcgm_url of each upstream: the rules of base_url.
	for i, m := range raw.Models {
		if i >= len(c.ModelGroups) {
			break
		}
		g := &c.ModelGroups[i]
		for j, u := range m.Upstreams {
			if u.DcgmURL == nil || j >= len(g.Upstreams) {
				continue
			}
			if err := energy.ValidateURL(*u.DcgmURL); err != nil {
				v.errf(u.DcgmURLRange, "Invalid dcgm_url", "%v", err)
				continue
			}
			g.Upstreams[j].DcgmURL = *u.DcgmURL
		}
	}
	f := raw.Fluids
	if f == nil {
		return
	}
	out := &Fluids{
		PUE: 1, CurrencyDecimals: 2, IdleEnergy: energy.IdleByShare, WPrefill: energy.DefaultWeights.Prefill, WDecode: energy.DefaultWeights.Decode,
		Interval: DefaultFluidsInterval, Settle: DefaultFluidsSettle, Periods: map[string]FluidsPeriod{},
	}
	pos := func(p *float64, r hcl.Range, name string, lo, hi float64) float64 {
		if p == nil {
			return 0
		}
		if math.IsNaN(*p) || *p < lo || *p > hi {
			v.errf(r, "Invalid "+name, "%s = %v: want %v..%v", name, *p, lo, hi)
		}
		return *p
	}
	if f.PUE != nil {
		out.PUE = pos(f.PUE, f.PUERange, "pue", 1, MaxPUE) // a facility never uses less than its IT load
	}
	out.PricePerKWh = pos(f.Price, f.PriceRange, "price_per_kwh", 0, MaxPricePerKWh)
	out.Currency = strOr(f.Currency, "")
	if out.Currency != "" && !currencyRE.MatchString(out.Currency) || out.Currency == "gpu_hours" || out.Currency == "kwh" {
		v.errf(f.CurrencyRange, "Invalid currency", "currency = %q: want 2 to 16 letters, digits or _ (EUR, USD), not a unit of the reports", out.Currency)
	}
	if out.PricePerKWh > 0 && out.Currency == "" {
		v.errf(f.DefRange, "Missing currency", "price_per_kwh is set: name its currency (currency = \"EUR\"); a price without a unit cannot be billed")
	}
	if f.Decimals != nil {
		if *f.Decimals < 0 || *f.Decimals > 4 {
			v.errf(f.DecimalsRange, "Invalid currency_decimals", "currency_decimals = %d: want 0..4 (2 for EUR and USD)", *f.Decimals)
		}
		out.CurrencyDecimals = *f.Decimals
	}
	out.IdleEnergy = strOr(f.IdleEnergy, energy.IdleByShare)
	if !energy.ValidIdleMode(out.IdleEnergy) {
		v.errf(f.IdleRange, "Invalid idle_energy", "idle_energy = %q: want %q or %q", out.IdleEnergy, energy.IdleByShare, energy.IdleByUsage)
	}
	if f.WPrefill != nil {
		out.WPrefill = pos(f.WPrefill, f.WPrefillRange, "w_prefill", 0, MaxWeight)
	}
	if f.WDecode != nil {
		out.WDecode = pos(f.WDecode, f.WDecodeRange, "w_decode", 0, MaxWeight)
	}
	if out.WPrefill == 0 && out.WDecode == 0 {
		v.errf(f.DefRange, "Invalid weights", "w_prefill and w_decode are both 0: no work could be weighed")
	}
	out.IdleWatts = pos(f.IdleWatts, f.IdleWattsRange, "idle_watts", 0, MaxIdleWatts)
	out.Interval = v.dur(f.Interval, f.IntervalRange, "interval", DefaultFluidsInterval)
	if out.Interval < time.Second || out.Interval > 10*time.Minute {
		v.errf(f.IntervalRange, "Invalid interval", "interval = %s: want 1s..10m", out.Interval)
	}
	out.Settle = v.dur(f.Settle, f.SettleRange, "settle", DefaultFluidsSettle)
	if out.Settle < 0 || out.Settle > time.Hour {
		v.errf(f.SettleRange, "Invalid settle", "settle = %s: want 0..1h", out.Settle)
	}
	seen := map[string]bool{}
	for _, p := range f.Periods {
		if !monthRE.MatchString(p.Month) {
			v.errf(p.MonthRange, "Invalid period", "period %q: want a month, YYYY-MM", p.Month)
			continue
		}
		if seen[p.Month] {
			v.errf(p.MonthRange, "Duplicate period", "period %q is declared twice", p.Month)
		}
		seen[p.Month] = true
		o := FluidsPeriod{PUE: pos(p.PUE, p.PUERange, "pue", 1, MaxPUE), PricePerKWh: pos(p.Price, p.PriceRange, "price_per_kwh", 0, MaxPricePerKWh)}
		if o.PricePerKWh > 0 && out.Currency == "" {
			v.errf(p.PriceRange, "Missing currency", "a period price needs fluids.currency")
		}
		out.Periods[p.Month] = o
	}
	c.Fluids = out
}

// FluidsMonths lists the months with an override, sorted.
func (f *Fluids) FluidsMonths() []string {
	ms := make([]string, 0, len(f.Periods))
	for m := range f.Periods {
		ms = append(ms, m)
	}
	sort.Strings(ms)
	return ms
}
