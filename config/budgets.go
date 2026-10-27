package config

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/hcl/v2"
)

// Budgets tunes the escrow that keeps every budget from being exceeded
// (hard budgets, #87): a replica admits a request against a grant it holds
// in the store, and refills it in one transaction when it runs short.
type Budgets struct {
	// GrantShare is the share of a budget's room left that a refill takes
	// (at least the request's worst-case cost): near the limit, grants
	// shrink to exactly one request.
	GrantShare float64
	// GrantIdle is how long a grant may sit unused before it is given back,
	// so that an idle replica does not starve the others.
	GrantIdle time.Duration
	// MinMaxTokens is the smallest max_tokens the gateway picks for a
	// request that sent none, to fit the room left; below it the request is
	// refused.
	MinMaxTokens int64
}

// Hard-budget defaults and bounds.
const (
	DefaultGrantShare   = 0.1
	DefaultGrantIdle    = 5 * time.Second
	DefaultMinMaxTokens = 16
	// MaxTokensBound bounds max_model_len, context_window and
	// max_output_tokens (vLLM's are Python ints read as int32 by its
	// engine).
	MaxTokensBound = math.MaxInt32
	// MaxPrice bounds every price (per million tokens, per hour of audio):
	// in nano-units, a price times the largest token count of a request
	// stays far inside the integers the escrow adds (security audit #87:
	// overflow).
	MaxPrice = 1e6
	// ContextBetaPrefix names Anthropic's context-window betas
	// (context-1m-2025-08-07, platform.claude.com/docs/en/release-notes/overview),
	// which widened the window to ContextBetaWindow tokens.
	ContextBetaPrefix = "context-1m-"
	ContextBetaWindow = 1_000_000
	// CompactBetaPrefix names Anthropic's compaction betas: threshold
	// compaction (compact-2026-01-12) adds a sampling step that the
	// top-level usage does not report (build-with-claude/compaction-threshold,
	// "Understanding usage"); on-demand compaction is compact-2026-09-04.
	CompactBetaPrefix = "compact-"
)

type budgetsHCL struct {
	GrantShare        *float64  `hcl:"grant_share,optional"`
	GrantShareRange   hcl.Range `hcl:"grant_share,attr_value_range"`
	GrantIdle         *string   `hcl:"grant_idle,optional"`
	GrantIdleRange    hcl.Range `hcl:"grant_idle,attr_value_range"`
	MinMaxTokens      *int64    `hcl:"min_max_tokens,optional"`
	MinMaxTokensRange hcl.Range `hcl:"min_max_tokens,attr_value_range"`
}

// budgets validates the budgets block, the models' token bounds and the
// price bound, once the models are built (external.go sets External).
func (v *validator) budgets(raw *fileHCL, c *Config) {
	c.Budgets = Budgets{GrantShare: DefaultGrantShare, GrantIdle: DefaultGrantIdle, MinMaxTokens: DefaultMinMaxTokens}
	if b := raw.Budgets; b != nil {
		if b.GrantShare != nil {
			if !(*b.GrantShare > 0 && *b.GrantShare <= 1) {
				v.errf(b.GrantShareRange, "Invalid grant_share", "grant_share = %v: want more than 0 and at most 1", *b.GrantShare)
			}
			c.Budgets.GrantShare = *b.GrantShare
		}
		c.Budgets.GrantIdle = v.dur(b.GrantIdle, b.GrantIdleRange, "grant_idle", DefaultGrantIdle)
		if c.Budgets.GrantIdle < 100*time.Millisecond || c.Budgets.GrantIdle > 10*time.Minute {
			v.errf(b.GrantIdleRange, "Invalid grant_idle", "grant_idle = %s: want 100ms..10m", c.Budgets.GrantIdle)
		}
		if b.MinMaxTokens != nil {
			if *b.MinMaxTokens < 1 || *b.MinMaxTokens > MaxTokensBound {
				v.errf(b.MinMaxTokensRange, "Invalid min_max_tokens", "min_max_tokens = %d: want 1..%d", *b.MinMaxTokens, MaxTokensBound)
			}
			c.Budgets.MinMaxTokens = *b.MinMaxTokens
		}
	}
	for i, m := range raw.Models {
		if i >= len(c.ModelGroups) {
			break
		}
		g := &c.ModelGroups[i]
		where := "model " + strconv.Quote(m.Name)
		tokens := func(p *int64, r hcl.Range, name string) int64 {
			if p == nil {
				return 0
			}
			if *p < 1 || *p > MaxTokensBound {
				v.errf(r, "Invalid "+name, "%s: %s = %d: want 1..%d", where, name, *p, MaxTokensBound)
			}
			return *p
		}
		if g.External != nil {
			if m.MaxModelLen != nil {
				v.errf(m.MaxModelLenRange, "Invalid max_model_len", "%s: a provider's model sets context_window, not max_model_len", where)
			}
			g.MaxModelLen = tokens(m.ContextWindow, m.ContextWindowRange, "context_window")
			// A beta may widen the window past the bound (review F1):
			// context-1m-2025-08-07 gave Claude Sonnet 4 and 4.5 a 1M-token
			// window (release notes, 2026-03-30 and 2026-04-30).
			for _, b := range m.AnthropicBetas {
				if (b == "*" || strings.HasPrefix(b, ContextBetaPrefix)) && g.MaxModelLen < ContextBetaWindow {
					v.errf(m.BetasRange, "Context beta wider than context_window",
						"%s: anthropic_betas admits %q, which can extend the context window to %d tokens: set context_window = %d (or more), or do not allow the beta; a budget's bound would otherwise be too small",
						where, b, ContextBetaWindow, ContextBetaWindow)
					break
				}
			}
		} else {
			if m.ContextWindow != nil {
				v.errf(m.ContextWindowRange, "Invalid context_window", "%s: a vLLM model sets max_model_len (or reads it from /v1/models), not context_window", where)
			}
			g.MaxModelLen = tokens(m.MaxModelLen, m.MaxModelLenRange, "max_model_len")
		}
		g.MaxOutputTokens = tokens(m.MaxOutputTokens, m.MaxOutputTokensRange, "max_output_tokens")
		if g.MaxModelLen > 0 && g.MaxOutputTokens > g.MaxModelLen {
			v.errf(m.MaxOutputTokensRange, "Invalid max_output_tokens", "%s: max_output_tokens %d exceeds the context length %d (vLLM refuses it)", where, g.MaxOutputTokens, g.MaxModelLen)
		}
		if m.Cost != nil {
			r := m.Cost.DefRange
			for _, p := range pricesOf(g.Cost) {
				if p > MaxPrice {
					v.errf(r, "Price too high", "%s: a price of %v per million tokens (or per hour of audio) is above %g; the budgets' integer arithmetic is bounded for prices up to it", where, p, float64(MaxPrice))
					break
				}
			}
		}
	}
}

// pricesOf lists every price of a cost, its long-context tier included.
func pricesOf(c Cost) []float64 {
	ps := []float64{c.InputPerMTok, c.OutputPerMTok, c.CachedPerMTok, c.AudioPerHour, c.CacheWrite5mPerMTok, c.CacheWrite1hPerMTok}
	if c.LongContext != nil {
		lc := *c.LongContext
		ps = append(ps, lc.InputPerMTok, lc.OutputPerMTok, lc.CachedPerMTok, lc.CacheWrite5mPerMTok, lc.CacheWrite1hPerMTok)
	}
	return ps
}
