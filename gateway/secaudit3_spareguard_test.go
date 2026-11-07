package gateway

// Security audit 3, 2026-11-07 (untracked proof). A spare-only borrower
// (borrow = "spare", the ILaaS third party) on a FULL pool is refused 429 at
// proxy.go:370-374, but only after its pre-call guardrails ran at
// proxy.go:322-368: every refused request still sends a guard-model call, at
// vLLM priority 0 (the highest, guardrails.go:498-505, 530) and outside the
// fair-share pool. The comment at proxy.go:316-317 says the guard "is not spent
// on a request refused for cheaper reasons"; the spare refusal is one.

import (
	"fmt"
	"testing"

	"github.com/go-llmflow/gateway/config"
)

func TestSecaudit3SpareRefusalStillRunsGuardModel(t *testing.T) {
	up, gup := newFakeVLLM(t), newFakeVLLM(t)
	gup.setReply("safe")
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
  capacity = 1
  shares   = { etabA = 1 }
  borrow   = "spare"
}
pool "guard" { capacity = 2 }
model "llama" {
  upstream_model = "meta/llama"
  pool           = "b300"
  key_file       = "/unused"
  upstream "u0" { base_url = %q }
}
model "llama-guard" {
  upstream_model = "meta-llama/Llama-Guard-3-8B"
  pool           = "guard"
  class          = "guard"
  key_file       = "/unused"
  upstream "g0" { base_url = %q }
}
guardrail "lg" {
  type   = "llama_guard"
  model  = "llama-guard"
  orgs   = ["etabB"]
  action = "block"
}
`, up.URL(), gup.URL())), "spareguard.hcl")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, cfg)
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	up.gate = make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		readAll(t, e.do("POST", "/v1/chat/completions", keyA, fmt.Sprintf(sparePrompt, "llama", "A0")))
	}()
	waitUpstream(t, up, 1) // the owner holds the pool's only slot
	const n = 5
	refused := 0
	for i := 0; i < n; i++ {
		resp := e.do("POST", "/v1/chat/completions", keyB, fmt.Sprintf(sparePrompt, "llama", fmt.Sprintf("B%d", i)))
		readAll(t, resp)
		if resp.StatusCode == 429 {
			refused++
		}
	}
	close(up.gate)
	<-done
	if refused != n {
		t.Fatalf("the borrower was refused %d times of %d", refused, n)
	}
	if g := gup.count(); g != 0 {
		t.Errorf("EXPLOIT: %d requests refused for lack of spare capacity still made %d priority-0 guard-model calls (want 0)", n, g)
	}
	if gup.count() > 0 {
		t.Logf("guard call priority header: %q", gup.last().Header.Get("X-Vllm-Priority"))
	}
}
