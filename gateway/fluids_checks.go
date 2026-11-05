package gateway

import (
	"context"
	"time"

	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

// Two things the fluids report shows that change no bill:
//
//   - the sub-meter variance: a monthly reading of the racks' sub-meter
//     (fluids { meter "2026-10" { kwh } }) against the IT energy measured for
//     the month (the GPUs' and the nodes' beyond them), as a difference. It is
//     a check on the measurement, never used to rescale it;
//   - the preemptions: the energy of the intervals during which vLLM preempted
//     requests, per month and pool. The recompute a preemption causes is not
//     billed to anyone; this says whether it is negligible.
//
// Both describe the platform, not an organisation: the proxy roles and the
// finance viewer read them.

// MeterVarianceRow compares one sub-meter reading with the measured energy.
type MeterVarianceRow struct {
	Month string `json:"month"`
	Pool  string `json:"pool,omitempty"`
	// MeterKWh is the sub-meter's reading, MeasuredKWh the IT energy measured
	// for the same month (and pool): GPUs plus the nodes' overhead.
	MeterKWh    float64 `json:"meter_kwh"`
	MeasuredKWh float64 `json:"measured_it_kwh"`
	// DifferenceKWh is meter minus measured; DifferencePct that difference as a
	// percentage of the measured energy (absent when nothing was measured).
	DifferenceKWh float64  `json:"difference_kwh"`
	DifferencePct *float64 `json:"difference_pct,omitempty"`
}

// meterVariance builds the rows for the whole months of the view.
func (s *Server) meterVariance(view *billingView, p *principal) []MeterVarianceRow {
	f := s.cfg.Fluids
	if f == nil || view == nil || !(p.reportView()) {
		return nil
	}
	whole := map[string]bool{}
	for _, sp := range view.spans {
		if wholeMonth(sp) {
			whole[monthKey(sp[0])] = true
		}
	}
	var out []MeterVarianceRow
	for _, m := range f.Meters {
		if !whole[m.Month] {
			continue
		}
		var mj int64
		for _, c := range view.cells {
			if c.month() == m.Month && (m.Pool == "" || c.Pool == m.Pool) {
				mj += c.ActiveMJ + c.IdleMJ
			}
		}
		r := MeterVarianceRow{Month: m.Month, Pool: m.Pool, MeterKWh: m.KWh, MeasuredKWh: energy.KWh(mj)}
		r.DifferenceKWh = r.MeterKWh - r.MeasuredKWh
		if r.MeasuredKWh > 0 {
			pct := 100 * r.DifferenceKWh / r.MeasuredKWh
			r.DifferencePct = &pct
		}
		out = append(out, r)
	}
	return out
}

// PreemptionOut is the preemption measurement of one month and pool.
type PreemptionOut struct {
	Month string `json:"month"`
	Pool  string `json:"pool,omitempty"`
	// Intervals counts the measured intervals, Known those whose counter could
	// be read, WithPreemptions those with at least one preemption.
	Intervals       int64 `json:"intervals"`
	Known           int64 `json:"intervals_counter_read"`
	WithPreemptions int64 `json:"intervals_with_preemptions"`
	Preemptions     int64 `json:"preemptions"`
	// KWh is the energy of every interval, KWhPreempted that of the intervals
	// with preemptions, and SharePct the second as a percentage of the first:
	// an upper bound of the energy a preemption's recompute can account for.
	KWh          float64 `json:"kwh"`
	KWhPreempted float64 `json:"kwh_in_preempted_intervals"`
	SharePct     float64 `json:"share_pct"`
}

// preemptions reads the measurement for the period (proxy roles and finance
// viewers only).
func (s *Server) preemptions(ctx context.Context, p *principal, from, to time.Time) ([]PreemptionOut, error) {
	es, ok := s.st.(store.EnergyStore)
	if !ok || !p.reportView() {
		return nil, nil
	}
	rows, err := es.PreemptionBetween(ctx, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]PreemptionOut, 0, len(rows))
	for _, r := range rows {
		o := PreemptionOut{Month: r.Month, Pool: r.Pool, Intervals: r.Intervals, Known: r.Known, WithPreemptions: r.WithPreemptions,
			Preemptions: r.Preemptions, KWh: energy.KWh(r.TotalMJ), KWhPreempted: energy.KWh(r.PreemptedMJ)}
		if r.TotalMJ > 0 {
			o.SharePct = 100 * float64(r.PreemptedMJ) / float64(r.TotalMJ)
		}
		out = append(out, o)
	}
	return out, nil
}
