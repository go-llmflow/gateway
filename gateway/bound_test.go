package gateway

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
)

// The test prices: input 1000, cached 100, output 5000 per million tokens.
var testPrices = pricesFor(config.Cost{InputPerMTok: 1000, OutputPerMTok: 5000, CachedPerMTok: 100})

func TestCostNanoMatchesFloatCost(t *testing.T) {
	g := &group{cfg: config.ModelGroup{Cost: config.Cost{InputPerMTok: 1000, OutputPerMTok: 5000, CachedPerMTok: 100}}}
	u := openai.Usage{PromptTokens: 100, CompletionTokens: 20, PromptTokensDetails: &struct {
		CachedTokens int64 `json:"cached_tokens"`
	}{40}}
	got := testPrices.costNano(u)
	// (60×1000 + 40×100 + 20×5000)/1e6 = 0.164 units.
	if got != 164_000_000 || math.Abs(float64(got)/nanoPerUnit-g.cost(u)) > 1e-12 {
		t.Fatalf("costNano %d, float %v", got, g.cost(u))
	}
	if testPrices.costNano(openai.Usage{PromptTokens: -5, CompletionTokens: -1}) != 0 {
		t.Fatal("negative counts charged")
	}
}

func TestWorstCase(t *testing.T) {
	sh := openai.Shape{Generates: true, Inputs: 1, Choices: 1}
	// L=128, M=20: max((108×1000 + 20×5000), 128×1000)/1e6 = 0.208.
	if w, ok := testPrices.worstCase(sh, 128, 20, false); !ok || w != 208_000_000 {
		t.Fatalf("W = %d %v", w, ok)
	}
	// Output cheaper than input: the whole context as input dominates.
	cheapOut := pricesFor(config.Cost{InputPerMTok: 5000, OutputPerMTok: 1000})
	if w, _ := cheapOut.worstCase(sh, 100, 10, false); w != 500_000_000 {
		t.Fatalf("cheap output W = %d", w)
	}
	// n and prompts multiply; M above L is read as L.
	sh2 := openai.Shape{Generates: true, Inputs: 3, Choices: 2}
	if w, _ := testPrices.worstCase(sh2, 100, 1000, false); w != 3*(2*100*5000)*1000 {
		t.Fatalf("W = %d", w)
	}
	// A provider: input and output bounds add.
	if w, _ := testPrices.worstCase(sh, 128, 20, true); w != (128*1000+20*5000)*1000 {
		t.Fatalf("provider W = %d", w)
	}
	// Pooling: every input up to L.
	if w, _ := testPrices.worstCase(openai.Shape{Inputs: 4, Choices: 1}, 512, 0, false); w != 4*512*1000*1000 {
		t.Fatalf("pooling W = %d", w)
	}
	if _, ok := testPrices.worstCase(sh, 0, 20, false); ok {
		t.Fatal("unknown context length bounded")
	}
	if _, ok := testPrices.worstCase(openai.Shape{Generates: true, Unbounded: "x"}, 128, 1, false); ok {
		t.Fatal("unbounded shape bounded")
	}
	if w, ok := pricesFor(config.Cost{}).worstCase(openai.Shape{Unbounded: "x"}, 0, 0, false); !ok || w != 0 {
		t.Fatal("an unpriced model costs something")
	}
	// Saturation: absurd n never wraps around.
	if w, _ := testPrices.worstCase(openai.Shape{Generates: true, Inputs: 1 << 20, Choices: math.MaxInt64}, math.MaxInt32, math.MaxInt32, false); w <= maxLedgerNano {
		t.Fatalf("saturated W = %d", w)
	}
	maxP := pricesFor(config.Cost{InputPerMTok: math.Inf(1), OutputPerMTok: math.NaN()})
	if maxP.worstIn != uint64(config.MaxPrice*nanoPerUnit) || maxP.worstOut != 0 {
		t.Fatalf("prices beyond the bound: %+v", maxP)
	}
}

func TestLargestFit(t *testing.T) {
	sh := openai.Shape{Generates: true, Inputs: 1, Choices: 1}
	// W(M) = 0.128 + 0.004·M units; room 0.2 → M = 18.
	if m := testPrices.largestFit(sh, 128, 16, 128, 200_000_000, false); m != 18 {
		t.Fatalf("largest fit %d", m)
	}
	if m := testPrices.largestFit(sh, 128, 16, 128, 191_999_999, false); m != -1 {
		t.Fatalf("below the floor: %d", m)
	}
	if m := testPrices.largestFit(sh, 128, 16, 64, 1<<60, false); m != 64 {
		t.Fatalf("the cap: %d", m)
	}
	if m := testPrices.largestFit(sh, 128, 20, 10, 1<<60, false); m != -1 {
		t.Fatalf("empty interval: %d", m)
	}
}

// The context length is read from the upstreams' /v1/models at health
// check, unless configured.
func TestContextLenFromModels(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.ModelGroups[1].MaxModelLen = 64
	e := newEnv(t, cfg)
	<-e.srv.probed
	if l := e.srv.groups["llama"].contextLen(); l != 128 {
		t.Fatalf("read %d", l)
	}
	if l := e.srv.groups["llama-batch"].contextLen(); l != 64 {
		t.Fatalf("configured %d", l)
	}
	g := &group{}
	g.noteLen(100)
	g.noteLen(50)
	g.noteLen(-1)
	g.noteLen(1 << 40)
	if g.contextLen() != 100 {
		t.Fatalf("kept %d", g.contextLen())
	}
	cards := []modelCard{{ID: "a", MaxModelLen: 10}, {ID: "b", MaxModelLen: 30}}
	if lenFor(cards, "a") != 10 || lenFor(cards, "lora") != 30 || lenFor(nil, "a") != 0 {
		t.Fatal("lenFor")
	}
}

// fakeEngine reads a body as vLLM v0.32.0 does, independently of package
// openai's ShapeOf, and returns the largest usage it could report for an
// input of i tokens per prompt (the adversary picks i).
type fakeEngine struct {
	prompts, n, m int64
	ok            bool // vLLM accepts the request
}

func readAsVLLM(ep openai.Endpoint, body []byte, l int64) fakeEngine {
	var b map[string]any
	if json.Unmarshal(body, &b) != nil {
		return fakeEngine{}
	}
	num := func(k string) (int64, bool) {
		v, ok := b[k].(float64)
		if !ok || v != math.Trunc(v) || v < 0 || v > math.MaxInt64/2 {
			return 0, false
		}
		return int64(v), true
	}
	count := func(v any) int64 {
		switch x := v.(type) {
		case nil:
			return 0
		case []any:
			if len(x) == 0 {
				return 1
			}
			allNum, allMsg := true, true
			for _, e := range x {
				if _, ok := e.(float64); !ok {
					allNum = false
				}
				if o, ok := e.(map[string]any); !ok {
					allMsg = false
				} else if _, ok := o["role"].(string); !ok {
					allMsg = false
				}
			}
			if allNum || allMsg {
				return 1
			}
			return int64(len(x))
		}
		return 1
	}
	e := fakeEngine{prompts: 1, n: 1, ok: true}
	if v, ok := num("n"); ok {
		e.n = max(v, 1)
	}
	if v, ok := num("best_of"); ok {
		e.n = max(e.n, v)
	}
	switch ep {
	case openai.Chat:
		if v, ok := num("max_completion_tokens"); ok {
			e.m = v
		} else if v, ok := num("max_tokens"); ok {
			e.m = v
		} else {
			e.m = l
		}
	case openai.Completions:
		e.m = 16
		if v, ok := num("max_tokens"); ok {
			e.m = v
		}
		e.prompts = max(1, count(b["prompt"])+count(b["prompt_embeds"]))
	case openai.Responses:
		e.n = 1
		e.m = l
		if v, ok := num("max_output_tokens"); ok {
			e.m = v
		}
	case openai.Messages:
		e.n = 1
		if v, ok := num("max_tokens"); ok {
			e.m = v
		}
	case openai.Embeddings:
		e.n, e.m = 1, 0
		e.prompts = max(1, count(b["input"])+count(b["messages"]))
	case openai.Rerank:
		e.n, e.m = 1, 0
		docs := int64(1)
		switch d := b["documents"].(type) {
		case []any:
			docs = int64(len(d))
		case map[string]any:
			if c, ok := d["content"].([]any); ok {
				docs = max(1, int64(len(c)))
			}
		}
		e.prompts = 1 + docs
	}
	if e.m > l {
		e.ok = false // vLLM refuses max_tokens above max_model_len
	}
	return e
}

// usage returns the costliest usage with inputs of i tokens: every prompt
// token in the most expensive input category of the tier that applies.
func (e fakeEngine) usage(p groupPrices, l, i int64, provider bool) openai.Usage {
	out := min(e.m, l-i)
	if provider {
		out = e.m
	}
	u := openai.Usage{PromptTokens: e.prompts * i, CompletionTokens: e.prompts * e.n * max(out, 0)}
	t := p.base
	if p.hasLong && u.PromptTokens > p.longAbove {
		t = p.long
	}
	switch max(t.in, t.cached, t.w5, t.w1h) {
	case t.in:
	case t.cached:
		u.PromptTokensDetails = &struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{u.PromptTokens}
	case t.w5:
		u.CacheWrite5m = u.PromptTokens
	default:
		u.CacheWrite1h = u.PromptTokens
	}
	return u
}

// FuzzWorstCase checks W against a fake engine that maximises usage within
// vLLM's rules: W never panics, is never negative, and is never below the
// cost of a usage vLLM could report for the body the gateway sends.
func FuzzWorstCase(f *testing.F) {
	for _, s := range []string{`{"n":2}`, `{"prompt":["a","b"],"n":3}`, `{"input":["x","y","z"]}`, `{"documents":["a","b"]}`,
		`{"max_completion_tokens":5,"max_tokens":500}`, `{"best_of":4,"n":1}`, `{"input":[{"role":"user","content":"x"}]}`,
		`{"prompt":"a","prompt_embeds":["x","y"]}`, `{"messages":[[{"role":"user"}],[{"role":"user"}]],"input":["a"]}`,
		`{"documents":{"content":[{"type":"text"},{"type":"text"}]}}`, `{"prompt":[[1,2],[3]],"n":2,"max_tokens":3}`} {
		f.Add(int(openai.Chat), []byte(s), int64(4096), int64(100), 1.0, 5.0, 0.1, 2.0, int64(0), false)
		f.Add(int(openai.Completions), []byte(s), int64(32), int64(31), 3.0, 0.5, 9.0, 0.0, int64(10), true)
		f.Add(int(openai.Embeddings), []byte(s), int64(512), int64(0), 0.02, 0.0, 0.0, 0.0, int64(0), false)
		f.Add(int(openai.Rerank), []byte(s), int64(512), int64(0), 0.02, 0.0, 0.0, 0.0, int64(0), false)
	}
	f.Fuzz(func(t *testing.T, ep int, body []byte, l, m int64, pin, pout, pcached, pw float64, above int64, provider bool) {
		e := openai.Endpoint(ep % int(openai.Rerank+1))
		if ep < 0 {
			e = openai.Chat
		}
		l = 1 + abs64(l)%config.MaxTokensBound
		m = abs64(m) % (l + 1)
		c := config.Cost{InputPerMTok: pin, OutputPerMTok: pout, CachedPerMTok: pcached, CacheWrite5mPerMTok: pw, CacheWrite1hPerMTok: pw / 2}
		if above > 0 {
			c.LongContextAbove = above % l
			lc := config.Cost{InputPerMTok: 2 * pin, OutputPerMTok: pout * 1.5, CachedPerMTok: pcached}
			c.LongContext = &lc
		}
		p := pricesFor(c)
		sh, err := openai.ShapeOf(e, body)
		if err != nil {
			return
		}
		out, err := openai.SetMaxTokens(e, body, m)
		if err != nil {
			return
		}
		w, ok := p.worstCase(sh, l, m, provider)
		if w < 0 {
			t.Fatalf("negative W %d", w)
		}
		if !ok {
			return
		}
		eng := readAsVLLM(e, out, l)
		if !eng.ok {
			return
		}
		if eng.prompts > sh.Inputs || eng.n > sh.Choices && sh.Generates {
			t.Fatalf("shape undercounts: engine %+v, shape %+v, body %s", eng, sh, out)
		}
		for _, i := range []int64{0, 1, l - m, l / 2, l - 1, l} {
			if i < 0 || i > l {
				continue
			}
			if c := p.costNano(eng.usage(p, l, i, provider)); c > w && w <= maxLedgerNano {
				t.Fatalf("usage at input %d costs %d > W %d (engine %+v, shape %+v, L %d, M %d, body %s)", i, c, w, eng, sh, l, m, out)
			}
		}
	})
}

func abs64(v int64) int64 {
	if v < 0 {
		if v == math.MinInt64 {
			return math.MaxInt64
		}
		return -v
	}
	return v
}
