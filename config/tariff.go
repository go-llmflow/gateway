package config

import (
	"fmt"
	"math"
	"strings"
	"time"
	_ "time/tzdata" // zone names work on a host without a tz database

	"github.com/hashicorp/hcl/v2"
)

// Time-of-use tariff (fluids { timezone, tariff }). Energy is stored by hour
// (UTC), and each hour is priced at the band its local start falls in:
//
//	fluids {
//	  timezone      = "Europe/Paris"
//	  price_per_kwh = 0.15          # the hours no band covers
//	  tariff "peak" {
//	    price_per_kwh = 0.25
//	    days          = ["mon", "tue", "wed", "thu", "fri"]
//	    from          = "07:00"
//	    to            = "23:00"
//	  }
//	}
//
// A band covers the local hours from..to (to exclusive, "24:00" allowed, from
// after to wraps past midnight), on the listed days (all when omitted), at
// whole hours. Local time follows the zone's rules, so on the day the clocks
// go forward there are 23 local hours and on the day they go back 25: the
// 02:00 hour that happens twice is priced twice. Bands may not overlap. The
// `period` blocks override the price of the hours no band covers, not the
// bands'. The Redfish and DCGM intervals are attributed to the hour their
// start falls in, as they are to the day.

// TariffBand is one time-of-use band.
type TariffBand struct {
	Name  string
	Price float64
	// Days are the local weekdays covered; empty: every day.
	Days []time.Weekday
	// From and To are local hours, 0..24 (To exclusive); From > To wraps.
	From, To int
}

type tariffHCL struct {
	Name       string    `hcl:"name,label"`
	NameRange  hcl.Range `hcl:"name,label_range"`
	Price      float64   `hcl:"price_per_kwh"`
	PriceRange hcl.Range `hcl:"price_per_kwh,attr_value_range"`
	Days       []string  `hcl:"days,optional"`
	DaysRange  hcl.Range `hcl:"days,attr_value_range"`
	From       string    `hcl:"from"`
	FromRange  hcl.Range `hcl:"from,attr_value_range"`
	To         string    `hcl:"to"`
	ToRange    hcl.Range `hcl:"to,attr_value_range"`
}

var weekdays = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

// WeekdayOf returns the weekday of a three-letter name (sun..sat); Sunday for
// anything else.
func WeekdayOf(s string) time.Weekday { return weekdays[strings.ToLower(s)] }

// parseHour parses "HH:00" (0..24) into hours.
func parseHour(s string) (int, error) {
	var h, m int
	var rest string
	if n, _ := fmt.Sscanf(s, "%d:%d%s", &h, &m, &rest); n != 2 || len(s) != 5 || s[2] != ':' {
		return 0, fmt.Errorf("%q: want HH:00", s)
	}
	if m != 0 || h < 0 || h > 24 {
		return 0, fmt.Errorf("%q: want a whole hour, 00:00 to 24:00", s)
	}
	return h, nil
}

// Covers reports whether the band covers a local weekday and hour.
func (b TariffBand) Covers(day time.Weekday, hour int) bool {
	if len(b.Days) > 0 {
		ok := false
		for _, d := range b.Days {
			ok = ok || d == day
		}
		if !ok {
			return false
		}
	}
	if b.From <= b.To {
		return hour >= b.From && hour < b.To
	}
	return hour >= b.From || hour < b.To
}

// TariffBandAt returns the band an instant falls in and its price; ok false
// when no band covers it (the default price applies) or no tariff is set.
func (f *Fluids) TariffBandAt(t time.Time) (name string, price float64, ok bool) {
	if len(f.Tariff) == 0 {
		return "", 0, false
	}
	l := t.In(f.Location)
	for _, b := range f.Tariff {
		if b.Covers(l.Weekday(), l.Hour()) {
			return b.Name, b.Price, true
		}
	}
	return "", 0, false
}

// BandPrice is the price per kWh of a band by name; "" or an unknown name: the
// month's default price.
func (f *Fluids) BandPrice(name, month string) float64 {
	for _, b := range f.Tariff {
		if b.Name == name && name != "" {
			return b.Price
		}
	}
	return f.Params(month).PricePerKWh
}

// tariff validates the timezone and the bands.
func (v *validator) tariff(f *fluidsHCL, out *Fluids) {
	out.Location = time.UTC
	if f.Timezone != nil {
		loc, err := time.LoadLocation(*f.Timezone)
		if err != nil || *f.Timezone == "" || strings.EqualFold(*f.Timezone, "local") {
			v.errf(f.TimezoneRange, "Invalid timezone", "timezone = %q: want an IANA zone name such as Europe/Paris", *f.Timezone)
		} else {
			out.Location = loc
		}
	}
	if len(f.Tariffs) == 0 {
		return
	}
	if out.Currency == "" {
		v.errf(f.DefRange, "Missing currency", "tariff prices an energy: name its currency (currency = \"EUR\")")
	}
	seen := map[string]bool{}
	var owner [7][24]string
	for _, t := range f.Tariffs {
		if t.Name == "" || seen[t.Name] {
			v.errf(t.NameRange, "Invalid tariff", "tariff %q: empty or declared twice", t.Name)
			continue
		}
		seen[t.Name] = true
		if math.IsNaN(t.Price) || t.Price < 0 || t.Price > MaxPricePerKWh {
			v.errf(t.PriceRange, "Invalid price_per_kwh", "tariff %q: price_per_kwh = %v: want 0..%v", t.Name, t.Price, float64(MaxPricePerKWh))
			continue
		}
		from, err1 := parseHour(t.From)
		to, err2 := parseHour(t.To)
		if err1 != nil {
			v.errf(t.FromRange, "Invalid from", "tariff %q from: %v", t.Name, err1)
		}
		if err2 != nil {
			v.errf(t.ToRange, "Invalid to", "tariff %q to: %v", t.Name, err2)
		}
		if err1 != nil || err2 != nil {
			continue
		}
		if from == 24 {
			v.errf(t.FromRange, "Invalid from", "tariff %q from = \"24:00\": a band starts before 24:00", t.Name)
			continue
		}
		if from == to {
			v.errf(t.FromRange, "Invalid band", "tariff %q: from and to are the same hour", t.Name)
			continue
		}
		b := TariffBand{Name: t.Name, Price: t.Price, From: from, To: to}
		dup := map[time.Weekday]bool{}
		for _, d := range t.Days {
			wd, ok := weekdays[strings.ToLower(d)]
			if !ok || dup[wd] {
				v.errf(t.DaysRange, "Invalid days", "tariff %q days: %q is not a weekday (sun, mon, tue, wed, thu, fri, sat) or is repeated", t.Name, d)
				continue
			}
			dup[wd] = true
			b.Days = append(b.Days, wd)
		}
		for wd := time.Sunday; wd <= time.Saturday; wd++ {
			for h := 0; h < 24; h++ {
				if !b.Covers(wd, h) {
					continue
				}
				if o := owner[wd][h]; o != "" {
					v.errf(t.FromRange, "Overlapping tariffs", "tariff %q and %q both cover %s %02d:00", t.Name, o, wd, h)
					wd, h = time.Saturday, 24
					break
				}
				owner[wd][h] = t.Name
			}
		}
		out.Tariff = append(out.Tariff, b)
	}
}
