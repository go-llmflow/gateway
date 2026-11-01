package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/fairshare"
	"github.com/go-llmflow/gateway/store"
)

// Two machines, two apportionments (per-pool shares):
//
//	b300: capacity 10, the organisations' shares 0.4 / 0.3 / 0.2 / 0.1
//	rtx : capacity C,  its own shares etabA 0.5, etabC 0.3, etabD 0.2
//
// etabB holds nothing of the rtx machine.
func poolsGatewayHCL(vllm string, rtxCap int, rtxExtra string) string {
	return `
master_key_file = "/unused"
cache_salt_key_file = "/unused"
budget_refresh  = "1h"
key_cache_ttl   = "1h"
health {
  interval = "1h"
  cooldown = "1h"
}
fluids {
  pue = 1
}
org "etabA" {
  share = 0.4
  team "etabA" {}
}
org "etabB" {
  share = 0.3
  team "etabB" {}
}
org "etabC" {
  share = 0.2
  team "etabC" {}
}
org "etabD" {
  share = 0.1
  team "etabD" {}
}
pool "b300" { capacity = 10 }
pool "rtx" {
  capacity = ` + strconv.Itoa(rtxCap) + `
  shares   = { etabA = 0.5, etabC = 0.3, etabD = 0.2 }
` + rtxExtra + `
}
model "llama" {
  pool     = "b300"
  key_file = "/unused"
  upstream "u" { base_url = "` + vllm + `" }
}
model "rtxllama" {
  pool     = "rtx"
  key_file = "/unused"
  upstream "u" { base_url = "` + vllm + `" }
}
`
}

func newPoolsEnv(t *testing.T, rtxCap int, rtxExtra string) (*env, *fakeVLLM) {
	t.Helper()
	up := newFakeVLLM(t)
	cfg, err := config.Parse([]byte(poolsGatewayHCL(up.URL(), rtxCap, rtxExtra)), "pools.hcl")
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	e := &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	t.Cleanup(func() { gw.Close(); srv.Close(context.Background()) })
	return e, up
}

func localReserved(t *testing.T, e *env, pool string) map[string]int {
	t.Helper()
	lp, ok := e.srv.pools[pool].(localPool)
	if !ok {
		t.Fatalf("pool %s is not local", pool)
	}
	return lp.p.Reserved()
}

// The integer reservations of each pool, computed by hand.
//
//	rtx, C = 64, shares 0.5 / 0.3 / 0.2: quotas 32 / 19.2 / 12.8, floors
//	32 + 19 + 12 = 63, the one seat left goes to the largest remainder (0.8):
//	etabA 32, etabC 19, etabD 13, etabB 0.
//	b300, C = 10, the organisations' shares 0.4 / 0.3 / 0.2 / 0.1: 4 / 3 / 2 / 1.
func TestPoolSharesReservations(t *testing.T) {
	e, _ := newPoolsEnv(t, 64, "")
	rtx := localReserved(t, e, "rtx")
	if rtx["etabA"] != 32 || rtx["etabC"] != 19 || rtx["etabD"] != 13 || rtx["etabB"] != 0 || len(rtx) != 3 {
		t.Errorf("rtx reservations %v, want A 32, C 19, D 13 (B absent: 0)", rtx)
	}
	b300 := localReserved(t, e, "b300")
	if b300["etabA"] != 4 || b300["etabB"] != 3 || b300["etabC"] != 2 || b300["etabD"] != 1 {
		t.Errorf("b300 reservations %v, want the organisations' shares 4/3/2/1", b300)
	}
	// The snapshot (admin pools view, metrics) shows each pool's own shares.
	for _, ts := range e.srv.pools["rtx"].snapshot() {
		if ts.Team == "etabA" && (ts.Share != 0.5 || ts.Reserved != 32) {
			t.Errorf("rtx snapshot of etabA: %+v", ts)
		}
	}
	for _, ts := range e.srv.pools["b300"].snapshot() {
		if ts.Team == "etabB" && (ts.Share != 0.3 || ts.Reserved != 3) {
			t.Errorf("b300 snapshot of etabB: %+v", ts)
		}
	}
}

// One organisation floods the machine and borrows the idle slots; when a
// shareholder arrives it is served within its share, first, and the borrower
// is the one to wait or be preempted: its requests carry the lower priority
// vLLM acts on.
func TestPoolSharesLiveAdmission(t *testing.T) {
	// C = 4: etabA 2, etabC 1, etabD 1 (quotas 2 / 1.2 / 0.8: floors 2 + 1 + 0,
	// the seat left to the largest remainder 0.8), etabB 0.
	e, up := newPoolsEnv(t, 4, "")
	if r := localReserved(t, e, "rtx"); r["etabA"] != 2 || r["etabC"] != 1 || r["etabD"] != 1 {
		t.Fatalf("reservations %v", r)
	}
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	_, keyC := e.newKey("etabC")
	up.gate = make(chan struct{})
	send := func(key, model, tag string, wg *sync.WaitGroup, code *int) {
		defer wg.Done()
		resp := e.do("POST", "/v1/chat/completions", key, `{"model":"`+model+`","max_tokens":5,"messages":[{"role":"user","content":"`+tag+`"}]}`)
		readAll(t, resp)
		*code = resp.StatusCode
	}
	waitHits := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for up.count() < n {
			if time.Now().After(deadline) {
				t.Fatalf("%d requests reached the upstream, want %d", up.count(), n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	prio := func(tag string) int {
		t.Helper()
		up.mu.Lock()
		defer up.mu.Unlock()
		for _, r := range up.reqs {
			if m, _ := r.Body["messages"].([]any); len(m) > 0 {
				if c, _ := m[0].(map[string]any)["content"].(string); c == tag {
					p, err := strconv.Atoi(r.Header.Get("X-Vllm-Priority"))
					if err != nil {
						t.Fatalf("%s: priority %q", tag, r.Header.Get("X-Vllm-Priority"))
					}
					return p
				}
			}
		}
		t.Fatalf("no request %q at the upstream", tag)
		return 0
	}
	var wg sync.WaitGroup
	codes := make([]int, 5)
	// etabC (reservation 1) floods the machine: three requests in flight.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go send(keyC, "rtxllama", fmt.Sprintf("C%d", i), &wg, &codes[i])
		waitHits(i + 1)
	}
	within, borrowing := 0, 0
	for i := 0; i < 3; i++ {
		if p := prio(fmt.Sprintf("C%d", i)); p < fairshare.BorrowBase {
			within++
		} else {
			borrowing++
		}
	}
	if within != 1 || borrowing != 2 {
		t.Fatalf("etabC: %d within its share of 1, %d borrowing; want 1 and 2", within, borrowing)
	}
	// etabA (reservation 2, nothing in flight) arrives: within its share.
	wg.Add(1)
	go send(keyA, "rtxllama", "A0", &wg, &codes[3])
	waitHits(4)
	pa := prio("A0")
	if pa >= fairshare.BorrowBase {
		t.Fatalf("a shareholder with an unused reservation was admitted as a borrower (priority %d)", pa)
	}
	for i := 0; i < 3; i++ {
		if p := prio(fmt.Sprintf("C%d", i)); p >= fairshare.BorrowBase && p <= pa {
			t.Fatalf("the borrower C%d (priority %d) is not behind the shareholder (%d)", i, p, pa)
		}
	}
	// etabB holds nothing of the rtx machine; with borrow = "all" it is a
	// borrower, at borrowing priority, never within a share.
	wg.Add(1)
	go send(keyB, "rtxllama", "B0", &wg, &codes[4])
	waitHits(5)
	if p := prio("B0"); p < fairshare.BorrowBase {
		t.Fatalf("etabB has no share of the rtx pool and was admitted within a share (priority %d)", p)
	}
	// ...while on the b300 pool etabB holds a share (the organisations' own).
	// The fake is gated: this request would block; read its priority only.
	var wg2 sync.WaitGroup
	var codeB int
	wg2.Add(1)
	go send(keyB, "llama", "B-b300", &wg2, &codeB)
	waitHits(6)
	if p := prio("B-b300"); p >= fairshare.BorrowBase {
		t.Fatalf("etabB's share of the b300 pool (the organisations' shares) was not used: priority %d", p)
	}
	close(up.gate)
	wg.Wait()
	wg2.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Errorf("request %d: %d", i, c)
		}
	}
}

// borrow = "shareholders": the machine lends only to its contributors.
func TestPoolBorrowShareholders(t *testing.T) {
	e, up := newPoolsEnv(t, 4, `  borrow   = "shareholders"`)
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	body := `{"model":"rtxllama","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	resp := e.do("POST", "/v1/chat/completions", keyB, body)
	out := readAll(t, resp)
	if resp.StatusCode != 403 || !strings.Contains(out, "holds none") || up.count() != 0 {
		t.Fatalf("a non-contributor: %d %s (upstream calls %d)", resp.StatusCode, out, up.count())
	}
	if resp := e.do("POST", "/v1/chat/completions", keyA, body); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("a contributor: %d", resp.StatusCode)
	}
	// The same organisation uses the other pool, whose borrowing is open.
	if resp := e.do("POST", "/v1/chat/completions", keyB, `{"model":"llama","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("b300: %d", resp.StatusCode)
	}
	metrics := readAll(t, e.do("GET", "/metrics", "", ""))
	if !strings.Contains(metrics, `reason="pool_shareholders"`) {
		t.Errorf("the refusal is not counted:\n%s", metrics)
	}
}

func near3(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// The lending and decision reports over two pools with different shares, with
// the guaranteed capacity worked out by hand for a day (24 h):
//
//	b300 (C = 10, 4/3/2/1 slots):  A 96, B 72, C 48, D 24 GPU-hours
//	rtx  (C = 4,  2/0/1/1 slots):  A 48, B  0, C 24, D 24
//	totals:                        A 144, B 72, C 72, D 48
func TestReportsPerPool(t *testing.T) {
	e, _ := newPoolsEnv(t, 4, "")
	day := time.Date(2026, 10, 14, 0, 0, 0, 0, time.UTC)
	end := day.Add(12 * time.Hour)
	rec := func(org, pool string, borrowing bool, cost float64) store.Record {
		return store.Record{ID: "r-" + org + pool, Time: end, Org: org, Team: org, Model: "m", Pool: pool, Status: 200, Borrowing: borrowing,
			Cost: cost, LatencyMS: 3600_000, PromptTokens: 10, CompletionTokens: 10}
	}
	if err := e.st.InsertRecords(context.Background(), []store.Record{
		rec("etabC", "rtx", false, 3),   // etabC used 3 GPU-hours inside its rtx share
		rec("etabA", "b300", false, 12), // etabA 12 inside its b300 share
	}); err != nil {
		t.Fatal(err)
	}
	q := "?from=2026-10-14&to=2026-10-15"
	var lr struct {
		Orgs []struct {
			Org                string
			Guaranteed         float64 `json:"guaranteed_gpu_hours"`
			Used               float64 `json:"used_in_share_gpu_hours"`
			GuaranteeUtilisaed float64 `json:"guarantee_utilisation"`
		}
		Pools []struct {
			Pool string
			Orgs []struct {
				Org        string
				Guaranteed float64 `json:"guaranteed_gpu_hours"`
				Used       float64 `json:"used_in_share_gpu_hours"`
				Util       float64 `json:"guarantee_utilisation"`
			}
		}
	}
	if err := json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/lending"+q, master, ""))), &lr); err != nil {
		t.Fatal(err)
	}
	totals := map[string]float64{}
	for _, o := range lr.Orgs {
		totals[o.Org] = o.Guaranteed
	}
	want := map[string]map[string]float64{
		"b300": {"etabA": 96, "etabB": 72, "etabC": 48, "etabD": 24},
		"rtx":  {"etabA": 48, "etabB": 0, "etabC": 24, "etabD": 24},
	}
	if len(lr.Pools) != 2 {
		t.Fatalf("%d pools in the report: %+v", len(lr.Pools), lr.Pools)
	}
	sum := map[string]float64{}
	for _, p := range lr.Pools {
		for _, o := range p.Orgs {
			if !near3(o.Guaranteed, want[p.Pool][o.Org]) {
				t.Errorf("pool %s %s: guaranteed %v, want %v", p.Pool, o.Org, o.Guaranteed, want[p.Pool][o.Org])
			}
			sum[o.Org] += o.Guaranteed
			if p.Pool == "rtx" && o.Org == "etabC" && (!near3(o.Used, 3) || !near3(o.Util, 3.0/24)) {
				t.Errorf("etabC in rtx: used %v utilisation %v, want 3 and 0.125", o.Used, o.Util)
			}
			if p.Pool == "b300" && o.Org == "etabA" && !near3(o.Used, 12) {
				t.Errorf("etabA in b300: used %v, want 12", o.Used)
			}
		}
	}
	for org, g := range map[string]float64{"etabA": 144, "etabB": 72, "etabC": 72, "etabD": 48} {
		if !near3(totals[org], g) || !near3(sum[org], g) {
			t.Errorf("%s: total %v, sum of the pools %v, want %v", org, totals[org], sum[org], g)
		}
	}
	// CSV: a pool column, the totals first ("*"), then one row per pool.
	csv := readAll(t, e.do("GET", "/admin/lending"+q+"&format=csv", master, ""))
	if !strings.Contains(csv, ",pool\n") || !strings.Contains(csv, ",*\n") || !strings.Contains(csv, ",rtx\n") || !strings.Contains(csv, ",b300\n") {
		t.Errorf("csv:\n%s", csv)
	}

	// The decision report: Share is the capacity-weighted mean, the pools by
	// pool. etabA: (0.4 × 10 + 0.5 × 4) / 14 = 6/14.
	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	var dr struct {
		Partners []struct {
			Org        string
			Share      float64
			Guaranteed float64
			Pools      []struct {
				Pool       string
				Share      float64
				Guaranteed float64
			}
		}
	}
	if err := json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/decision"+q+"&format=json", fin, ""))), &dr); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range dr.Partners {
		if p.Org != "etabA" {
			continue
		}
		found = true
		if !near3(p.Share, 6.0/14) || !near3(p.Guaranteed, 144) || len(p.Pools) != 2 {
			t.Fatalf("etabA: %+v", p)
		}
		for _, pp := range p.Pools {
			if pp.Pool == "rtx" && (!near3(pp.Share, 0.5) || !near3(pp.Guaranteed, 48)) || pp.Pool == "b300" && (!near3(pp.Share, 0.4) || !near3(pp.Guaranteed, 96)) {
				t.Errorf("pool row %+v", pp)
			}
		}
	}
	if !found {
		t.Fatal("no etabA partner")
	}
	// An organisation's admin reads its own partner only.
	oa := e.adminToken("oadminA", `{"role":"org_admin","org":"etabA"}`)
	var own struct{ Partners []struct{ Org string } }
	if err := json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/decision"+q+"&format=json", oa, ""))), &own); err != nil || len(own.Partners) != 1 || own.Partners[0].Org != "etabA" {
		t.Fatalf("org admin: %+v %v", own, err)
	}
	for _, f := range []string{"pdf", "xlsx"} {
		resp := e.do("GET", "/admin/decision"+q+"&format="+f, fin, "")
		if data := readAll(t, resp); resp.StatusCode != 200 || len(data) < 1000 {
			t.Fatalf("%s: %d, %d bytes", f, resp.StatusCode, len(data))
		}
	}
}

// Idle energy goes by the shares of the pool whose GPUs were idle (1000 MJ on
// each machine, one UTC month, idle_energy = by_share):
//
//	rtx  (shares 0.5 / 0.3 / 0.2):             A 500, C 300, D 200
//	b300 (the organisations' 0.4/0.3/0.2/0.1): A 400, B 300, C 200, D 100
func TestFluidsIdleByPoolShares(t *testing.T) {
	e, _ := newPoolsEnv(t, 4, "")
	ctx := context.Background()
	t0 := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	cell := func(org, pool string) energy.Attributed {
		return energy.Attributed{Consumer: energy.Consumer{Org: org, Team: org, Model: "m", Pool: pool}, MJ: 100}
	}
	// Two nodes, one per machine; each interval measured 1000 idle + 200 active.
	for _, iv := range []struct {
		node, pool string
		cells      []energy.Attributed
	}{
		{"http://rtx-node", "rtx", []energy.Attributed{cell("etabA", "rtx"), cell("etabC", "rtx")}},
		{"http://b300-node", "b300", []energy.Attributed{cell("etabA", "b300"), cell("etabB", "b300")}},
	} {
		ei := store.EnergyInterval{Upstream: iv.node, Start: t0, End: t0.Add(time.Minute), TotalMJ: 1200, IdleMJ: 1000, Pool: iv.pool}
		if err := e.st.SaveEnergy(ctx, ei, iv.cells); err != nil {
			t.Fatal(err)
		}
	}
	cells, err := e.srv.allocatedEnergy(ctx, e.st, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	idle := map[string]int64{}
	var total int64
	for _, c := range cells {
		idle[c.Org+"@"+c.Pool] += c.IdleMJ
		total += c.ActiveMJ + c.IdleMJ
	}
	if total != 2400 {
		t.Errorf("energy %d mJ, measured 2400", total)
	}
	byOrg := map[string]int64{}
	for k, v := range idle {
		org, _, _ := strings.Cut(k, "@")
		byOrg[org] += v
	}
	// rtx idle: A 500, C 300, D 200 (D has no cell: its own row). b300 idle,
	// allocated together as before per-pool shares: A 400, B 300, C 200, D 100.
	want := map[string]int64{"etabA": 500 + 400, "etabB": 300, "etabC": 300 + 200, "etabD": 200 + 100}
	for org, w := range want {
		if byOrg[org] != w {
			t.Errorf("%s: idle %d mJ, want %d (all: %v)", org, byOrg[org], w, byOrg)
		}
	}
	// The rtx idle energy is on rtx rows of the report, the b300's on b300
	// rows (etabC and etabD have no b300 cell: their share of it falls back to
	// their rtx cell, or a row of its own).
	if idle["etabA@rtx"] != 500 || idle["etabA@b300"] != 400 || idle["etabC@rtx"] != 300+200 || idle["etabD@rtx"] != 200 || idle["etabD@"] != 100 {
		t.Errorf("rtx rows: %v", idle)
	}
}

// By usage, a pool's idle energy follows what the organisations used in THAT
// pool; and a pool without own shares keeps the whole-platform allocation.
func TestAllocateFluidsPoolsByUsage(t *testing.T) {
	f := &config.Fluids{PUE: 1, IdleEnergy: energy.IdleByUsage}
	cells := []store.EnergyCell{
		{Day: "2026-10-01", Consumer: energy.Consumer{Org: "a", Pool: "rtx"}, MJ: 100},
		{Day: "2026-10-01", Consumer: energy.Consumer{Org: "c", Pool: "rtx"}, MJ: 300},
		{Day: "2026-10-01", Consumer: energy.Consumer{Org: "a", Pool: "b300"}, MJ: 600},
	}
	idle := []store.EnergyIdle{{Day: "2026-10-02", Pool: "rtx", MJ: 400}, {Day: "2026-10-02", Pool: "", MJ: 1000}}
	sharesOf := func(string) map[string]float64 { return map[string]float64{"a": 0.5, "c": 0.5} }
	cs, err := allocateFluidsPools(cells, idle, f, sharesOf, func(p string) bool { return p == "rtx" }, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	var total int64
	for _, c := range cs {
		got[c.Org+"@"+c.Pool] += c.IdleMJ
		total += c.ActiveMJ + c.IdleMJ
	}
	if total != 1000+1400 {
		t.Fatalf("total %d", total)
	}
	// rtx idle 400 by rtx usage a:c = 100:300 → 100 / 300, on their rtx cells
	// (c's rtx cell also carries 300 of the platform's idle, below).
	if got["a@rtx"] != 100 {
		t.Errorf("rtx idle %v", got)
	}
	// The unattributed 1000 by all usage a:c = 700:300 → 700 / 300, spread over
	// the cells in pools without shares of their own (a: b300), else over all
	// the organisation's cells (c has none but rtx).
	if got["a@b300"] != 700 || got["a@rtx"] != 100 || got["c@rtx"] != 300+300 {
		t.Errorf("whole-platform idle %v", got)
	}
}
