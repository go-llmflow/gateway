package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-gitsafe/gitsafe/redact"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/guard"
	"github.com/go-llmflow/gateway/openai"
	"github.com/go-llmflow/gateway/store"
	"github.com/go-llmflow/pii"
)

// Guardrails. A request goes through the guardrails of its key's org, its
// team and the key itself (a key can add guardrails, never remove one).
//
//   - Pre-call, on the prompt: the masking guards (hide_secrets, pii) run
//     first, in configuration order, on every text of the request; then the
//     classifying guards (llama_guard, prompt_guard, http) run concurrently,
//     each with its timeout, on the MASKED request, so that no classifier —
//     an external one least of all — sees what was masked.
//   - Post-call, on the completion: the same, on the answer; for a stream,
//     through a bounded hold-back (guard.Holdback) and a gate that releases
//     text only once the classifiers accepted it (guard.StreamFilter).
//
// A guard that errs or times out refuses the request (503) unless it is
// on_error = "allow". Every finding (mask, block, log) and every error is
// audited — kind "guardrail", the guard, the category and the action, never
// the text — and counted; a request that passes every guard is only
// counted in llmflow_requests_total, since auditing every request would
// put the audit log's database write on the path of each one.

// streamWindow is the hold-back of masking guards on streams: the longest
// PII match (pii.MaxLen) fits; a secret, which may be longer, is held as
// long as it grows.
const streamWindow = 256

// guardAnswerLimit bounds a guard model's or service's answer.
const guardAnswerLimit = 1 << 20

type guardrail struct {
	cfg config.Guardrail
	// find is set for masking guards, classify for classifying ones.
	find     guard.Finder
	classify func(ctx context.Context, rq *guardReq, msgs []guard.Message, completion string, post bool) (guard.Verdict, error)
}

func actionFor(c config.Guardrail, category string) string {
	if a, ok := c.Actions[category]; ok {
		return a
	}
	return c.Action
}

// buildGuardrails prepares the configured guardrails (New).
func (s *Server) buildGuardrails() error {
	s.guardrails = map[string]*guardrail{}
	s.orgGuards, s.teamGuards = map[string][]string{}, map[string][]string{}
	s.guardOrder = map[string]int{}
	for i, c := range s.cfg.Guardrails {
		g := &guardrail{cfg: c}
		switch c.Type {
		case config.GuardSecrets:
			r := redact.New()
			g.find = func(text string) []guard.Span {
				var out []guard.Span
				for _, sp := range r.Find(text) {
					cat := string(sp.Kind)
					out = append(out, guard.Span{Start: sp.Start, End: sp.End, Guard: c.Name, Category: cat, Action: actionFor(c, cat)})
				}
				return out
			}
		case config.GuardPII:
			sc := pii.New()
			g.find = func(text string) []guard.Span {
				var out []guard.Span
				for _, m := range sc.Find(text) {
					cat := string(m.Kind)
					out = append(out, guard.Span{Start: m.Start, End: m.End, Guard: c.Name, Category: cat, Action: actionFor(c, cat)})
				}
				return out
			}
		case config.GuardLlamaGuard:
			g.classify = s.llamaGuard(c)
		case config.GuardPromptGuard:
			g.classify = s.promptGuard(c)
		case config.GuardHTTP:
			tok := s.secrets.GuardrailTokens[c.Name]
			g.classify = s.httpGuard(c, tok)
			s.log.Warn("guardrail sends prompts and completions off the site", "guardrail", c.Name, "url", c.URL)
		default:
			return fmt.Errorf("gateway: guardrail %q: unknown type %q", c.Name, c.Type)
		}
		s.guardrails[c.Name] = g
		s.guardOrder[c.Name] = i
		for _, o := range c.Orgs {
			s.orgGuards[o] = append(s.orgGuards[o], c.Name)
		}
		for _, t := range c.Teams {
			s.teamGuards[t] = append(s.teamGuards[t], c.Name)
		}
	}
	if len(s.guardrails) > 0 {
		s.guardAudit = newGuardAuditQueue(4096)
	}
	return nil
}

// errGuardMissing: a key names a guardrail the configuration no longer has.
// The request is refused: running it without the guard its owner asked for
// would fail open.
var errGuardMissing = errors.New("a guardrail this key requires is no longer configured")

// guardsFor returns the guardrails of a key, in configuration order.
func (s *Server) guardsFor(k store.Key) ([]*guardrail, error) {
	if len(s.guardrails) == 0 {
		if len(k.Guardrails) > 0 {
			return nil, errGuardMissing
		}
		return nil, nil
	}
	seen := map[string]bool{}
	var names []string
	for _, list := range [][]string{s.orgGuards[k.Org], s.teamGuards[k.Team], k.Guardrails} {
		for _, n := range list {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	sort.Slice(names, func(i, j int) bool { return s.guardOrder[names[i]] < s.guardOrder[names[j]] })
	out := make([]*guardrail, 0, len(names))
	for _, n := range names {
		g := s.guardrails[n]
		if g == nil {
			return nil, errGuardMissing
		}
		out = append(out, g)
	}
	return out, nil
}

// guardReq is the guardrail state of one request.
type guardReq struct {
	s      *Server
	key    store.Key
	scope  reqScope
	rid    string
	guards []*guardrail
	// msgs are the request's texts after the pre-call guards.
	msgs []guard.Message

	mu        sync.Mutex
	decisions map[decision]int
}

type decision struct{ stage, guard, category, action string }

func (rq *guardReq) has(stage string, masking bool) bool {
	for _, g := range rq.guards {
		if (stage == "pre" && g.cfg.Pre || stage == "post" && g.cfg.Post) && (g.find != nil) == masking {
			return true
		}
	}
	return false
}

func (rq *guardReq) hasStage(stage string) bool { return rq.has(stage, true) || rq.has(stage, false) }

func (rq *guardReq) note(stage, g, category, action string) {
	rq.mu.Lock()
	defer rq.mu.Unlock()
	if rq.decisions == nil {
		rq.decisions = map[decision]int{}
	}
	rq.decisions[decision{stage, g, category, action}]++
}

// finish counts and audits the request's findings: one audit entry per
// guard, stage, category and action, with the number of findings.
func (rq *guardReq) finish() {
	rq.mu.Lock()
	ds := rq.decisions
	rq.mu.Unlock()
	keys := make([]decision, 0, len(ds))
	for d := range ds {
		keys = append(keys, d)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		return a.stage+a.guard+a.category+a.action < b.stage+b.guard+b.category+b.action
	})
	for _, d := range keys {
		n := ds[d]
		rq.s.met.guardDecisions.add(float64(n), d.guard, d.stage, d.action)
		after, _ := json.Marshal(map[string]any{
			"guardrail": d.guard, "stage": d.stage, "category": d.category, "action": d.action, "count": n,
			"request_id": rq.rid, "team": rq.key.Team, "org": rq.key.Org,
		})
		rq.s.guardAudit.add(store.AuditEntry{Kind: "guardrail", Actor: "key:" + rq.key.ID, Roles: "[]",
			Action: "guardrail." + d.action, Target: d.guard, Before: "null", After: string(after), RequestID: rq.rid})
	}
}

// guardBlock is a request refused by a guardrail.
type guardBlock struct {
	guard      string
	categories []string
}

func (b *guardBlock) Error() string {
	return "refused by guardrail " + b.guard + " (" + strings.Join(b.categories, ", ") + ")"
}

// guardFailure is a fail-closed guard that could not judge.
type guardFailure struct {
	guard string
	err   error
}

func (f *guardFailure) Error() string { return "guardrail " + f.guard + " is unavailable" }

// guardRefusal answers a request a guardrail stopped.
func (s *Server) guardRefusal(w http.ResponseWriter, err error) int {
	var b *guardBlock
	if errors.As(err, &b) {
		apiError(w, http.StatusBadRequest, "guardrail_violation", b.Error())
		return http.StatusBadRequest
	}
	var f *guardFailure
	if errors.As(err, &f) {
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", f.Error())
		return http.StatusServiceUnavailable
	}
	apiError(w, http.StatusBadRequest, "invalid_request_error", "the request could not be inspected by its guardrails")
	return http.StatusBadRequest
}

// maskWith returns the text function of the masking guards of a stage: it
// masks, notes findings, and records the first block.
func (rq *guardReq) maskWith(stage string, blocked *error) func(string) string {
	var finders []guard.Finder
	for _, g := range rq.guards {
		if g.find != nil && (stage == "pre" && g.cfg.Pre || stage == "post" && g.cfg.Post) {
			finders = append(finders, g.find)
		}
	}
	if len(finders) == 0 {
		return func(s string) string { return s }
	}
	return func(text string) string {
		spans := guard.Combine(text, finders)
		for _, sp := range spans {
			rq.note(stage, sp.Guard, sp.Category, sp.Action)
			if sp.Action == config.ActionBlock && *blocked == nil {
				*blocked = &guardBlock{guard: sp.Guard, categories: []string{sp.Category}}
			}
		}
		return guard.Apply(text, spans, guard.Placeholder)
	}
}

// pre runs the pre-call guards on a request body and returns it masked.
func (s *Server) pre(ctx context.Context, rq *guardReq, body []byte) ([]byte, error) {
	t0 := time.Now()
	defer func() { s.met.guardSeconds.observe(time.Since(t0).Seconds()) }()
	var blocked error
	mask := rq.maskWith("pre", &blocked)
	out, msgs, err := guard.MapRequest(body, func(_, text string) string { return mask(text) })
	if err != nil {
		return nil, err
	}
	rq.msgs = msgs
	if blocked != nil {
		return nil, blocked
	}
	if err := s.classifyAll(ctx, rq, "pre", ""); err != nil {
		return nil, err
	}
	return out, nil
}

// post runs the post-call guards on a non-streamed answer.
func (s *Server) post(ctx context.Context, rq *guardReq, data []byte) ([]byte, error) {
	t0 := time.Now()
	defer func() { s.met.guardSeconds.observe(time.Since(t0).Seconds()) }()
	var blocked error
	mask := rq.maskWith("post", &blocked)
	out, texts, err := guard.MapResponse(data, true, mask)
	if err != nil {
		return nil, err
	}
	if blocked != nil {
		return nil, blocked
	}
	if err := s.classifyAll(ctx, rq, "post", strings.Join(texts, "\n")); err != nil {
		return nil, err
	}
	return out, nil
}

// classifyAll runs the classifying guards of a stage concurrently, each
// under its timeout.
func (s *Server) classifyAll(ctx context.Context, rq *guardReq, stage, completion string) error {
	var gs []*guardrail
	for _, g := range rq.guards {
		if g.classify != nil && (stage == "pre" && g.cfg.Pre || stage == "post" && g.cfg.Post) {
			gs = append(gs, g)
		}
	}
	if len(gs) == 0 {
		return nil
	}
	errs := make([]error, len(gs))
	var wg sync.WaitGroup
	for i, g := range gs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
			defer cancel()
			v, err := g.classify(cctx, rq, rq.msgs, completion, stage == "post")
			errs[i] = s.judge(rq, g, stage, v, err)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// judge turns a classifier's verdict into a decision.
func (s *Server) judge(rq *guardReq, g *guardrail, stage string, v guard.Verdict, err error) error {
	name := g.cfg.Name
	if err != nil {
		if g.cfg.FailOpen {
			s.met.guardErrors.add(1, name, "allowed")
			rq.note(stage, name, "error", "allow")
			s.log.Warn("guardrail failed, request allowed (fail-open)", "guardrail", name, "request_id", rq.rid, "err", err)
			return nil
		}
		s.met.guardErrors.add(1, name, "blocked")
		rq.note(stage, name, "error", config.ActionBlock)
		s.log.Error("guardrail failed, request refused (fail-closed)", "guardrail", name, "request_id", rq.rid, "err", err)
		return &guardFailure{guard: name, err: err}
	}
	if !v.Flagged {
		return nil
	}
	var block []string
	for _, c := range v.Categories {
		a := v.Action
		if a == "" {
			a = actionFor(g.cfg, c)
		}
		rq.note(stage, name, c, a)
		if a == config.ActionBlock {
			block = append(block, c)
		}
	}
	if len(block) > 0 {
		return &guardBlock{guard: name, categories: block}
	}
	return nil
}

// streamFilter returns the writer that applies the post-call guards to a
// stream written to dst, or nil when there are none.
func (s *Server) streamFilter(ctx context.Context, rq *guardReq, dst io.Writer) *guard.StreamFilter {
	if !rq.hasStage("post") {
		return nil
	}
	f := &guard.StreamFilter{Dst: dst}
	var finders []guard.Finder
	for _, g := range rq.guards {
		if g.find != nil && g.cfg.Post {
			finders = append(finders, g.find)
		}
		if g.classify != nil && g.cfg.Post {
			if f.CheckEvery == 0 || g.cfg.StreamCheckBytes < f.CheckEvery {
				f.CheckEvery = g.cfg.StreamCheckBytes
			}
		}
	}
	if len(finders) > 0 {
		f.NewHoldback = func() *guard.Holdback {
			return &guard.Holdback{Window: streamWindow, Finders: finders, Repl: guard.Placeholder}
		}
		f.Spans = func(spans []guard.Span) error {
			var err error
			for _, sp := range spans {
				rq.note("post", sp.Guard, sp.Category, sp.Action)
				if sp.Action == config.ActionBlock && err == nil {
					err = &guardBlock{guard: sp.Guard, categories: []string{sp.Category}}
				}
			}
			return err
		}
	}
	if f.CheckEvery > 0 {
		f.Check = func(_ int, completion string, _ bool) error {
			t0 := time.Now()
			defer func() { s.met.guardSeconds.observe(time.Since(t0).Seconds()) }()
			return s.classifyAll(ctx, rq, "post", completion)
		}
	}
	f.BlockEvent = func(err error) []byte {
		msg, typ := "the stream was stopped by a guardrail", "guardrail_violation"
		var b *guardBlock
		var gf *guardFailure
		switch {
		case errors.As(err, &b):
			msg = b.Error()
		case errors.As(err, &gf):
			msg, typ = gf.Error(), "service_unavailable"
		}
		ev, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": typ}})
		return ev
	}
	return f
}

// ---- guard models: Llama Guard, Prompt Guard ----
//
// A guard model is a model group of class "guard", reached through the
// gateway's own routing (upstream choice, retries, cooldown) at vLLM
// priority 0, the highest: the guard is on the critical path of the
// request it judges. It does not go through the fair-share pool — it is
// not the team's work but the gateway's check of it, and making it wait
// behind the team's own requests would add their queueing to every one —
// but what it costs is charged to the team: a spend record and its
// budgets, under the guard model's name.

func (s *Server) callGuardModel(ctx context.Context, rq *guardReq, model, path string, body []byte) ([]byte, error) {
	g := s.groups[model]
	if g == nil {
		return nil, fmt.Errorf("guard model %q is not configured", model)
	}
	t0 := time.Now()
	resp, u, _, err := s.send(ctx, g, path, body, "application/json", 0, rq.rid)
	if err != nil {
		return nil, err
	}
	defer u.inflight.Add(-1)
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, guardAnswerLimit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > guardAnswerLimit {
		return nil, errors.New("guard model answer too large")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("guard model answered %d", resp.StatusCode)
	}
	usage, ok := openai.ParseUsage(data)
	cost := 0.0
	if ok {
		cost = g.cost(usage)
		sc := rq.scope
		sc.model = g.cfg.Name
		s.budget.add(sc, cost)
		s.met.tokens.add(float64(usage.PromptTokens), rq.key.Team, g.cfg.Name, "prompt")
		s.met.tokens.add(float64(usage.CompletionTokens), rq.key.Team, g.cfg.Name, "completion")
		s.met.gpuHours.add(cost, rq.key.Team, g.cfg.Pool)
	}
	end := time.Now()
	s.rec.record(store.Record{
		Time: end.UTC(), Org: rq.key.Org, Team: rq.key.Team, Project: rq.key.Project, KeyID: rq.key.ID, Model: g.cfg.Name, Pool: g.cfg.Pool,
		Upstream: redactURL(u.base), PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
		CachedTokens: usage.Cached(), Cost: cost, LatencyMS: float64(end.Sub(t0).Microseconds()) / 1000,
		Status: resp.StatusCode, Estimated: !ok, Tags: rq.scope.tags,
	})
	return data, nil
}

func (s *Server) llamaGuard(c config.Guardrail) func(context.Context, *guardReq, []guard.Message, string, bool) (guard.Verdict, error) {
	return func(ctx context.Context, rq *guardReq, msgs []guard.Message, completion string, post bool) (guard.Verdict, error) {
		turns := guard.LlamaGuardConversation(msgs, completion, post)
		if len(turns) == 0 {
			return guard.Verdict{}, nil
		}
		g := s.groups[c.Model]
		if g == nil {
			return guard.Verdict{}, fmt.Errorf("guard model %q is not configured", c.Model)
		}
		data, err := s.callGuardModel(ctx, rq, c.Model, "/v1/chat/completions", guard.LlamaGuardRequest(g.cfg.UpstreamModel, turns))
		if err != nil {
			return guard.Verdict{}, err
		}
		answer, err := guard.ChatAnswer(data)
		if err != nil {
			return guard.Verdict{}, err
		}
		return guard.ParseLlamaGuard(answer)
	}
}

func (s *Server) promptGuard(c config.Guardrail) func(context.Context, *guardReq, []guard.Message, string, bool) (guard.Verdict, error) {
	return func(ctx context.Context, rq *guardReq, msgs []guard.Message, _ string, _ bool) (guard.Verdict, error) {
		segs, ok := guard.PromptGuardInput(msgs)
		if !ok {
			return guard.Verdict{}, fmt.Errorf("prompt too long for %d segments", guard.MaxSegments)
		}
		if len(segs) == 0 {
			return guard.Verdict{}, nil
		}
		g := s.groups[c.Model]
		if g == nil {
			return guard.Verdict{}, fmt.Errorf("guard model %q is not configured", c.Model)
		}
		data, err := s.callGuardModel(ctx, rq, c.Model, "/classify", guard.PromptGuardRequest(g.cfg.UpstreamModel, segs))
		if err != nil {
			return guard.Verdict{}, err
		}
		return guard.ParsePromptGuard(data, len(segs), c.Threshold)
	}
}

// ---- generic HTTP guardrail (off by default: allow_external) ----

func (s *Server) httpGuard(c config.Guardrail, token string) func(context.Context, *guardReq, []guard.Message, string, bool) (guard.Verdict, error) {
	return func(ctx context.Context, rq *guardReq, msgs []guard.Message, completion string, post bool) (guard.Verdict, error) {
		stage := "pre"
		if post {
			stage = "post"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(guard.HTTPRequest(c.Name, stage, msgs, completion)))
		if err != nil {
			return guard.Verdict{}, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Request-Id", rq.rid)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := s.guardClient.Do(req)
		if err != nil {
			return guard.Verdict{}, err
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10+1))
		if err != nil {
			return guard.Verdict{}, err
		}
		if len(data) > 64<<10 {
			return guard.Verdict{}, errors.New("guardrail service answer too large")
		}
		if resp.StatusCode != http.StatusOK {
			return guard.Verdict{}, fmt.Errorf("guardrail service answered %d", resp.StatusCode)
		}
		v, err := guard.ParseHTTP(data)
		if err == nil && v.Action != "" && !slices.Contains([]string{config.ActionBlock, config.ActionLog}, v.Action) {
			err = errors.New("guardrail service asked for an action it may not take")
		}
		return v, err
	}
}

// ---- asynchronous audit of guardrail decisions ----
//
// Audit appends serialise on the database; decisions are queued and written
// in the background so a request never waits on them. A full queue drops
// the entry, counts it and alerts (at most once a minute).

type guardAuditQueue struct {
	ch       chan store.AuditEntry
	dropped  atomic.Int64
	lastWarn atomic.Int64
	onDrop   func()
}

func newGuardAuditQueue(n int) *guardAuditQueue {
	return &guardAuditQueue{ch: make(chan store.AuditEntry, n)}
}

func (q *guardAuditQueue) add(e store.AuditEntry) {
	if q == nil {
		return
	}
	select {
	case q.ch <- e:
	default:
		q.dropped.Add(1)
		if now := time.Now().Unix(); q.lastWarn.Load() < now-60 {
			q.lastWarn.Store(now)
			if q.onDrop != nil {
				q.onDrop()
			}
		}
	}
}

// guardAuditLoop writes queued decisions until ctx ends, then drains.
func (s *Server) guardAuditLoop(ctx context.Context) {
	q := s.guardAudit
	for {
		select {
		case e := <-q.ch:
			s.appendAudit(context.Background(), e)
		case <-ctx.Done():
			for {
				select {
				case e := <-q.ch:
					s.appendAudit(context.Background(), e)
				default:
					return
				}
			}
		}
	}
}
