package energy

import (
	"fmt"
	"math"
	"sort"
)

// Consumer is the finest grain energy is attributed to: the dimensions of a
// request record that a report can group by.
type Consumer struct {
	Org, Lab, Team, Project, User, Model, Pool string
}

// Work is what a consumer's requests did on one upstream in one interval.
// Prefill is the prompt tokens the GPU computed (the prompt minus those read
// from the prefix cache), Decode the tokens it generated.
type Work struct {
	Consumer
	Prefill, Decode int64
}

// Weights give the relative energy of a prefill and of a decode token. They
// are a modelling choice of the agreement, not a measurement: calibrate them
// on the platform's own models (the defaults weigh both the same).
type Weights struct{ Prefill, Decode float64 }

// DefaultWeights weigh a prefill and a decode token the same.
var DefaultWeights = Weights{Prefill: 1, Decode: 1}

// Of returns the weighted work of w.
func (k Weights) Of(w Work) float64 {
	return float64(max(w.Prefill, 0))*k.Prefill + float64(max(w.Decode, 0))*k.Decode
}

// Apportion splits total (millijoules) in proportion to weights so that the
// parts sum to total exactly: each part is the floor of its exact share and
// the remainder goes, one unit at a time, to the largest fractional parts
// (ties to the lowest index). Negative, NaN and infinite weights count as 0;
// nil when no weight is positive or total is not positive.
func Apportion(total int64, weights []float64) []int64 {
	if total <= 0 {
		return nil
	}
	var sum float64
	w := make([]float64, len(weights))
	for i, x := range weights {
		if x > 0 && !math.IsInf(x, 0) && !math.IsNaN(x) {
			w[i] = x
			sum += x
		}
	}
	if sum <= 0 || math.IsInf(sum, 0) {
		return nil
	}
	out := make([]int64, len(w))
	frac := make([]float64, len(w))
	var given int64
	for i, x := range w {
		exact := float64(total) * (x / sum)
		f := math.Floor(exact)
		if f > float64(total) {
			f = float64(total)
		}
		out[i] = int64(f)
		frac[i] = exact - f
		given += out[i]
	}
	idx := make([]int, 0, len(w))
	for i, x := range w {
		if x > 0 {
			idx = append(idx, i)
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return frac[idx[a]] > frac[idx[b]] })
	// Floating point can leave the floors a little over or under: settle it
	// unit by unit, so the sum is exact whatever the weights.
	for rem, n := total-given, 0; rem != 0; n++ {
		i := idx[n%len(idx)]
		if rem > 0 {
			out[i]++
			rem--
		} else if out[i] > 0 {
			out[i]--
			rem++
		}
	}
	return out
}

// Attributed is the active energy of one consumer.
type Attributed struct {
	Consumer
	MJ int64
}

// AttributeActive splits an interval's active energy between the consumers
// that did work in it, by weighted work. Without any work (or active == 0)
// nothing is attributed and the caller books the energy as idle: the second
// result is the energy left unattributed (always active when there was no
// work, 0 otherwise).
func AttributeActive(active int64, work []Work, k Weights) ([]Attributed, int64) {
	ws := make([]float64, len(work))
	for i, w := range work {
		ws[i] = k.Of(w)
	}
	parts := Apportion(active, ws)
	if parts == nil {
		return nil, max(active, 0)
	}
	out := make([]Attributed, 0, len(work))
	for i, p := range parts {
		if p > 0 {
			out = append(out, Attributed{work[i].Consumer, p})
		}
	}
	return out, 0
}

// Idle allocation modes (config fluids.idle_energy).
const (
	IdleByShare = "by_share"
	IdleByUsage = "by_usage"
)

// ValidIdleMode reports whether m is a known mode.
func ValidIdleMode(m string) bool { return m == IdleByShare || m == IdleByUsage }

// AllocateIdle splits idle energy between organisations: pro rata to the
// guaranteed shares (IdleByShare, the default of the agreement) or to what
// each used (IdleByUsage, weights are the orgs' attributed energy or work).
// The parts sum to idle exactly. Idle energy nobody can be charged for (no
// share configured, no usage) stays under the organisation "" so that the
// total remains the measured one.
func AllocateIdle(idle int64, mode string, shares, usage map[string]float64) (map[string]int64, error) {
	if !ValidIdleMode(mode) {
		return nil, fmt.Errorf("energy: idle_energy %q: want %s or %s", mode, IdleByShare, IdleByUsage)
	}
	out := map[string]int64{}
	if idle <= 0 {
		return out, nil
	}
	src := shares
	if mode == IdleByUsage {
		src = usage
	}
	orgs := make([]string, 0, len(src))
	for o := range src {
		orgs = append(orgs, o)
	}
	sort.Strings(orgs)
	ws := make([]float64, len(orgs))
	for i, o := range orgs {
		ws[i] = src[o]
	}
	parts := Apportion(idle, ws)
	if parts == nil {
		out[""] = idle
		return out, nil
	}
	for i, o := range orgs {
		if parts[i] > 0 {
			out[o] = parts[i]
		}
	}
	return out, nil
}

// Params are the price terms of a period.
type Params struct {
	PUE         float64 // total facility energy ÷ IT energy, ≥ 1; 0 means 1
	PricePerKWh float64
	Currency    string
}

// MJPerKWh is the millijoules in a kilowatt-hour (3.6 MJ).
const MJPerKWh = 3_600_000_000

// KWh converts millijoules to kilowatt-hours.
func KWh(mj int64) float64 { return float64(mj) / MJPerKWh }

// Cost is the fluids cost of IT energy mj: kWh_IT × PUE × price per kWh, in
// p.Currency. The PUE covers cooling and distribution losses.
func (p Params) Cost(mj int64) float64 {
	pue := p.PUE
	if pue == 0 {
		pue = 1
	}
	return KWh(mj) * pue * p.PricePerKWh
}
