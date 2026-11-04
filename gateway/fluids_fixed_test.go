package gateway

import (
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
)

func fixedFluids(mode string, amount float64) *config.Fluids {
	return &config.Fluids{PUE: 1, Currency: "EUR", CurrencyDecimals: 2, IdleEnergy: mode,
		FixedCosts: []config.FixedCost{{Name: "racks", Pool: "b300", Monthly: amount}}}
}

func fixedCells(day string) []fluidsCell {
	c := func(org string, mj int64) fluidsCell {
		return fluidsCell{Day: day, Consumer: energy.Consumer{Org: org, Pool: "b300", User: org + "-u"}, ActiveMJ: mj}
	}
	return []fluidsCell{c("a", 100), c("b", 300), c("tiers", 600)}
}

func fixedByOrg(cells []fluidsCell) (map[string]int64, int64) {
	m := map[string]int64{}
	var n int64
	for _, c := range cells {
		m[c.Org] += c.FixedMinor
		n += c.FixedMinor
	}
	return m, n
}

var (
	oct1  = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	nov1  = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	shAB  = func(string) map[string]float64 { return map[string]float64{"a": 0.5, "b": 0.5, "tiers": 0} }
	noSh  = func(string) map[string]float64 { return nil }
	usage = map[string]map[string]map[string]float64{"2026-10": {"": {"a": 100, "b": 300, "tiers": 600}, "b300": {"a": 100, "b": 300, "tiers": 600}}}
)

// A whole month adds up to the configured amount to the cent, the third party
// pays its part of it, and the split is the usage's (largest remainder).
func TestFixedCostWholeMonthExactAndThirdPartyPays(t *testing.T) {
	f := fixedFluids(energy.IdleByUsage, 1800.01)
	cells := allocateFixedCosts(fixedCells("2026-10-05"), f, oct1, nov1, shAB, nil)
	by, total := fixedByOrg(cells)
	// 180001 minor units over 10 % / 30 % / 60 %: 18000.1, 54000.3, 108000.6;
	// the floors leave 1, which goes to the largest remainder (the third party).
	if total != 180001 || by["a"] != 18000 || by["b"] != 54000 || by["tiers"] != 108001 {
		t.Fatalf("%v total %d", by, total)
	}
	rows := groupFluids(cells, []string{"org"}, f)
	for _, r := range rows {
		if r.Org == "tiers" && (r.FixedCost != 1080.01 || r.TotalCost != 1080.01) {
			t.Errorf("tiers row: %+v", r)
		}
	}
	rows = groupFluids(cells, nil, f)
	if len(rows) != 1 || rows[0].FixedCost != 1800.01 {
		t.Fatalf("total row %+v", rows)
	}
	// by_share: the third party, share 0, pays none of it.
	cells = allocateFixedCosts(fixedCells("2026-10-05"), fixedFluids(energy.IdleByShare, 1800.01), oct1, nov1, shAB, nil)
	by, total = fixedByOrg(cells)
	if total != 180001 || by["tiers"] != 0 {
		t.Fatalf("by_share %v", by)
	}
	// An override for the month replaces the amount; another month does not.
	f.FixedCosts[0].Periods = map[string]float64{"2026-10": 100, "2026-09": 5}
	_, total = fixedByOrg(allocateFixedCosts(fixedCells("2026-10-05"), f, oct1, nov1, shAB, nil))
	if total != 10000 {
		t.Fatalf("period override: %d", total)
	}
}

// Parts of a month add up, per organisation, to the month, and a part is the
// part of the month by time.
func TestFixedCostPartialMonthAdditivity(t *testing.T) {
	f := fixedFluids(energy.IdleByUsage, 1800.01)
	whole, _ := fixedByOrg(allocateFixedCosts(fixedCells("2026-10-05"), f, oct1, nov1, shAB, nil))
	cuts := []time.Time{oct1, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 17, 13, 30, 7, 0, time.UTC), time.Date(2026, 10, 29, 1, 1, 1, 0, time.UTC), nov1}
	sum := map[string]int64{}
	for i := 0; i+1 < len(cuts); i++ {
		part, _ := fixedByOrg(allocateFixedCosts(fixedCells("2026-10-05"), f, cuts[i], cuts[i+1], shAB, usage))
		for o, v := range part {
			sum[o] += v
		}
	}
	for _, o := range []string{"a", "b", "tiers"} {
		if sum[o] != whole[o] {
			t.Errorf("%s: the parts add up to %d, the month is %d", o, sum[o], whole[o])
		}
	}
	// The first 15 of 31 days: 180001 × 15 / 31 = 87097.9 (± 1 per organisation).
	part, n := fixedByOrg(allocateFixedCosts(fixedCells("2026-10-05"), f, oct1, oct1.AddDate(0, 0, 15), shAB, usage))
	if n < 87095 || n > 87101 {
		t.Errorf("15 days of 31: %d (%v)", n, part)
	}
}

// With no usage at all the shares bear the cost; with neither it is
// "(unallocated)", still counted.
func TestFixedCostNoUsageFallsBackToShares(t *testing.T) {
	f := fixedFluids(energy.IdleByUsage, 100)
	none := map[string]map[string]map[string]float64{"2026-10": {}}
	by, total := fixedByOrg(allocateFixedCosts(nil, f, oct1, nov1, shAB, none))
	if total != 10000 || by["a"] != 5000 || by["b"] != 5000 || by[Unallocated] != 0 {
		t.Fatalf("shares fallback: %v", by)
	}
	by, total = fixedByOrg(allocateFixedCosts(nil, f, oct1, nov1, noSh, none))
	if total != 10000 || by[Unallocated] != 10000 {
		t.Fatalf("neither: %v", by)
	}
}

func TestCumulativeShareRoundsHalfToEven(t *testing.T) {
	for _, c := range []struct {
		a, e, l, want int64
	}{{5, 1, 2, 2}, {7, 1, 2, 4}, {3, 1, 4, 1}, {10, 0, 3, 0}, {10, 3, 3, 10}, {1 << 40, 1, 3, 1 << 40 / 3}} {
		if got := cumulativeShare(c.a, time.Duration(c.e), time.Duration(c.l)); got != c.want {
			t.Errorf("cumulativeShare(%d, %d, %d) = %d, want %d", c.a, c.e, c.l, got, c.want)
		}
	}
}
