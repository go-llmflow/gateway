package gateway

import (
	"fmt"
	"testing"

	"github.com/go-llmflow/gateway/config"
)

// A request that asks for n completions runs n sequences in vLLM at once, so
// it holds n slots (audit 2026-11-08: n and best_of only changed a statistic;
// fair share and admission counted requests, so one request with n=64 cost what
// a request with n=1 cost). Proven on the pool's in-flight count, and on a
// spare-only borrower: it needs room for every choice, or is refused whole.
func TestARequestHoldsOneSlotPerChoice(t *testing.T) {
	const body = `{"model":"llama","max_tokens":10,%s"messages":[{"role":"user","content":"hello"}]}`
	for _, tc := range []struct {
		name, extra string
		slots       int
	}{
		{"one choice", ``, 1},
		{"n = 4", `"n":4,`, 4},
		{"best_of = 6", `"best_of":6,`, 6},
		{"n = 3, best_of = 5", `"n":3,"best_of":5,`, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeVLLM(t)
			e := newEnv(t, testConfig(up.URL()))
			_, key := e.newKey("etabA")
			up.gate = make(chan struct{})
			done := make(chan struct{})
			go func() {
				defer close(done)
				readAll(t, e.do("POST", "/v1/chat/completions", key, fmt.Sprintf(body, tc.extra)))
			}()
			waitUpstream(t, up, 1)
			var inflight int
			for _, s := range e.srv.groups["llama"].pool.snapshot() {
				if s.Team == "etabA" {
					inflight = s.InFlight
				}
			}
			close(up.gate)
			<-done
			if inflight != tc.slots {
				t.Errorf("%d slots held, want %d", inflight, tc.slots)
			}
			// Released with the request.
			for _, s := range e.srv.groups["llama"].pool.snapshot() {
				if s.InFlight != 0 {
					t.Errorf("%s still holds %d slots after the request", s.Team, s.InFlight)
				}
			}
		})
	}
}

func TestASpareOnlyBorrowerNeedsRoomForEveryChoice(t *testing.T) {
	up := newFakeVLLM(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(`
master_key_file = "/unused"
cache_salt_key_file = "/unused"
budget_refresh  = "1h"
key_cache_ttl   = "1h"
health {
  interval = "1h"
  cooldown = "1h"
}
org "etabA" {
  share = 1
  team "etabA" {}
}
org "etabB" {
  share = 0
  team "etabB" {}
}
pool "b300" {
  capacity = 3
  shares   = { etabA = 1 }
  borrow   = "spare"
}
model "llama" {
  upstream_model = "meta/llama"
  pool           = "b300"
  key_file       = "/unused"
  upstream "u0" { base_url = %q }
}
`, up.URL())), "slots.hcl")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, cfg)
	_, keyB := e.newKey("etabB")
	four := `{"model":"llama","max_tokens":5,"n":4,"messages":[{"role":"user","content":"x"}]}`
	two := `{"model":"llama","max_tokens":5,"n":2,"messages":[{"role":"user","content":"x"}]}`
	resp := e.do("POST", "/v1/chat/completions", keyB, four)
	if readAll(t, resp); resp.StatusCode != 429 {
		t.Fatalf("n=4 in a pool of 3: %d, want 429 (no room for all its choices)", resp.StatusCode)
	}
	for _, s := range e.srv.groups["llama"].pool.snapshot() {
		if s.InFlight != 0 {
			t.Fatalf("a refused request left %d slots of %s held", s.InFlight, s.Team)
		}
	}
	resp = e.do("POST", "/v1/chat/completions", keyB, two)
	if readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("n=2 in a pool of 3: %d", resp.StatusCode)
	}
}
