package gateway

import (
	"bufio"
	"context"
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
}

func (u *upstream) available(now time.Time) bool {
	return u.healthy.Load() && now.UnixNano() >= u.cooldown.Load()
}

// group is a client-facing model.
type group struct {
	cfg       config.ModelGroup
	upstreams []*upstream
	pool      *fairshare.Pool
	class     fairshare.Class
	// ext is set for a model of an external provider (anthropic.go).
	ext *extRuntime
}

// queueSaturated reports whether vLLM's own queues, as last scraped, reach
// the group's saturation_waiting (#58, signal 2). Off when it is 0.
func (g *group) queueSaturated() bool {
	if g.cfg.SaturationWaiting <= 0 {
		return false
	}
	var sum int64
	known := false
	for _, u := range g.upstreams {
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
	for _, u := range g.upstreams {
		if tried[u] {
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
	for {
		s.probeAll(ctx)
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
			for _, u := range g.upstreams {
				scrape[u] = true
			}
		}
	}
	seen := map[*upstream]bool{}
	for _, g := range s.groups {
		for _, u := range g.upstreams {
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
