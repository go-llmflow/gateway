package gateway

// Security audit 2026-11-05 (untracked proof). finance_viewer may not group a
// report by person (report.go:68), but GET /admin/billing/closed?month= serves
// the raw snapshot, whose usage is grouped by user (billing.go:417, 745).

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

func TestSecauditClosedSnapshotShowsPeopleToFinance(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).UnixNano())
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Fluids = &config.Fluids{PUE: 1, PricePerKWh: 1, Currency: "EUR", CurrencyDecimals: 2, IdleEnergy: energy.IdleByShare, WPrefill: 1, WDecode: 1}
	e := newEnv(t, cfg, WithClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() }))
	saveEnergy(t, e, time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC), kWhMJ, 0, "etabA")
	if err := e.st.InsertRecords(context.Background(), []store.Record{{ID: "r1", Time: time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC),
		Org: "etabA", Team: "etabA", User: "alice-secret-person", Model: "llama", Pool: "b300", Cost: 2, Status: 200}}); err != nil {
		t.Fatal(err)
	}
	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	if resp := e.do("GET", "/admin/spend?group_by=user&from=2026-10-01&to=2026-11-01", fin, ""); resp.StatusCode != 403 {
		t.Fatalf("control: finance_viewer grouping by user: %d", resp.StatusCode)
	}
	if err := e.st.SaveEnergy(context.Background(), store.EnergyInterval{Upstream: "n", Start: time.Date(2026, 10, 7, 10, 0, 0, 0, time.UTC), End: time.Date(2026, 10, 7, 10, 1, 0, 0, time.UTC), TotalMJ: 100},
		[]energy.Attributed{{Consumer: energy.Consumer{Org: "etabA", Team: "etabA", User: "bob-secret-person"}, MJ: 100}}); err != nil {
		t.Fatal(err)
	}
	fl := e.do("GET", "/admin/fluids?group_by=user&from=2026-10-01&to=2026-11-01", fin, "")
	if b := readAll(t, fl); fl.StatusCode == 200 && strings.Contains(b, "bob-secret-person") {
		t.Errorf("EXPLOIT: finance_viewer /admin/fluids?group_by=user shows people: %.200s", b)
	}
	if resp := e.do("POST", "/admin/billing/close?month=2026-10", master, ""); resp.StatusCode != 200 {
		t.Fatalf("close: %d %s", resp.StatusCode, readAll(t, resp))
	}
	snap := readAll(t, e.do("GET", "/admin/billing/closed?month=2026-10", fin, ""))
	if strings.Contains(snap, "alice-secret-person") {
		t.Errorf("EXPLOIT: finance_viewer reads per-person usage from the closed snapshot: %.200s", snap)
	}
}
