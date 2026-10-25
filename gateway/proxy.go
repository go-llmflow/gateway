package gateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
	"github.com/go-llmflow/gateway/store"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// nextFallback returns the next fallback of primary the key may use and
// afford (its team allowed the model, no budget of the model exceeded), or
// nil; idx walks primary's list.
func (s *Server) nextFallback(primary *group, idx *int, key store.Key, scope reqScope) *group {
	for *idx < len(primary.cfg.Fallbacks) {
		name := primary.cfg.Fallbacks[*idx]
		*idx++
		g := s.groups[name]
		if g == nil || g.cfg.Class == config.Guard || !s.budget.modelAllowed(key.Team, name) {
			continue
		}
		sc := scope
		sc.model = name
		if s.budget.check(sc) != nil {
			continue
		}
		return g
	}
	return nil
}

// guardRID returns the request id: the one the guardrails already used
// (their guard-model calls and audit entries carry it), or a new one.
func guardRID(rq *guardReq) string {
	if rq != nil {
		return rq.rid
	}
	return requestID()
}

var errUnknownModel = errors.New("unknown model")

// requestID returns a random identifier for one request.
func requestID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "llmf-" + hex.EncodeToString(b[:])
}

// cost converts usage to GPU-hours with the group's internal prices.
// Cached prompt tokens are a subset of prompt tokens and are priced
// separately.
func (g *group) cost(u openai.Usage) float64 {
	c := g.cfg.Cost
	uncached := u.PromptTokens - u.Cached()
	return (float64(uncached)*c.InputPerMTok+float64(u.Cached())*c.CachedPerMTok+
		float64(u.CompletionTokens)*c.OutputPerMTok)/1e6 + u.AudioSeconds/3600*c.AudioPerHour
}

// proxy handles one OpenAI-compatible endpoint of the chat family.
func (s *Server) proxy(path string) http.HandlerFunc { return s.proxyFor(openai.Chat, path) }

// errStreamGuarded refuses a stream whose events the post-call guardrails
// cannot rewrite yet.
var errStreamGuarded = errors.New("this endpoint cannot be streamed under post-call guardrails yet: send it with \"stream\": false")

// proxyFor handles one endpoint: the pipeline (auth, policy, budget, rate
// limit, guardrails, fair share, accounting) is shared; the body rewrite
// and the usage extraction are the endpoint's.
func (s *Server) proxyFor(ep openai.Endpoint, path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		sctx, span := s.trace.startRequest(r, path)
		defer span.End()
		r = r.WithContext(sctx)
		_, authSpan := s.trace.tracer.Start(sctx, "auth")
		key, ok := s.authenticate(w, r)
		authSpan.End()
		if !ok {
			span.SetAttributes(attribute.Bool("llmflow.authenticated", false))
			return
		}
		span.SetAttributes(attribute.String("llmflow.org", key.Org), attribute.String("llmflow.team", key.Team),
			attribute.String("llmflow.key", key.ID))
		pol := s.policyFor(key)
		var body []byte
		var audio *audioFile
		var err error
		if ep == openai.Transcriptions {
			// The form's fields become the JSON view the pipeline reads;
			// the audio is kept aside (see audio.go).
			body, audio, err = parseAudioForm(w, r, s.cfg.Limits.MaxAudioBytes)
			switch {
			case errors.Is(err, errAudioTooBig):
				apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "audio file too large")
				return
			case err != nil:
				apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
				return
			}
		} else {
			body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBody(pol)))
		}
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "request body too large")
				return
			}
			apiError(w, http.StatusBadRequest, "invalid_request_error", "could not read the request body")
			return
		}
		var g *group
		resolve := func(m string) (string, error) {
			g = s.groups[m]
			// A model hidden from the key's team, or a guard model (the
			// gateway's own), answers exactly like an unknown one, so its
			// existence is not revealed.
			if g == nil || g.cfg.Class == config.Guard || !s.budget.modelAllowed(key.Team, m) {
				return "", errUnknownModel
			}
			return g.cfg.UpstreamModel, nil
		}
		out, req, err := openai.RewriteFor(ep, body, resolve, s.isolation(key))
		switch {
		case errors.Is(err, errUnknownModel):
			apiError(w, http.StatusNotFound, "model_not_found", "the model does not exist")
			return
		case err != nil:
			apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if len(req.Stripped) > 0 {
			s.log.Info("client fields stripped", "key", key.ID, "team", key.Team, "fields", strings.Join(req.Stripped, ","))
		}

		tags, err := s.requestTags(r, key)
		if err != nil {
			apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
			return
		}
		if msg := policyRefusal(pol, body, tags); msg != "" {
			s.met.denied.add(1, key.Team, "policy")
			apiError(w, http.StatusBadRequest, "invalid_request_error", msg)
			return
		}
		scope := reqScope{org: key.Org, team: key.Team, project: key.Project, key: key.ID, model: g.cfg.Name, tags: tags}
		switch err := s.budget.check(scope); {
		case errors.Is(err, errBudgetExceeded):
			s.met.denied.add(1, key.Team, "budget")
			apiError(w, http.StatusTooManyRequests, "insufficient_quota", err.Error())
			return
		case err != nil:
			s.met.denied.add(1, key.Team, "store_unavailable")
			apiError(w, http.StatusServiceUnavailable, "service_unavailable", err.Error())
			return
		}
		if err := s.limiter.admit(key.ID, key.RPM, key.TPM, key.MaxInFlight); err != nil {
			s.met.denied.add(1, key.Team, "rate_limit")
			apiError(w, http.StatusTooManyRequests, "rate_limit_exceeded", err.Error())
			return
		}
		// Guardrails, last of the checks before vLLM: a guard model costs
		// GPU time, not spent on a request refused for cheaper reasons.
		var guards []*guardrail
		if ep.Guarded() {
			guards, err = s.guardsFor(key)
		}
		if err != nil {
			s.limiter.release(key.ID, 0)
			s.met.denied.add(1, key.Team, "guardrail")
			apiError(w, http.StatusServiceUnavailable, "service_unavailable", err.Error())
			return
		}
		var rq *guardReq
		if len(guards) > 0 {
			rq = &guardReq{s: s, key: key, scope: scope, rid: requestID(), guards: guards}
			defer rq.finish()
			if ep == openai.Responses && req.Stream && rq.hasStage("post") {
				s.limiter.release(key.ID, 0)
				apiError(w, http.StatusBadRequest, "invalid_request_error", errStreamGuarded.Error())
				return
			}
			masked, gerr := s.pre(r.Context(), rq, body)
			if gerr != nil {
				s.limiter.release(key.ID, 0)
				s.met.denied.add(1, key.Team, "guardrail")
				s.guardRefusal(w, gerr)
				return
			}
			if !bytes.Equal(masked, body) {
				// vLLM gets the masked prompt; so does the request log.
				body = masked
				if out, req, err = openai.RewriteFor(ep, body, resolve, s.isolation(key)); err != nil {
					s.limiter.release(key.ID, 0)
					apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
					return
				}
			}
		}
		promptEst := int64(len(out) / 4)
		grant := g.pool.Acquire(key.Org, g.class, promptEst+req.MaxTokens)

		// Everything below ends in finish, which charges what vLLM did.
		// The usage vLLM reports is used when it arrives. When it does not
		// (the client left before the final usage chunk, or before a
		// non-streaming answer), the work is still charged, with an
		// estimate: prompt bytes / 4, plus the content deltas already
		// forwarded for a stream. Charging zero would let a client use the
		// GPUs for free and keep its fair-share level high.
		var (
			st            openai.StreamResult
			haveUsage     bool
			worked        bool // vLLM received the request and may have computed
			u             *upstream
			tUp, tUpEnd   time.Time
			status        int
			pre           time.Duration
			rid           = guardRID(rq)
			usage         openai.Usage
			estimated     bool
			upstreamLabel string
			respBody      []byte // kept only for a content log
			respCut       bool
		)
		defer func() {
			if !haveUsage && worked {
				usage = openai.Usage{PromptTokens: promptEst, CompletionTokens: st.Deltas}
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				haveUsage, estimated = true, true
				s.met.estimated.add(1, key.Team, g.cfg.Name)
			}
			cost, consumed := 0.0, 0.0
			if haveUsage {
				cost = g.cost(usage)
				consumed = cost
				if consumed == 0 {
					// No price configured: rank fair share by tokens.
					consumed = float64(usage.PromptTokens+usage.CompletionTokens) / 1e6
				}
				s.budget.add(scope, cost)
				s.met.tokens.add(float64(usage.PromptTokens), key.Team, g.cfg.Name, "prompt")
				s.met.tokens.add(float64(usage.CompletionTokens), key.Team, g.cfg.Name, "completion")
				s.met.tokens.add(float64(usage.Cached()), key.Team, g.cfg.Name, "cached")
				s.met.gpuHours.add(cost, key.Team, g.cfg.Pool)
			}
			grant.Release(consumed)
			s.limiter.release(key.ID, usage.PromptTokens+usage.CompletionTokens)
			if status != 0 {
				s.met.requests.add(1, key.Team, g.cfg.Name, strconv.Itoa(status))
			}
			if !worked && status == 0 {
				return
			}
			end := time.Now()
			if !tUpEnd.IsZero() {
				s.met.overhead.observe((pre + end.Sub(tUpEnd)).Seconds())
				s.met.upstreamT.observe(tUpEnd.Sub(tUp).Seconds())
			}
			if u != nil {
				upstreamLabel = redactURL(u.base)
			}
			span.SetAttributes(attribute.String("llmflow.model", g.cfg.Name), attribute.String("llmflow.pool", g.cfg.Pool),
				attribute.Int("llmflow.priority", grant.Priority), attribute.Bool("llmflow.borrowing", grant.Borrowing),
				attribute.Int("http.response.status_code", status), attribute.Int64("llmflow.prompt_tokens", usage.PromptTokens),
				attribute.Int64("llmflow.completion_tokens", usage.CompletionTokens), attribute.Bool("llmflow.estimated", estimated),
				attribute.Bool("llmflow.stream", req.Stream))
			if status >= 500 {
				span.SetStatus(codes.Error, http.StatusText(status))
			}
			rec := store.Record{
				Time: end.UTC(), Org: key.Org, Team: key.Team, Project: key.Project, KeyID: key.ID, Model: g.cfg.Name, Pool: g.cfg.Pool,
				Upstream: upstreamLabel, PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
				CachedTokens: usage.Cached(), Cost: cost, LatencyMS: float64(end.Sub(t0).Microseconds()) / 1000,
				Status: status, Priority: grant.Priority, Borrowing: grant.Borrowing, Stream: req.Stream,
				Estimated: estimated, Tags: tags,
			}
			s.rec.record(rec)
			s.logRequest(rec, rid, body, respBody, respCut)
		}()
		maxContent, keepContent := s.logsContent(key.Team)

		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Limits.MaxRequestDuration)
		defer cancel()
		// The upstream span covers the call and the response (the stream,
		// for a stream); its context is what vLLM receives as traceparent.
		ctx, upSpan := s.trace.tracer.Start(ctx, "upstream", trace.WithSpanKind(trace.SpanKindClient))
		defer upSpan.End()
		var resp *http.Response
		sendOnce := func() error {
			if audio != nil {
				form, ctype, ferr := buildAudioForm(out, audio)
				if ferr != nil {
					return ferr
				}
				resp, u, tUp, err = s.sendTyped(ctx, g, path, form, ctype, r.Header.Get("Accept"), grant.Priority, rid)
			} else {
				resp, u, tUp, err = s.send(ctx, g, path, out, r.Header.Get("Accept"), grant.Priority, rid)
			}
			return nil
		}
		// Fallbacks: when no upstream of the group took the request (all
		// down or cooling down) or vLLM answered 429/503 (saturated), and
		// nothing has reached the client, the request moves to the next
		// fallback group its team may use and afford. It leaves the first
		// pool and acquires in the target pool, so it gets its org's
		// priority THERE; it is charged, accounted and recorded under the
		// model that served it (g, grant and scope are what the deferred
		// accounting reads).
		primary, fallbackIdx := g, 0
		for {
			if ferr := sendOnce(); ferr != nil {
				status = http.StatusBadRequest
				apiError(w, status, "invalid_request_error", ferr.Error())
				return
			}
			saturated := err == nil && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable)
			if err == nil && !saturated || r.Context().Err() != nil {
				break
			}
			next := s.nextFallback(primary, &fallbackIdx, key, scope)
			if next == nil {
				break
			}
			if resp != nil {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
				resp.Body.Close()
				u.inflight.Add(-1)
				resp, u = nil, nil
			}
			s.met.fallbacks.add(1, g.cfg.Name, next.cfg.Name)
			upSpan.AddEvent("fallback", trace.WithAttributes(attribute.String("from", g.cfg.Name), attribute.String("to", next.cfg.Name)))
			grant.Release(0)
			g = next
			scope.model = g.cfg.Name
			grant = g.pool.Acquire(key.Org, g.class, promptEst+req.MaxTokens)
			target := g.cfg.UpstreamModel
			if out, _, err = openai.RewriteFor(ep, body, func(string) (string, error) { return target, nil }, s.isolation(key)); err != nil {
				status = http.StatusBadRequest
				apiError(w, status, "invalid_request_error", err.Error())
				return
			}
			w.Header().Set("X-Llmflow-Model", g.cfg.Name)
		}
		pre = tUp.Sub(t0)
		if err != nil {
			if r.Context().Err() != nil {
				status = 499 // client closed the request
				// send returns the context error only when a request was
				// in flight at vLLM: the prompt may have been computed.
				worked = errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
				return
			}
			status = http.StatusBadGateway
			apiError(w, status, "upstream_error", "no upstream could serve the request")
			return
		}
		defer u.inflight.Add(-1)
		defer resp.Body.Close()

		h := w.Header()
		h.Set("X-Request-Id", rid)
		h.Set("X-Llmflow-Priority", strconv.Itoa(grant.Priority))
		if s.cfg.ExposeOverheadHdr {
			h.Set("X-Llmflow-Overhead-Ms", strconv.FormatFloat(float64(pre.Microseconds())/1000, 'f', 3, 64))
		}
		for _, k := range []string{"Content-Type", "Cache-Control"} {
			if v := resp.Header.Get(k); v != "" {
				h.Set(k, v)
			}
		}
		status = resp.StatusCode
		// vLLM computed only for a successful answer; an error status
		// (validation, overload) is not charged.
		worked = resp.StatusCode < 300
		if req.Stream && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			h.Set("X-Accel-Buffering", "no")
			w.WriteHeader(resp.StatusCode)
			rc := http.NewResponseController(w)
			_ = rc.Flush()
			// When the client leaves, CopySSE stops at the first failed
			// write and the request context cancels the upstream call:
			// vLLM is not kept generating for nobody.
			var dst io.Writer = w
			var cw *captureWriter
			if keepContent {
				cw = &captureWriter{ResponseWriter: w, max: maxContent}
				dst = cw
			}
			// Post-call guardrails sit between vLLM and the client (and the
			// content log, which keeps what the client got).
			var sf streamGuard
			if rq != nil && resp.StatusCode < 300 {
				if sf = s.streamGuardFor(ep, r.Context(), rq, dst); sf != nil {
					dst = sf
				}
			}
			st, err = openai.CopySSEFor(ep, dst, func() { _ = rc.Flush() }, resp.Body, !req.ClientWantsUsage)
			if sf != nil && err == nil {
				err = sf.Close()
				_ = rc.Flush()
			}
			tUpEnd = time.Now()
			if cw != nil {
				respBody, respCut = cw.buf.Bytes(), cw.truncated
			}
			usage, haveUsage = st.Usage, st.HaveUsage
			if err != nil {
				s.log.Info("stream ended early", "request_id", rid, "err", err)
				if r.Context().Err() != nil {
					status = 499
				}
			}
			return
		}
		maxResp := s.maxResponse(pol)
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxResp+1))
		tUpEnd = time.Now()
		if err != nil || int64(len(data)) > maxResp {
			if r.Context().Err() != nil {
				status = 499 // client gone while vLLM was answering
				return
			}
			status = http.StatusBadGateway
			worked = false
			apiError(w, status, "upstream_error", "upstream response unreadable or too large")
			return
		}
		if resp.StatusCode < 300 {
			usage, haveUsage = openai.ParseUsageFor(ep, data)
			if rq != nil && rq.hasStage("post") {
				// The work is done and charged whatever the guards say.
				guarded, gerr := s.post(r.Context(), rq, data)
				if gerr != nil {
					status = s.guardRefusal(w, gerr)
					return
				}
				data = guarded
			}
			if req.TextOut {
				// The client asked for plain text; vLLM was asked for JSON
				// (for its usage).
				text, ok := openai.TranscriptText(data)
				if !ok {
					status = http.StatusBadGateway
					apiError(w, status, "upstream_error", "the transcription answer has no text")
					return
				}
				data = []byte(text)
				h.Set("Content-Type", "text/plain; charset=utf-8")
			}
		}
		if keepContent {
			respBody = data
		}
		h.Set("Content-Length", strconv.Itoa(len(data)))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(data)
	}
}

// send tries upstreams, least loaded first, retrying on connection errors
// and 5xx answers while nothing has been sent to the client. The returned
// upstream has its in-flight count incremented; the caller decrements it.
func (s *Server) send(ctx context.Context, g *group, path string, body []byte, accept string, priority int, rid string) (*http.Response, *upstream, time.Time, error) {
	return s.sendTyped(ctx, g, path, body, "application/json", accept, priority, rid)
}

// sendTyped is send for a body of another media type (a multipart form).
func (s *Server) sendTyped(ctx context.Context, g *group, path string, body []byte, ctype, accept string, priority int, rid string) (*http.Response, *upstream, time.Time, error) {
	tried := map[*upstream]bool{}
	tFirst := time.Time{}
	lastErr := errors.New("no upstream")
	for attempt := 0; attempt <= s.cfg.Retries; attempt++ {
		u := g.pick(tried, s.now())
		if u == nil {
			break
		}
		tried[u] = true
		if attempt > 0 {
			s.met.retries.add(1, g.cfg.Name)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.joinURL(path), bytes.NewReader(body))
		if err != nil {
			return nil, nil, time.Now(), err
		}
		// Headers are built from scratch: nothing the client sent is
		// forwarded, so client X-Vllm-Priority, x-pass-* or Authorization
		// headers cannot reach vLLM.
		req.Header = http.Header{}
		req.Header.Set("Content-Type", ctype)
		if accept == "text/event-stream" || accept == "application/json" {
			req.Header.Set("Accept", accept)
		}
		req.Header.Set("Authorization", "Bearer "+u.apiKey)
		req.Header.Set("X-Vllm-Priority", strconv.Itoa(priority))
		req.Header.Set("X-Request-Id", rid)
		// W3C trace context (vLLM reads traceparent/tracestate): built by
		// the gateway's tracer, never copied from the client.
		s.trace.inject(ctx, req.Header)
		req.ContentLength = int64(len(body))
		u.inflight.Add(1)
		t := time.Now()
		if tFirst.IsZero() {
			tFirst = t
		}
		resp, err := s.client.Do(req)
		if err != nil {
			u.inflight.Add(-1)
			if ctx.Err() != nil {
				return nil, nil, tFirst, ctx.Err()
			}
			s.coolDown(u)
			lastErr = err
			continue
		}
		if resp.StatusCode >= 500 && attempt < s.cfg.Retries && len(tried) < len(g.upstreams) {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			u.inflight.Add(-1)
			s.coolDown(u)
			lastErr = fmt.Errorf("upstream status %d", resp.StatusCode)
			continue
		}
		if resp.StatusCode >= 500 {
			s.coolDown(u)
		}
		return resp, u, t, nil
	}
	if tFirst.IsZero() {
		tFirst = time.Now()
	}
	return nil, nil, tFirst, lastErr
}

func (s *Server) coolDown(u *upstream) {
	u.cooldown.Store(s.now().Add(s.cfg.Health.Cooldown).UnixNano())
}
