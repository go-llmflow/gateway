package gateway

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// fallbackConfig: llama (pool big, upstream a) falls back to llama-small
// (pool small, upstream b), priced differently.
func fallbackConfig(t *testing.T, a, b string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(fmt.Sprintf(`
master_key_file = "/unused"
cache_salt_key_file = "/unused"
budget_refresh  = "1h"
key_cache_ttl   = "1h"
health {
  interval = "1h"
  cooldown = "1h"
}
org "etabA" {
  share = 0.75
  team "etabA" {}
}
org "etabB" {
  share = 0.25
  team "etabB" {}
}
pool "big" { capacity = 4 }
pool "small" { capacity = 2 }
model "llama" {
  upstream_model = "meta/llama-70b"
  pool           = "big"
  key_file       = "/unused"
  fallbacks      = ["llama-small"]
  cost {
    input_per_mtok  = 1000
    output_per_mtok = 5000
  }
  upstream "a" { base_url = %q }
}
model "llama-small" {
  upstream_model = "meta/llama-8b"
  pool           = "small"
  key_file       = "/unused"
  cost {
    input_per_mtok  = 100
    output_per_mtok = 500
  }
  upstream "b" { base_url = %q }
}
`, a, b)), "test.hcl")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFallbackOnUpstream503(t *testing.T) {
	a, b := newFakeVLLM(t), newFakeVLLM(t)
	e := newEnv(t, fallbackConfig(t, a.URL(), b.URL()))
	_, key := e.newKey("etabA")

	a.mu.Lock()
	a.status = 503
	a.mu.Unlock()
	resp := e.do("POST", "/v1/chat/completions", key, chat)
	out := readAll(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("X-Llmflow-Model") != "llama-small" {
		t.Fatalf("%d %s %v", resp.StatusCode, out, resp.Header)
	}
	got := b.last()
	if got.Body["model"] != "meta/llama-8b" {
		t.Fatalf("fallback got model %v", got.Body["model"])
	}
	// Charged to the model that served, in its pool, with the priority it
	// was given there (the header vLLM got).
	r := waitRecord(t, e.st, func(r store.Record) bool { return r.Status == 200 })
	prio, _ := strconv.Atoi(got.Header.Get("X-Vllm-Priority"))
	if r.Model != "llama-small" || r.Pool != "small" || r.Priority != prio || r.Cost <= 0 || r.Cost > 0.02 {
		t.Fatalf("%+v (priority header %d)", r, prio)
	}
	if m := readAll(t, e.do("GET", "/metrics", "", "")); !strings.Contains(m, `llmflow_fallbacks_total{from="llama",to="llama-small",reason="unavailable"} 1`) {
		t.Fatalf("metric missing")
	}
}

// saturationConfig: llama (pool big, C=2) falls back to llama-small (pool
// small, C=2) on "saturated" too. etabA has 0.75 of every pool (1 slot of 2
// within share, by Hamilton), etabB 0.25 (none: it always borrows).
func saturationConfig(t *testing.T, a, b string, extra string) *config.Config {
	t.Helper()
	c, err := config.Parse([]byte(fmt.Sprintf(`
master_key_file = "/unused"
cache_salt_key_file = "/unused"
budget_refresh  = "1h"
key_cache_ttl   = "1h"
health {
  interval = "1h"
  cooldown = "1h"
}
org "etabA" {
  share = 0.75
  team "etabA" {}
}
org "etabB" {
  share = 0.25
  team "etabB" {}
}
pool "big" { capacity = 2 }
pool "small" { capacity = 2 }
model "llama" {
  upstream_model = "meta/llama-70b"
  pool           = "big"
  key_file       = "/unused"
  fallbacks      = ["llama-small"]
  fallback_on    = ["unavailable", "saturated"]
  %s
  upstream "a" { base_url = %q }
}
model "llama-small" {
  upstream_model = "meta/llama-8b"
  pool           = "small"
  key_file       = "/unused"
  fallbacks      = ["llama"]
  fallback_on    = ["unavailable", "saturated"]
  upstream "b" { base_url = %q }
}
`, extra, a, b)), "test.hcl")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// idleAll waits until no pool has anything in flight.
func idleAll(t *testing.T, e *env) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		busy := 0
		for _, p := range e.srv.pools {
			for _, s := range p.snapshot() {
				busy += s.InFlight + s.Borrowing
			}
		}
		if busy == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d still in flight", busy)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// held starts n requests that the upstream keeps waiting on gate, and
// waits until it holds them.
func held(t *testing.T, e *env, up *fakeVLLM, key string, n int, done chan<- int) {
	t.Helper()
	before := up.count()
	for i := 0; i < n; i++ {
		go func() { done <- e.chatWith(key, chat) }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for up.count() < before+n {
		if time.Now().After(deadline) {
			t.Fatalf("the upstream holds %d of %d", up.count()-before, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// #58: the primary pool is full (C=2 in flight); a BORROWING request moves
// to the fallback group, whose pool has room, and is charged there; a
// within-share request stays and queues in its own pool.
func TestFallbackOnSaturationMovesBorrowersOnly(t *testing.T) {
	a, b := newFakeVLLM(t), newFakeVLLM(t)
	a.gate = make(chan struct{})
	e := newEnv(t, saturationConfig(t, a.URL(), b.URL(), ""))
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	done := make(chan int, 8)
	// Fill the big pool: B borrows twice (A has nothing in flight yet).
	held(t, e, a, keyB, 2, done)

	// A borrower arrives: B again. It goes to llama-small at once.
	resp := e.do("POST", "/v1/chat/completions", keyB, chat)
	readAll(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("X-Llmflow-Model") != "llama-small" || b.count() != 1 {
		t.Fatalf("borrower on a saturated pool: %d, model %q, fallback hits %d", resp.StatusCode, resp.Header.Get("X-Llmflow-Model"), b.count())
	}
	r := waitRecord(t, e.st, func(r store.Record) bool { return r.Model == "llama-small" })
	if r.Pool != "small" || r.Team != "etabB" {
		t.Fatalf("charged %+v", r)
	}

	// A within-share request of etabA: it stays in big and waits there.
	held(t, e, a, keyA, 1, done)
	if b.count() != 1 {
		t.Fatalf("a within-share request left its reservation (fallback hits %d)", b.count())
	}
	close(a.gate)
	for i := 0; i < 3; i++ {
		if st := <-done; st != 200 {
			t.Fatalf("held request: %d", st)
		}
	}
	m := readAll(t, e.do("GET", "/metrics", "", ""))
	if !strings.Contains(m, `llmflow_fallbacks_total{from="llama",to="llama-small",reason="saturated"} 1`) {
		t.Fatalf("metric: %s", m)
	}
	idleAll(t, e)
}

// The default (fallback_on absent = ["unavailable"]) keeps today's
// behaviour: a borrower on a saturated pool queues where it is.
func TestSaturationIgnoredByDefault(t *testing.T) {
	a, b := newFakeVLLM(t), newFakeVLLM(t)
	a.gate = make(chan struct{})
	e := newEnv(t, fallbackConfig(t, a.URL(), b.URL()))
	_, keyB := e.newKey("etabB")
	done := make(chan int, 8)
	held(t, e, a, keyB, 5, done) // pool big has C=4
	close(a.gate)
	for i := 0; i < 5; i++ {
		<-done
	}
	if b.count() != 0 {
		t.Fatalf("fell back on saturation without fallback_on: %d", b.count())
	}
}

// Chaos: both pools saturated. Nothing ping-pongs between the two groups
// (each falls back to the other): the borrower queues in its own pool.
func TestBothPoolsSaturatedNoPingPong(t *testing.T) {
	a, b := newFakeVLLM(t), newFakeVLLM(t)
	a.gate, b.gate = make(chan struct{}), make(chan struct{})
	e := newEnv(t, saturationConfig(t, a.URL(), b.URL(), ""))
	_, keyB := e.newKey("etabB")
	done := make(chan int, 16)
	held(t, e, a, keyB, 2, done) // big full
	// Two more borrowers move to small and fill it.
	held(t, e, b, keyB, 2, done)
	aBefore, bBefore := a.count(), b.count()
	// A fifth borrower: no room anywhere. It must queue on big, once.
	go func() { done <- e.chatWith(keyB, chat) }()
	deadline := time.Now().Add(5 * time.Second)
	for a.count()+b.count() < aBefore+bBefore+1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if a.count() != aBefore+1 || b.count() != bBefore {
		t.Fatalf("with both pools full: big +%d, small +%d, want +1 on big only", a.count()-aBefore, b.count()-bBefore)
	}
	close(a.gate)
	close(b.gate)
	for i := 0; i < 5; i++ {
		if st := <-done; st != 200 {
			t.Fatalf("status %d", st)
		}
	}
	m := readAll(t, e.do("GET", "/metrics", "", ""))
	if strings.Contains(m, `from="llama-small",to="llama",reason="saturated"`) {
		t.Fatalf("a request came back: %s", m)
	}
	idleAll(t, e)
}

// Signal 2: vLLM's own queue (vllm:num_requests_waiting), scraped from the
// upstream's /metrics, marks the group saturated even when the gateway's
// pool is not full. The exposition below is what vLLM 0.32.1rc1.dev505
// (the go-llmflow/bench E2E image) serves, read from its /metrics on
// 2026-10-26, with the waiting count changed: note the _by_reason metric
// that shares the name's prefix and must not be added.
func TestQueueMetricSaturation(t *testing.T) {
	const metrics = "# HELP vllm:num_requests_waiting Number of requests waiting to be processed.\n" +
		"# TYPE vllm:num_requests_waiting gauge\n" +
		`vllm:num_requests_waiting{engine="0",model_name="Qwen/Qwen2.5-0.5B-Instruct"} 3.0` + "\n" +
		"# TYPE vllm:num_requests_waiting_by_reason gauge\n" +
		`vllm:num_requests_waiting_by_reason{engine="0",model_name="Qwen/Qwen2.5-0.5B-Instruct",reason="capacity"} 3.0` + "\n" +
		`vllm:num_requests_waiting_by_reason{engine="0",model_name="Qwen/Qwen2.5-0.5B-Instruct",reason="deferred"} 0.0` + "\n"
	if got := parseWaiting(strings.NewReader(metrics)); got != 3 {
		t.Fatalf("parseWaiting = %d", got)
	}
	if got := parseWaiting(strings.NewReader("vllm:num_requests_running 2\n")); got != -1 {
		t.Fatalf("absent metric = %d, want -1", got)
	}
	a, b := newFakeVLLM(t), newFakeVLLM(t)
	e := newEnv(t, saturationConfig(t, a.URL(), b.URL(), "saturation_waiting = 2"))
	_, keyB := e.newKey("etabB")
	g := e.srv.groups["llama"]
	// The health loop's first round scrapes the fake upstream (no queue
	// metric: -1). Planting the value before that round ends let the scrape
	// overwrite it under load (failed once at a load of 34): wait for it.
	<-e.srv.probed
	g.members()[0].waiting.Store(5) // as the health loop scraped it
	expect(t, "borrower while vLLM queues", e.chatWith(keyB, chat), 200)
	if b.count() != 1 {
		t.Fatalf("vLLM's queue was not taken as saturation: fallback hits %d", b.count())
	}
	g.members()[0].waiting.Store(-1) // unknown: never saturated
	expect(t, "unknown queue", e.chatWith(keyB, chat), 200)
	if b.count() != 1 {
		t.Fatalf("an unknown queue counted as saturated")
	}
}

// Security audit 2026-10-26 (verified, no finding): a request moved to a
// fallback keeps its own tenant's cache salt and scoped uuids (two
// organisations still never share a salt in the target pool), and the
// client's salt does not survive the second rewrite.
func TestFallbackKeepsTenantIsolation(t *testing.T) {
	a, b := newFakeVLLM(t), newFakeVLLM(t)
	e := newEnv(t, fallbackConfig(t, a.URL(), b.URL()))
	_, ka := e.newKey("etabA")
	_, kb := e.newKey("etabB")
	saltDirect := saltOf(t, e, a, ka, mmChat)
	a.mu.Lock()
	a.status = 503
	a.mu.Unlock()
	saltA := saltOf(t, e, b, ka, mmChat)
	uuidA := b.last().Body["messages"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)["uuid"]
	saltB := saltOf(t, e, b, kb, mmChat)
	if saltA != saltDirect || saltA == saltB || saltA == "client-chosen" || saltB == "client-chosen" {
		t.Fatalf("salts: direct %q, fallback A %q, fallback B %q", saltDirect, saltA, saltB)
	}
	if u, _ := uuidA.(string); !strings.HasPrefix(u, "llmf-") {
		t.Fatalf("uuid on the fallback: %v", uuidA)
	}
}

func TestFallbackOnUpstreamDown(t *testing.T) {
	a, b := newFakeVLLM(t), newFakeVLLM(t)
	cfg := fallbackConfig(t, a.URL(), b.URL())
	a.srv.Close() // connection refused
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	if resp := e.do("POST", "/v1/chat/completions", key, chat); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if b.count() != 1 {
		t.Fatalf("fallback hit %d times", b.count())
	}
}

func TestNoFallback(t *testing.T) {
	a, b := newFakeVLLM(t), newFakeVLLM(t)
	e := newEnv(t, fallbackConfig(t, a.URL(), b.URL()))
	_, key := e.newKey("etabA")

	// A client error is not saturation: no fallback.
	a.mu.Lock()
	a.status = 400
	a.mu.Unlock()
	if resp := e.do("POST", "/v1/chat/completions", key, chat); readAll(t, resp) == "" || resp.StatusCode != 400 || b.count() != 0 {
		t.Fatalf("400: %d, fallback hit %d", resp.StatusCode, b.count())
	}
	// The team may not use the fallback: the saturation answer stands.
	a.mu.Lock()
	a.status = 503
	a.mu.Unlock()
	if resp := e.do("PUT", "/admin/teams/etabA/models", master, `{"models":["llama"]}`); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("models: %d", resp.StatusCode)
	}
	e.srv.budget.refresh(t.Context())
	if resp := e.do("POST", "/v1/chat/completions", key, chat); readAll(t, resp) == "" || resp.StatusCode != 503 || b.count() != 0 {
		t.Fatalf("hidden fallback: %d, hit %d", resp.StatusCode, b.count())
	}
	// Over budget on the fallback model: not used either. The control
	// first: before the budget, etabB does fall back.
	_, keyB := e.newKey("etabB")
	if resp := e.do("POST", "/v1/chat/completions", keyB, chat); readAll(t, resp) == "" || resp.StatusCode != 200 || b.count() != 1 {
		t.Fatalf("control: %d, hit %d", resp.StatusCode, b.count())
	}
	e.setBudget(`{"kind":"team","name":"etabB","model":"llama-small","period":"day","max_cost":0.0001}`)
	if resp := e.do("POST", "/v1/chat/completions", keyB, chat); readAll(t, resp) == "" || resp.StatusCode != 503 || b.count() != 1 {
		t.Fatalf("over budget: %d, hit %d", resp.StatusCode, b.count())
	}
}
