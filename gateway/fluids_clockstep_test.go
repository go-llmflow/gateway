package gateway

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Reproducer (found by the go-llmflow/bench chaos campaign, scenario
// clock-jump-tariff-band: a gateway whose clock steps back one hour, an NTP
// correction or the end of daylight saving time). The same exporter, the same
// energy moved, the same scrapes: only the clock differs. The energy the report
// holds must not depend on a step of the clock.
func fluidsTotalKWh(t *testing.T, step time.Duration) float64 {
	up := newFakeVLLM(t)
	dc := newFakeDCGM(t)
	cfg := fluidsConfig(up.URL(), dc.srv.URL)
	var clockNs atomic.Int64
	clockNs.Store(time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clockNs.Load()).UTC() }
	nodes := energyNodes(cfg, Secrets{})
	for i := range cfg.ModelGroups {
		for j := range cfg.ModelGroups[i].Upstreams {
			cfg.ModelGroups[i].Upstreams[j].DcgmURL = ""
		}
	}
	e := newEnv(t, cfg, WithClock(now))
	ctx := context.Background()
	es := store.EnergyStore(e.st)
	round := func(d time.Duration) { clockNs.Add(int64(d)); e.srv.fluidsRound(ctx, es, nodes) }
	burn := func() { dc.add("GPU-aaa", kWhMJ); dc.add("GPU-bbb", kWhMJ) }

	round(0) // baseline
	burn()
	round(5 * time.Minute)
	burn()
	round(step) // the clock steps; 0 for the control run
	burn()
	round(5 * time.Minute)
	burn()
	round(5 * time.Minute)
	round(5 * time.Minute)
	_, a := e.fluids(master, "&group_by=org")
	var total float64
	for _, r := range a.Rows {
		total += r.KWh
	}
	return total
}

func TestAClockStepBackDoesNotLoseEnergy(t *testing.T) {
	control := fluidsTotalKWh(t, 5*time.Minute)
	if control <= 0 {
		t.Fatalf("control run: %v kWh", control)
	}
	for _, step := range []time.Duration{-time.Hour, 2 * time.Hour} {
		got := fluidsTotalKWh(t, step)
		if got < control*0.999 || got > control*1.001 {
			t.Errorf("a clock step of %v: the report holds %.6f kWh, the same energy without the step gives %.6f", step, got, control)
		}
	}
}
