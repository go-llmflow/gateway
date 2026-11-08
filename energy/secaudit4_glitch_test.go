package energy

// Security audit 4, 2026-11-08 (untracked proof). One scrape whose counter
// reads LOWER than the last (a glitching or lying exporter, not a driver
// reload) is taken as a reset (meter.go:65-67) and the counter then restarts
// from that low value, so the next normal reading bills everything between the
// glitch and the real counter: with a transient 0, the GPU's whole lifetime
// energy lands in one interval, and attributeInterval splits it between the
// organisations that worked in it (fluids_collect.go:108-124). maxDelta
// (2^50 mJ, ~312 MWh) is the only bound.

import (
	"testing"
	"time"
)

func TestSecaudit4TransientLowReadingBillsLifetimeEnergy(t *testing.T) {
	var m Meter
	t0 := time.Unix(1_700_000_000, 0)
	const lifetime = uint64(2_600_000_000_000) // 2.6e12 mJ: 1 kW for 30 days
	const minute = uint64(60_000_000)          // 1 kW for 60 s
	m.Observe(Reading{At: t0, MJ: map[string]uint64{"gpu0": lifetime}})
	m.Observe(Reading{At: t0.Add(time.Minute), MJ: map[string]uint64{"gpu0": 0}}) // one bad scrape
	iv, _ := m.Observe(Reading{At: t0.Add(2 * time.Minute), MJ: map[string]uint64{"gpu0": lifetime + 2*minute}})
	if iv.MJ > int64(10*minute) {
		t.Errorf("EXPLOIT: one transient 0 reading billed %d mJ (%.0f kWh) in a 60 s interval of a 1 kW GPU",
			iv.MJ, float64(iv.MJ)/3.6e9)
	}
}
