package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/fairshare"
)

// upstream is one vLLM server.
type upstream struct {
	base     string // without trailing slash
	apiKey   string // vLLM --api-key of this server
	inflight atomic.Int64
	cooldown atomic.Int64 // unix nanoseconds until which it is skipped
	healthy  atomic.Bool
	// waiting is vLLM's vllm:num_requests_waiting as last scraped, -1 when
	// unknown (not scraped, or the scrape failed): an unknown queue never
	// counts as saturated.
	waiting atomic.Int64
	// external: a provider's API, not probed (it has no /health, and its
	// keys are per organisation); a failed connection still cools it down.
	external bool
	// draining: a discovered instance on its way out (its Slurm job ended
	// or it failed health, slurm.go): it takes no new request, not even as
	// the last resort of pick, and is removed once idle.
	draining atomic.Bool
}

func (u *upstream) available(now time.Time) bool {
	return u.healthy.Load() && !u.draining.Load() && now.UnixNano() >= u.cooldown.Load()
}

// group is a client-facing model.
type group struct {
	cfg   config.ModelGroup
	ups   atomic.Pointer[[]*upstream]
	pool  admitter
	class fairshare.Class
	// ext is set for a model of an external provider (anthropic.go).
	ext *extRuntime
	// disc is set when Slurm jobs provide upstreams (slurm.go).
	disc *slurmDiscovery
	// prices are the cost's prices as integers (bound.go).
	prices groupPrices
	// seenLen is the largest max_model_len the upstreams' /v1/models
	// reported for this model (0: none yet), used when the configuration
	// does not set max_model_len.
	seenLen atomic.Int64
}

// contextLen is the context length that bounds a request's input: the
// configured one, else the largest an upstream reported; 0 when unknown.
func (g *group) contextLen() int64 {
	if g.cfg.MaxModelLen > 0 {
		return g.cfg.MaxModelLen
	}
	return g.seenLen.Load()
}

// noteLen records a max_model_len an upstream reported: the group keeps
// the largest, since a request may be served by any of its upstreams.
func (g *group) noteLen(n int64) {
	if n <= 0 || n > config.MaxTokensBound {
		return
	}
	for {
		cur := g.seenLen.Load()
		if n <= cur || g.seenLen.CompareAndSwap(cur, n) {
			return
		}
	}
}

// members is the group's current upstream set: static for a configured
// group, replaced as a whole (never modified in place) by discovery.
func (g *group) members() []*upstream {
	if p := g.ups.Load(); p != nil {
		return *p
	}
	return nil
}

func (g *group) setMembers(us []*upstream) { g.ups.Store(&us) }

// queueSaturated reports whether vLLM's own queues, as last scraped, reach
// the group's saturation_waiting (#58, signal 2). Off when it is 0.
func (g *group) queueSaturated() bool {
	if g.cfg.SaturationWaiting <= 0 {
		return false
	}
	var sum int64
	known := false
	for _, u := range g.members() {
		if w := u.waiting.Load(); w >= 0 {
			sum += w
			known = true
		}
	}
	return known && sum >= int64(g.cfg.SaturationWaiting)
}

// pick returns the available upstream with the fewest requests in flight,
// skipping those already tried. When none is available (all unhealthy or
// cooling down) it falls back to the least loaded untried one: a stale
// health verdict must not turn into a full outage.
func (g *group) pick(tried map[*upstream]bool, now time.Time) *upstream {
	var best, fallback *upstream
	for _, u := range g.members() {
		if tried[u] || u.draining.Load() {
			continue
		}
		if fallback == nil || u.inflight.Load() < fallback.inflight.Load() {
			fallback = u
		}
		if u.available(now) && (best == nil || u.inflight.Load() < best.inflight.Load()) {
			best = u
		}
	}
	if best != nil {
		return best
	}
	return fallback
}

func newUpstream(raw, apiKey string) *upstream {
	u := &upstream{base: strings.TrimRight(raw, "/"), apiKey: apiKey}
	u.healthy.Store(true)
	u.waiting.Store(-1)
	return u
}

// joinURL appends an endpoint path to an upstream base.
func (u *upstream) joinURL(path string) string {
	return u.base + path
}

// healthLoop probes every upstream until ctx ends.
func (s *Server) healthLoop(ctx context.Context) {
	t := time.NewTicker(s.cfg.Health.Interval)
	defer t.Stop()
	for first := true; ; first = false {
		s.probeAll(ctx)
		if first {
			close(s.probed)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Server) probeAll(ctx context.Context) {
	scrape := map[*upstream]bool{}
	for _, g := range s.groups {
		if g.cfg.SaturationWaiting > 0 {
			for _, u := range g.members() {
				scrape[u] = true
			}
		}
	}
	seen := map[*upstream]bool{}
	for _, g := range s.groups {
		for _, u := range g.members() {
			if seen[u] || u.external {
				continue
			}
			seen[u] = true
			u.healthy.Store(s.probe(ctx, u))
			if scrape[u] {
				u.waiting.Store(s.scrapeWaiting(ctx, u))
			}
		}
	}
	// The context lengths (hard budgets): each healthy upstream's
	// /v1/models, once per pass, for the models that do not configure one.
	cards := map[*upstream][]modelCard{}
	for _, g := range s.groups {
		if g.cfg.MaxModelLen > 0 || g.ext != nil {
			continue
		}
		for _, u := range g.members() {
			if u.external || !u.healthy.Load() {
				continue
			}
			cs, ok := cards[u]
			if !ok {
				cs = s.readModels(ctx, u)
				cards[u] = cs
			}
			g.noteLen(lenFor(cs, g.cfg.UpstreamModel))
		}
	}
}

// modelCard is the part of vLLM's ModelCard the gateway reads
// (vllm/entrypoints/openai/models/protocol.py, served by GET /v1/models;
// serving.py fills max_model_len from the model config).
type modelCard struct {
	ID          string `json:"id"`
	MaxModelLen int64  `json:"max_model_len"`
}

// readModels reads an upstream's model cards; nil when it cannot.
func (s *Server) readModels(ctx context.Context, u *upstream) []modelCard {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Health.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.joinURL("/v1/models"), nil)
	if err != nil {
		return nil
	}
	req.Header.Set("Authorization", "Bearer "+u.apiKey)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var list struct {
		Data []modelCard `json:"data"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, maxModelsBody)).Decode(&list) != nil {
		return nil
	}
	return list.Data
}

// lenFor returns the max_model_len of the card named model, or, when no
// card has that name (a LoRA naming scheme, an alias), the largest of all:
// the bound must not be too small. 0 when none reports one.
func lenFor(cards []modelCard, model string) int64 {
	var best int64
	for _, c := range cards {
		if c.ID == model && c.MaxModelLen > 0 {
			return c.MaxModelLen
		}
		best = max(best, c.MaxModelLen)
	}
	return best
}

func (s *Server) probe(ctx context.Context, u *upstream) bool {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Health.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.joinURL(s.cfg.Health.Path), nil)
	if err != nil {
		return false
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// maxMetricsBody bounds an upstream's /metrics answer.
const maxMetricsBody = 4 << 20

// scrapeWaiting reads vllm:num_requests_waiting (summed over its label sets)
// from an upstream's /metrics, at the health interval and within its
// timeout; -1 when it cannot be read. vLLM serves /metrics without
// authentication: one more reason its ports stay on the internal network.
func (s *Server) scrapeWaiting(ctx context.Context, u *upstream) int64 {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.Health.Timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.joinURL("/metrics"), nil)
	if err != nil {
		return -1
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return -1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return -1
	}
	return parseWaiting(io.LimitReader(resp.Body, maxMetricsBody))
}

// parseWaiting sums the samples of vllm:num_requests_waiting in a Prometheus
// text exposition; -1 when there is none.
func parseWaiting(r io.Reader) int64 {
	const name = "vllm:num_requests_waiting"
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	var sum float64
	found := false
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), name)
		if !ok || rest == "" || rest[0] != ' ' && rest[0] != '{' {
			continue // another metric, or a longer name with this prefix
		}
		if rest[0] == '{' {
			end := strings.LastIndexByte(rest, '}')
			if end < 0 {
				continue
			}
			rest = rest[end+1:]
		}
		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}
		v, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || v < 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		sum += v
		found = true
	}
	if !found || sc.Err() != nil {
		return -1
	}
	return int64(sum)
}

// redactURL keeps scheme and host only, for logs and metric labels.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid"
	}
	return u.Scheme + "://" + u.Host
}
