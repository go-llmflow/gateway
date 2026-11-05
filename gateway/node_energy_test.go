package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

// fakeBMC serves a node's EnvironmentMetrics over TLS, with basic auth.
type fakeBMC struct {
	mu        sync.Mutex
	kwh       float64
	watts     float64
	powerOnly bool
	fail      bool
	srv       *httptest.Server
	ca        string // path of its certificate, as a CA file
	auth      atomic.Int64
}

func newFakeBMC(t *testing.T, user, password string) *fakeBMC {
	b := &fakeBMC{}
	b.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != user || p != password {
			b.auth.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		if b.fail || r.URL.Path != "/redfish/v1/Chassis/1/EnvironmentMetrics" {
			http.Error(w, "down", http.StatusInternalServerError)
			return
		}
		if b.powerOnly { // a BMC with no energy counter, only a power
			_ = json.NewEncoder(w).Encode(map[string]any{"PowerWatts": map[string]any{"Reading": b.watts}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"EnergykWh": map[string]any{"Reading": b.kwh}})
	}))
	t.Cleanup(b.srv.Close)
	b.ca = filepath.Join(t.TempDir(), "bmc-ca.pem")
	pemb := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: b.srv.Certificate().Raw})
	if err := os.WriteFile(b.ca, pemb, 0o600); err != nil {
		t.Fatal(err)
	}
	return b
}

func (b *fakeBMC) url() string        { return b.srv.URL + "/redfish/v1/Chassis/1/EnvironmentMetrics" }
func (b *fakeBMC) add(kwh float64)    { b.mu.Lock(); b.kwh += kwh; b.mu.Unlock() }
func (b *fakeBMC) setFail(f bool)     { b.mu.Lock(); b.fail = f; b.mu.Unlock() }
func (b *fakeBMC) set(kwh float64)    { b.mu.Lock(); b.kwh = kwh; b.mu.Unlock() }
func (b *fakeBMC) setPower(w float64) { b.mu.Lock(); b.powerOnly, b.watts = true, w; b.mu.Unlock() }
func (b *fakeBMC) unauthorised() int  { return int(b.auth.Load()) }

// fakeVLLMMetrics serves vLLM's preemption counter.
type fakeVLLMMetrics struct {
	n   atomic.Int64
	srv *httptest.Server
}

func newFakeVLLMMetrics(t *testing.T) *fakeVLLMMetrics {
	m := &fakeVLLMMetrics{}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			http.NotFound(w, r)
			return
		}
		fmt.Fprintf(w, "# TYPE vllm:num_preemptions_total counter\nvllm:num_preemptions_total{engine=\"0\",model_name=\"m\"} %d.0\n", m.n.Load())
	}))
	t.Cleanup(m.srv.Close)
	return m
}

type nodeFixture struct {
	e     *env
	dc    *fakeDCGM
	bmc   *fakeBMC
	vm    *fakeVLLMMetrics
	nodes []*energyNode
	round func(time.Duration)
}

// nodeFixtureFor wires a node with a fake DCGM exporter, a BMC and a vLLM
// /metrics, and drives its rounds on a clock of its own.
func nodeFixtureFor(t *testing.T, factor float64, withBMC bool) *nodeFixture {
	t.Helper()
	dc := newFakeDCGM(t)
	vm := newFakeVLLMMetrics(t)
	bmc := newFakeBMC(t, "admin", "s3cret")
	cfg := fluidsConfig(vm.srv.URL, dc.srv.URL)
	cfg.Fluids.NodeOverheadFactor = factor
	if withBMC {
		for i := range cfg.ModelGroups {
			for j := range cfg.ModelGroups[i].Upstreams {
				u := &cfg.ModelGroups[i].Upstreams[j]
				u.RedfishURL, u.RedfishUser, u.RedfishPasswordFile, u.RedfishCAFile = bmc.url(), "admin", "pw-ref", bmc.ca
			}
		}
	}
	nodes := energyNodes(cfg, Secrets{RedfishPasswords: map[string]string{"pw-ref": "s3cret"}})
	for i := range cfg.ModelGroups { // the test drives the rounds itself
		for j := range cfg.ModelGroups[i].Upstreams {
			cfg.ModelGroups[i].Upstreams[j].DcgmURL = ""
		}
	}
	var clockNs atomic.Int64
	clockNs.Store(time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC).UnixNano())
	now := func() time.Time { return time.Unix(0, clockNs.Load()).UTC() }
	e := newEnv(t, cfg, WithClock(now))
	es := store.EnergyStore(e.st)
	f := &nodeFixture{e: e, dc: dc, bmc: bmc, vm: vm, nodes: nodes}
	f.round = func(d time.Duration) { clockNs.Add(int64(d)); e.srv.fluidsRound(context.Background(), es, nodes) }
	return f
}

func (f *nodeFixture) records(t *testing.T, at time.Time) {
	t.Helper()
	rec := func(org string, p, c int64, at time.Time) store.Record {
		return store.Record{Time: at, Org: org, Team: org, KeyID: "k", Model: "llama", Pool: "b300", Upstream: redactURL(f.vm.srv.URL),
			PromptTokens: p, CompletionTokens: c, Cost: 1, Status: 200}
	}
	if err := f.e.st.InsertRecords(context.Background(), []store.Record{
		rec("etabA", 100, 100, at.Add(time.Minute)), rec("etabB", 300, 300, at.Add(2*time.Minute)),
	}); err != nil {
		t.Fatal(err)
	}
}

type fluidsOut struct {
	Rows        []FluidsRow        `json:"rows"`
	Preemptions []PreemptionOut    `json:"preemptions"`
	Meter       []MeterVarianceRow `json:"meter_variance"`
	Overhead    []NodeOverheadOut  `json:"node_overhead"`
}

func (f *nodeFixture) report(t *testing.T) fluidsOut {
	t.Helper()
	resp := f.e.do("GET", "/admin/fluids?from=2026-10-01&to=2026-11-01&group_by=org", master, "")
	body := readAll(t, resp)
	var o fluidsOut
	if err := json.Unmarshal([]byte(body), &o); err != nil || resp.StatusCode != 200 {
		t.Fatalf("%d %s %v", resp.StatusCode, body, err)
	}
	return o
}

// The node's energy beyond the GPUs, from its BMC's counter: the node's energy
// minus the GPUs' is split like theirs (the active part by the same weights,
// the idle part as idle), the totals are exact, a wrong password reads nothing,
// and the preemptions of vLLM are counted per interval, a restart of its
// counter included.
func TestNodeEnergyFromTheBMCAndPreemptions(t *testing.T) {
	f := nodeFixtureFor(t, 1, true)
	t0 := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	f.round(0) // baselines
	f.records(t, t0)
	// 10:00-10:05: the GPUs used 2 kWh, the node 2.5 kWh (0.5 beyond them); 3
	// requests were preempted.
	f.dc.add("GPU-aaa", kWhMJ)
	f.dc.add("GPU-bbb", kWhMJ)
	f.bmc.add(2.5)
	f.vm.n.Store(3)
	f.round(5 * time.Minute)
	// 10:05-10:10: nobody works; GPUs 1 kWh, the node 1.2 kWh: 0.2 beyond, idle.
	f.dc.add("GPU-aaa", kWhMJ/2)
	f.dc.add("GPU-bbb", kWhMJ/2)
	f.bmc.add(1.2)
	f.round(5 * time.Minute)
	// 10:10-10:15: vLLM restarted (its counter went down to 2): 2 preemptions.
	f.dc.add("GPU-aaa", kWhMJ/2)
	f.bmc.add(0.5)
	f.vm.n.Store(2)
	f.round(5 * time.Minute)

	o := f.report(t)
	by := map[string]FluidsRow{}
	var sum float64
	for _, r := range o.Rows {
		by[r.Org] = r
		sum += r.KWh
	}
	// 2.5 + 1.2 + 0.5 measured at the node.
	if !near3(sum, 4.2) {
		t.Fatalf("the orgs add up to %v kWh, the node used 4.2: %+v", sum, o.Rows)
	}
	// Active: the 2 kWh of GPUs and the 0.5 beyond them, split 25 / 75 %.
	if !near3(by["etabA"].KWhActive, 0.625) || !near3(by["etabB"].KWhActive, 1.875) {
		t.Fatalf("active %v / %v", by["etabA"].KWhActive, by["etabB"].KWhActive)
	}
	// Idle: 1.2 + 0.5 kWh, by share (0.75 / 0.25).
	if !near3(by["etabA"].KWhIdle, 1.275) || !near3(by["etabB"].KWhIdle, 0.425) {
		t.Fatalf("idle %v / %v", by["etabA"].KWhIdle, by["etabB"].KWhIdle)
	}
	// The preemption measurement: 3 intervals, 2 with preemptions (3, then the
	// restarted counter's 2), the energy of those intervals.
	if len(o.Preemptions) != 1 {
		t.Fatalf("preemptions %+v", o.Preemptions)
	}
	p := o.Preemptions[0]
	if p.Intervals != 3 || p.Known != 3 || p.WithPreemptions != 2 || p.Preemptions != 5 || !near3(p.KWh, 4.2) || !near3(p.KWhPreempted, 3.0) {
		t.Fatalf("preemption row %+v", p)
	}
	if f.bmc.unauthorised() != 0 {
		t.Fatal("the BMC refused the credentials")
	}
}

// A BMC that cannot be read: the fallback factor gives the overhead; with no
// factor there is none; a BMC that answers less than the GPUs adds nothing.
func TestNodeEnergyFallbackFactor(t *testing.T) {
	f := nodeFixtureFor(t, 1.25, true)
	f.round(0)
	f.bmc.setFail(true)
	f.dc.add("GPU-aaa", kWhMJ)
	f.dc.add("GPU-bbb", kWhMJ) // 2 kWh of GPUs
	f.round(5 * time.Minute)
	o := f.report(t)
	var sum float64
	for _, r := range o.Rows {
		sum += r.KWh
	}
	if !near3(sum, 2.5) { // 2 × 1.25
		t.Fatalf("with the factor: %v kWh, want 2.5", sum)
	}

	g := nodeFixtureFor(t, 1, false) // no BMC, no factor: the GPUs only
	g.round(0)
	g.dc.add("GPU-aaa", kWhMJ)
	g.round(5 * time.Minute)
	sum = 0
	for _, r := range g.report(t).Rows {
		sum += r.KWh
	}
	if !near3(sum, 1) {
		t.Fatalf("no overhead configured: %v kWh", sum)
	}

	h := nodeFixtureFor(t, 1, true)
	h.round(0)
	h.dc.add("GPU-aaa", kWhMJ)
	h.dc.add("GPU-bbb", kWhMJ)
	h.bmc.add(1.5) // less than the GPUs' 2 kWh: nonsense, not counted
	h.round(5 * time.Minute)
	sum = 0
	for _, r := range h.report(t).Rows {
		sum += r.KWh
	}
	if !near3(sum, 2) {
		t.Fatalf("a BMC below the GPUs: %v kWh", sum)
	}
}

// attributeInterval: the overhead is split like the GPUs' energy and the parts
// add up to the GPUs' plus the overhead, to the millijoule.
func TestAttributeIntervalWithOverhead(t *testing.T) {
	f := fluidsConfig("http://x", "http://y").Fluids
	f.IdleWatts = 100 // 100 W per GPU idle floor
	iv := energy.Interval{Start: time.Unix(0, 0), End: time.Unix(10, 0), MJ: 2_500_000, PerGPU: map[string]int64{"a": 1_500_000, "b": 1_000_000}}
	work := []energy.Work{{Consumer: energy.Consumer{Org: "x"}, Prefill: 1, Decode: 1}, {Consumer: energy.Consumer{Org: "y"}, Prefill: 3, Decode: 3}}
	for _, oh := range []int64{0, 1, 999_999, 1_000_001} {
		ei, att := attributeInterval("n", "p", pendingInterval{iv: iv, overheadMJ: oh, overheadBy: "redfish", preempt: 2}, work, f)
		var a int64
		for _, x := range att {
			a += x.MJ
		}
		if ei.TotalMJ != iv.MJ+oh || a+ei.IdleMJ != ei.TotalMJ || ei.OverheadMJ != oh || ei.Preemptions != 2 {
			t.Fatalf("overhead %d: %+v active %d", oh, ei, a)
		}
	}
	// The GPUs' idle floor is 1 000 000 mJ each (100 W × 10 s), at most what they
	// used: idle 2 000 000... bounded by use: 1 000 000 + 1 000 000, active 500 000.
	// An overhead of 1 000 000 splits 2:8 (idle:active in the proportion of the
	// GPUs: active 500 000, idle 2 000 000): 800 000 idle, 200 000 active.
	ei, att := attributeInterval("n", "p", pendingInterval{iv: iv, overheadMJ: 1_000_000, overheadBy: "redfish"}, work, f)
	var a int64
	for _, x := range att {
		a += x.MJ
	}
	if a != 500_000+200_000 || ei.IdleMJ != 2_000_000+800_000 {
		t.Fatalf("active %d idle %d", a, ei.IdleMJ)
	}
}

// The Redfish scraper reads over TLS with basic authentication, refuses what
// it should, and never leaks the password.
func TestRedfishScraper(t *testing.T) {
	b := newFakeBMC(t, "admin", "s3cret")
	b.add(12.5)
	cl := b.srv.Client()
	sc := energy.RedfishScraper{URL: b.url(), User: "admin", Password: "s3cret", Client: cl}
	rd, err := sc.Scrape(context.Background(), time.Now())
	if err != nil || rd.MJ[energy.RedfishKey] != uint64(12.5*kWhMJ) {
		t.Fatalf("%+v %v", rd, err)
	}
	bad := energy.RedfishScraper{URL: b.url(), User: "admin", Password: "wrong", Client: cl}
	if _, err := bad.Scrape(context.Background(), time.Now()); err == nil {
		t.Fatal("a wrong password was accepted")
	}
	// A client that does not trust the BMC's certificate: an error that does
	// not carry the password.
	_, err = (&energy.RedfishScraper{URL: b.url(), User: "admin", Password: "s3cret", Client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{}}}}).Scrape(context.Background(), time.Now())
	if err == nil || strings.Contains(err.Error(), "s3cret") {
		t.Fatalf("untrusted certificate: %v", err)
	}
	// Plain http is refused: the password would travel in clear.
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	if _, err := (&energy.RedfishScraper{URL: plain.URL, User: "a", Password: "b"}).Scrape(context.Background(), time.Now()); err == nil {
		t.Fatal("http accepted")
	}
}

// vLLM's preemption counter: its two names, label sets added up, a missing
// metric read as unknown.
func TestParseMetricSumReadsThePreemptionCounter(t *testing.T) {
	text := "# TYPE vllm:num_preemptions_total counter\n" +
		"vllm:num_preemptions_total{engine=\"0\",model_name=\"a\"} 3.0\n" +
		"vllm:num_preemptions_total{engine=\"1\",model_name=\"a\"} 4\n" +
		"vllm:num_preemptions_created{engine=\"0\"} 1.7e9\n" +
		"vllm:num_requests_waiting 5\n"
	v, ok := parseMetricSum(strings.NewReader(text), preemptionMetrics...)
	if !ok || v != 7 {
		t.Fatalf("%v %v", v, ok)
	}
	// The name without the suffix.
	if v, ok := parseMetricSum(strings.NewReader("vllm:num_preemptions 9\n"), preemptionMetrics...); !ok || v != 9 {
		t.Fatalf("%v %v", v, ok)
	}
	if _, ok := parseMetricSum(strings.NewReader("vllm:num_requests_waiting 5\n"), preemptionMetrics...); ok {
		t.Fatal("a missing metric was read")
	}
	// The queue parser is unchanged.
	if parseWaiting(strings.NewReader("vllm:num_requests_waiting{engine=\"0\"} 3.0\nvllm:num_requests_waiting_by_reason 9\n")) != 3 || parseWaiting(strings.NewReader("x 1\n")) != -1 {
		t.Fatal("parseWaiting changed")
	}
}

func overheadBy(o fluidsOut) map[string]NodeOverheadOut {
	m := map[string]NodeOverheadOut{}
	for _, r := range o.Overhead {
		m[r.Source] = r
	}
	return m
}

func totalKWh(o fluidsOut) (sum float64) {
	for _, r := range o.Rows {
		sum += r.KWh
	}
	return
}

// A BMC that reports only a power: its trapezoid, minus the GPUs', is the
// node's overhead; a missed read leaves the interval to the factor (stamped
// as such), and the next interval, whose BMC samples are not the GPUs', too.
func TestNodeEnergyFromBMCPowerWithAGap(t *testing.T) {
	f := nodeFixtureFor(t, 1.5, true)
	f.bmc.setPower(1200)                                                                  // 1200 W: 0.1 kWh per 5 minutes
	gpus := func() { f.dc.add("GPU-aaa", kWhMJ*3/100); f.dc.add("GPU-bbb", kWhMJ*3/100) } // 0.06 kWh
	f.round(0)
	gpus()
	f.round(5 * time.Minute) // 0.1 from the BMC: overhead 0.04
	gpus()
	f.round(5 * time.Minute) // 0.04
	f.bmc.setFail(true)
	gpus()
	f.round(5 * time.Minute) // BMC down: factor, 0.06 x 0.5 = 0.03
	f.bmc.setFail(false)
	gpus()
	f.round(5 * time.Minute) // the BMC's interval spans two rounds: factor, 0.03
	gpus()
	f.round(5 * time.Minute) // aligned again: 0.04

	o := f.report(t)
	by := overheadBy(o)
	if p := by["redfish_power"]; p.Intervals != 3 || !near3(p.KWh, 0.12) {
		t.Fatalf("power: %+v (all %+v)", p, o.Overhead)
	}
	if fa := by["factor"]; fa.Intervals != 2 || !near3(fa.KWh, 0.06) {
		t.Fatalf("factor: %+v (all %+v)", fa, o.Overhead)
	}
	if !near3(totalKWh(o), 5*0.06+0.12+0.06) {
		t.Fatalf("the orgs add up to %v kWh", totalKWh(o))
	}
}

// A counter that goes down (a BMC reboot) is never billed as negative or as the
// whole new value: the interval falls to the factor.
func TestNodeEnergyBMCCounterResetIsNotBilled(t *testing.T) {
	f := nodeFixtureFor(t, 1.5, true)
	gpus := func() { f.dc.add("GPU-aaa", kWhMJ/5); f.dc.add("GPU-bbb", kWhMJ/5) } // 0.4 kWh
	f.bmc.set(10)
	f.round(0)
	gpus()
	f.bmc.add(0.6) // 0.2 beyond the GPUs
	f.round(5 * time.Minute)
	gpus()
	f.bmc.set(0.3) // rebooted: the counter went from 10.6 down to 0.3
	f.round(5 * time.Minute)
	gpus()
	f.bmc.add(0.5) // 0.1 beyond the GPUs
	f.round(5 * time.Minute)

	o := f.report(t)
	by := overheadBy(o)
	if r := by["redfish"]; r.Intervals != 2 || !near3(r.KWh, 0.3) {
		t.Fatalf("counter: %+v (all %+v)", r, o.Overhead)
	}
	if fa := by["factor"]; fa.Intervals != 1 || !near3(fa.KWh, 0.2) {
		t.Fatalf("the reset interval: %+v (all %+v)", fa, o.Overhead)
	}
	if !near3(totalKWh(o), 3*0.4+0.3+0.2) {
		t.Fatalf("the orgs add up to %v kWh", totalKWh(o))
	}
}
