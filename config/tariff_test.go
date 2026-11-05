package config

import (
	"strings"
	"testing"
	"time"
)

func tariffSrc(fluids string) []byte {
	return fixedCostSrc(fluids)
}

const parisTariff = `fluids {
  currency      = "EUR"
  price_per_kwh = 0.15
  timezone      = "Europe/Paris"
  tariff "peak" {
    price_per_kwh = 0.25
    days          = ["mon", "tue", "wed", "thu", "fri"]
    from          = "07:00"
    to            = "23:00"
  }
  tariff "night" {
    price_per_kwh = 0.08
    from          = "23:00"
    to            = "07:00"
  }
}`

func TestTariffParsesAndPricesTheHours(t *testing.T) {
	c, err := Parse(tariffSrc(parisTariff), "t.hcl")
	if err != nil {
		t.Fatal(err)
	}
	f := c.Fluids
	if f.Location.String() != "Europe/Paris" || len(f.Tariff) != 2 {
		t.Fatalf("%v %+v", f.Location, f.Tariff)
	}
	at := func(s string) (string, float64, bool) {
		ts, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return f.TariffBandAt(ts)
	}
	for _, tc := range []struct {
		utc, band string
		price     float64
	}{
		{"2026-10-14T10:00:00Z", "peak", 0.25},  // Wednesday 12:00 in Paris (CEST)
		{"2026-10-14T21:00:00Z", "night", 0.08}, // 23:00 CEST: the night band wraps midnight
		{"2026-10-14T04:59:00Z", "night", 0.08}, // 06:59 CEST
		{"2026-10-14T05:00:00Z", "peak", 0.25},  // 07:00 CEST
		{"2026-10-17T10:00:00Z", "", 0},         // Saturday 12:00: no band, the default price
		{"2026-12-02T06:00:00Z", "peak", 0.25},  // Wednesday 07:00 CET (UTC+1)
		{"2026-12-02T05:59:00Z", "night", 0.08}, // 06:59 CET
	} {
		band, price, ok := at(tc.utc)
		if band != tc.band || price != tc.price || ok != (tc.band != "") {
			t.Errorf("%s: %q %v %v, want %q %v", tc.utc, band, price, ok, tc.band, tc.price)
		}
	}
	if f.BandPrice("peak", "2026-10") != 0.25 || f.BandPrice("", "2026-10") != 0.15 || f.BandPrice("nope", "2026-10") != 0.15 {
		t.Fatal("BandPrice")
	}
}

// The day the clocks go forward has no 02:00 local hour and the day they go
// back has two: a band on that hour covers 0 and 2 UTC hours of those days.
func TestTariffDaylightSavingDays(t *testing.T) {
	src := strings.Replace(parisTariff, `from          = "23:00"
    to            = "07:00"`, `from          = "02:00"
    to            = "03:00"`, 1)
	c, err := Parse(tariffSrc(src), "t.hcl")
	if err != nil {
		t.Fatal(err)
	}
	count := func(day string) (night int) {
		d, _ := time.Parse("2006-01-02", day)
		for h := 0; h < 24; h++ {
			if b, _, _ := c.Fluids.TariffBandAt(d.Add(time.Duration(h) * time.Hour)); b == "night" {
				night++
			}
		}
		return
	}
	if n := count("2026-03-29"); n != 0 { // CET to CEST: 02:00 does not exist
		t.Errorf("spring forward: %d UTC hours in the 02:00 local band, want 0", n)
	}
	if n := count("2026-10-25"); n != 2 { // CEST to CET: 02:00 happens twice
		t.Errorf("fall back: %d UTC hours in the 02:00 local band, want 2", n)
	}
	if n := count("2026-10-24"); n != 1 {
		t.Errorf("an ordinary day: %d, want 1", n)
	}
	// Every UTC hour of those days is priced exactly once: the band that covers
	// 07:00-23:00 holds 16 hours of each.
	peak := func(day string) (n int) {
		d, _ := time.Parse("2006-01-02", day)
		for h := 0; h < 24; h++ {
			if b, _, _ := c.Fluids.TariffBandAt(d.Add(time.Duration(h) * time.Hour)); b == "peak" {
				n++
			}
		}
		return
	}
	// 2026-10-26 is a Monday: peak 07:00-23:00 local is 16 hours in every season.
	if n := peak("2026-10-26"); n < 15 || n > 17 {
		t.Errorf("peak hours on a Monday: %d", n)
	}
}

func TestTariffValidation(t *testing.T) {
	for name, tc := range map[string]struct{ src, want string }{
		"overlap": {strings.Replace(parisTariff, `from          = "23:00"`, `from          = "20:00"`, 1), "Overlapping"},
		"half hour": {strings.Replace(parisTariff, `"07:00"
    to            = "23:00"`, `"07:30"
    to            = "23:00"`, 1), "whole hour"},
		"zone":  {strings.Replace(parisTariff, "Europe/Paris", "Mars/Olympus", 1), "timezone"},
		"day":   {strings.Replace(parisTariff, `"mon"`, `"lun"`, 1), "weekday"},
		"price": {strings.Replace(parisTariff, "0.25", "-1", 1), "price_per_kwh"},
		"same hour": {strings.Replace(parisTariff, `"23:00"
    to            = "07:00"`, `"07:00"
    to            = "07:00"`, 1), "same hour"},
		"no cur": {strings.Replace(parisTariff, `currency      = "EUR"`, "", 1), "currency"},
	} {
		if _, err := Parse(tariffSrc(tc.src), "bad.hcl"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}
}
