package gateway

import (
	"fmt"
	"testing"

	"github.com/go-llmflow/gateway/config"
)

// A guard call runs in the band of the request it judges (audit 2026-11-07: it
// ran at priority 0, ahead of every owner's work, for a spare-only borrower).
func TestAGuardCallRunsInTheCallersPriorityBand(t *testing.T) {
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
  capacity = 4
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
`, up.URL(), gup.URL())), "guardpriority.hcl")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, cfg)
	_, keyB := e.newKey("etabB")
	resp := e.do("POST", "/v1/chat/completions", keyB, fmt.Sprintf(sparePrompt, "llama", "B"))
	readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("the borrower was refused: %d", resp.StatusCode)
	}
	if gup.count() == 0 {
		t.Fatal("no guard call")
	}
	got, want := gup.last().Header.Get("X-Vllm-Priority"), resp.Header.Get("X-Llmflow-Priority")
	if got == "0" || got != want {
		t.Errorf("the guard call ran at priority %q; the request's own band is %q (a borrower is never at 0)", got, want)
	}
}
