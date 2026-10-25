package gateway

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

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

func TestFallbackOnSaturation(t *testing.T) {
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
	if m := readAll(t, e.do("GET", "/metrics", "", "")); !strings.Contains(m, `llmflow_fallbacks_total{from="llama",to="llama-small"} 1`) {
		t.Fatalf("metric missing")
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
