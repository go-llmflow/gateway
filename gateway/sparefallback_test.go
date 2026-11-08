package gateway

import (
	"fmt"
	"testing"

	"github.com/go-llmflow/gateway/config"
)

// The slot taken for a request that moves to a fallback group goes through the
// same test as the first one: a spare-only organisation (borrow = "spare", no
// share) takes it only while vLLM's queue is not saturated either. The fallback
// loop called the pool's tryAcquire directly and skipped that test (audit
// 2026-11-07).
func TestASpareOnlyOrganisationDoesNotTakeAFallbackSlotOnASaturatedQueue(t *testing.T) {
	a, b := newFakeVLLM(t), newFakeVLLM(t)
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
pool "big" {
  capacity = 2
  shares   = { etabA = 1 }
  borrow   = "spare"
}
pool "small" {
  capacity = 2
  shares   = { etabA = 1 }
  borrow   = "spare"
}
model "llama" {
  upstream_model = "meta/llama-70b"
  pool           = "big"
  key_file       = "/unused"
  fallbacks      = ["llama-small"]
  fallback_on    = ["unavailable", "saturated"]
  upstream "a" { base_url = %q }
}
model "llama-small" {
  upstream_model = "meta/llama-8b"
  pool           = "small"
  key_file       = "/unused"
  fallbacks      = ["llama"]
  fallback_on    = ["unavailable", "saturated"]
  saturation_waiting = 2
  upstream "b" { base_url = %q }
}
`, a.URL(), b.URL())), "sparefallback.hcl")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, cfg)
	<-e.srv.probed
	small := e.srv.groups["llama-small"]
	if !e.srv.cfg.PoolSpareOnly("small", "etabB") {
		t.Fatal("setup: etabB is not spare-only in the pool")
	}

	small.members()[0].waiting.Store(5) // as the health loop scraped it: vLLM queues
	if g, ok := e.srv.tryAcquireFor(small, "etabB", 10, 1); ok {
		g.Release(0)
		t.Fatal("a spare-only organisation took a slot in a pool whose vLLM queue is saturated")
	}
	if g, ok := e.srv.tryAcquireFor(small, "etabA", 10, 1); !ok { // an owner is not held to it
		t.Fatal("an owner was refused")
	} else {
		g.Release(0)
	}
	small.members()[0].waiting.Store(-1) // unknown: never saturated
	g, ok := e.srv.tryAcquireFor(small, "etabB", 10, 1)
	if !ok {
		t.Fatal("a spare-only organisation was refused with room and no queue")
	}
	g.Release(0)
}
