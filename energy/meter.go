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
	// Glitches counts decreases that the next reading showed to be bad
	// readings, not resets: discarded. Capped counts GPUs whose energy was cut
	// to what the device could have drawn (a lying exporter). Both belong in the
	// method/variance report.
	Glitches, Capped int
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

// DefaultMaxWatts is the MaxWatts of a zero Meter: above any single GPU on the
// market (a B300 is about 1.4 kW), so that only an absurd reading is capped.
const DefaultMaxWatts = 3000

// capMargin multiplies the physical bound: a legitimate reading never nears it.
const capMargin = 2

// Meter turns successive readings of one exporter into intervals. It is not
// safe for concurrent use.
//
// A counter is cumulative, so a decrease is never energy: it is a driver reload
// (the counter restarts) or a bad reading. A decrease is NOT taken as a reset on
// sight (the audit of 2026-11-08: one transient 0, then the normal counter, was
// a "reset" followed by the whole lifetime counter billed in one interval).
// It is held as a candidate and judged by the next reading:
//
//   - the counter is back at or above the old baseline: the decrease was a
//     glitch, discarded (Glitches), and the energy since the old baseline is
//     billed, once, bounded;
//   - it is at or above the candidate and still below the old baseline: a real
//     reset, confirmed (Resets); the candidate is the new baseline and the
//     energy since it is billed;
//   - lower again: still a candidate.
//
// While a candidate is pending the GPU adds no energy. Every GPU's energy in an
// interval is capped at MaxWatts x margin x the time since its baseline
// (Capped counts the caps): a lying exporter cannot bill more than the device
// could have drawn. Mirrors go-redfish's Meter, where a decrease is never a
// negative delta and an interval it cannot trust is not billed.
type Meter struct {
	// MaxGap marks an interval longer than it as a Gap; 0 disables the flag.
	MaxGap time.Duration
	// MaxWatts is the most one device can draw; 0 means DefaultMaxWatts.
	MaxWatts float64

	prev  map[string]uint64
	prevT time.Time
	// baseT is when each GPU's baseline was last confirmed; cand holds a
	// decrease awaiting confirmation.
	baseT map[string]time.Time
	cand  map[string]uint64
}

// Observe records a successful scrape. The first one only sets the baseline
// (ok false). A reading that is not after the previous one (clock stepped
// back, a replayed scrape) is ignored.
func (m *Meter) Observe(r Reading) (Interval, bool) {
	if m.prev == nil {
		m.prev, m.prevT = copyMJ(r.MJ), r.At
		m.baseT, m.cand = map[string]time.Time{}, map[string]uint64{}
		for k := range r.MJ {
			m.baseT[k] = r.At
		}
		return Interval{}, false
	}
	if !r.At.After(m.prevT) {
		return Interval{}, false
	}
	maxW := m.MaxWatts
	if maxW <= 0 {
		maxW = DefaultMaxWatts
	}
	iv := Interval{Start: m.prevT, End: r.At, PerGPU: map[string]int64{}}
	for k, cur := range r.MJ {
		old, known := m.prev[k]
		if !known {
			iv.New++
			m.prev[k], m.baseT[k] = cur, r.At
			iv.PerGPU[k] = 0
			continue
		}
		c, pending := m.cand[k]
		from := old // the value the energy is measured from
		switch {
		case cur >= old:
			if pending { // the decrease was a glitch: discarded
				iv.Glitches++
				delete(m.cand, k)
			}
		case pending && cur >= c:
			// Still below the old baseline and consistent with the candidate:
			// the counter really restarted, and the candidate is the baseline.
			iv.Resets++
			delete(m.cand, k)
			from = c
		default:
			// A decrease, or a lower one still: a candidate, nothing billed, the
			// baseline kept.
			m.cand[k] = cur
			iv.PerGPU[k] = 0
			continue
		}
		d := cur - from
		// Bounded by what the device could have drawn since its baseline.
		since := r.At.Sub(m.baseT[k]).Seconds()
		if bound := uint64(maxW * capMargin * since * 1000); d > bound {
			d = bound
			iv.Capped++
		}
		if d > maxDelta {
			d = maxDelta // so sums cannot overflow
		}
		m.prev[k], m.baseT[k] = cur, r.At
		iv.PerGPU[k] = int64(d)
		iv.MJ += int64(d)
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
