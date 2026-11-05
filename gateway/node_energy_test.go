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
	mu   sync.Mutex
	kwh  float64
	fail bool
	srv  *httptest.Server
	ca   string // path of its certificate, as a CA file
	auth atomic.Int64
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

func (b *fakeBMC) url() string       { return b.srv.URL + "/redfish/v1/Chassis/1/EnvironmentMetrics" }
func (b *fakeBMC) add(kwh float64)   { b.mu.Lock(); b.kwh += kwh; b.mu.Unlock() }
func (b *fakeBMC) setFail(f bool)    { b.mu.Lock(); b.fail = f; b.mu.Unlock() }
func (b *fakeBMC) unauthorised() int { return int(b.auth.Load()) }

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
