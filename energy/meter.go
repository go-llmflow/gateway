package energy

import (
	"sort"
	"time"
)

// Interval is the energy a set of GPUs used between two successful scrapes.
type Interval struct {
	Start, End time.Time
	// MJ is the total, and PerGPU its parts, in millijoules; MJ is exactly
	// the sum of PerGPU.
	MJ     int64
	PerGPU map[string]int64
	// Resets counts GPUs whose counter went down (a driver reload): the new
	// value is taken as the energy since the reload.
	Resets int
	// New counts GPUs seen for the first time: they set a baseline and add no
	// energy, since what they used before is unknown.
	New int
	// Missing counts GPUs known before and absent from this scrape: their
	// energy shows up in the interval where they reappear.
	Missing int
	// Gap is true when the interval is longer than the meter's MaxGap: one or
	// more scrapes failed in between. No energy is lost (the counters are
	// cumulative) but it cannot be placed any finer than the interval.
	Gap bool
}

// Seconds is the interval's length.
func (iv Interval) Seconds() float64 { return iv.End.Sub(iv.Start).Seconds() }

const maxDelta = 1 << 50

// Meter turns successive readings of one exporter into intervals. It is not
// safe for concurrent use.
type Meter struct {
	// MaxGap marks an interval longer than it as a Gap; 0 disables the flag.
	MaxGap time.Duration

	prev  map[string]uint64
	prevT time.Time
}

// Observe records a successful scrape. The first one only sets the baseline
// (ok false). A reading that is not after the previous one (clock stepped
// back, a replayed scrape) is ignored.
func (m *Meter) Observe(r Reading) (Interval, bool) {
	if m.prev == nil {
		m.prev, m.prevT = copyMJ(r.MJ), r.At
		return Interval{}, false
	}
	if !r.At.After(m.prevT) {
		return Interval{}, false
	}
	iv := Interval{Start: m.prevT, End: r.At, PerGPU: map[string]int64{}}
	for k, cur := range r.MJ {
		old, known := m.prev[k]
		var d uint64
		switch {
		case !known:
			iv.New++
		case cur >= old:
			d = cur - old
		default:
			iv.Resets++
			d = cur
		}
		if d > maxDelta {
			d = maxDelta // an absurd jump (300 MWh in one interval): bounded, so sums cannot overflow
		}
		iv.PerGPU[k] = int64(d)
		iv.MJ += int64(d)
		m.prev[k] = cur
	}
	for k := range m.prev {
		if _, ok := r.MJ[k]; !ok {
			iv.Missing++
		}
	}
	iv.Gap = m.MaxGap > 0 && iv.End.Sub(iv.Start) > m.MaxGap
	m.prevT = r.At
	return iv, true
}

func copyMJ(in map[string]uint64) map[string]uint64 {
	out := make(map[string]uint64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// SplitIdle separates an interval's energy into the part the GPUs would have
// used doing nothing (idle watts × time, per GPU, at most what they used)
// and the rest. idleWatts 0: everything is active.
func (iv Interval) SplitIdle(idleWatts float64) (active, idle int64) {
	if idleWatts <= 0 {
		return iv.MJ, 0
	}
	floor := int64(idleWatts * iv.Seconds() * 1000) // W·s = J, ×1000 = mJ
	keys := make([]string, 0, len(iv.PerGPU))
	for k := range iv.PerGPU {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		d := iv.PerGPU[k]
		i := min(d, floor)
		idle += i
		active += d - i
	}
	return active, idle
}
