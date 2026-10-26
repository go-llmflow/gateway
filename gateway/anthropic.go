package gateway

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
	"github.com/go-llmflow/gateway/store"
)

// An external provider: the Anthropic API, reached with API keys under
// Anthropic's Commercial Terms. References opened 2026-10-26:
//
//   - Messages API and headers (x-api-key, anthropic-version,
//     anthropic-beta): platform.claude.com/docs/en/api/messages,
//     /api/versioning, /api/beta-headers (several betas: one header,
//     comma-separated, or the header repeated);
//   - errors: /api/errors (429 rate_limit_error, 529 overloaded_error,
//     request-id; the Messages API takes up to 32 MB);
//   - rate limits: /api/rate-limits (retry-after in seconds, "not every
//     429 carries it; the spend-cap 429 never does"; limits are per
//     organisation, optionally per workspace; anthropic-workspace-id names
//     the workspace a key resolved to);
//   - prompt caching: /build-with-claude/prompt-caching ("Caches are
//     isolated by workspace" on the Claude API; usage reports
//     cache_creation_input_tokens, split by TTL in cache_creation, and
//     cache_read_input_tokens; input_tokens excludes both);
//   - OpenAI compatibility: /cli-sdks-libraries/libraries/openai-sdk ("not
//     considered a long-term or production-ready solution"), hence not
//     routed: an Anthropic model is reachable at /v1/messages only;
//   - Claude Code gateways: code.claude.com/docs/en/llm-gateway-protocol
//     (forward anthropic-version and anthropic-beta; the OAuth beta carries
//     a claude.ai subscription, which a gateway must not relay:
//     code.claude.com/docs/en/legal-and-compliance).

// errExternalEndpoint refuses an endpoint other than /v1/messages for a
// model of an external provider.
var errExternalEndpoint = errors.New("this model is served by Anthropic and is reachable at /v1/messages only: Anthropic's OpenAI-compatibility layer is \"not considered a long-term or production-ready solution\" (platform.claude.com/docs/en/cli-sdks-libraries/libraries/openai-sdk)")

// errBackoff: the provider asked this organisation to wait (429/529 with
// retry-after) and the wait is not over.
var errBackoff = errors.New("the provider asked to wait")

// errSharedWorkspace: two organisations reached the same provider
// workspace, so their prompt caches would be one.
var errSharedWorkspace = errors.New("the provider workspace is shared between organisations; requests are refused until the configuration gives each its own key")

// statusOverloaded is Anthropic's 529 overloaded_error.
const statusOverloaded = 529

const (
	// maxBackoff bounds a retry-after the gateway honours by itself.
	maxBackoff = 10 * time.Minute
	// defaultBackoff applies to a 429/529 without retry-after (an
	// overload, or the spend cap, which retrying cannot fix soon).
	defaultBackoff = 5 * time.Second
)

// extRuntime is what an external group keeps besides its configuration.
type extRuntime struct {
	cfg        *config.External
	keys       map[string]string // org → API key
	defaultKey string
	isolated   bool // cache_isolation is not "none"

	mu     sync.Mutex
	until  map[string]time.Time // org → provider back-off end
	ws     map[string]string    // org → anthropic-workspace-id seen
	wsOrg  map[string]string    // workspace → org
	poison map[string]bool      // orgs refused: shared workspace
}

func newExtRuntime(g config.ModelGroup, keys map[string]string, isolated bool) (*extRuntime, error) {
	e := &extRuntime{cfg: g.External, keys: map[string]string{}, isolated: isolated,
		until: map[string]time.Time{}, ws: map[string]string{}, wsOrg: map[string]string{}, poison: map[string]bool{}}
	for org, f := range g.External.OrgKeys {
		k := keys[f]
		if k == "" {
			return nil, fmt.Errorf("gateway: no key loaded for organisation %q of model %q", org, g.Name)
		}
		e.keys[org] = k
	}
	if len(g.Upstreams) > 0 {
		e.defaultKey = keys[g.Upstreams[0].KeyFile]
	}
	return e, nil
}

// key is the API key requests of org are sent with.
func (e *extRuntime) key(org string) string {
	if k := e.keys[org]; k != "" {
		return k
	}
	return e.defaultKey
}

// wait returns how long org must still wait, and whether it is refused
// outright (shared workspace).
func (e *extRuntime) wait(org string, now time.Time) (time.Duration, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.poison[org] {
		return 0, errSharedWorkspace
	}
	if d := e.until[org].Sub(now); d > 0 {
		return d, errBackoff
	}
	return 0, nil
}

// observe reads an answer: the workspace it was served from, and a back-off
// on 429 and 529. It reports a workspace newly found shared.
func (e *extRuntime) observe(org string, resp *http.Response, now time.Time) (sharedWith string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if ws := resp.Header.Get("Anthropic-Workspace-Id"); ws != "" && e.isolated {
		if other, ok := e.wsOrg[ws]; ok && other != org {
			e.poison[org], e.poison[other] = true, true
			sharedWith = other
		}
		e.wsOrg[ws], e.ws[org] = org, ws
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
		d := defaultBackoff
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s >= 0 {
			d = time.Duration(s) * time.Second
		}
		d = min(d, maxBackoff)
		if t := now.Add(d); t.After(e.until[org]) {
			e.until[org] = t
		}
	}
	return sharedWith
}

// egress is what an external request carries from the client to the
// header builder: the organisation (its key) and the client's Anthropic
// headers, filtered there.
type egress struct {
	org      string
	version  string
	betas    []string
	external bool
}

type egressKey struct{}

func withEgress(ctx context.Context, e egress) context.Context {
	return context.WithValue(ctx, egressKey{}, e)
}

func egressFrom(ctx context.Context) egress {
	e, _ := ctx.Value(egressKey{}).(egress)
	return e
}

// clientEgress reads the client's anthropic-version and anthropic-beta.
func clientEgress(r *http.Request, org string) egress {
	var betas []string
	for _, h := range r.Header.Values("Anthropic-Beta") {
		for _, b := range strings.Split(h, ",") {
			if b = strings.TrimSpace(b); b != "" && len(betas) < 32 {
				betas = append(betas, b)
			}
		}
	}
	return egress{org: org, version: strings.TrimSpace(r.Header.Get("Anthropic-Version")), betas: betas, external: true}
}

// externalHeaders builds the headers of a request to the provider from
// scratch: nothing the client sent is copied, except an allow-listed
// anthropic-version and the allow-listed anthropic-beta values. The key is
// the organisation's; the client's x-api-key or Authorization never leaves.
func (e *extRuntime) headers(h http.Header, eg egress, ctype, accept string) []string {
	h.Set("Content-Type", ctype)
	if accept == "text/event-stream" || accept == "application/json" {
		h.Set("Accept", accept)
	}
	h.Set("X-Api-Key", e.key(eg.org))
	ver := e.cfg.Versions[0]
	if slices.Contains(e.cfg.Versions, eg.version) {
		ver = eg.version
	}
	h.Set("Anthropic-Version", ver)
	var out []string
	for _, b := range eg.betas {
		if e.cfg.AllowsBeta(b) && !slices.Contains(out, b) {
			out = append(out, b)
		}
	}
	if len(out) > 0 {
		h.Set("Anthropic-Beta", strings.Join(out, ","))
	}
	return out
}

// externalAllowed reports whether a key may use an external group, and
// whether its guardrails mask PII and secrets before egress (checked at
// configuration time too; checked again here, since a key's guardrails come
// from the store).
func (s *Server) externalGuarded(g *group, guards []*guardrail) bool {
	if g.ext == nil || !g.ext.cfg.RequireGuardrails {
		return true
	}
	have := map[string]bool{}
	for _, gr := range guards {
		for _, typ := range []string{config.GuardPII, config.GuardSecrets} {
			if config.MaskingGuard(gr.cfg, typ) {
				have[typ] = true
			}
		}
	}
	return have[config.GuardPII] && have[config.GuardSecrets]
}

// isoFor is the cache isolation of a request to g: none for an external
// provider, which refuses vLLM's cache_salt and isolates its cache per
// workspace (one per organisation, see config/external.go).
func (s *Server) isoFor(g *group, k store.Key) openai.Isolation {
	if g != nil && g.ext != nil {
		return openai.Isolation{}
	}
	return s.isolation(k)
}

// externalVisible reports whether key may see group g at all.
func externalVisible(g *group, key store.Key) bool {
	return g.ext == nil || g.ext.cfg.Allows(key.Org, key.Team)
}
