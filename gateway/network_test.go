package gateway

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

// The record of a request carries the bytes the gateway forwarded: the body
// sent to the upstream and the answer written to the client, exactly.
func TestRecordCountsNetworkBytes(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA")
	resp := e.do("POST", "/v1/chat/completions", key, chat)
	answer := readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, answer)
	}
	r := waitRecord(t, e.st, func(r store.Record) bool { return r.Status == 200 })
	if r.BytesOut != int64(len(answer)) || r.BytesOut == 0 {
		t.Errorf("bytes out %d, the client read %d", r.BytesOut, len(answer))
	}
	if sent := up.last().RawLen; r.BytesIn != int64(sent) || r.BytesIn == 0 {
		t.Errorf("bytes in %d, the upstream received %d", r.BytesIn, sent)
	}
}

// Network and storage reach the report: GB and GB-months, priced at the
// month's terms, and a fixed cost can be spread by them.
func TestNetworkAndStorageInTheReports(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Fluids = &config.Fluids{PUE: 1, Currency: "EUR", CurrencyDecimals: 2, IdleEnergy: energy.IdleByUsage, WPrefill: 1, WDecode: 1,
		PricePerGB: 0.5, PricePerGBMonth: 2,
		FixedCosts: []config.FixedCost{{Name: "switch", Monthly: 100, Charge: config.ChargeAll, Basis: config.BasisNetworkBytes}}}
	e := newEnv(t, cfg)
	ts := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	rec := func(id, org string, in, out int64) store.Record {
		return store.Record{ID: id, Time: ts, Org: org, Team: org, Model: "llama", Pool: "default", Status: 200, BytesIn: in, BytesOut: out}
	}
	if err := e.st.InsertRecords(context.Background(), []store.Record{
		rec("r1", "etabA", 1e9, 1e9), // 2 GB
		rec("r2", "etabB", 3e9, 3e9), // 6 GB
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.AddStorage(context.Background(), "2026-10-01", "etabA", 1e9); err != nil {
		t.Fatal(err)
	}
	cells, err := e.srv.liveCells(context.Background(), e.st, oct1, nov1)
	if err != nil {
		t.Fatal(err)
	}
	rows := groupFluidsM(cells, []string{"org"}, cfg.Fluids, nil)
	got := map[string]FluidsRow{}
	for _, r := range rows {
		got[r.Org] = r
	}
	a, b := got["etabA"], got["etabB"]
	// 2 GB at 0.5 = 1.00 and 6 GB = 3.00; the storage of etabA: 31 GB present
	// 1 GB present on every day of the 31-day month is 1 GB-month (the usage
	// rows add a few bytes): about 2.00 at 2 per GB-month.
	if a.NetworkGB != 2 || b.NetworkGB != 6 || a.NetworkCost != 1 || b.NetworkCost != 3 {
		t.Fatalf("network: %+v %+v", a, b)
	}
	if a.StorageGBMonth < 1 || a.StorageGBMonth > 1.001 || a.StorageCost < 2 || a.StorageCost > 2.01 || b.StorageGBMonth > 0.0001 {
		t.Fatalf("storage: %+v %+v", a, b)
	}
	// The switch (100.00) is spread by traffic: 25 / 75 %.
	if a.FixedCost != 25 || b.FixedCost != 75 {
		t.Fatalf("by traffic: %v %v", a.FixedCost, b.FixedCost)
	}
	if want := a.Cost + a.FixedCost + a.NetworkCost + a.StorageCost; a.TotalCost != want {
		t.Fatalf("total %v, want %v", a.TotalCost, want)
	}
}

// The compressed size of a request-log object is split between the
// organisations of the batch pro rata to their bytes, and the parts add up.
func TestRequestLogStorageIsApportionedExactly(t *testing.T) {
	var mu sync.Mutex
	got := map[string]int64{}
	var wg sync.WaitGroup
	l := &requestLogger{orgBytes: map[string]int64{"etabA": 100, "etabB": 300}}
	wg.Add(2)
	l.meter = func(day, org string, n int64) {
		defer wg.Done()
		mu.Lock()
		got[day+"|"+org] += n
		mu.Unlock()
	}
	// 1001 over 100 : 300 is 250.25 and 750.75: the floors leave one byte,
	// which goes to the largest remainder.
	l.meterBatch("2026-10-05", 1001)
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	if got["2026-10-05|etabA"] != 250 || got["2026-10-05|etabB"] != 751 {
		t.Fatalf("%v", got)
	}
	if len(l.orgBytes) != 0 {
		t.Fatal("the batch's counts were not reset")
	}
}
