package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

// fakeDCGM serves the two energy counters the way dcgm-exporter prints them.
type fakeDCGM struct {
	mu   sync.Mutex
	mj   map[string]uint64
	fail bool
	srv  *httptest.Server
}

func newFakeDCGM(t testing.TB) *fakeDCGM {
	d := &fakeDCGM{mj: map[string]uint64{"GPU-aaa": 0, "GPU-bbb": 0}}
	d.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.fail || r.URL.Path != "/metrics" {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		fmt.Fprint(w, "# TYPE DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION counter\n")
		i := 0
		for _, u := range []string{"GPU-aaa", "GPU-bbb"} {
			fmt.Fprintf(w, "DCGM_FI_DEV_TOTAL_ENERGY_CONSUMPTION{gpu=\"%d\",UUID=%q} %d\n", i, u, d.mj[u])
			i++
		}
	}))
	t.Cleanup(d.srv.Close)
	return d
}

func (d *fakeDCGM) add(u string, mj uint64) { d.mu.Lock(); d.mj[u] += mj; d.mu.Unlock() }
func (d *fakeDCGM) set(u string, mj uint64) { d.mu.Lock(); d.mj[u] = mj; d.mu.Unlock() }
func (d *fakeDCGM) setFail(f bool)          { d.mu.Lock(); d.fail = f; d.mu.Unlock() }

const kWhMJ = 3_600_000_000 // 1 kWh in mJ

func fluidsConfig(vllm, dcgm string) *config.Config {
	cfg := testConfig(vllm)
	for i := range cfg.ModelGroups {
		for j := range cfg.ModelGroups[i].Upstreams {
			cfg.ModelGroups[i].Upstreams[j].DcgmURL = dcgm + "/metrics"
		}
	}
	cfg.Fluids = &config.Fluids{
		PUE: 1.4, PricePerKWh: 0.18, Currency: "EUR", IdleEnergy: energy.IdleByShare,
		WPrefill: 1, WDecode: 1, Interval: time.Hour, Settle: 0,
		Periods: map[string]config.FluidsPeriod{},
	}
	return cfg
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

type fluidsAnswer struct {
	Rows []FluidsRow `json:"rows"`
}

func (e *env) fluids(tok, query string) (int, fluidsAnswer) {
	e.t.Helper()
	resp := e.do("GET", "/admin/fluids?from=2026-10-01&to=2026-11-01"+query, tok, "")
	body := readAll(e.t, resp)
	var a fluidsAnswer
	if resp.StatusCode == 200 {
		if err := json.Unmarshal([]byte(body), &a); err != nil {
			e.t.Fatalf("%v: %s", err, body)
		}
	}
	return resp.StatusCode, a
}

// A fake exporter, scraped round by round: work is attributed by tokens,
// idle energy by share, a counter reset and a failed scrape lose nothing,
// and what each principal reads adds up to what was measured.
func TestFluidsEndToEnd(t *testing.T) {
	up := newFakeVLLM(t)
	dc := newFakeDCGM(t)
	cfg := fluidsConfig(up.URL(), dc.srv.URL)
	clock := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	e := newEnv(t, cfg, WithClock(func() time.Time { return clock }))
	ctx := context.Background()
	es := store.EnergyStore(e.st)
	nodes := energyNodes(cfg)
	if len(nodes) != 1 || len(nodes[0].labels) != 1 || nodes[0].labels[0] != redactURL(up.URL()) {
		t.Fatalf("nodes %+v", nodes)
	}
	round := func(d time.Duration) { clock = clock.Add(d); e.srv.fluidsRound(ctx, es, nodes) }

	round(0) // baseline
	// 10:00-10:05: etabA 200 tokens of work, etabB 600; 2 kWh measured.
	rec := func(org string, p, c int64, at time.Time) store.Record {
		return store.Record{Time: at, Org: org, Team: org, KeyID: "k", Model: "llama", Pool: "b300", Upstream: redactURL(up.URL()),
			PromptTokens: p, CompletionTokens: c, Cost: 1, Status: 200}
	}
	if err := e.st.InsertRecords(ctx, []store.Record{
		rec("etabA", 100, 100, clock.Add(time.Minute)), rec("etabB", 300, 300, clock.Add(2*time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
	dc.add("GPU-aaa", kWhMJ)
	dc.add("GPU-bbb", kWhMJ)
	round(5 * time.Minute)
	// 10:05-10:10: nobody works; 1 kWh of idle energy, by share 0.75/0.25.
	dc.add("GPU-aaa", kWhMJ/2)
	dc.add("GPU-bbb", kWhMJ/2)
	round(5 * time.Minute)
	// 10:10-10:15: the exporter is down (a missed scrape), then a driver
	// reload on GPU-aaa: its counter restarts from 1 kWh. The interval spans
	// 10:10-10:20 and holds the energy of the reload (1 kWh) plus GPU-bbb's 1 kWh.
	dc.setFail(true)
	round(5 * time.Minute)
	dc.setFail(false)
	dc.set("GPU-aaa", kWhMJ)
	dc.add("GPU-bbb", kWhMJ)
	round(5 * time.Minute)

	const measured = 5.0 // kWh: 2 + 1 + 2 (the reload interval: 1 kWh since the reload, 1 kWh on the other GPU)
	code, a := e.fluids(master, "&group_by=org")
	if code != 200 || len(a.Rows) != 2 {
		t.Fatalf("%d %+v", code, a)
	}
	var sum, cost float64
	by := map[string]FluidsRow{}
	for _, r := range a.Rows {
		sum += r.KWh
		cost += r.Cost
		by[r.Org] = r
	}
	if !near(sum, measured) {
		t.Fatalf("sum of orgs %v kWh, measured %v", sum, measured)
	}
	// Active: A 0.5, B 1.5. Idle (1 + the 2 kWh of the reload interval has no
	// work either): 3 kWh by share: A 2.25, B 0.75.
	if !near(by["etabA"].KWh, 0.5+2.25) || !near(by["etabB"].KWh, 1.5+0.75) || !near(by["etabA"].KWhActive, 0.5) {
		t.Fatalf("by org %+v", by)
	}
	if !near(cost, measured*1.4*0.18) || by["etabA"].Currency != "EUR" {
		t.Fatalf("cost %v", cost)
	}
	// Any grouping adds up to the same energy.
	for _, gb := range []string{"org,team,user", "model", "day", "pool,lab,project"} {
		_, a := e.fluids(master, "&group_by="+gb)
		var s float64
		for _, r := range a.Rows {
			s += r.KWh
		}
		if !near(s, measured) {
			t.Errorf("group_by=%s: %v kWh, want %v", gb, s, measured)
		}
	}
	// Per principal: an organisation sees its own rows and nothing of another.
	e.check(nil, []call{{"master", "POST", "/admin/orgs", `{"name":"orgC"}`, 201}})
	oa := e.adminToken("oadminA", `{"role":"org_admin","org":"etabA"}`)
	oc := e.adminToken("oadminC", `{"role":"org_admin","org":"orgC"}`)
	tm := e.adminToken("tmember", `{"role":"member","team":"etabA"}`)
	_, ra := e.fluids(oa, "&group_by=org")
	if len(ra.Rows) != 1 || ra.Rows[0].Org != "etabA" || !near(ra.Rows[0].KWh, 2.75) {
		t.Fatalf("org admin: %+v", ra.Rows)
	}
	_, ra = e.fluids(oa, "") // no group_by: still only its own
	if len(ra.Rows) != 1 || ra.Rows[0].Org != "etabA" {
		t.Fatalf("default grouping leaks: %+v", ra.Rows)
	}
	_, ra = e.fluids(oa, "&group_by=model") // a grouping without org holds etabA only
	if len(ra.Rows) != 1 || !near(ra.Rows[0].KWh, 2.75) {
		t.Fatalf("model grouping leaks: %+v", ra.Rows)
	}
	if code, ra = e.fluids(oc, "&group_by=org"); code != 200 || len(ra.Rows) != 0 {
		t.Fatalf("other org: %d %+v", code, ra.Rows)
	}
	if code, _ = e.fluids(tm, "&group_by=org"); code != 403 {
		t.Fatalf("team member: %d", code)
	}
	// Bad input.
	for _, q := range []string{"&group_by=key", "&group_by=tag", "&format=pdf"} {
		if code, _ := e.fluids(master, q); code != 400 {
			t.Errorf("%s: %d, want 400", q, code)
		}
	}
	for _, q := range []string{"from=2000-01-01&to=2026-11-01", "from=x&to=y", "to=2026-10-01&from=2026-11-01"} {
		if resp := e.do("GET", "/admin/fluids?"+q, master, ""); resp.StatusCode != 400 {
			t.Errorf("%s: %d, want 400", q, resp.StatusCode)
		}
	}
	resp := e.do("GET", "/admin/fluids?from=2026-10-01&to=2026-11-01", master, "")
	_ = readAll(t, resp)
	resp = e.do("GET", "/admin/fluids?from=2026-10-01&to=2026-11-01&group_by=org,day&format=csv", master, "")
	csv := readAll(t, resp)
	if !strings.HasPrefix(csv, "from,to,org,day,gpu_hours,kwh_active,kwh_idle,kwh,fluids_cost,currency\n") || !strings.Contains(csv, "etabB,2026-10-15,") {
		t.Fatalf("csv:\n%s", csv)
	}
}

// Two replicas scraping one node: the interval is stored once.
func TestFluidsOverlapStoredOnce(t *testing.T) {
	st := store.NewMemory()
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	iv := store.EnergyInterval{Upstream: "n", Start: t0, End: t0.Add(time.Minute), TotalMJ: 5, IdleMJ: 5}
	if err := st.SaveEnergy(context.Background(), iv, nil); err != nil {
		t.Fatal(err)
	}
	iv.Start, iv.End = t0.Add(30*time.Second), t0.Add(90*time.Second)
	if err := st.SaveEnergy(context.Background(), iv, nil); err != store.ErrConflict {
		t.Fatalf("overlap: %v", err)
	}
}

// The agreement's two idle rules, on the same energy.
func TestAllocateFluidsModes(t *testing.T) {
	f := &config.Fluids{PUE: 1, IdleEnergy: energy.IdleByShare}
	cells := []store.EnergyCell{
		{Day: "2026-10-01", Consumer: energy.Consumer{Org: "a", User: "u1"}, MJ: 100},
		{Day: "2026-10-02", Consumer: energy.Consumer{Org: "a", User: "u2"}, MJ: 300},
		{Day: "2026-10-02", Consumer: energy.Consumer{Org: "b", User: "u3"}, MJ: 600},
	}
	idle := []store.EnergyIdle{{Day: "2026-10-01", MJ: 500}, {Day: "2026-10-03", MJ: 500}}
	shares := map[string]float64{"a": 0.5, "b": 0.5, "c": 0}
	total := func(cs []fluidsCell) (n int64) {
		for _, c := range cs {
			n += c.ActiveMJ + c.IdleMJ
		}
		return
	}
	for _, mode := range []string{energy.IdleByShare, energy.IdleByUsage} {
		f.IdleEnergy = mode
		cs, err := allocateFluids(cells, idle, f, shares)
		if err != nil || total(cs) != 2000 {
			t.Fatalf("%s: %d %v", mode, total(cs), err)
		}
		var a, b int64
		for _, c := range cs {
			switch c.Org {
			case "a":
				a += c.IdleMJ
			case "b":
				b += c.IdleMJ
			}
		}
		if mode == energy.IdleByShare && (a != 500 || b != 500) || mode == energy.IdleByUsage && (a != 400 || b != 600) {
			t.Errorf("%s: a %d b %d", mode, a, b)
		}
	}
	// No share and no usage: unallocated, still counted.
	f.IdleEnergy = energy.IdleByShare
	cs, _ := allocateFluids(nil, idle, f, nil)
	if len(cs) != 1 || cs[0].Org != Unallocated || cs[0].IdleMJ != 1000 {
		t.Fatalf("%+v", cs)
	}
	// Per-month terms price each month's energy on its own.
	f2 := &config.Fluids{PUE: 1, PricePerKWh: 1, Currency: "EUR", Periods: map[string]config.FluidsPeriod{"2026-11": {PricePerKWh: 3}}}
	rows := groupFluids([]fluidsCell{
		{Day: "2026-10-31", Consumer: energy.Consumer{Org: "a"}, ActiveMJ: kWhMJ},
		{Day: "2026-11-01", Consumer: energy.Consumer{Org: "a"}, ActiveMJ: kWhMJ},
	}, []string{"org"}, f2)
	if len(rows) != 1 || !near(rows[0].Cost, 4) || !near(rows[0].KWh, 2) {
		t.Fatalf("%+v", rows)
	}
}

// Property: for any interval, the stored parts sum to the measured energy.
func TestAttributeIntervalSums(t *testing.T) {
	f := &config.Fluids{WPrefill: 1, WDecode: 2, IdleWatts: 50}
	for n := int64(0); n < 200; n++ {
		iv := energy.Interval{Start: time.Unix(0, 0), End: time.Unix(15, 0), PerGPU: map[string]int64{"a": 1000 * n * n, "b": 7 * n}}
		iv.MJ = iv.PerGPU["a"] + iv.PerGPU["b"]
		var work []energy.Work
		for k := int64(0); k < n%5; k++ {
			work = append(work, energy.Work{Consumer: energy.Consumer{Org: fmt.Sprint(k)}, Prefill: n*3 + k, Decode: n + 1})
		}
		ei, att := attributeInterval("n", iv, work, f)
		sum := ei.IdleMJ
		for _, a := range att {
			sum += a.MJ
		}
		if sum != iv.MJ || ei.TotalMJ != iv.MJ {
			t.Fatalf("n=%d: %d attributed+idle, measured %d", n, sum, iv.MJ)
		}
	}
}
