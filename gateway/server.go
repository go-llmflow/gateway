// Package gateway is the HTTP server: authentication with virtual keys,
// budgets and rate limits, fair-share priorities, routing to vLLM,
// streaming pass-through, accounting, metrics and the admin API.
package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/fairshare"
	"github.com/go-llmflow/gateway/store"
)

// Secrets are read from files by the caller and handed over here; the
// gateway never reads the environment.
type Secrets struct {
	// MasterKey protects the admin API.
	MasterKey string
	// UpstreamKeys maps an upstream key_file path to the vLLM --api-key
	// it holds.
	UpstreamKeys map[string]string
	// MetricsToken, when set, is required as a bearer token on /metrics.
	MetricsToken string
}

// Server is the gateway.
type Server struct {
	cfg     *config.Config
	st      store.Store
	secrets Secrets
	log     *slog.Logger
	now     func() time.Time

	groups  map[string]*group
	gorder  []string
	pools   map[string]*fairshare.Pool
	porder  []string
	client  *http.Client
	limiter *limiter
	budget  *budgeter
	rec     *recorder
	met     *metrics
	keys    *keyCache

	cancel context.CancelFunc
	bg     sync.WaitGroup
}

// Option tunes a Server (tests).
type Option func(*Server)

// WithClock replaces time.Now.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithLogger sets the logger (default: discard).
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// New builds a server. Call Start to run its background loops and Close to
// stop them.
func New(cfg *config.Config, st store.Store, secrets Secrets, opts ...Option) (*Server, error) {
	if secrets.MasterKey == "" {
		return nil, errors.New("gateway: empty master key")
	}
	s := &Server{
		cfg: cfg, st: st, secrets: secrets, now: time.Now,
		log:    slog.New(slog.DiscardHandler),
		groups: map[string]*group{}, pools: map[string]*fairshare.Pool{},
		met: newMetrics(),
	}
	for _, o := range opts {
		o(s)
	}
	shares := map[string]float64{}
	for _, t := range cfg.Teams {
		shares[t.Name] = t.Share
	}
	for _, p := range cfg.Pools {
		s.pools[p.Name] = fairshare.NewPool(p.Name, p.Capacity, shares, cfg.FairShare.HalfLife, s.now)
		s.porder = append(s.porder, p.Name)
	}
	// An upstream shared by several model groups (same URL, same key) is
	// one object, so its in-flight count and cooldown are shared too.
	ups := map[[2]string]*upstream{}
	for _, g := range cfg.ModelGroups {
		gr := &group{cfg: g, pool: s.pools[g.Pool], class: fairshare.Interactive}
		if g.Class == config.Batch {
			gr.class = fairshare.Batch
		}
		for _, cu := range g.Upstreams {
			if err := config.ValidateUpstream(cu.BaseURL); err != nil {
				return nil, err
			}
			key := secrets.UpstreamKeys[cu.KeyFile]
			if key == "" {
				return nil, fmt.Errorf("gateway: no key loaded for upstream %q of model %q", cu.Name, g.Name)
			}
			id := [2]string{cu.BaseURL, cu.KeyFile}
			u := ups[id]
			if u == nil {
				u = newUpstream(cu.BaseURL, key)
				ups[id] = u
			}
			gr.upstreams = append(gr.upstreams, u)
		}
		s.groups[g.Name] = gr
		s.gorder = append(s.gorder, g.Name)
	}
	s.client = &http.Client{
		Transport: &http.Transport{
			Proxy: nil, // never route upstream traffic through an environment proxy
			DialContext: (&net.Dialer{
				Timeout: 5 * time.Second, KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:          4096,
			MaxIdleConnsPerHost:   1024,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: cfg.Limits.UpstreamHeaderTimeout,
			DisableCompression:    true,
			ForceAttemptHTTP2:     false,
		},
		// Upstream redirects are not followed: a redirect could send the
		// internal key elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	s.limiter = newLimiter(s.now)
	s.budget = newBudgeter(st, cfg.BudgetRefresh, cfg.FailClosed(), s.now)
	s.rec = newRecorder(st, cfg.Store.QueueSize, cfg.Store.BatchSize, func(err error) {
		s.log.Error("accounting write failed", "err", err)
	})
	s.keys = newKeyCache(cfg.KeyCacheTTL, s.now)
	return s, nil
}

// Start ensures configured teams exist, loads budgets and starts the
// health, budget and accounting loops.
func (s *Server) Start(ctx context.Context) error {
	for _, t := range s.cfg.Teams {
		if err := s.st.CreateTeam(ctx, t.Name); err != nil && !errors.Is(err, store.ErrExists) {
			return fmt.Errorf("gateway: ensure team %s: %w", t.Name, err)
		}
	}
	if err := s.budget.refresh(ctx); err != nil {
		s.log.Error("initial budget refresh failed", "err", err)
	}
	bctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.rec.start()
	s.bg.Add(2)
	go func() { defer s.bg.Done(); s.healthLoop(bctx) }()
	go func() {
		defer s.bg.Done()
		s.budget.loop(bctx, s.cfg.BudgetRefresh, func(err error) { s.log.Error("budget refresh failed", "err", err) })
	}()
	return nil
}

// Close stops the background loops and drains the accounting queue.
func (s *Server) Close(ctx context.Context) {
	if s.cancel != nil {
		s.cancel()
	}
	s.bg.Wait()
	s.rec.close(ctx)
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.proxy("/v1/chat/completions"))
	mux.HandleFunc("POST /v1/completions", s.proxy("/v1/completions"))
	mux.HandleFunc("POST /v1/embeddings", s.proxy("/v1/embeddings"))
	mux.HandleFunc("GET /v1/models", s.models)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /metrics", s.metrics)
	s.adminRoutes(mux)
	return mux
}

// HTTPServer returns an http.Server with the configured limits.
func (s *Server) HTTPServer() *http.Server {
	return &http.Server{
		Addr:              s.cfg.Listen,
		Handler:           s.Handler(),
		ReadHeaderTimeout: s.cfg.Limits.ReadHeaderTimeout,
		ReadTimeout:       s.cfg.Limits.ReadTimeout,
		IdleTimeout:       s.cfg.Limits.IdleTimeout,
		MaxHeaderBytes:    s.cfg.Limits.MaxHeaderBytes,
		// No WriteTimeout: streams last as long as generation does;
		// max_request_duration bounds each request instead.
	}
}

// apiError writes an OpenAI-style error body.
func apiError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": typ, "code": status},
	})
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}

func (s *Server) models(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.authenticate(w, r); !ok {
		return
	}
	data := make([]map[string]any, 0, len(s.gorder))
	for _, n := range s.gorder {
		data = append(data, map[string]any{"id": n, "object": "model", "owned_by": "llmflow"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.FailClosed() && !s.budget.healthy() {
		http.Error(w, "accounting store unreachable", http.StatusServiceUnavailable)
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) metrics(w http.ResponseWriter, r *http.Request) {
	if s.secrets.MetricsToken != "" && !store.HashEqual(store.HashSecret(bearer(r)), store.HashSecret(s.secrets.MetricsToken)) {
		apiError(w, http.StatusUnauthorized, "authentication_error", "metrics token required")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	bw := bufio.NewWriter(w)
	defer bw.Flush()
	s.writeMetrics(bw)
}

func (s *Server) writeMetrics(w *bufio.Writer) {
	type gauge struct {
		name, help string
		get        func(fairshare.TeamStat) float64
	}
	gauges := []gauge{
		{"llmflow_inflight", "Requests in flight (sent to vLLM, not finished), by pool and team.", func(t fairshare.TeamStat) float64 { return float64(t.InFlight) }},
		{"llmflow_inflight_borrowing", "Requests in flight above the team's reservation.", func(t fairshare.TeamStat) float64 { return float64(t.Borrowing) }},
		{"llmflow_inflight_tokens", "Estimated tokens (prompt bytes/4 + max tokens) of requests in flight.", func(t fairshare.TeamStat) float64 { return float64(t.InFlightTokens) }},
		{"llmflow_borrowed_fraction", "Fraction of the team's in-flight requests that are borrowing.", func(t fairshare.TeamStat) float64 {
			if t.InFlight == 0 {
				return 0
			}
			return float64(t.Borrowing) / float64(t.InFlight)
		}},
		{"llmflow_reserved", "Within-share slots: share x C apportioned to integers by largest remainder.", func(t fairshare.TeamStat) float64 { return float64(t.Reserved) }},
		{"llmflow_fairshare_usage", "Decayed usage (cost units) used for the fair-share ranking.", func(t fairshare.TeamStat) float64 { return t.Usage }},
		{"llmflow_fairshare_level", "Level fairshare LF = S/U (+Inf when the team has no usage).", func(t fairshare.TeamStat) float64 { return t.LevelFairshare }},
	}
	snaps := map[string][]fairshare.TeamStat{}
	for _, p := range s.porder {
		snaps[p] = s.pools[p].Snapshot()
	}
	for _, g := range gauges {
		writeHeader(w, g.name, g.help, "gauge")
		for _, p := range s.porder {
			for _, t := range snaps[p] {
				writeSample(w, g.name, []string{"pool", "team"}, []string{p, t.Team}, g.get(t))
			}
		}
	}
	writeHeader(w, "llmflow_upstream_healthy", "1 when the upstream answers its health check and is not cooling down.", "gauge")
	writeHeader(w, "llmflow_upstream_inflight", "Requests in flight per upstream.", "gauge")
	now := s.now()
	for _, gn := range s.gorder {
		for _, u := range s.groups[gn].upstreams {
			v := 0.0
			if u.available(now) {
				v = 1
			}
			writeSample(w, "llmflow_upstream_healthy", []string{"group", "upstream"}, []string{gn, redactURL(u.base)}, v)
			writeSample(w, "llmflow_upstream_inflight", []string{"group", "upstream"}, []string{gn, redactURL(u.base)}, float64(u.inflight.Load()))
		}
	}
	s.met.requests.write(w)
	s.met.tokens.write(w)
	s.met.gpuHours.write(w)
	s.met.denied.write(w)
	s.met.retries.write(w)
	s.met.estimated.write(w)
	s.met.overhead.write(w)
	s.met.upstreamT.write(w)
	writeHeader(w, "llmflow_accounting_dropped_total", "Usage records dropped because the queue was full.", "counter")
	writeSample(w, "llmflow_accounting_dropped_total", nil, nil, float64(s.rec.dropped.Load()))
	writeHeader(w, "llmflow_accounting_failed_total", "Usage records whose write to the store failed.", "counter")
	writeSample(w, "llmflow_accounting_failed_total", nil, nil, float64(s.rec.failed.Load()))
	writeHeader(w, "llmflow_accounting_written_total", "Usage records written to the store.", "counter")
	writeSample(w, "llmflow_accounting_written_total", nil, nil, float64(s.rec.written.Load()))
	writeHeader(w, "llmflow_accounting_queue_length", "Usage records waiting to be written.", "gauge")
	writeSample(w, "llmflow_accounting_queue_length", nil, nil, float64(len(s.rec.queue)))
}
