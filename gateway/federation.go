package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/netip"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/config"
)

// Federation mounts: a peer dispatcher (ILaaS, CentraleSupelec/
// aristote-dispatcher) sends its users' requests here as to one more vLLM
// node. Under the mount's prefix the inference API of the root answers for the
// keys of one organisation, and GET <prefix>/metrics gives the dispatcher the
// signal its "least busy" strategy reads (src/consumer/strategy/
// metrics_tracker.py, histogram.py, least_busy.py): vLLM's
// vllm:time_to_first_token_seconds histogram, scraped without credentials.
//
// What the dispatcher needs from the histogram: nothing until the node served
// something (an empty histogram scores -1, "never used, send it everything"),
// a p95 that goes up with the node's latency, and +Inf when the node refuses
// for lack of room, so that the traffic goes to its other nodes. The mount
// therefore observes, for the requests that came through it only, the time to
// the first byte of the answer, and counts every refusal in the +Inf bucket.
// Nothing about the organisations that own the pool is in it.

// ttftBounds are vLLM's own bucket upper bounds for
// vllm:time_to_first_token_seconds: TIME_TO_FIRST_TOKEN_BUCKETS in
// vllm/v1/metrics/buckets.py (selected by histogram_buckets("time_to_first_token")
// at vllm/v1/metrics/loggers.py:831, vllm-project/vllm main at 3204273). The
// text is how prometheus_client prints the float, which the dispatcher parses.
var ttftBounds = [...]float64{0.001, 0.005, 0.01, 0.02, 0.04, 0.06, 0.08, 0.1, 0.25, 0.5, 0.75,
	1.0, 2.5, 5.0, 7.5, 10.0, 20.0, 40.0, 80.0, 160.0, 640.0, 2560.0}

// ttftLE are the bounds as the `le` label prints them.
var ttftLE = func() (s [len(ttftBounds)]string) {
	for i, b := range ttftBounds {
		s[i] = fedNum(b)
	}
	return
}()

// fedNum prints f the way prometheus_client does: always with a decimal
// point ("3.0"), never an exponent. The dispatcher's bucket pattern
// (histogram.py: le="([\d+.inf]+)".*? (\d+\.\d+)) does not match "3".
func fedNum(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// fedSeries is one model's histogram.
type fedSeries struct {
	finite [len(ttftBounds)]uint64 // observations per bucket, not cumulative
	count  uint64                  // every observation, refusals included: the +Inf bucket
	sum    float64                 // the finite observations only
}

// fedMount is one federation: its configuration and its histograms.
type fedMount struct {
	cfg config.Federation
	mu  sync.Mutex
	m   map[string]*fedSeries // by model name
}

func newFedMount(f config.Federation) *fedMount {
	return &fedMount{cfg: f, m: map[string]*fedSeries{}}
}

func (f *fedMount) series(model string) *fedSeries {
	s := f.m[model]
	if s == nil {
		s = &fedSeries{}
		f.m[model] = s
	}
	return s
}

// observe records the time to the first byte of an answered request.
func (f *fedMount) observe(model string, d time.Duration) {
	v := d.Seconds()
	f.mu.Lock()
	s := f.series(model)
	s.count++
	s.sum += v
	for i, b := range ttftBounds {
		if v <= b {
			s.finite[i]++
			break
		}
	}
	f.mu.Unlock()
}

// refused records a request refused for lack of room: in the +Inf bucket
// only, as an observation of infinite time (and not in the sum, which would
// become +Inf for good).
func (f *fedMount) refused(model string) {
	f.mu.Lock()
	f.series(model).count++
	f.mu.Unlock()
}

// render writes the Prometheus text exposition. Nothing is printed for a
// model with no observation, and the series are sorted by model: the
// dispatcher keeps, per bucket, the value of the last line it reads.
func (f *fedMount) render() []byte {
	f.mu.Lock()
	names := make([]string, 0, len(f.m))
	snap := make(map[string]fedSeries, len(f.m))
	for n, s := range f.m {
		names = append(names, n)
		snap[n] = *s
	}
	f.mu.Unlock()
	sort.Strings(names)
	var b bytes.Buffer
	b.WriteString("# HELP vllm:time_to_first_token_seconds Histogram of time to first byte of the answer, in seconds; a request refused for lack of room is observed as +Inf.\n")
	b.WriteString("# TYPE vllm:time_to_first_token_seconds histogram\n")
	for _, n := range names {
		s := snap[n]
		lbl := `model_name="` + promEscape(n) + `"`
		var cum uint64
		for i := range ttftBounds {
			cum += s.finite[i]
			b.WriteString("vllm:time_to_first_token_seconds_bucket{" + lbl + `,le="` + ttftLE[i] + `"} ` + fedNum(float64(cum)) + "\n")
		}
		b.WriteString("vllm:time_to_first_token_seconds_bucket{" + lbl + `,le="+Inf"} ` + fedNum(float64(s.count)) + "\n")
		b.WriteString("vllm:time_to_first_token_seconds_sum{" + lbl + "} " + fedNum(s.sum) + "\n")
		b.WriteString("vllm:time_to_first_token_seconds_count{" + lbl + "} " + fedNum(float64(s.count)) + "\n")
	}
	return b.Bytes()
}

// promEscape escapes a label value (exposition format: \, " and newline).
func promEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// fedRoute is a mount and the mux that answers under it.
type fedRoute struct {
	m   *fedMount
	mux *http.ServeMux
}

// match reports the part of p after the prefix ("/v1/models"), when p is
// under it. The prefix itself and "<prefix>x" are not under it.
func (fr fedRoute) match(p string) (string, bool) {
	pre := fr.m.cfg.Prefix
	if len(p) > len(pre) && p[len(pre)] == '/' && p[:len(pre)] == pre {
		return p[len(pre):], true
	}
	return "", false
}

// fedMux holds what is reachable under a prefix: the inference API of the
// root, and the metrics. Not the administration, the UI, SCIM, MCP, the
// gateway's own /metrics or /healthz: they are not registered here.
func (s *Server) fedMux(f *fedMount) *http.ServeMux {
	mux := http.NewServeMux()
	s.inferenceRoutes(mux)
	nets := f.cfg.MetricsNetworks
	if nets == nil {
		nets = []netip.Prefix{} // none: never "any address"
	}
	mux.Handle("GET /metrics", s.withClientAddr(nets, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(f.render())
	})))
	return mux
}

type fedKey struct{}

// fedCall is the federation state of one request.
type fedCall struct {
	m     *fedMount
	model string // set once the request's model is resolved (proxyFor)
}

func fedCallOf(r *http.Request) *fedCall {
	fc, _ := r.Context().Value(fedKey{}).(*fedCall)
	return fc
}

// serveFed answers a request under a mount's prefix from the mount's mux, the
// prefix removed. A path that is not in its clean form is refused here: the
// mux would redirect it, and "<prefix>/../admin/..." must lead nowhere.
func (s *Server) serveFed(w http.ResponseWriter, r *http.Request, fr fedRoute, rest string) {
	if path.Clean(rest) != rest {
		apiError(w, http.StatusNotFound, "not_found", "no such route")
		return
	}
	fc := &fedCall{m: fr.m}
	r2 := r.WithContext(context.WithValue(r.Context(), fedKey{}, fc))
	u := *r.URL
	u.Path, u.RawPath = rest, ""
	r2.URL = &u
	fr.mux.ServeHTTP(&fedWriter{ResponseWriter: w, fc: fc, t0: time.Now()}, r2)
}

// refusedStatus: a status that tells the dispatcher this node has no room (or
// cannot serve): 429 (pool_spare, a rate limit), 502, 503, 504.
//
// A variable so that a test can switch the recording off and show that its
// assertions notice.
var refusedStatus = func(code int) bool {
	return code == http.StatusTooManyRequests || code == http.StatusBadGateway ||
		code == http.StatusServiceUnavailable || code == http.StatusGatewayTimeout
}

// fedWriter observes the answer of a request that came through a mount: the
// time to its first body byte (the first event of a stream), or, for a
// refusal, an infinite time. Requests that resolved no model (the metrics,
// authentication failures, unknown models) observe nothing.
type fedWriter struct {
	http.ResponseWriter
	fc     *fedCall
	t0     time.Time
	status int
	done   bool
}

func (w *fedWriter) model() string {
	// A fallback moved the request: the model that served it.
	if m := w.Header().Get("X-Llmflow-Model"); m != "" {
		return m
	}
	return w.fc.model
}

func (w *fedWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
		if m := w.model(); m != "" && refusedStatus(code) {
			w.fc.m.refused(m)
			w.done = true
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *fedWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if !w.done && len(b) > 0 {
		w.done = true
		if m := w.model(); m != "" && w.status < 300 {
			w.fc.m.observe(m, time.Since(w.t0))
		}
	}
	return w.ResponseWriter.Write(b)
}

func (w *fedWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.NewResponseController reach the connection (deadlines).
func (w *fedWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
