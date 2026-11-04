package gateway

import (
	"math"
	"math/big"
	"sort"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
)

// Fixed costs (fluids { fixed_cost }): amounts in the report's currency per
// month that no energy meter sees, such as the hosting of the racks.
//
// Each month's amount, in the currency's minor units (an integer), is split
// between the organisations on the same basis as the idle energy
// (energy.AllocateIdle: by_usage, pro rata to the active energy of the month
// in the cost's pool, a third party included; no usage, the pool's shares;
// neither, "(unallocated)"), then, inside an organisation, over its cells pro
// rata to their active energy. Every step is an integer largest-remainder
// split, so every grouping adds up to the configured amount to the minor unit.
//
// A report over part of a month gets the part of the month's amount that the
// window covers, by time: an organisation's share A of the month is charged
// round(A × elapsed / month length) at the window's end minus the same at its
// start, elapsed being the time since the first of the month (UTC) and the
// rounding half to even. The parts of a month therefore add up, per
// organisation, to its share of the month exactly (the sum telescopes), and a
// whole month is the configured amount. The split between organisations uses
// the whole month's usage, as the idle energy does.

// cumulativeShare returns round_half_even(a × e / l) for 0 ≤ e ≤ l.
func cumulativeShare(a int64, e, l time.Duration) int64 {
	if l <= 0 || e <= 0 || a == 0 {
		return 0
	}
	if e >= l {
		return a
	}
	num := new(big.Int).Mul(big.NewInt(a), big.NewInt(int64(e)))
	q, r := new(big.Int).QuoRem(num, big.NewInt(int64(l)), new(big.Int))
	switch c := new(big.Int).Lsh(r, 1).Cmp(big.NewInt(int64(l))); {
	case c > 0, c == 0 && q.Bit(0) == 1:
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}

// allocateFixedCosts adds the fixed costs of [from, to) to the cells (and
// rows for organisations that have none). basis is allocatedEnergyBasis's.
func allocateFixedCosts(cells []fluidsCell, f *config.Fluids, from, to time.Time, sharesOf func(string) map[string]float64, basis map[string]map[string]map[string]float64) []fluidsCell {
	if f == nil || len(f.FixedCosts) == 0 {
		return cells
	}
	from, to = from.UTC(), to.UTC()
	for ms := time.Date(from.Year(), from.Month(), 1, 0, 0, 0, 0, time.UTC); ms.Before(to); ms = ms.AddDate(0, 1, 0) {
		me := ms.AddDate(0, 1, 0)
		lo, hi := from, to
		if lo.Before(ms) {
			lo = ms
		}
		if hi.After(me) {
			hi = me
		}
		l, e0, e1 := me.Sub(ms), lo.Sub(ms), hi.Sub(ms)
		month := ms.Format("2006-01")
		for _, fc := range f.FixedCosts {
			total := int64(math.RoundToEven(fc.MonthlyFor(month) * math.Pow10(f.CurrencyDecimals)))
			if total <= 0 {
				continue
			}
			var usage map[string]float64
			if bm, ok := basis[month]; ok {
				usage = bm[fc.Pool]
			} else {
				usage = map[string]float64{}
				for _, c := range cells {
					if c.month() == month && (fc.Pool == "" || c.Pool == fc.Pool) {
						usage[c.Org] += float64(c.ActiveMJ)
					}
				}
			}
			alloc, err := energy.AllocateIdle(total, f.IdleEnergy, sharesOf(fc.Pool), usage)
			if err != nil {
				continue // the mode is validated at load
			}
			orgs := make([]string, 0, len(alloc))
			for o := range alloc {
				orgs = append(orgs, o)
			}
			sort.Strings(orgs)
			for _, o := range orgs {
				part := cumulativeShare(alloc[o], e1, l) - cumulativeShare(alloc[o], e0, l)
				if part <= 0 {
					continue
				}
				name := o
				if o == "" {
					name = Unallocated
				}
				var idx []int
				var ws []float64
				for _, narrow := range []bool{fc.Pool != "", false} {
					for i, c := range cells {
						if c.month() == month && c.Org == name && (!narrow || c.Pool == fc.Pool) {
							idx = append(idx, i)
							ws = append(ws, float64(c.ActiveMJ))
						}
					}
					if len(idx) > 0 {
						break
					}
				}
				parts := energy.Apportion(part, ws)
				if parts == nil {
					cells = append(cells, fluidsCell{Day: month + "-01", Consumer: energy.Consumer{Org: name, Pool: fc.Pool}, FixedMinor: part})
					continue
				}
				for j, i := range idx {
					cells[i].FixedMinor += parts[j]
				}
			}
		}
	}
	return cells
}
