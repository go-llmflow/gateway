package config

import (
	"math"
	"net"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/hashicorp/hcl/v2"
)

// ProviderAnthropic is the Anthropic API (api.anthropic.com), reached with
// an API key under Anthropic's Commercial Terms.
const ProviderAnthropic = "anthropic"

// Default Anthropic API versions a client may ask for (anthropic-version
// header, platform.claude.com/docs/en/api/versioning, opened 2026-10-26);
// the first is sent when the client asks for none or another.
var DefaultAnthropicVersions = []string{"2023-06-01", "2026-10-21"}

// External configures a model served by an external provider: prompts
// leave the site, so everything about it is explicit.
type External struct {
	Provider string
	// Orgs and Teams may use the model; nobody else sees it (it answers as
	// an unknown model).
	Orgs, Teams []string
	// OrgKeys maps an organisation to the key file of ITS provider key.
	// Anthropic isolates the prompt cache per workspace, so one workspace
	// and key per organisation keeps the institutions' caches apart (the
	// vLLM cache_salt question, CVE-2025-46570). OrgKeyOrder is their
	// configuration order.
	OrgKeys     map[string]string
	OrgKeyOrder []string
	// Versions are the anthropic-version values a client may ask for.
	Versions []string
	// Betas are the anthropic-beta values forwarded; "*" forwards any value
	// (except an OAuth one, never forwarded). Empty: none.
	Betas []string
	// RequireGuardrails is external_requires_guardrails for this model.
	RequireGuardrails bool
}

// AllowsBeta reports whether an anthropic-beta value is forwarded. An
// OAuth capability (subscription credentials, which a gateway must never
// relay: Claude Code's legal and compliance page) never is.
func (e *External) AllowsBeta(v string) bool {
	if !validBeta(v) || strings.HasPrefix(strings.ToLower(v), "oauth") {
		return false
	}
	return slices.Contains(e.Betas, "*") || slices.Contains(e.Betas, v)
}

// Allows reports whether a key of team (in org) may use the model.
func (e *External) Allows(org, team string) bool {
	return slices.Contains(e.Orgs, org) || slices.Contains(e.Teams, team)
}

type orgKeyHCL struct {
	Org      string    `hcl:"org,label"`
	OrgRange hcl.Range `hcl:"org,label_range"`
	KeyFile  string    `hcl:"key_file"`
	KeyRange hcl.Range `hcl:"key_file,attr_value_range"`
	DefRange hcl.Range `hcl:",def_range"`
}

type longContextHCL struct {
	AboveInputTokens int64     `hcl:"above_input_tokens"`
	AboveRange       hcl.Range `hcl:"above_input_tokens,attr_value_range"`
	InputPerMTok     *float64  `hcl:"input_per_mtok,optional"`
	OutputPerMTok    *float64  `hcl:"output_per_mtok,optional"`
	CachedPerMTok    *float64  `hcl:"cached_per_mtok,optional"`
	CacheWrite5m     *float64  `hcl:"cache_write_5m_per_mtok,optional"`
	CacheWrite1h     *float64  `hcl:"cache_write_1h_per_mtok,optional"`
	DefRange         hcl.Range `hcl:",def_range"`
}

var (
	betaRe    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
	versionRe = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)
)

func validBeta(v string) bool { return betaRe.MatchString(v) }

func finite(ps ...*float64) bool {
	for _, p := range ps {
		if p != nil && !(*p >= 0 && *p < math.Inf(1)) {
			return false
		}
	}
	return true
}

// external validates the models with a provider, once everything else is
// built (it reads the organisations, the guardrails and the fallbacks).
func (v *validator) external(raw *fileHCL, c *Config) {
	requireGuards := raw.ExternalGuardrails == nil || *raw.ExternalGuardrails
	teamOrg := map[string]string{}
	orgs := map[string]bool{}
	for _, o := range c.Orgs {
		orgs[o.Name] = true
		for _, t := range o.Teams {
			teamOrg[t] = o.Name
		}
	}
	external := map[string]bool{}
	for i, m := range raw.Models {
		if i >= len(c.ModelGroups) {
			break
		}
		g := &c.ModelGroups[i]
		where := "model " + strconv.Quote(m.Name)
		if m.Cost != nil {
			if !finite(m.Cost.CacheWrite5m, m.Cost.CacheWrite1h) {
				v.errf(m.Cost.DefRange, "Invalid cost", "costs must be finite and non-negative")
			}
			g.Cost.CacheWrite5mPerMTok, g.Cost.CacheWrite1hPerMTok = f64(m.Cost.CacheWrite5m), f64(m.Cost.CacheWrite1h)
			if (m.Cost.CacheWrite5m == nil) != (m.Cost.CacheWrite1h == nil) {
				v.errf(m.Cost.DefRange, "Incomplete cache prices", "%s: set both cache_write_5m_per_mtok and cache_write_1h_per_mtok (a write of the other lifetime would be free)", where)
			}
			if lc := m.Cost.LongContext; lc != nil {
				if lc.AboveInputTokens <= 0 {
					v.errf(lc.AboveRange, "Invalid long_context", "%s: above_input_tokens must be positive", where)
				}
				if !finite(lc.InputPerMTok, lc.OutputPerMTok, lc.CachedPerMTok, lc.CacheWrite5m, lc.CacheWrite1h) {
					v.errf(lc.DefRange, "Invalid cost", "costs must be finite and non-negative")
				}
				g.Cost.LongContextAbove = lc.AboveInputTokens
				g.Cost.LongContext = &Cost{InputPerMTok: f64(lc.InputPerMTok), OutputPerMTok: f64(lc.OutputPerMTok),
					CachedPerMTok: f64(lc.CachedPerMTok), CacheWrite5mPerMTok: f64(lc.CacheWrite5m), CacheWrite1hPerMTok: f64(lc.CacheWrite1h),
					Unit: g.Cost.Unit, PriceDate: g.Cost.PriceDate}
			}
		}
		if m.Provider == nil {
			for _, set := range []bool{m.AllowExternal != nil, len(m.Orgs) > 0, len(m.Teams) > 0, len(m.AnthropicVersions) > 0,
				len(m.AnthropicBetas) > 0, len(m.OrgKeys) > 0} {
				if set {
					v.errf(m.DefRange, "Unexpected external settings", "%s: allow_external, orgs, teams, anthropic_* and org_key belong to a model with a provider", where)
					break
				}
			}
			continue
		}
		external[m.Name] = true
		e := &External{Provider: *m.Provider, Orgs: m.Orgs, Teams: m.Teams, OrgKeys: map[string]string{}, RequireGuardrails: requireGuards}
		g.External = e
		if e.Provider != ProviderAnthropic {
			v.errf(m.ProviderRange, "Unknown provider", "%s: provider = %q: want \"anthropic\"", where, e.Provider)
			continue
		}
		if m.AllowExternal == nil || !*m.AllowExternal {
			v.errf(m.DefRange, "External model not allowed",
				"%s sends prompts and completions to %s: set allow_external = true to accept that they leave the site", where, upstreamHosts(g))
		}
		if g.Class == Guard {
			v.errf(m.ClassRange, "Invalid class", "%s: an external model cannot be a guard model", where)
		}
		// Who may use it: an explicit allow-list, of declared names.
		if len(e.Orgs) == 0 && len(e.Teams) == 0 {
			v.errf(m.DefRange, "No allow-list", "%s: list the orgs and/or teams that may send prompts to %s", where, e.Provider)
		}
		for _, o := range e.Orgs {
			v.known(orgs, o, m.OrgsRange, "organisation", where)
		}
		for _, t := range e.Teams {
			if _, ok := teamOrg[t]; !ok {
				v.errf(m.TeamsRange, "Unknown team", "%s: team %q is not declared in an org block", where, t)
			}
		}
		// Upstreams: https (http only to a loopback address, for tests).
		for j, u := range g.Upstreams {
			pu, err := url.Parse(u.BaseURL)
			if err != nil {
				continue // ValidateUpstream reported it
			}
			if pu.Scheme != "https" && !(pu.Scheme == "http" && loopbackHost(pu.Hostname())) {
				r := m.DefRange
				if j < len(m.Upstreams) {
					r = m.Upstreams[j].BaseURLRange
				}
				v.errf(r, "Invalid base_url", "%s: an external provider is reached over https (http only to a loopback address, for tests): %q", where, u.BaseURL)
			}
		}
		// Versions and betas.
		e.Versions = m.AnthropicVersions
		if len(e.Versions) == 0 {
			e.Versions = DefaultAnthropicVersions
		}
		for _, ver := range e.Versions {
			if !versionRe.MatchString(ver) {
				v.errf(m.VersionsRange, "Invalid anthropic_versions", "%s: %q: want YYYY-MM-DD", where, ver)
			}
		}
		for _, b := range m.AnthropicBetas {
			switch {
			case b == "*":
			case !validBeta(b):
				v.errf(m.BetasRange, "Invalid anthropic_betas", "%s: %q: want a beta name such as \"context-management-2025-06-27\", or \"*\"", where, b)
			case strings.HasPrefix(b, "oauth"):
				v.errf(m.BetasRange, "OAuth beta refused", "%s: %q is the capability of subscription (claude.ai) credentials, which a gateway must not relay; the gateway only uses API keys", where, b)
			}
		}
		e.Betas = m.AnthropicBetas
		// Keys: one per organisation when several may use the model and
		// the institutions' caches must stay apart.
		reach := map[string]bool{}
		for _, o := range e.Orgs {
			reach[o] = true
		}
		for _, t := range e.Teams {
			if o := teamOrg[t]; o != "" {
				reach[o] = true
			}
		}
		seenFile := map[string]string{}
		for _, ok := range m.OrgKeys {
			switch {
			case !reach[ok.Org]:
				v.errf(ok.OrgRange, "Useless org_key", "%s: organisation %q is not allowed to use the model (orgs/teams)", where, ok.Org)
			case e.OrgKeys[ok.Org] != "":
				v.errf(ok.DefRange, "Duplicate org_key", "%s: organisation %q has two keys", where, ok.Org)
			case ok.KeyFile == "":
				v.errf(ok.KeyRange, "Missing key file", "%s: org_key %q has no key_file", where, ok.Org)
			}
			if prev, dup := seenFile[ok.KeyFile]; dup && prev != ok.Org {
				v.errf(ok.KeyRange, "Shared org_key", "%s: organisations %q and %q use the same key_file; each needs a key of its own workspace", where, prev, ok.Org)
			}
			seenFile[ok.KeyFile] = ok.Org
			if e.OrgKeys[ok.Org] == "" {
				e.OrgKeys[ok.Org] = ok.KeyFile
				e.OrgKeyOrder = append(e.OrgKeyOrder, ok.Org)
			}
		}
		defaultKey := ""
		if len(g.Upstreams) > 0 {
			defaultKey = g.Upstreams[0].KeyFile
		}
		isolated := c.CacheIsolation != "none"
		for o := range reach {
			if e.OrgKeys[o] != "" {
				continue
			}
			if isolated && len(reach) > 1 {
				v.errf(m.DefRange, "Shared provider workspace",
					"%s: organisation %q has no org_key. Anthropic isolates its prompt cache per workspace "+
						"(platform.claude.com/docs/en/build-with-claude/prompt-caching), so institutions sharing one key share one cache: "+
						"give each organisation an org_key of its own workspace, or set cache_isolation = \"none\" to accept a shared cache", where, o)
			} else if defaultKey == "" {
				v.errf(m.DefRange, "Missing key file", "%s: organisation %q has no org_key and the model has no key_file", where, o)
			}
		}
		// Cost: a currency with its price date (the provider bills in
		// money, not GPU-hours).
		if g.Cost.Unit == "" {
			v.errf(m.DefRange, "Missing cost unit", "%s: an external provider bills in a currency: set cost { unit = \"USD\", price_date = \"YYYY-MM-DD\", … } from its price list", where)
		}
	}
	// A pool is either external or not, and an external model is never a
	// fallback target: prompts must not leave the site because a local
	// model was busy.
	poolExternal := map[string]bool{}
	poolSeen := map[string]bool{}
	for i, g := range c.ModelGroups {
		ext := g.External != nil
		if poolSeen[g.Pool] && poolExternal[g.Pool] != ext {
			v.errf(raw.Models[i].DefRange, "Mixed pool", "pool %q holds external and vLLM models; give the external provider a pool of its own (its capacity is the concurrency allowed at the provider)", g.Pool)
		}
		poolSeen[g.Pool], poolExternal[g.Pool] = true, ext
		for _, fb := range g.Fallbacks {
			if external[fb] {
				v.errf(raw.Models[i].FallbacksRange, "External fallback", "model %q: fallback %q is an external provider; prompts must not leave the site because another model was unavailable", g.Name, fb)
			}
		}
	}
	// Pre-call masking of PII and secrets, mandatory by default for every
	// scope allowed to use an external model.
	for i, g := range c.ModelGroups {
		e := g.External
		if e == nil || !e.RequireGuardrails {
			continue
		}
		for _, o := range e.Orgs {
			for _, typ := range []string{GuardPII, GuardSecrets} {
				if !maskingGuard(c.Guardrails, typ, o, "") {
					v.errf(raw.Models[i].OrgsRange, "Unguarded external model",
						"model %q: organisation %q needs a pre-stage %s guardrail (action mask or block, fail-closed) on the organisation before its prompts leave the site; or set external_requires_guardrails = false", g.Name, o, typ)
				}
			}
		}
		for _, t := range e.Teams {
			for _, typ := range []string{GuardPII, GuardSecrets} {
				if !maskingGuard(c.Guardrails, typ, teamOrg[t], t) {
					v.errf(raw.Models[i].TeamsRange, "Unguarded external model",
						"model %q: team %q needs a pre-stage %s guardrail (action mask or block, fail-closed) on the team or its organisation before its prompts leave the site; or set external_requires_guardrails = false", g.Name, t, typ)
				}
			}
		}
	}
}

// MaskingGuard reports whether guardrail g, of type typ, masks or blocks
// prompts before they are sent and fails closed.
func MaskingGuard(g Guardrail, typ string) bool {
	return g.Type == typ && g.Pre && !g.FailOpen && (g.Action == ActionMask || g.Action == ActionBlock)
}

// maskingGuard reports whether a guardrail of type typ covers the scope:
// the organisation (team ""), or the team or its organisation.
func maskingGuard(gs []Guardrail, typ, org, team string) bool {
	for _, g := range gs {
		if MaskingGuard(g, typ) && (slices.Contains(g.Orgs, org) || team != "" && slices.Contains(g.Teams, team)) {
			return true
		}
	}
	return false
}

func upstreamHosts(g *ModelGroup) string {
	var hs []string
	for _, u := range g.Upstreams {
		if pu, err := url.Parse(u.BaseURL); err == nil {
			hs = append(hs, pu.Host)
		}
	}
	return strings.Join(hs, ", ")
}

func loopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}
