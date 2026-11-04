package gateway

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/fairshare"
)

const sparePrompt = `{"model":"%s","max_tokens":5,"messages":[{"role":"user","content":"%s"}]}`

func waitUpstream(t *testing.T, up *fakeVLLM, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for up.count() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d requests reached the upstream, want %d", up.count(), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// borrow = "spare": the owners (etabA, etabC, etabD hold the rtx machine) fill
// its C slots; the third party (etabB, no share of it) is refused with a 429
// and a Retry-After that says nothing of the owners. With room left, it is
// admitted in the last priority band.
func TestPoolBorrowSpare(t *testing.T) {
	e, up := newPoolsEnv(t, 2, `  borrow   = "spare"`)
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	up.gate = make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			readAll(t, e.do("POST", "/v1/chat/completions", keyA, fmt.Sprintf(sparePrompt, "rtxllama", fmt.Sprintf("A%d", i))))
		}(i)
		waitUpstream(t, up, i+1)
	}
	resp := e.do("POST", "/v1/chat/completions", keyB, fmt.Sprintf(sparePrompt, "rtxllama", "B0"))
	out := readAll(t, resp)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != strconv.Itoa(spareRetryAfter) || up.count() != 2 {
		t.Fatalf("a third party on a full pool: %d %s, Retry-After %q, upstream calls %d", resp.StatusCode, out, resp.Header.Get("Retry-After"), up.count())
	}
	if !strings.Contains(out, "rate_limit_exceeded") || !strings.Contains(out, "rtx") || !strings.Contains(out, "spare capacity only") {
		t.Errorf("message: %s", out)
	}
	for _, o := range []string{"etabA", "etabC", "etabD"} {
		if strings.Contains(out, o) {
			t.Errorf("the refusal names the owner %s: %s", o, out)
		}
	}
	// An owner is not refused: it queues at vLLM as before.
	close(up.gate)
	wg.Wait()
	if m := readAll(t, e.do("GET", "/metrics", "", "")); !strings.Contains(m, `reason="pool_spare"`) {
		t.Errorf("the refusal is not counted:\n%s", m)
	}
	// Room left (both slots free): admitted, in the last band.
	resp = e.do("POST", "/v1/chat/completions", keyB, fmt.Sprintf(sparePrompt, "rtxllama", "B1"))
	if readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("a third party on a pool with room: %d", resp.StatusCode)
	}
	p, err := strconv.Atoi(up.last().Header.Get("X-Vllm-Priority"))
	if err != nil || p != fairshare.BorrowBase+fairshare.RankStep*fairshare.MaxRank {
		t.Fatalf("priority %d (%v), want the last band %d", p, err, fairshare.BorrowBase+fairshare.RankStep*fairshare.MaxRank)
	}
	// The other pool (b300, borrow all) is unaffected, and etabB holds a share
	// of it.
	if resp := e.do("POST", "/v1/chat/completions", keyB, fmt.Sprintf(sparePrompt, "llama", "B2")); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("b300: %d", resp.StatusCode)
	}
}

// The fallback path: a request that falls back on "unavailable" into a pool
// lending spare capacity only is refused there (429, Retry-After) when that
// pool is full, and served when it has room.
func TestPoolBorrowSpareFallback(t *testing.T) {
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
  share = 0.75
  team "etabA" {}
}
org "etabB" {
  share = 0.25
  team "etabB" {}
}
pool "big" { capacity = 4 }
pool "small" {
  capacity = 1
  shares   = { etabA = 1 }
  borrow   = "spare"
}
model "llama" {
  pool      = "big"
  key_file  = "/unused"
  fallbacks = ["llama-small"]
  upstream "a" { base_url = %q }
}
model "llama-small" {
  pool     = "small"
  key_file = "/unused"
  upstream "b" { base_url = %q }
}
`, a.URL(), b.URL())), "spare.hcl")
	if err != nil {
		t.Fatal(err)
	}
	e := newEnv(t, cfg)
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	a.mu.Lock()
	a.status = 503
	a.mu.Unlock()
	// etabA holds the only slot of the small pool.
	b.gate = make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		readAll(t, e.do("POST", "/v1/chat/completions", keyA, fmt.Sprintf(sparePrompt, "llama-small", "A0")))
	}()
	waitUpstream(t, b, 1)
	// etabB holds a share of "big" (so it is admitted there), falls back on
	// the 503 into "small", where it holds none and there is no room.
	resp := e.do("POST", "/v1/chat/completions", keyB, fmt.Sprintf(sparePrompt, "llama", "B0"))
	out := readAll(t, resp)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != strconv.Itoa(spareRetryAfter) || b.count() != 1 {
		t.Fatalf("fallback into a full spare pool: %d %s, Retry-After %q, upstream calls %d", resp.StatusCode, out, resp.Header.Get("Retry-After"), b.count())
	}
	close(b.gate)
	<-done
	// With room, the fallback is served, in the last band of the small pool.
	resp = e.do("POST", "/v1/chat/completions", keyB, fmt.Sprintf(sparePrompt, "llama", "B1"))
	if readAll(t, resp); resp.StatusCode != 200 || resp.Header.Get("X-Llmflow-Model") != "llama-small" {
		t.Fatalf("fallback into a pool with room: %d %v", resp.StatusCode, resp.Header)
	}
	if p, _ := strconv.Atoi(b.last().Header.Get("X-Vllm-Priority")); p != fairshare.BorrowBase+fairshare.RankStep*fairshare.MaxRank {
		t.Fatalf("priority %d, want the last band", p)
	}
}
