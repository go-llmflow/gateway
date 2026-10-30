// Package gateway is the HTTP server: authentication with virtual keys,
// budgets and rate limits, fair-share priorities, routing to vLLM,
// streaming pass-through, accounting, metrics and the admin API.
package gateway

import (
	"bufio"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-authn/oidc"
	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/export"
	"github.com/go-llmflow/gateway/fairshare"
	"github.com/go-llmflow/gateway/ha"
	"github.com/go-llmflow/gateway/openai"
	"github.com/go-llmflow/gateway/spool"
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
	// CacheSaltKey is the HMAC key the per-tenant cache_salt values are
	// derived from (at least 32 bytes; unused with cache_isolation none).
	CacheSaltKey string
	// WebhookToken and SMTPPassword authenticate soft-budget alerts.
	WebhookToken string
	SMTPPassword string
	// SSOClientSecret is the admin login client's secret (empty: a public
	// client).
	SSOClientSecret string
	// SCIMToken is the bearer token of the provisioning client.
	SCIMToken string
	// AuditKey is the audit checkpoint signing key (an Ed25519 seed, base64
	// or hex); empty: checkpoints are not signed.
	AuditKey string
	// ExportCredentials are the S3 keys of each export, by name.
	ExportCredentials map[string]export.Credentials
	// GuardrailTokens are the bearer tokens of http guardrails, by name.
	GuardrailTokens map[string]string
	// TracingToken is the OTLP collector's bearer token (empty: none).
	TracingToken string
	// MCPTokens are the bearer tokens of the tool servers, by name.
	MCPTokens map[string]string
	// HASecret is the replicas' shared secret (ha block).
	HASecret string
	// HATLSCert, HATLSKey and HATLSCA (PEM) put the internal channel under
	// mutual TLS (empty: plain HTTP, HMAC-signed).
	HATLSCert, HATLSKey, HATLSCA string
}

// Server is the gateway.
type Server struct {
	cfg     *config.Config
	st      store.Store
	secrets Secrets
	log     *slog.Logger
	now     func() time.Time

	groups map[string]*group
	// probed is closed when the health loop's first round has stored every
	// upstream's health and queue: a value set after it is not overwritten
	// before the next interval.
	probed chan struct{}
	gorder []string
	pools  map[string]admitter
	porder []string
	// ha shares the pools between replicas (nil: one replica); haServer
	// serves its internal API.
	ha         *ha.Node
	haServer   *http.Server
	haListener net.Listener
	// haTLS is the internal listener's mutual-TLS configuration (nil:
	// plain HTTP).
	haTLS *tls.Config
	// shared is the rest of the replicas' shared state (SSO, alert
	// outbox, revocations); nil without an ha block.
	shared store.HAStateStore
	// capMu serialises the recomputation of a pool's C (slurm.go).
	capMu sync.Mutex
	// capNotes remembers the C last written to the pool's history (#97).
	capNotes capacityNotes
	client   *http.Client
	limiter  *limiter
	budget   *budgeter
	// escrow holds requests' worst-case costs on the budget ledger (hard
	// budgets); softBudgets, a test hook, restores the pre-#87 check for
	// the planted control of the property test.
	escrow      *escrow
	softBudgets bool
	// tracker keeps every admitted request until its record is stored, for
	// the arming budgets (arming.go).
	tracker *tracker
	// prof holds the workload profiles (profiles.go, #90); nil without
	// profile blocks.
	prof *profiler
	rec  *recorder
	met  *metrics
	keys *keyCache
	// authFails limits failed authentications per client network.
	authFails *failLimiter
	// allowedTags is the request-tag allow-list; alerts sends soft-budget
	// alerts (nil when no channel is configured).
	allowedTags map[string]bool
	alerts      *alerter
	// jwt verifies OpenID Connect access tokens (nil: virtual keys only);
	// jwtCache keeps verified tokens until their expiry or the key TTL.
	jwt       *oidc.Verifier
	jwtCache  *keyCache
	jwtClient *http.Client
	// sso holds the admin login client, sessions and logins in progress.
	sso *ssoState
	// secretWriter delivers secrets of scheduled key rotations.
	secretWriter SecretWriter
	// scimBackend is the SCIM provisioning backend (nil: no scim block).
	scimBackend *scimBackend
	// auditLog records administrative actions; auditKey signs checkpoints.
	auditLog *audit.Log
	auditKey ed25519.PrivateKey
	// exports are the export queues by name; anchor exports the audit
	// checkpoints to the one audit.anchor names.
	exports map[string]*export.Queue
	anchor  *anchorState
	// requestLogs are the per-team request logs, by team; a team absent
	// has none.
	requestLogs map[string]*requestLogger
	// policies by name, and the policy of each team that has one.
	policies   map[string]*config.Policy
	teamPolicy map[string]string
	// guardrails by name, the guardrails of each org and team, their
	// configuration order, the audit queue of their decisions and the
	// client of http guardrails.
	guardrails  map[string]*guardrail
	orgGuards   map[string][]string
	teamGuards  map[string][]string
	guardOrder  map[string]int
	guardAudit  *guardAuditQueue
	guardClient *http.Client
	// trace is the OpenTelemetry tracer (a noop one without a tracing
	// block).
	trace *tracing
	// adminFuncs are the admin API handlers by route pattern, audited, for
	// the admin UI.
	adminFuncs map[string]adminHandler
	// mcpServers are the tool servers by name, in configuration order
	// (mcpOrder); toolLists caches the teams' tool allow-lists.
	mcpServers map[string]*mcpServer
	mcpOrder   []string
	toolLists  *toolListCache

	cancel context.CancelFunc
	bg     sync.WaitGroup
	// alertCtx bounds soft-budget alert retries; Close cancels it.
	alertCtx    context.Context
	alertCancel context.CancelFunc
}

// Option tunes a Server (tests).
type Option func(*Server)

// WithClock replaces time.Now.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithLogger sets the logger (default: discard).
func WithLogger(l *slog.Logger) Option { return func(s *Server) { s.log = l } }

// WithJWTClient sets the HTTP client used for OIDC discovery and the key
// set (a private CA, a proxy).
func WithJWTClient(c *http.Client) Option { return func(s *Server) { s.jwtClient = c } }

// New builds a server. Call Start to run its background loops and Close to
// stop them.
func New(cfg *config.Config, st store.Store, secrets Secrets, opts ...Option) (*Server, error) {
	if secrets.MasterKey == "" {
		return nil, errors.New("gateway: empty master key")
	}
	if cfg.CacheIsolation != "none" && len(secrets.CacheSaltKey) < minCacheSaltKey {
		return nil, fmt.Errorf("gateway: cache_isolation %q needs a cache salt key of at least %d bytes", cfg.CacheIsolation, minCacheSaltKey)
	}
	s := &Server{
		cfg: cfg, st: st, secrets: secrets, now: time.Now,
		log:    slog.New(slog.DiscardHandler),
		groups: map[string]*group{}, pools: map[string]admitter{},
		met: newMetrics(), probed: make(chan struct{}),
	}
	for _, o := range opts {
		o(s)
	}
	shares := map[string]float64{}
	// Shares, reservations and borrowing are per organisation: the
	// institutions own the GPUs. The teams of an organisation draw on its
	// reservation together.
	for _, o := range cfg.Orgs {
		shares[o.Name] = o.Share
	}
	for _, p := range cfg.Pools {
		s.pools[p.Name] = localPool{fairshare.NewPool(p.Name, p.Capacity, shares, cfg.FairShare.HalfLife, s.now)}
		s.porder = append(s.porder, p.Name)
	}
	if cfg.HA != nil {
		if err := s.buildHA(shares); err != nil {
			return nil, err
		}
	}
	// An upstream shared by several model groups (same URL, same key) is
	// one object, so its in-flight count and cooldown are shared too.
	ups := map[[2]string]*upstream{}
	for _, g := range cfg.ModelGroups {
		gr := &group{cfg: g, pool: s.pools[g.Pool], class: fairshare.Interactive, prices: pricesFor(g.Cost)}
		if g.Class == config.Batch {
			gr.class = fairshare.Batch
		}
		if g.External != nil {
			ext, err := newExtRuntime(g, secrets.UpstreamKeys, cfg.CacheIsolation != "none")
			if err != nil {
				return nil, err
			}
			gr.ext = ext
		}
		var members []*upstream
		for _, cu := range g.Upstreams {
			if err := config.ValidateUpstream(cu.BaseURL); err != nil {
				return nil, err
			}
			key := secrets.UpstreamKeys[cu.KeyFile]
			// An external provider's keys are per organisation (gr.ext);
			// the upstream's own key, if any, is the default one.
			if key == "" && g.External == nil {
				return nil, fmt.Errorf("gateway: no key loaded for upstream %q of model %q", cu.Name, g.Name)
			}
			id := [2]string{cu.BaseURL, cu.KeyFile}
			if g.External != nil {
				id[0] = "external:" + cu.BaseURL
			}
			u := ups[id]
			if u == nil {
				u = newUpstream(cu.BaseURL, key)
				u.external = g.External != nil
				ups[id] = u
			}
			members = append(members, u)
		}
		gr.setMembers(members)
		if g.Slurm != nil {
			d, err := newSlurmDiscovery(s, gr, secrets.UpstreamKeys)
			if err != nil {
				return nil, err
			}
			gr.disc = d
		}
		s.groups[g.Name] = gr
		s.gorder = append(s.gorder, g.Name)
		if g.External != nil {
			hosts := make([]string, 0, len(g.Upstreams))
			for _, cu := range g.Upstreams {
				hosts = append(hosts, redactURL(cu.BaseURL))
			}
			s.log.Warn("model sends prompts and completions off the site, to an external provider",
				"model", g.Name, "provider", g.External.Provider, "upstream", strings.Join(hosts, ","),
				"orgs", strings.Join(g.External.Orgs, ","), "teams", strings.Join(g.External.Teams, ","),
				"guardrails_required", g.External.RequireGuardrails)
		}
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
	s.allowedTags = map[string]bool{}
	for _, t := range cfg.Tags.Allowed {
		s.allowedTags[t] = true
	}
	s.alertCtx, s.alertCancel = context.WithCancel(context.Background())
	if cfg.Alerts.WebhookURL != "" || cfg.Alerts.SMTP != nil {
		s.alerts = newAlerter(cfg.Alerts, secrets.WebhookToken, secrets.SMTPPassword, func(msg string, args ...any) { s.log.Error(msg, args...) })
	}
	s.budget.onSoft = s.onSoftLimit
	s.rec = newRecorder(st, cfg.Store.QueueSize, cfg.Store.BatchSize, func(err error) {
		s.log.Error("accounting write failed", "err", err)
	})
	s.tracker = newTracker()
	s.budget.onBudgets = s.tracker.sync
	s.rec.done = func(recs []store.Record) {
		s.budget.persisted(recs)
		ids := make([]string, 0, len(recs))
		for _, r := range recs {
			ids = append(ids, r.ID)
		}
		s.tracker.untrack(ids...)
	}
	if cfg.Store.SpoolDir != "" {
		sp, err := spool.Open(cfg.Store.SpoolDir, cfg.Store.SpoolMaxBytes)
		if err != nil {
			return nil, fmt.Errorf("gateway: accounting spool: %w", err)
		}
		s.rec.withSpool(sp, cfg.Store.SpoolDir, cfg.Store.SpoolMaxBytes)
		s.rec.onLost = func(n int, err error) {
			s.raise(context.Background(), "accounting_records_lost", map[string]any{"records": n, "err": err.Error()})
		}
	}
	// The escrow's holder: this replica, and this process (its spool's id,
	// so that the process that replays the spool after a crash releases
	// exactly this process's grants).
	holder := store.Holder{Replica: replicaName(cfg), Incarnation: randomHex(8)}
	if s.rec.spool != nil {
		holder.Incarnation = s.rec.spool.ID()
	}
	s.escrow = newEscrow(st, holder, cfg.Budgets.GrantShare, cfg.Budgets.GrantIdle, cfg.Limits.MaxRequestDuration, s.now)
	if cfg.Profiles.Enabled() {
		s.prof = newProfiler(cfg.Profiles)
	}
	s.rec.onAdopted = func(id string) error {
		ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
		defer cancel()
		n, err := st.ReleaseIncarnation(ctx, id)
		if err == nil && n > 0 {
			s.log.Info("budget grants of a finished process released after its spool was replayed", "incarnation", id, "nano", n)
		}
		return err
	}
	s.keys = newKeyCache(cfg.KeyCacheTTL, s.now)
	s.authFails = newFailLimiter(cfg.AuthFailuresPerMinute, s.now)
	s.jwtCache = newKeyCache(cfg.KeyCacheTTL, s.now)
	if s.jwtClient == nil {
		s.jwtClient = defaultJWTClient()
	}
	if cfg.SCIM != nil {
		if len(secrets.SCIMToken) < minSCIMToken {
			return nil, fmt.Errorf("gateway: the scim token must be at least %d bytes", minSCIMToken)
		}
		s.scimBackend = &scimBackend{s: s}
	}
	if secrets.AuditKey != "" {
		k, err := audit.ParsePrivateKey(secrets.AuditKey)
		if err != nil {
			return nil, fmt.Errorf("gateway: audit signing_key_file: %w", err)
		}
		s.auditKey = k
	}
	s.auditLog = audit.New(st, s.auditKey, s.now)
	s.policies, s.teamPolicy = map[string]*config.Policy{}, map[string]string{}
	for i := range cfg.Policies {
		p := &cfg.Policies[i]
		s.policies[p.Name] = p
		for _, t := range p.Teams {
			s.teamPolicy[t] = p.Name
		}
	}
	s.guardClient = &http.Client{
		Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext, MaxIdleConnsPerHost: 64},
		// A guardrail service's redirect could send prompts elsewhere.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if err := s.buildGuardrails(); err != nil {
		return nil, err
	}
	s.buildMCP()
	tr, err := newTracing(cfg.Tracing, secrets.TracingToken)
	if err != nil {
		return nil, err
	}
	s.trace = tr
	if s.guardAudit != nil {
		s.guardAudit.onDrop = func() {
			s.raise(context.Background(), "guardrail_audit_dropped", map[string]any{"dropped": s.guardAudit.dropped.Load()})
		}
	}
	return s, nil
}

// Start ensures configured teams exist, loads budgets and starts the
// health, budget and accounting loops.
func (s *Server) Start(ctx context.Context) error {
	for _, o := range s.cfg.Orgs {
		if err := s.st.CreateOrg(ctx, o.Name); err != nil && !errors.Is(err, store.ErrExists) {
			return fmt.Errorf("gateway: ensure org %s: %w", o.Name, err)
		}
		for _, t := range o.Teams {
			switch err := s.st.CreateTeam(ctx, t, o.Name); {
			case errors.Is(err, store.ErrConflict):
				return fmt.Errorf("gateway: team %s is configured under org %s but belongs to another org in the store", t, o.Name)
			case err != nil && !errors.Is(err, store.ErrExists):
				return fmt.Errorf("gateway: ensure team %s: %w", t, err)
			}
		}
	}
	if err := s.startJWT(ctx); err != nil {
		return err
	}
	if err := s.startSSO(ctx); err != nil {
		return err
	}
	if err := s.openExports(ctx); err != nil {
		return err
	}
	if s.scimBackend != nil {
		// Repair what a change interrupted half-way left behind.
		if err := s.scimBackend.reconcile(ctx); err != nil {
			s.log.Error("scim: reconciliation at start failed", "err", err)
		}
	}
	// Registered before the first budget refresh, and before anything is
	// admitted: a budget created from now on waits for this process's
	// arming report, and one created before is in that refresh (arming.go).
	if err := s.st.RegisterHolder(ctx, s.escrow.holder); err != nil {
		return fmt.Errorf("gateway: register in the budget ledger: %w", err)
	}
	if err := s.budget.refresh(ctx); err != nil {
		s.log.Error("initial budget refresh failed", "err", err)
	}
	if s.prof != nil {
		// The assignments and calibrations before the first request; the
		// profile budgets brought up to date (profilesLoop keeps them so).
		if err := s.reconcileProfiles(ctx); err != nil {
			s.log.Error("workload profiles: initial reconciliation failed", "err", err)
		}
	}
	if err := s.startHA(ctx); err != nil {
		return err
	}
	bctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.rec.start()
	s.bg.Add(4)
	go func() { defer s.bg.Done(); s.escrow.loop(bctx) }()
	go func() { defer s.bg.Done(); s.armingLoop(bctx) }()
	go func() { defer s.bg.Done(); s.healthLoop(bctx) }()
	for _, gn := range s.gorder {
		if d := s.groups[gn].disc; d != nil {
			s.bg.Add(1)
			go func() { defer s.bg.Done(); d.loop(bctx) }()
		}
	}
	go func() {
		defer s.bg.Done()
		s.budget.loop(bctx, s.cfg.BudgetRefresh, func(err error) { s.log.Error("budget refresh failed", "err", err) })
	}()
	if s.prof != nil {
		s.bg.Add(1)
		go func() { defer s.bg.Done(); s.profilesLoop(bctx) }()
	}
	s.startFluids(bctx)
	s.startCapacityHistory(bctx)
	if s.cfg.KeyRotation != nil {
		if s.secretWriter == nil {
			return errors.New("gateway: key_rotation needs a secret writer (vault)")
		}
		s.bg.Add(1)
		go func() { defer s.bg.Done(); s.rotationLoop(bctx) }()
	}
	if s.guardAudit != nil {
		s.bg.Add(1)
		go func() { defer s.bg.Done(); s.guardAuditLoop(bctx) }()
	}
	if s.shared != nil {
		s.bg.Add(2)
		go func() { defer s.bg.Done(); s.listenRevocations(bctx) }()
		go func() { defer s.bg.Done(); s.outboxLoop(bctx) }()
	}
	for _, l := range s.uniqueRequestLogs() {
		s.bg.Add(1)
		go func() { defer s.bg.Done(); l.loop(bctx) }()
	}
	if s.auditKey != nil {
		s.bg.Add(1)
		go func() { defer s.bg.Done(); s.checkpointLoop(bctx) }()
		if len(s.cfg.Audit.Retention) > 0 {
			s.bg.Add(1)
			go func() { defer s.bg.Done(); s.pruneLoop(bctx) }()
		}
	}
	return nil
}

// Close stops the background loops and drains the accounting queue.
func (s *Server) Close(ctx context.Context) {
	if s.cancel != nil {
		s.cancel()
	}
	s.alertCancel()
	s.bg.Wait()
	// After the client listener drained: the pools' owners hand over.
	s.closeHA(ctx)
	s.rec.close(ctx)
	// Once the records are stored (their holds settled): give back every
	// unused grant. What a lost record held stays reserved until an
	// administrator releases this replica (a record that never reaches
	// the store would otherwise leave its cost uncounted).
	s.escrow.tick(ctx, true)
	// Every record stored: this process leaves the ledger. With records
	// still in its spool it stays registered, so an arming budget waits for
	// the process that replays them.
	if s.rec.spooled.Load() == 0 && len(s.rec.queue) == 0 {
		if err := s.st.UnregisterHolder(ctx, s.escrow.holder); err != nil {
			s.log.Error("budget ledger: unregister failed", "err", err)
		}
	}
	// Last: the final checkpoint above queued its anchor.
	for _, q := range s.exports {
		q.Close(ctx)
	}
	// Flush the spans still batched.
	if err := s.trace.shutdown(ctx); err != nil {
		s.log.Error("tracing shutdown", "err", err)
	}
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	inf := func(h http.HandlerFunc) http.Handler { return s.withClientAddr(s.cfg.Routes.Inference, h) }
	mux.Handle("POST /v1/chat/completions", inf(s.proxy("/v1/chat/completions")))
	mux.Handle("POST /v1/completions", inf(s.proxy("/v1/completions")))
	mux.Handle("POST /v1/embeddings", inf(s.proxy("/v1/embeddings")))
	// Stateless: GET /v1/responses/{id} and cancel are not proxied (stored
	// responses would be shared by every tenant; see openai.ErrStateful).
	mux.Handle("POST /v1/responses", inf(s.proxyFor(openai.Responses, "/v1/responses")))
	// Anthropic Messages, served by vLLM itself (no translation here).
	mux.Handle("POST /v1/messages", inf(s.proxyFor(openai.Messages, "/v1/messages")))
	// Token counting (Claude Code calls it, and estimates from characters
	// without it): vLLM's own count of the rendered prompt, or Anthropic's
	// with the organisation's key. Authenticated and rate limited like
	// everything else, not charged.
	mux.Handle("POST /v1/messages/count_tokens", inf(s.proxyFor(openai.CountTokens, "/v1/messages/count_tokens")))
	// vLLM's rerank (score models), Jina/Cohere v1 and Cohere v2 shapes.
	mux.Handle("POST /v1/rerank", inf(s.proxyFor(openai.Rerank, "/v1/rerank")))
	mux.Handle("POST /v2/rerank", inf(s.proxyFor(openai.Rerank, "/v2/rerank")))
	// Whisper-style ASR on vLLM; a multipart form, bounded by
	// limits.max_audio_bytes.
	mux.Handle("POST /v1/audio/transcriptions", inf(s.proxyFor(openai.Transcriptions, "/v1/audio/transcriptions")))
	// Tokenization: authenticated and rate limited like everything else,
	// not charged, not guarded.
	mux.Handle("POST /tokenize", inf(s.proxyFor(openai.Tokenize, "/tokenize")))
	mux.Handle("POST /detokenize", inf(s.proxyFor(openai.Detokenize, "/detokenize")))
	mux.Handle("GET /v1/models", inf(s.models))
	mux.Handle("GET /healthz", s.withClientAddr(s.cfg.Routes.Healthz, http.HandlerFunc(s.healthz)))
	// /metrics: from an allowed network, or anywhere with the metrics token.
	mux.Handle("GET /metrics", s.withClientAddr(nil, http.HandlerFunc(s.metrics)))
	// MCP: one endpoint for every method (GET and DELETE answer 405).
	if len(s.mcpServers) > 0 {
		mux.Handle("/mcp", inf(s.mcpHandler))
	}
	s.adminRoutes(mux)
	if s.scimBackend != nil {
		mux.Handle("/scim/v2/", s.scimHandler())
	}
	// The UI's security headers go on every response under /ui/, including
	// the ones the mux writes itself (the redirect that cleans "//", 404s,
	// 405s), not only on the pages the UI handlers render.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No server-wide WriteTimeout (a stream lasts as long as generation
		// does), but no response outlives max_request_duration either: a
		// client that stops reading would otherwise block its handler in a
		// write forever, holding the buffered answer, its key's in-flight
		// slot and its fair-share grant (security audit 2026-10-26, A2).
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(s.cfg.Limits.MaxRequestDuration + writeGrace))
		if strings.HasPrefix(r.URL.Path, "/ui/") {
			uiHeaders(w)
		}
		adminHeaders(w, r.URL.Path)
		// Administration (API, UI, SSO, SCIM) reads the store on every
		// request: bounded, so a partitioned store answers 503 instead of
		// holding requests (A6). The chain verification reads the whole log.
		if p := r.URL.Path; strings.HasPrefix(p, "/admin/") || strings.HasPrefix(p, "/ui/") || strings.HasPrefix(p, "/scim/") {
			d := adminTimeout
			if p == "/admin/audit/verify" || p == "/ui/audit/verify" {
				d = s.cfg.Limits.MaxRequestDuration
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			r = r.WithContext(ctx)
		}
		mux.ServeHTTP(w, r)
	})
}

// adminTimeout bounds an administration request.
const adminTimeout = 60 * time.Second

// adminHeaders hardens the answers of the admin API, SSO and SCIM, errors
// included: JSON is never sniffed into a page, never framed, and loads
// nothing (security audit 2026-10-26, A8; the UI has its own uiHeaders).
func adminHeaders(w http.ResponseWriter, path string) {
	if !strings.HasPrefix(path, "/admin/") && !strings.HasPrefix(path, "/scim/") {
		return
	}
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

// writeGrace lets the error answer of a request that reached
// max_request_duration still be written.
const writeGrace = 5 * time.Second

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

// apiError writes an error body that both OpenAI and Anthropic clients
// read: OpenAI's {"error": {message, type, code}}, plus Anthropic's
// top-level "type": "error" (its error object has the same type and
// message fields).
func apiError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
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
	key, ok := s.authenticate(w, r)
	if !ok {
		return
	}
	data := make([]map[string]any, 0, len(s.gorder))
	for _, n := range s.gorder {
		if s.groups[n].cfg.Class != config.Guard && s.budget.modelAllowed(key.Team, n) && externalVisible(s.groups[n], key) {
			data = append(data, map[string]any{"id": n, "object": "model", "owned_by": "llmflow"})
		}
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
	a, _ := clientAddrOf(r)
	tokenOK := s.secrets.MetricsToken != "" && store.HashEqual(store.HashSecret(bearer(r)), store.HashSecret(s.secrets.MetricsToken))
	if !tokenOK && !inAny(a, s.cfg.Routes.Metrics) {
		if s.secrets.MetricsToken != "" {
			apiError(w, http.StatusUnauthorized, "authentication_error", "metrics token required")
		} else {
			apiError(w, http.StatusForbidden, "permission_denied", "address not allowed")
		}
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
		{"llmflow_inflight", "Requests in flight (sent to vLLM, not finished), by pool and organisation.", func(t fairshare.TeamStat) float64 { return float64(t.InFlight) }},
		{"llmflow_inflight_borrowing", "Requests in flight above the organisation's reservation.", func(t fairshare.TeamStat) float64 { return float64(t.Borrowing) }},
		{"llmflow_inflight_tokens", "Estimated tokens (prompt bytes/4 + max tokens) of requests in flight.", func(t fairshare.TeamStat) float64 { return float64(t.InFlightTokens) }},
		{"llmflow_borrowed_fraction", "Fraction of the organisation's in-flight requests that are borrowing.", func(t fairshare.TeamStat) float64 {
			if t.InFlight == 0 {
				return 0
			}
			return float64(t.Borrowing) / float64(t.InFlight)
		}},
		{"llmflow_reserved", "Within-share slots: share x C apportioned to integers by largest remainder.", func(t fairshare.TeamStat) float64 { return float64(t.Reserved) }},
		{"llmflow_fairshare_usage", "Decayed usage (cost units) used for the fair-share ranking.", func(t fairshare.TeamStat) float64 { return t.Usage }},
		{"llmflow_fairshare_level", "Level fairshare LF = S/U (+Inf when the organisation has no usage).", func(t fairshare.TeamStat) float64 { return t.LevelFairshare }},
		{"llmflow_inflight_phantoms", "Requests counted in flight without a grant (ha: a failover's safety margin, a silent replica's requests).", func(t fairshare.TeamStat) float64 { return float64(t.Phantoms) }},
	}
	if s.ha != nil {
		writeHeader(w, "llmflow_ha_owner", "1 when this replica decides the pool's fair share (holds its lease).", "gauge")
		writeHeader(w, "llmflow_ha_epoch", "The pool lease's epoch (fencing token) as this replica last saw it.", "gauge")
		writeHeader(w, "llmflow_ha_local_grants", "This replica's requests in flight in the pool.", "gauge")
		for _, p := range s.porder {
			owner, epoch := s.ha.Status(p)
			v := 0.0
			if owner {
				v = 1
			}
			writeSample(w, "llmflow_ha_owner", []string{"pool"}, []string{p}, v)
			writeSample(w, "llmflow_ha_epoch", []string{"pool"}, []string{p}, float64(epoch))
			writeSample(w, "llmflow_ha_local_grants", []string{"pool"}, []string{p}, float64(s.ha.Local(p)))
		}
		writeHeader(w, "llmflow_ha_unowned_total", "Requests admitted at the lowest priority (or refused) because no owner answered.", "counter")
		writeSample(w, "llmflow_ha_unowned_total", nil, nil, float64(s.ha.Degraded()))
	}
	snaps := map[string][]fairshare.TeamStat{}
	for _, p := range s.porder {
		snaps[p] = s.pools[p].snapshot()
	}
	for _, g := range gauges {
		writeHeader(w, g.name, g.help, "gauge")
		for _, p := range s.porder {
			for _, t := range snaps[p] {
				writeSample(w, g.name, []string{"pool", "org"}, []string{p, t.Team}, g.get(t))
			}
		}
	}
	writeHeader(w, "llmflow_pool_capacity", "The pool's C: its configured capacity plus that of its Slurm-discovered instances.", "gauge")
	for _, p := range s.porder {
		writeSample(w, "llmflow_pool_capacity", []string{"pool"}, []string{p}, float64(s.poolCapacity(p)))
	}
	writeHeader(w, "llmflow_slurm_last_success_timestamp_seconds", "Time of the last successful slurmrestd poll, per Slurm-discovered model (0: none yet).", "gauge")
	for _, gn := range s.gorder {
		if d := s.groups[gn].disc; d != nil {
			v := 0.0
			if t := d.lastSuccess(); !t.IsZero() {
				v = float64(t.UnixNano()) / 1e9
			}
			writeSample(w, "llmflow_slurm_last_success_timestamp_seconds", []string{"model"}, []string{gn}, v)
		}
	}
	s.met.slurmPolls.write(w)
	s.met.slurmRejected.write(w)
	writeHeader(w, "llmflow_upstream_healthy", "1 when the upstream answers its health check and is not cooling down.", "gauge")
	writeHeader(w, "llmflow_upstream_inflight", "Requests in flight per upstream.", "gauge")
	now := s.now()
	for _, gn := range s.gorder {
		for _, u := range s.groups[gn].members() {
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
	s.met.cost.write(w)
	s.met.denied.write(w)
	s.met.retries.write(w)
	s.met.fallbacks.write(w)
	s.met.estimated.write(w)
	s.met.softAlerts.write(w)
	if s.shared != nil {
		s.met.revocations.write(w)
	}
	if s.alerts != nil {
		s.alerts.failed.write(w)
		s.alerts.retried.write(w)
	}
	s.met.auditFailures.write(w)
	s.met.guardDecisions.write(w)
	s.met.guardErrors.write(w)
	s.met.toolCalls.write(w)
	s.met.guardSeconds.write(w)
	if s.guardAudit != nil {
		writeHeader(w, "llmflow_guardrail_audit_dropped_total", "Guardrail decisions not audited because the audit queue was full.", "counter")
		writeSample(w, "llmflow_guardrail_audit_dropped_total", nil, nil, float64(s.guardAudit.dropped.Load()))
	}
	s.writeExportMetrics(w)
	s.met.overhead.write(w)
	s.met.upstreamT.write(w)
	writeHeader(w, "llmflow_accounting_dropped_total", "Usage records dropped because the queue was full.", "counter")
	writeSample(w, "llmflow_accounting_dropped_total", nil, nil, float64(s.rec.dropped.Load()))
	writeHeader(w, "llmflow_accounting_failed_total", "Usage records lost: neither stored nor spooled (spool full or unwritable, or no spool).", "counter")
	writeSample(w, "llmflow_accounting_failed_total", nil, nil, float64(s.rec.failed.Load()))
	writeHeader(w, "llmflow_accounting_spooled", "Usage records in the durable spool, waiting for the store.", "gauge")
	writeSample(w, "llmflow_accounting_spooled", nil, nil, float64(s.rec.spooled.Load()))
	writeHeader(w, "llmflow_accounting_replayed_total", "Usage records adopted from the spool of a process that died, and stored.", "counter")
	writeSample(w, "llmflow_accounting_replayed_total", nil, nil, float64(s.rec.replayed.Load()))
	writeHeader(w, "llmflow_accounting_written_total", "Usage records written to the store.", "counter")
	writeSample(w, "llmflow_accounting_written_total", nil, nil, float64(s.rec.written.Load()))
	writeHeader(w, "llmflow_accounting_queue_length", "Usage records waiting to be written.", "gauge")
	writeSample(w, "llmflow_accounting_queue_length", nil, nil, float64(len(s.rec.queue)))
	// Hard budgets (#87).
	writeHeader(w, "llmflow_budget_hold_exceeded_total", "Requests whose cost exceeded the worst-case bound held for them (a bug in the bound: alert on any).", "counter")
	writeSample(w, "llmflow_budget_hold_exceeded_total", nil, nil, float64(s.escrow.exceeded.Load()))
	writeHeader(w, "llmflow_budget_grant_refills_total", "Grant growths in the budget ledger (one store transaction each).", "counter")
	writeSample(w, "llmflow_budget_grant_refills_total", nil, nil, float64(s.escrow.refills.Load()))
	writeHeader(w, "llmflow_budget_hold_refused_total", "Requests refused because a budget could not hold their worst-case cost.", "counter")
	writeSample(w, "llmflow_budget_hold_refused_total", nil, nil, float64(s.escrow.refused.Load()))
	writeHeader(w, "llmflow_budget_grant_return_failures_total", "Returns of unused grant the store refused (retried).", "counter")
	writeSample(w, "llmflow_budget_grant_return_failures_total", nil, nil, float64(s.escrow.returnErr.Load()))
}
