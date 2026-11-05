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
// month that no energy meter sees: the hosting of the racks, a storage array,
// a switch, the depreciation of the hardware, maintenance, licences, staff.
//
// Each month's amount, in the currency's minor units (an integer), is split
// between the organisations on the cost's basis (energy: the idle-energy rule,
// by_usage on active energy with the shares as fallback; shares; network_bytes;
// storage_gb_month), and with charge = "non_shareholders" among the
// organisations that hold no share of the cost's pool only. Inside an
// organisation it goes over its cells pro rata to their active energy. Every
// step is an integer largest-remainder split, so every grouping adds up to the
// configured amount to the minor unit. Neither usage nor shares: the amount is
// "(unallocated)", except for charge = "non_shareholders", where nobody to bill
// leaves the cost with the owners.
//
// A report over part of a month gets the part of the month's amount that the
// window covers, by time: an organisation's share A of the month is charged
// round(A × elapsed / month length) at the window's end minus the same at its
// start, elapsed being the time since the first of the month (UTC) and the
// rounding half to even. The parts of a month therefore add up, per
// organisation, to its share of the month exactly (the sum telescopes), and a
// whole month is the configured amount. The split between organisations uses
// the whole month's usage, as the idle energy does.
//
// What a non-shareholder pays for a cost the owners already bore is credited to
// the owners, pro rata to the GPU-hours they lent it (the lending matrix of the
// window; the owners' shares of the pool when nothing was lent).

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

// fixedEnv is what the allocation needs from the rest of the gateway.
type fixedEnv struct {
	sharesOf func(pool string) map[string]float64
	// shareOf is an organisation's share of a pool.
	shareOf func(pool, org string) float64
	// monthCells returns the cells of a whole month (energy, network, storage),
	// the weights of a window that does not hold it; nil: the window's cells.
	monthCells func(month string) []fluidsCell
	// lent returns the lending matrix [lender][borrower] in GPU-hours over a
	// window of a pool ("" all); nil: no evidence.
	lent func(pool string, from, to time.Time) map[string]map[string]float64
}

// basisUsage sums the weight of each organisation on a basis, in a month, for
// a pool ("" all pools; storage is not per pool).
func basisUsage(cells []fluidsCell, month, pool, basis string) map[string]float64 {
	u := map[string]float64{}
	for _, c := range cells {
		if c.month() != month {
			continue
		}
		switch basis {
		case config.BasisNetworkBytes:
			if pool == "" || c.Pool == pool {
				u[c.Org] += float64(c.NetIn + c.NetOut)
			}
		case config.BasisStorageGBMonth:
			u[c.Org] += float64(c.StorageByteDays)
		default:
			if pool == "" || c.Pool == pool {
				u[c.Org] += float64(c.ActiveMJ)
			}
		}
	}
	return u
}

// allocateFixedCosts adds the fixed costs of [from, to) to the cells (and rows
// for organisations that have none).
func allocateFixedCosts(cells []fluidsCell, f *config.Fluids, from, to time.Time, env fixedEnv) []fluidsCell {
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
		whole := !from.After(ms) && !to.Before(me)
		var monthCells []fluidsCell
		for _, fc := range f.FixedCosts {
			total := int64(math.RoundToEven(fc.MonthlyFor(month) * math.Pow10(f.CurrencyDecimals)))
			if total <= 0 {
				continue
			}
			if monthCells == nil {
				monthCells = cells
				if !whole && env.monthCells != nil {
					monthCells = env.monthCells(month)
				}
			}
			mode := f.IdleEnergy
			switch fc.Basis {
			case config.BasisShares:
				mode = energy.IdleByShare
			case config.BasisNetworkBytes, config.BasisStorageGBMonth:
				mode = energy.IdleByUsage
			}
			shares := env.sharesOf(fc.Pool)
			usage := basisUsage(monthCells, month, fc.Pool, fc.Basis)
			nonOwners := fc.Charge == config.ChargeNonShareholders
			if nonOwners {
				shares, usage = onlyNonShareholders(shares, env, fc.Pool), onlyNonShareholders(usage, env, fc.Pool)
			}
			alloc, err := energy.AllocateIdle(total, mode, shares, usage)
			if err != nil {
				continue // the mode is validated at load
			}
			if nonOwners {
				delete(alloc, "") // nobody to bill: the cost stays with the owners
			}
			orgs := make([]string, 0, len(alloc))
			for o := range alloc {
				orgs = append(orgs, o)
			}
			sort.Strings(orgs)
			revenue := map[string]int64{}
			for _, o := range orgs {
				part := cumulativeShare(alloc[o], e1, l) - cumulativeShare(alloc[o], e0, l)
				if part <= 0 {
					continue
				}
				name := o
				if o == "" {
					name = Unallocated
				}
				if nonOwners {
					revenue[name] += part
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
			cells = creditOwners(cells, revenue, env, fc.Pool, month, lo, hi)
		}
	}
	return cells
}

// onlyNonShareholders keeps the organisations with no share of the pool.
func onlyNonShareholders(m map[string]float64, env fixedEnv, pool string) map[string]float64 {
	out := map[string]float64{}
	for o, v := range m {
		if env.shareOf(pool, o) <= 0 {
			out[o] = v
		}
	}
	return out
}

// creditOwners credits what non-shareholders paid to the owners: pro rata to
// the GPU-hours each lent that borrower, else to the pool's shares.
func creditOwners(cells []fluidsCell, revenue map[string]int64, env fixedEnv, pool, month string, lo, hi time.Time) []fluidsCell {
	if len(revenue) == 0 {
		return cells
	}
	var matrix map[string]map[string]float64
	if env.lent != nil {
		matrix = env.lent(pool, lo, hi)
	}
	borrowers := make([]string, 0, len(revenue))
	for b := range revenue {
		borrowers = append(borrowers, b)
	}
	sort.Strings(borrowers)
	for _, b := range borrowers {
		weights := map[string]float64{}
		for lender, row := range matrix {
			if h := row[b]; h > 0 {
				weights[lender] = h
			}
		}
		if len(weights) == 0 {
			for o, s := range env.sharesOf(pool) {
				if s > 0 {
					weights[o] = s
				}
			}
		}
		owners := make([]string, 0, len(weights))
		for o := range weights {
			owners = append(owners, o)
		}
		sort.Strings(owners)
		ws := make([]float64, len(owners))
		for i, o := range owners {
			ws[i] = weights[o]
		}
		parts := energy.Apportion(revenue[b], ws)
		for i, o := range owners {
			if parts != nil && parts[i] > 0 {
				cells = append(cells, fluidsCell{Day: month + "-01", Consumer: energy.Consumer{Org: o, Pool: pool}, CreditMinor: parts[i]})
			}
		}
	}
	return cells
}
