package gateway

import (
	"context"
	"net/http"
	"net/url"
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
	seen := map[*upstream]bool{}
	for _, g := range s.groups {
		for _, u := range g.upstreams {
			if seen[u] {
				continue
			}
			seen[u] = true
			u.healthy.Store(s.probe(ctx, u))
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

// redactURL keeps scheme and host only, for logs and metric labels.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid"
	}
	return u.Scheme + "://" + u.Host
}
