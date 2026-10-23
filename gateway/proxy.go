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

	"github.com/go-llmflow/gateway/openai"
	"github.com/go-llmflow/gateway/store"
)

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
	return (float64(uncached)*c.InputPerMTok + float64(u.Cached())*c.CachedPerMTok +
		float64(u.CompletionTokens)*c.OutputPerMTok) / 1e6
}

// proxy handles one OpenAI endpoint.
func (s *Server) proxy(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t0 := time.Now()
		key, ok := s.authenticate(w, r)
		if !ok {
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.cfg.Limits.MaxBodyBytes))
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
		out, req, err := openai.Rewrite(body, func(m string) (string, error) {
			g = s.groups[m]
			// A model hidden from the key's team answers exactly like an
			// unknown one, so its existence is not revealed.
			if g == nil || !s.budget.modelAllowed(key.Team, m) {
				return "", errUnknownModel
			}
			return g.cfg.UpstreamModel, nil
		}, s.isolation(key))
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
			rid           = requestID()
			usage         openai.Usage
			estimated     bool
			upstreamLabel string
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
			s.rec.record(store.Record{
				Time: end.UTC(), Org: key.Org, Team: key.Team, Project: key.Project, KeyID: key.ID, Model: g.cfg.Name, Pool: g.cfg.Pool,
				Upstream: upstreamLabel, PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens,
				CachedTokens: usage.Cached(), Cost: cost, LatencyMS: float64(end.Sub(t0).Microseconds()) / 1000,
				Status: status, Priority: grant.Priority, Borrowing: grant.Borrowing, Stream: req.Stream,
				Estimated: estimated, Tags: tags,
			})
		}()

		ctx, cancel := context.WithTimeout(r.Context(), s.cfg.Limits.MaxRequestDuration)
		defer cancel()
		var resp *http.Response
		resp, u, tUp, err = s.send(ctx, g, path, out, r.Header.Get("Accept"), grant.Priority, rid)
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
			st, err = openai.CopySSE(w, func() { _ = rc.Flush() }, resp.Body, !req.ClientWantsUsage)
			tUpEnd = time.Now()
			usage, haveUsage = st.Usage, st.HaveUsage
			if err != nil {
				s.log.Info("stream ended early", "request_id", rid, "err", err)
				if r.Context().Err() != nil {
					status = 499
				}
			}
			return
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.Limits.MaxResponseBytes+1))
		tUpEnd = time.Now()
		if err != nil || int64(len(data)) > s.cfg.Limits.MaxResponseBytes {
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
			usage, haveUsage = openai.ParseUsage(data)
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
		req.Header.Set("Content-Type", "application/json")
		if accept == "text/event-stream" || accept == "application/json" {
			req.Header.Set("Accept", accept)
		}
		req.Header.Set("Authorization", "Bearer "+u.apiKey)
		req.Header.Set("X-Vllm-Priority", strconv.Itoa(priority))
		req.Header.Set("X-Request-Id", rid)
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
