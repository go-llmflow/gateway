package gateway

import (
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
)

func fixedFluids(mode string, amount float64) *config.Fluids {
	return &config.Fluids{PUE: 1, Currency: "EUR", CurrencyDecimals: 2, IdleEnergy: mode,
		FixedCosts: []config.FixedCost{{Name: "racks", Pool: "b300", Monthly: amount, Charge: config.ChargeAll, Basis: config.BasisEnergy}}}
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

func creditByOrg(cells []fluidsCell) (map[string]int64, int64) {
	m := map[string]int64{}
	var n int64
	for _, c := range cells {
		m[c.Org] += c.CreditMinor
		n += c.CreditMinor
	}
	return m, n
}

var (
	oct1 = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	nov1 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
)

func shAB(string) map[string]float64 { return map[string]float64{"a": 0.5, "b": 0.5, "tiers": 0} }
func noSh(string) map[string]float64 { return nil }

func envOf(sharesOf func(string) map[string]float64, month []fluidsCell) fixedEnv {
	return fixedEnv{
		sharesOf: sharesOf,
		shareOf:  func(pool, org string) float64 { return sharesOf(pool)[org] },
		monthCells: func(string) []fluidsCell {
			return month
		},
	}
}

// A whole month adds up to the configured amount to the cent, the third party
// pays its part of it, and the split is the usage's (largest remainder).
func TestFixedCostWholeMonthExactAndThirdPartyPays(t *testing.T) {
	f := fixedFluids(energy.IdleByUsage, 1800.01)
	cells := allocateFixedCosts(fixedCells("2026-10-05"), f, oct1, nov1, envOf(shAB, nil))
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
	cells = allocateFixedCosts(fixedCells("2026-10-05"), fixedFluids(energy.IdleByShare, 1800.01), oct1, nov1, envOf(shAB, nil))
	by, total = fixedByOrg(cells)
	if total != 180001 || by["tiers"] != 0 {
		t.Fatalf("by_share %v", by)
	}
	// An override for the month replaces the amount; another month does not.
	f.FixedCosts[0].Periods = map[string]float64{"2026-10": 100, "2026-09": 5}
	_, total = fixedByOrg(allocateFixedCosts(fixedCells("2026-10-05"), f, oct1, nov1, envOf(shAB, nil)))
	if total != 10000 {
		t.Fatalf("period override: %d", total)
	}
}

// Parts of a month add up, per organisation, to the month, and a part is the
// part of the month by time.
func TestFixedCostPartialMonthAdditivity(t *testing.T) {
	f := fixedFluids(energy.IdleByUsage, 1800.01)
	whole, _ := fixedByOrg(allocateFixedCosts(fixedCells("2026-10-05"), f, oct1, nov1, envOf(shAB, nil)))
	env := envOf(shAB, fixedCells("2026-10-05"))
	cuts := []time.Time{oct1, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC), time.Date(2026, 10, 17, 13, 30, 7, 0, time.UTC), time.Date(2026, 10, 29, 1, 1, 1, 0, time.UTC), nov1}
	sum := map[string]int64{}
	for i := 0; i+1 < len(cuts); i++ {
		part, _ := fixedByOrg(allocateFixedCosts(fixedCells("2026-10-05"), f, cuts[i], cuts[i+1], env))
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
	part, n := fixedByOrg(allocateFixedCosts(fixedCells("2026-10-05"), f, oct1, oct1.AddDate(0, 0, 15), env))
	if n < 87095 || n > 87101 {
		t.Errorf("15 days of 31: %d (%v)", n, part)
	}
}

// With no usage at all the shares bear the cost; with neither it is
// "(unallocated)", still counted.
func TestFixedCostNoUsageFallsBackToShares(t *testing.T) {
	f := fixedFluids(energy.IdleByUsage, 100)
	by, total := fixedByOrg(allocateFixedCosts(nil, f, oct1, nov1, envOf(shAB, nil)))
	if total != 10000 || by["a"] != 5000 || by["b"] != 5000 || by[Unallocated] != 0 {
		t.Fatalf("shares fallback: %v", by)
	}
	by, total = fixedByOrg(allocateFixedCosts(nil, f, oct1, nov1, envOf(noSh, nil)))
	if total != 10000 || by[Unallocated] != 10000 {
		t.Fatalf("neither: %v", by)
	}
}

// A switch is spread by traffic and a storage array by storage used, not by
// energy.
func TestFixedCostBasisNetworkAndStorage(t *testing.T) {
	mk := func() []fluidsCell {
		cs := fixedCells("2026-10-05") // energy 10 / 30 / 60 %
		cs = append(cs,
			fluidsCell{Day: "2026-10-05", Consumer: energy.Consumer{Org: "a", Pool: "b300"}, NetIn: 300, NetOut: 100}, // a 400
			fluidsCell{Day: "2026-10-05", Consumer: energy.Consumer{Org: "tiers", Pool: "b300"}, NetIn: 600},          // tiers 600
			fluidsCell{Day: "2026-10-05", Consumer: energy.Consumer{Org: "b"}, StorageByteDays: 900},
			fluidsCell{Day: "2026-10-06", Consumer: energy.Consumer{Org: "tiers"}, StorageByteDays: 100})
		return cs
	}
	f := fixedFluids(energy.IdleByUsage, 10)
	f.FixedCosts[0].Basis = config.BasisNetworkBytes
	by, total := fixedByOrg(allocateFixedCosts(mk(), f, oct1, nov1, envOf(shAB, nil)))
	if total != 1000 || by["a"] != 400 || by["tiers"] != 600 || by["b"] != 0 {
		t.Fatalf("network_bytes: %v", by)
	}
	f.FixedCosts[0].Basis = config.BasisStorageGBMonth
	by, total = fixedByOrg(allocateFixedCosts(mk(), f, oct1, nov1, envOf(shAB, nil)))
	if total != 1000 || by["b"] != 900 || by["tiers"] != 100 || by["a"] != 0 {
		t.Fatalf("storage_gb_month: %v", by)
	}
}

// A cost the owners already bore is billed to the non-shareholders only, and
// credited to the owners pro rata to what they lent.
func TestFixedCostNonShareholdersAndOwnersCredit(t *testing.T) {
	f := fixedFluids(energy.IdleByUsage, 100.01)
	f.FixedCosts[0].Charge = config.ChargeNonShareholders
	env := envOf(shAB, nil)
	env.lent = func(pool string, lo, hi time.Time) map[string]map[string]float64 {
		return map[string]map[string]float64{"a": {"tiers": 30}, "b": {"tiers": 10}}
	}
	cells := allocateFixedCosts(fixedCells("2026-10-05"), f, oct1, nov1, env)
	by, total := fixedByOrg(cells)
	if total != 10001 || by["tiers"] != 10001 || by["a"] != 0 || by["b"] != 0 {
		t.Fatalf("billed: %v", by)
	}
	// 10001 over 30 : 10 = 7500.75 and 2500.25; the unit left goes to a.
	cr, rev := creditByOrg(cells)
	if rev != 10001 || cr["a"] != 7501 || cr["b"] != 2500 || cr["tiers"] != 0 {
		t.Fatalf("credits: %v", cr)
	}
	// Nothing lent: the owners' shares.
	env.lent = nil
	cr, rev = creditByOrg(allocateFixedCosts(fixedCells("2026-10-05"), f, oct1, nov1, env))
	if rev != 10001 || cr["a"]+cr["b"] != 10001 || cr["a"] < 5000 || cr["b"] < 5000 {
		t.Fatalf("credits by shares: %v", cr)
	}
	// No non-shareholder worked: nobody to bill, nothing credited.
	cs := fixedCells("2026-10-05")[:2]
	got := allocateFixedCosts(cs, f, oct1, nov1, env)
	if _, n := fixedByOrg(got); n != 0 {
		t.Fatalf("billed %d with no third party", n)
	}
	// The total row: billed + nothing else; the owners' rows carry the credit.
	rows := groupFluids(cells, []string{"org"}, f)
	for _, r := range rows {
		if r.Org == "a" && (r.Credit != 75.01 || r.TotalCost != -75.01) {
			t.Errorf("owner row %+v", r)
		}
	}
}

// Network and storage are priced per GB and GB-month at the month's terms, and
// the organisation's markup applies to its costs, rounded once per row.
func TestMeteredPricesAndMarkup(t *testing.T) {
	f := &config.Fluids{PUE: 1, PricePerKWh: 1, Currency: "EUR", CurrencyDecimals: 2, PricePerGB: 0.5, PricePerGBMonth: 2,
		Periods: map[string]config.FluidsPeriod{"2026-11": {PricePerGB: 1}}}
	cells := []fluidsCell{
		{Day: "2026-10-05", Consumer: energy.Consumer{Org: "tiers"}, ActiveMJ: kWhMJ},         // 1 kWh: 1.00
		{Day: "2026-10-05", Consumer: energy.Consumer{Org: "tiers"}, NetIn: 1e9, NetOut: 1e9}, // 2 GB at 0.5: 1.00
		{Day: "2026-10-05", Consumer: energy.Consumer{Org: "tiers"}, StorageByteDays: 31e9},   // 31 GB-days of 31: 1 GB-month at 2: 2.00
		{Day: "2026-10-05", Consumer: energy.Consumer{Org: "tiers"}, FixedMinor: 10000},       // 100.00
		{Day: "2026-11-05", Consumer: energy.Consumer{Org: "tiers"}, NetIn: 1e9},              // November price: 1 GB at 1: 1.00
		{Day: "2026-10-05", Consumer: energy.Consumer{Org: "etabA"}, ActiveMJ: kWhMJ},
	}
	markup := func(org string) float64 {
		if org == "tiers" {
			return 0.15
		}
		return 0
	}
	rows := groupFluidsM(cells, []string{"org"}, f, markup)
	for _, r := range rows {
		switch r.Org {
		case "tiers":
			// Markup 15 % of (1 + 100 + 1 + 2) in October and of 1 in November = 15.6 + 0.15.
			if r.NetworkCost != 2 || r.StorageCost != 2 || r.NetworkGB != 3 || r.StorageGBMonth != 1 || r.FixedCost != 100 || r.Markup != 15.75 || r.TotalCost != 1+100+2+2+15.75 {
				t.Errorf("tiers: %+v", r)
			}
		case "etabA":
			if r.Markup != 0 || r.TotalCost != 1 {
				t.Errorf("owner: %+v", r)
			}
		}
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
