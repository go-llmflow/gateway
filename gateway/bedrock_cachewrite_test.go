package gateway

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
)

// Cache writes whose lifetime the provider did not say are charged at the
// HIGHER write price. Prices: 5-minute writes 2.5, 1-hour writes 4 (bdrkPrices).
// The usage: 10 uncached, 100 read, 50 written, 5 out.
//
//	known as 5-minute: (10×2 + 100×0.1 + 50×2.5 + 5×10) / 1e6 = 205e-6 USD
//	unknown lifetime:  (10×2 + 100×0.1 + 50×4   + 5×10) / 1e6 = 280e-6 USD
func TestCacheWriteOfUnknownLifetimeCostsTheHigherPrice(t *testing.T) {
	const high, old = int64(280_000), int64(205_000) // nano-USD
	// chargedHigh is the property: the settled cost is not below the 1-hour price.
	chargedHigh := func(cost int64) bool { return cost >= high }

	answers := map[string]string{
		"no cacheDetails":       `{"output":{"message":{"content":[]}},"usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"cacheWriteInputTokens":50}}`,
		"split that is short":   `{"output":{"message":{"content":[]}},"usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"cacheWriteInputTokens":50,"cacheDetails":[{"inputTokens":30,"ttl":"5m"}]}}`,
		"split that is too big": `{"output":{"message":{"content":[]}},"usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"cacheWriteInputTokens":50,"cacheDetails":[{"inputTokens":30,"ttl":"5m"},{"inputTokens":40,"ttl":"1h"}]}}`,
	}
	cfgCost := config.Cost{Unit: "USD", InputPerMTok: 2, OutputPerMTok: 10, CachedPerMTok: 0.1, CacheWrite5mPerMTok: 2.5, CacheWrite1hPerMTok: 4}
	g := &group{cfg: config.ModelGroup{Cost: cfgCost}}
	for name, a := range answers {
		_, u, ok := translateConverse([]byte(a), "m", "x", time.Unix(1, 0))
		if !ok || u.CacheWriteUnknown != 50 || u.CacheWrite5m != 0 || u.CacheWrite1h != 0 {
			t.Fatalf("%s: %+v %v", name, u, ok)
		}
		if got := bdrkPrices.costNano(u); got != high || !chargedHigh(got) {
			t.Errorf("%s: costNano %d, want %d (the 1-hour price)", name, got, high)
		}
		if got := g.cost(u); math.Abs(got-280e-6) > 1e-12 {
			t.Errorf("%s: group.cost %v, want 280e-6", name, got)
		}
		// The planted control: the old reading (every write a 5-minute write)
		// is below the bound, and the property rejects it.
		u5 := u
		u5.CacheWrite5m, u5.CacheWriteUnknown = u.CacheWriteUnknown, 0
		if bdrkPrices.costNano(u5) != old || chargedHigh(bdrkPrices.costNano(u5)) {
			t.Errorf("%s: the control (5-minute reading) costs %d and passes the property", name, bdrkPrices.costNano(u5))
		}
	}
	// A known split is priced by lifetime, as before: 30 × 2.5 + 20 × 4.
	_, u, _ := translateConverse([]byte(`{"output":{"message":{"content":[]}},"usage":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"cacheWriteInputTokens":50,"cacheDetails":[{"inputTokens":30,"ttl":"5m"},{"inputTokens":20,"ttl":"1h"}]}}`), "m", "x", time.Unix(1, 0))
	if u.CacheWriteUnknown != 0 || bdrkPrices.costNano(u) != 235_000 {
		t.Errorf("known split: %+v costs %d", u, bdrkPrices.costNano(u))
	}
	// Without write prices the writes are input, known or not.
	plain := pricesFor(config.Cost{Unit: "USD", InputPerMTok: 2, OutputPerMTok: 10, CachedPerMTok: 0.1})
	_, un, _ := translateConverse([]byte(answers["no cacheDetails"]), "m", "x", time.Unix(1, 0))
	if got := plain.costNano(un); got != (10+50)*2*1000+100*100+5*10*1000 {
		t.Errorf("no write prices: %d", got)
	}
	// The InvokeModel path (Anthropic usage without cache_creation) reads the
	// same way.
	ua, ok := openai.ParseUsageFor(openai.Messages, []byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":50}}`))
	if !ok || ua.CacheWriteUnknown != 50 || bdrkPrices.costNano(ua) != high {
		t.Errorf("InvokeModel usage: %+v %v costs %d", ua, ok, bdrkPrices.costNano(ua))
	}
	ub, ok := openai.ParseUsageFor(openai.Messages, []byte(`{"usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":100,"cache_creation_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":30,"ephemeral_1h_input_tokens":20}}}`))
	if !ok || ub.CacheWriteUnknown != 0 || bdrkPrices.costNano(ub) != 235_000 {
		t.Errorf("Anthropic usage with its split: %+v %v", ub, ok)
	}
}

// End to end: an answer without cacheDetails is settled at the 1-hour price
// (280e-6), JSON and stream, and what is settled is never below the bill.
func TestConverseNoSplitEndToEnd(t *testing.T) {
	for _, stream := range []bool{false, true} {
		e, up, _, _ := newConverseEnv(t, "")
		_, keyA := e.newKey("etabA")
		up.set("no-split", 0, "")
		resp := e.do("POST", "/v1/chat/completions", keyA, chatMsg(stream, 16, ""))
		if readAll(t, resp); resp.StatusCode != 200 {
			t.Fatalf("stream=%v: %d", stream, resp.StatusCode)
		}
		recs := recordsOf(e)
		if len(recs) != 1 || math.Abs(recs[0].Cost-280e-6) > 1e-12 || recs[0].Estimated {
			t.Errorf("stream=%v: %+v, want 280e-6", stream, recs)
		}
	}
}

// The usage chunk ("choices": []) is written only when the client asked for
// it; the accounting reads the usage either way.
func TestConverseUsageChunkOnlyWhenAsked(t *testing.T) {
	stream := converseStreamBytes(bedrockUsage{10, 5, 100, 50})
	for _, ask := range []bool{false, true} {
		c := newConverseSSE(bytes.NewReader(stream), "m", "x", time.Unix(1, 0), ask)
		out := readAllSSE(t, c)
		if has := strings.Contains(out, `"choices":[]`); has != ask {
			t.Errorf("ask=%v: usage chunk present=%v:\n%s", ask, has, out)
		}
		if !strings.HasSuffix(out, "data: [DONE]\n\n") || !c.Complete() {
			t.Errorf("ask=%v: complete %v, tail %q", ask, c.Complete(), out[max(0, len(out)-20):])
		}
		if u, ok := c.Usage(); !ok || u.PromptTokens != 160 || u.CacheWrite5m != 30 || u.CacheWrite1h != 20 {
			t.Errorf("ask=%v: usage %+v %v", ask, u, ok)
		}
	}
}
