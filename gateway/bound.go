package gateway

import (
	"math"
	"math/bits"
	"strings"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
)

// The worst-case cost of a request, W (hard budgets, #87), and the exact
// cost of its usage, both as integers of nano-units (1e-9 of the model's
// cost unit) so that "spent + held ≤ limit" never drifts with float sums.
//
// The bound is vLLM's own (openai/bound.go): the input of one prompt is at
// most L = max_model_len tokens and its output at most min(M, L - input)
// per choice, so over the input length i the cost
//
//	i·p_in + n·min(M, L-i)·p_out
//
// is piecewise linear and peaks at i = L-M or i = L:
//
//	W = prompts · max((L-M)·p_in + n·M·p_out, L·p_in)
//
// with p_in the highest input price that can apply (input, cached, cache
// writes, either tier) and p_out the highest output price. A provider
// (Anthropic) bounds the input by its context window and the output by
// max_tokens separately: W = L·p_in + M·p_out.

// nanoPerUnit is the ledger's resolution.
const nanoPerUnit = 1_000_000_000

// maxLedgerNano bounds every amount the ledger holds (2^62 nano-units, about
// 4.6e9 units): a limit above it is enforced as it, and a cost above it can
// never be held.
const maxLedgerNano = int64(1) << 62

// priceTier holds one tier's prices in nano-units per million tokens.
type priceTier struct {
	in, cached, w5, w1h, out uint64
	writes                   bool // cache writes priced apart (else as input)
}

// groupPrices is a model's price list as integers.
type groupPrices struct {
	base, long   priceTier
	hasLong      bool
	longAbove    int64
	audioPerHour float64 // units per hour of audio
	// worstIn and worstOut are the highest input and output prices of
	// either tier.
	worstIn, worstOut uint64
	// zero: nothing is priced, every request costs 0.
	zero bool
}

// nanoPrice converts a price per million tokens to nano-units per million
// tokens, rounded up. check-config bounds prices to config.MaxPrice; a
// value beyond it (or not a number) is read as the bound.
func nanoPrice(p float64) uint64 {
	if math.IsNaN(p) {
		p = config.MaxPrice // never a free request (check-config refuses it)
	}
	if !(p > 0) {
		return 0
	}
	if p > config.MaxPrice {
		p = config.MaxPrice
	}
	return uint64(math.Ceil(p * nanoPerUnit))
}

func tierOf(c config.Cost) priceTier {
	t := priceTier{in: nanoPrice(c.InputPerMTok), cached: nanoPrice(c.CachedPerMTok), out: nanoPrice(c.OutputPerMTok),
		w5: nanoPrice(c.CacheWrite5mPerMTok), w1h: nanoPrice(c.CacheWrite1hPerMTok)}
	t.writes = c.CacheWrite5mPerMTok > 0 || c.CacheWrite1hPerMTok > 0
	return t
}

func (t priceTier) worstIn() uint64 { return max(t.in, t.cached, t.w5, t.w1h) }

func pricesFor(c config.Cost) groupPrices {
	p := groupPrices{base: tierOf(c), audioPerHour: c.AudioPerHour}
	if math.IsNaN(p.audioPerHour) || p.audioPerHour < 0 {
		p.audioPerHour = 0
	}
	p.worstIn, p.worstOut = p.base.worstIn(), p.base.out
	if c.LongContext != nil {
		p.hasLong, p.longAbove, p.long = true, c.LongContextAbove, tierOf(*c.LongContext)
		p.worstIn, p.worstOut = max(p.worstIn, p.long.worstIn()), max(p.worstOut, p.long.out)
	}
	p.zero = p.worstIn == 0 && p.worstOut == 0 && !(p.audioPerHour > 0)
	return p
}

// u128 is an unsigned 128-bit integer: a token count times a price in
// nano-units per million tokens exceeds 64 bits.
type u128 struct{ hi, lo uint64 }

func mul128(a, b uint64) u128 {
	hi, lo := bits.Mul64(a, b)
	return u128{hi, lo}
}

// add saturates at the largest value.
func (x u128) add(y u128) u128 {
	lo, c := bits.Add64(x.lo, y.lo, 0)
	hi, c2 := bits.Add64(x.hi, y.hi, c)
	if c2 != 0 {
		return u128{math.MaxUint64, math.MaxUint64}
	}
	return u128{hi, lo}
}

// mulSmall multiplies by a 64-bit factor, saturating.
func (x u128) mulSmall(f uint64) u128 {
	hiHi, hiLo := bits.Mul64(x.hi, f)
	loHi, loLo := bits.Mul64(x.lo, f)
	hi, c := bits.Add64(hiLo, loHi, 0)
	if hiHi != 0 || c != 0 {
		return u128{math.MaxUint64, math.MaxUint64}
	}
	return u128{hi, loLo}
}

// perMillion returns ceil(x / 1e6) as an amount of the ledger, saturated at
// maxLedgerNano+1 (more than any limit can hold).
func (x u128) perMillion() int64 {
	const d = 1_000_000
	if x.hi >= d {
		return maxLedgerNano + 1
	}
	q, r := bits.Div64(x.hi, x.lo, d)
	if r != 0 {
		q++
	}
	if q > uint64(maxLedgerNano) {
		return maxLedgerNano + 1
	}
	return int64(q)
}

func nonNeg(v int64) uint64 {
	if v < 0 {
		return 0
	}
	return uint64(v)
}

// costNano is the exact cost of a usage in nano-units, priced like
// group.cost (the long-context tier applies to the whole request above its
// threshold; cache writes are input unless priced apart), with one rounding
// for the token part, so that a usage within the bound never costs more
// than W.
func (p groupPrices) costNano(u openai.Usage) int64 {
	t := p.base
	if p.hasLong && u.PromptTokens > p.longAbove {
		t = p.long
	}
	prompt, cached, w5, w1h, wu := nonNeg(u.PromptTokens), nonNeg(u.Cached()), uint64(0), uint64(0), uint64(0)
	if t.writes {
		w5, w1h, wu = nonNeg(u.CacheWrite5m), nonNeg(u.CacheWrite1h), nonNeg(u.CacheWriteUnknown)
	}
	uncached := uint64(0)
	if s := cached + w5 + w1h + wu; prompt > s {
		uncached = prompt - s
	}
	// Writes of unknown lifetime cost the higher write price.
	sum := mul128(uncached, t.in).add(mul128(cached, t.cached)).add(mul128(w5, t.w5)).add(mul128(w1h, t.w1h)).
		add(mul128(wu, max(t.w5, t.w1h))).add(mul128(nonNeg(u.CompletionTokens), t.out))
	n := sum.perMillion()
	if p.audioPerHour > 0 && u.AudioSeconds > 0 {
		a := math.Ceil(u.AudioSeconds / 3600 * p.audioPerHour * nanoPerUnit)
		if !(a < float64(maxLedgerNano)) {
			return maxLedgerNano + 1
		}
		n = min(n+int64(a), maxLedgerNano+1)
	}
	return n
}

// worstCase returns W for a request of shape sh with the output bound m on
// a model whose context length is l; ok is false when there is no bound
// (unknown context length, or an unbounded shape on a priced model).
func (p groupPrices) worstCase(sh openai.Shape, l, m int64, provider bool) (int64, bool) {
	if sh.Free || p.zero {
		return 0, true
	}
	if sh.Unbounded != "" || l <= 0 {
		return 0, false
	}
	L, inputs, n := uint64(l), nonNeg(sh.Inputs), nonNeg(sh.Choices)
	if inputs == 0 {
		inputs = 1
	}
	if n == 0 {
		n = 1
	}
	if !sh.Generates {
		return mul128(L, p.worstIn).mulSmall(inputs).perMillion(), true
	}
	M := min(nonNeg(m), L)
	out := mul128(M, p.worstOut).mulSmall(n)
	var per u128
	if provider {
		per = mul128(L, p.worstIn).add(out)
	} else {
		a := mul128(L-M, p.worstIn).add(out)
		b := mul128(L, p.worstIn)
		per = a
		if b.hi > a.hi || b.hi == a.hi && b.lo > a.lo {
			per = b
		}
	}
	return per.mulSmall(inputs).perMillion(), true
}

// betaUnbounded returns why the Anthropic betas a request carries (those
// the model forwards) break the bound of a budgeted request, or "": a
// context beta wider than the configured context_window, or a compaction
// beta, whose extra sampling step the top-level usage does not report
// (config.CompactBetaPrefix). Review F1 of #87.
func betaUnbounded(g *group, betas []string) string {
	if g.cfg.External == nil {
		return ""
	}
	for _, b := range betas {
		if !g.cfg.External.AllowsBeta(b) {
			continue // not forwarded
		}
		switch {
		case strings.HasPrefix(b, config.ContextBetaPrefix) && g.cfg.MaxModelLen < config.ContextBetaWindow:
			return "the beta " + b + " widens the context window beyond this model's context_window"
		case strings.HasPrefix(b, config.CompactBetaPrefix):
			return "the beta " + b + " adds a compaction step whose tokens the response's top-level usage does not report"
		}
	}
	return ""
}

// largestFit returns the largest output bound in [lo, hi] whose W fits in
// room, or -1. W is convex in M (a maximum of two lines), so the bounds that
// fit form an interval: when lo fits, a binary search finds its end.
func (p groupPrices) largestFit(sh openai.Shape, l, lo, hi, room int64, provider bool) int64 {
	if hi < lo {
		return -1
	}
	fits := func(m int64) bool {
		w, ok := p.worstCase(sh, l, m, provider)
		return ok && w <= room
	}
	if fits(hi) {
		return hi
	}
	if !fits(lo) {
		return -1
	}
	for hi-lo > 1 { // lo fits, hi does not
		mid := lo + (hi-lo)/2
		if fits(mid) {
			lo = mid
		} else {
			hi = mid
		}
	}
	return lo
}
