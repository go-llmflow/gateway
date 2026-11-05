package gateway

import (
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
)

func parisFluids(t *testing.T) *config.Fluids {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	return &config.Fluids{PUE: 1, PricePerKWh: 0.15, Currency: "EUR", CurrencyDecimals: 2, IdleEnergy: energy.IdleByShare, WPrefill: 1, WDecode: 1,
		Location: loc, Tariff: []config.TariffBand{
			{Name: "peak", Price: 0.25, From: 7, To: 23},
			{Name: "night", Price: 0.08, From: 23, To: 7},
		}}
}

func tariffEnv(t *testing.T, opts ...Option) *env {
	t.Helper()
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Fluids = parisFluids(t)
	return newEnv(t, cfg, opts...)
}

// Each hour is priced at the band its local start falls in, the idle energy at
// the bands it was used in: 12:00 and 23:30 in Paris (CEST) on 14 October.
func saveTariffEnergy(t *testing.T, e *env) {
	t.Helper()
	// 10:00Z = 12:00 CEST (peak): etabA 1 kWh active.
	saveEnergy(t, e, time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC), kWhMJ, 0, "etabA")
	// 21:30Z = 23:30 CEST (night): etabB 2 kWh active.
	saveEnergy(t, e, time.Date(2026, 10, 14, 21, 30, 0, 0, time.UTC), 2*kWhMJ, 0, "etabB")
	// Idle: 1 kWh at 10:15Z (peak) and 1 kWh at 22:10Z (night).
	saveEnergy(t, e, time.Date(2026, 10, 14, 10, 15, 0, 0, time.UTC), kWhMJ, kWhMJ, "")
	saveEnergy(t, e, time.Date(2026, 10, 14, 22, 10, 0, 0, time.UTC), kWhMJ, kWhMJ, "")
}

func fluidsRowsByOrg(t *testing.T, e *env) map[string]FluidsRow {
	t.Helper()
	o := e.fluidsJSON(t)
	by := map[string]FluidsRow{}
	for _, r := range o.Rows {
		by[r.Org] = r
	}
	return by
}

func (e *env) fluidsJSON(t *testing.T) fluidsOut {
	t.Helper()
	resp := e.do("GET", "/admin/fluids?from=2026-10-01&to=2026-11-01&group_by=org", master, "")
	body := readAll(t, resp)
	var o fluidsOut
	if err := json.Unmarshal([]byte(body), &o); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %s %v", resp.StatusCode, body, err)
	}
	return o
}

func TestTimeOfUseTariffPricesEachHour(t *testing.T) {
	e := tariffEnv(t)
	saveTariffEnergy(t, e)
	by := fluidsRowsByOrg(t, e)
	// By share (0.75 / 0.25): etabA = 1 kWh at 0.25 + 0.75 of the peak idle kWh at 0.25
	// + 0.75 of the night idle kWh at 0.08; etabB = 2 kWh at 0.08 + 0.25 + 0.25.
	wantA := 1*0.25 + 0.75*0.25 + 0.75*0.08
	wantB := 2*0.08 + 0.25*0.25 + 0.25*0.08
	if !near3(by["etabA"].Cost, wantA) || !near3(by["etabB"].Cost, wantB) {
		t.Fatalf("A %v (want %v), B %v (want %v)", by["etabA"].Cost, wantA, by["etabB"].Cost, wantB)
	}
	// The energy is the energy: 5 kWh.
	if !near3(by["etabA"].KWh+by["etabB"].KWh, 5) {
		t.Fatalf("energy %v", by["etabA"].KWh+by["etabB"].KWh)
	}
	// Without the tariff the same energy costs the flat 0.15 per kWh.
	e.srv.cfg.Fluids.Tariff = nil
	flat := fluidsRowsByOrg(t, e)
	if !near3(flat["etabA"].Cost+flat["etabB"].Cost, 5*0.15) {
		t.Fatalf("flat: %v", flat["etabA"].Cost+flat["etabB"].Cost)
	}
}

// A closed month keeps the tariff it was closed with.
func TestClosedMonthKeepsItsTariff(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).UnixNano())
	e := tariffEnv(t, WithClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() }))
	saveTariffEnergy(t, e)
	before := fluidsRowsByOrg(t, e)
	if resp := e.do("POST", "/admin/billing/close?month=2026-10", master, ""); resp.StatusCode != 200 {
		t.Fatalf("close: %d %s", resp.StatusCode, readAll(t, resp))
	}
	e.srv.cfg.Fluids.Tariff[0].Price = 5 // a corrected tariff does not rewrite the closed month
	e.srv.cfg.Fluids.Location = time.UTC
	after := fluidsRowsByOrg(t, e)
	for _, o := range []string{"etabA", "etabB"} {
		if !near3(before[o].Cost, after[o].Cost) || before[o].Cost == 0 {
			t.Errorf("%s: %v before, %v after the closing", o, before[o].Cost, after[o].Cost)
		}
	}
}

// The sub-meter is shown against the measured IT energy and rescales nothing;
// only the proxy roles read it.
func TestSubMeterVariance(t *testing.T) {
	e := tariffEnv(t)
	e.srv.cfg.Fluids.Meters = []config.SubMeter{{Month: "2026-10", KWh: 6}, {Month: "2026-09", KWh: 3}}
	saveTariffEnergy(t, e) // 5 kWh measured in October
	o := e.fluidsJSON(t)
	if len(o.Meter) != 1 { // September is not in the period
		t.Fatalf("rows %+v", o.Meter)
	}
	m := o.Meter[0]
	if m.Month != "2026-10" || !near3(m.MeterKWh, 6) || !near3(m.MeasuredKWh, 5) || !near3(m.DifferenceKWh, 1) || m.DifferencePct == nil || !near3(*m.DifferencePct, 20) {
		t.Fatalf("%+v", m)
	}
	by := fluidsRowsByOrg(t, e)
	if !near3(by["etabA"].KWh+by["etabB"].KWh, 5) {
		t.Fatal("the sub-meter rescaled the measurement")
	}
	ob := e.adminToken("ob", `{"role":"org_admin","org":"etabB"}`)
	resp := e.do("GET", "/admin/fluids?from=2026-10-01&to=2026-11-01&group_by=org", ob, "")
	var oo fluidsOut
	if err := json.Unmarshal([]byte(readAll(t, resp)), &oo); err != nil || len(oo.Meter) != 0 || len(oo.Preemptions) != 0 {
		t.Fatalf("an organisation admin read platform data: %+v %v", oo, err)
	}
}
