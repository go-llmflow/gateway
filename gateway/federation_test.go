package gateway

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// ---------------------------------------------------------------------------
// The dispatcher's reading of the metrics, ported from
// CentraleSupelec/aristote-dispatcher main (v1.5.0):
//
//	src/consumer/strategy/metrics_tracker.py:13   time_to_first_token_pattern
//	src/consumer/strategy/metrics_tracker.py:41-51 update_histogram
//	src/consumer/strategy/histogram.py:10-43      Histogram (parse, __sub__)
//	src/consumer/strategy/least_busy.py:35-75     get_percentile, business_score
//
// Python's dict.update, float() and re semantics are kept: a value without a
// decimal point does not match, the last matching line is the +Inf bucket,
// the lines of several models are merged key by key.
// ---------------------------------------------------------------------------

var (
	aristoteLines  = regexp.MustCompile(`(?m)^vllm:time_to_first_token_seconds_bucket.*$`)
	aristoteBucket = regexp.MustCompile(`(?i)le="([\d+.inf]+)".*? (\d+\.\d+)`)
)

type aristoteHist map[float64]float64

func aristoteParse(t *testing.T, content string) aristoteHist {
	t.Helper()
	matches := aristoteLines.FindAllString(content, -1)
	h := aristoteHist{}
	if len(matches) == 0 {
		return h
	}
	put := func(line string, inf bool) {
		m := aristoteBucket.FindStringSubmatch(line)
		if m == nil {
			return
		}
		v, err := strconv.ParseFloat(m[2], 64)
		if err != nil {
			t.Fatalf("float(%q): %v", m[2], err)
		}
		if inf {
			h[math.Inf(1)] = v
			return
		}
		k, err := strconv.ParseFloat(m[1], 64)
		if err != nil {
			t.Fatalf("float(%q): %v", m[1], err) // Python raises ValueError
		}
		h[k] = v
	}
	for _, l := range matches[:len(matches)-1] {
		put(l, false)
	}
	put(matches[len(matches)-1], true)
	return h
}

func (h aristoteHist) sub(o aristoteHist) aristoteHist {
	out := aristoteHist{}
	for k, v := range h {
		out[k] = v - o[k]
	}
	for k := range o {
		if _, ok := h[k]; !ok {
			out[k] = -o[k]
		}
	}
	return out
}

// aristoteScore is business_score(get_percentile(h)): the upper bound of the
// bucket holding the 95th percentile, -1 for an empty histogram.
func aristoteScore(t *testing.T, h aristoteHist) float64 {
	t.Helper()
	if len(h) == 0 {
		return -1
	}
	keys := make([]float64, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Float64s(keys)
	total := h[keys[len(keys)-1]]
	pc := 0.95 * total
	for _, k := range keys {
		if h[k] >= pc {
			return k
		}
	}
	t.Fatal("PercentileComputationError")
	return 0
}

// aristoteTracker is MetricsTracker for one url: refresh_count_per_window
// slots of last histograms and the diff histogram, fed by tick.
type aristoteTracker struct {
	last [][]aristoteHist
	diff aristoteHist
	idx  int
}

func newAristoteTracker(window int) *aristoteTracker {
	l := make([]aristoteHist, window)
	for i := range l {
		l[i] = aristoteHist{}
	}
	return &aristoteTracker{last: [][]aristoteHist{l}, diff: aristoteHist{}}
}

// tick is update_all_metrics_for_server + the window index step.
func (a *aristoteTracker) tick(t *testing.T, content string) {
	t.Helper()
	if content == "" {
		return // `if not content: return`
	}
	nw := aristoteParse(t, content)
	slot := a.last[0][a.idx]
	nd := nw.sub(slot)
	for k, v := range nw { // last_histogram.update(new_histogram)
		slot[k] = v
	}
	for k, v := range nd { // diff_histogram.update(new_diff_histogram)
		a.diff[k] = v
	}
	a.idx = (a.idx + 1) % len(a.last[0])
}

func (a *aristoteTracker) score(t *testing.T) float64 { return aristoteScore(t, a.diff) }

// refresh_count_per_window of the dispatcher's default settings.
const aristoteWindow = 24

// ---------------------------------------------------------------------------

const fedHCL = `
master_key_file = "/unused"
cache_salt_key_file = "/unused"
budget_refresh  = "1h"
key_cache_ttl   = "1h"
health {
  interval = "1h"
  cooldown = "1h"
}
org "owner" {
  share = 1
  team "owner" {}
}
org "ilaas" {
  share = 0
  team "ilaas" {}
}
org "other" {
  share = 0
  team "other" {}
}
pool "b300" {
  capacity = 2
  borrow   = "spare"
}
model "llama" {
  pool     = "b300"
  key_file = "/unused"
  upstream "u" { base_url = %q }
}
federation "ilaas" {
  prefix           = "/fed/ilaas"
  org              = "ilaas"
  metrics_networks = %s
}
`

const fedPrefix = "/fed/ilaas"

func newFedEnv(t *testing.T, networks string) (*env, *fakeVLLM) {
	t.Helper()
	up := newFakeVLLM(t)
	cfg, err := config.Parse([]byte(fmt.Sprintf(fedHCL, up.URL(), networks)), "fed.hcl")
	if err != nil {
		t.Fatal(err)
	}
	return newEnv(t, cfg), up
}

const loopbackNets = `["127.0.0.0/8", "::1/128"]`

func fedChat(model, text string) string {
	return fmt.Sprintf(`{"model":%q,"max_tokens":5,"messages":[{"role":"user","content":%q}]}`, model, text)
}

func (e *env) fedMetrics(t *testing.T) (int, string) {
	t.Helper()
	r := e.do("GET", fedPrefix+"/metrics", "", "")
	return r.StatusCode, readAll(t, r)
}

// fedStages are the dispatcher's scores of one node history.
type fedStages struct {
	empty, admitted, refused, quiet float64
	emptyBody, refusedBody          string
}

// runFedScenario plays a history through the endpoint exactly as the
// dispatcher would read it: nothing served, three requests admitted (120 ms
// each), the pool full of the owners' requests and two refusals, then a
// quiet window.
func runFedScenario(t *testing.T) fedStages {
	t.Helper()
	e, up := newFedEnv(t, loopbackNets)
	_, ilaas := e.newKey("ilaas")
	_, owner := e.newKey("owner")
	tr := newAristoteTracker(aristoteWindow)
	scrape := func() string {
		code, body := e.fedMetrics(t)
		if code != 200 {
			t.Fatalf("metrics: %d %s", code, body)
		}
		tr.tick(t, body)
		return body
	}
	var st fedStages

	// 1. Never used.
	st.emptyBody = scrape()
	st.empty = tr.score(t)

	// 2. Admitted requests, a known latency.
	up.mu.Lock()
	up.delay = 120 * time.Millisecond
	up.mu.Unlock()
	for i := 0; i < 3; i++ {
		r := e.do("POST", fedPrefix+"/v1/chat/completions", ilaas, fedChat("llama", fmt.Sprint("ok", i)))
		if readAll(t, r); r.StatusCode != 200 {
			t.Fatalf("admitted request: %d", r.StatusCode)
		}
	}
	scrape()
	st.admitted = tr.score(t)

	// 3. The owners fill the pool; the peer is refused 429 pool_spare.
	up.mu.Lock()
	up.delay = 0
	up.mu.Unlock()
	up.gate = make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			readAll(t, e.do("POST", "/v1/chat/completions", owner, fedChat("llama", fmt.Sprint("owner", i))))
		}(i)
		waitUpstream(t, up, 3+i+1)
	}
	for i := 0; i < 2; i++ {
		r := e.do("POST", fedPrefix+"/v1/chat/completions", ilaas, fedChat("llama", fmt.Sprint("full", i)))
		if body := readAll(t, r); r.StatusCode != 429 || !strings.Contains(body, "spare capacity only") {
			t.Fatalf("refusal: %d %s", r.StatusCode, body)
		}
	}
	st.refusedBody = scrape()
	st.refused = tr.score(t)
	close(up.gate)
	wg.Wait()

	// 4. A quiet window: the counters do not move for refresh_count_per_window
	// refreshes, the diff of the window is zero everywhere.
	for i := 0; i < aristoteWindow+1; i++ {
		scrape()
	}
	st.quiet = tr.score(t)
	return st
}

func TestFederationLeastBusyScores(t *testing.T) {
	st := runFedScenario(t)
	if st.empty != -1 || strings.Contains(st.emptyBody, "_bucket{") {
		t.Errorf("before any traffic the score is %v (want -1, no bucket line):\n%s", st.empty, st.emptyBody)
	}
	// 120 ms lands in the le="0.25" bucket (0.5 under a very slow CI).
	if math.IsInf(st.admitted, 0) || st.admitted < 0.25 || st.admitted > 0.5 {
		t.Errorf("after admitted requests the p95 is %v, want finite in [0.25, 0.5]", st.admitted)
	}
	if !math.IsInf(st.refused, 1) {
		t.Errorf("after pool_spare refusals the p95 is %v, want +Inf:\n%s", st.refused, st.refusedBody)
	}
	// The dispatcher probes the node again once its window has no traffic: the
	// diff is zero in every bucket, so the score is the first bound (not -1:
	// the keys are present with a zero count).
	if st.quiet != 0.001 {
		t.Errorf("after a quiet window the score is %v, want 0.001", st.quiet)
	}
}

// Negative witness: with the refusals not recorded, the same assertions fail
// (the score after the refusals is the finite p95 of the admitted requests).
func TestFederationWitnessRefusalNotRecorded(t *testing.T) {
	saved := refusedStatus
	refusedStatus = func(int) bool { return false }
	defer func() { refusedStatus = saved }()
	st := runFedScenario(t)
	if math.IsInf(st.refused, 1) {
		t.Fatalf("the score is +Inf although no refusal was recorded: the +Inf assertion proves nothing")
	}
	if st.refused < 0.25 || st.refused > 0.5 {
		t.Fatalf("score without recorded refusals: %v", st.refused)
	}
}

// Negative witness for the parser port: a count printed without a decimal
// point, or a missing +Inf line, would make the dispatcher read nothing or the
// wrong bucket; the port notices both.
func TestFederationWitnessParserPort(t *testing.T) {
	ints := "vllm:time_to_first_token_seconds_bucket{model_name=\"m\",le=\"0.5\"} 3\nvllm:time_to_first_token_seconds_bucket{model_name=\"m\",le=\"+Inf\"} 3\n"
	if h := aristoteParse(t, ints); len(h) != 0 {
		t.Errorf("integer counts were read: %v", h)
	}
	ok := "vllm:time_to_first_token_seconds_bucket{model_name=\"m\",le=\"0.5\"} 3.0\nvllm:time_to_first_token_seconds_bucket{model_name=\"m\",le=\"+Inf\"} 5.0\n"
	h := aristoteParse(t, ok)
	if h[0.5] != 3 || h[math.Inf(1)] != 5 || !math.IsInf(aristoteScore(t, h), 1) {
		t.Errorf("port: %v", h)
	}
}

// The bounds are vLLM's TIME_TO_FIRST_TOKEN_BUCKETS (vllm/v1/metrics/
// buckets.py, vllm-project/vllm main 3204273), printed as prometheus_client
// does, and every count has a decimal point.
func TestFederationExpositionShape(t *testing.T) {
	e, _ := newFedEnv(t, loopbackNets)
	_, ilaas := e.newKey("ilaas")
	r := e.do("POST", fedPrefix+"/v1/chat/completions", ilaas, fedChat("llama", "x"))
	if readAll(t, r); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	code, body := e.fedMetrics(t)
	if code != 200 {
		t.Fatal(code)
	}
	want := []string{"0.001", "0.005", "0.01", "0.02", "0.04", "0.06", "0.08", "0.1", "0.25", "0.5", "0.75",
		"1.0", "2.5", "5.0", "7.5", "10.0", "20.0", "40.0", "80.0", "160.0", "640.0", "2560.0", "+Inf"}
	var got []string
	re := regexp.MustCompile(`^vllm:time_to_first_token_seconds_bucket\{model_name="llama",le="([^"]+)"\} (\d+\.\d+)$`)
	prev := -1.0
	for _, l := range strings.Split(body, "\n") {
		if !strings.HasPrefix(l, "vllm:time_to_first_token_seconds_bucket") {
			continue
		}
		m := re.FindStringSubmatch(l)
		if m == nil {
			t.Fatalf("bucket line %q", l)
		}
		v, _ := strconv.ParseFloat(m[2], 64)
		if v < prev {
			t.Errorf("buckets are not cumulative at %s", l)
		}
		prev = v
		got = append(got, m[1])
	}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("le = %v, want %v", got, want)
	}
	for _, l := range strings.Split(strings.TrimSpace(body), "\n") {
		if !strings.HasPrefix(l, "# HELP vllm:time_to_first_token_seconds") && !strings.HasPrefix(l, "# TYPE vllm:time_to_first_token_seconds") &&
			!strings.HasPrefix(l, "vllm:time_to_first_token_seconds_") {
			t.Errorf("a line that is not the TTFT histogram: %q", l)
		}
	}
	if !strings.Contains(body, `vllm:time_to_first_token_seconds_count{model_name="llama"} 1.0`) {
		t.Errorf("count:\n%s", body)
	}
}

// Only the peer's traffic is observed; nothing of the owners, no organisation.
func TestFederationMetricsCarryNoOwnerTraffic(t *testing.T) {
	e, _ := newFedEnv(t, loopbackNets)
	_, owner := e.newKey("owner")
	_, ilaas := e.newKey("ilaas")
	for i := 0; i < 4; i++ {
		r := e.do("POST", "/v1/chat/completions", owner, fedChat("llama", fmt.Sprint("o", i)))
		if readAll(t, r); r.StatusCode != 200 {
			t.Fatal(r.StatusCode)
		}
	}
	if _, body := e.fedMetrics(t); strings.Contains(body, "_bucket{") {
		t.Fatalf("the owners' requests were observed:\n%s", body)
	}
	r := e.do("POST", fedPrefix+"/v1/chat/completions", ilaas, fedChat("llama", "p"))
	if readAll(t, r); r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	_, body := e.fedMetrics(t)
	if !strings.Contains(body, `_count{model_name="llama"} 1.0`) {
		t.Errorf("count is not the peer's single request:\n%s", body)
	}
	for _, bad := range []string{"owner", "ilaas", "other", "org", "team", "llmflow_", "key"} {
		if strings.Contains(body, bad) {
			t.Errorf("the exposition mentions %q:\n%s", bad, body)
		}
	}
}

// The key must belong to the organisation: 403 before anything is held or
// sent upstream; the root API is unchanged for every key.
func TestFederationOrgGate(t *testing.T) {
	e, up := newFedEnv(t, loopbackNets)
	_, ilaas := e.newKey("ilaas")
	_, owner := e.newKey("owner")
	_, other := e.newKey("other")
	for name, key := range map[string]string{"owner": owner, "other": other} {
		r := e.do("POST", fedPrefix+"/v1/chat/completions", key, fedChat("llama", "x"))
		if body := readAll(t, r); r.StatusCode != 403 || !strings.Contains(body, "organisation served under this prefix") {
			t.Errorf("key of %s under the prefix: %d %s", name, r.StatusCode, body)
		}
		if r := e.do("GET", fedPrefix+"/v1/models", key, ""); r.StatusCode != 403 {
			t.Errorf("models of %s under the prefix: %d", name, r.StatusCode)
		}
		// The same key is valid at the root.
		if r := e.do("POST", "/v1/chat/completions", key, fedChat("llama", "y")); r.StatusCode != 200 && r.StatusCode != 429 {
			t.Errorf("key of %s at the root: %d", name, r.StatusCode)
		}
	}
	if up.count() > 3 { // the root requests of owner and other only (<= 2) plus none under the prefix
		t.Errorf("a refused request reached the upstream: %d calls", up.count())
	}
	if r := e.do("POST", fedPrefix+"/v1/chat/completions", "", fedChat("llama", "x")); r.StatusCode != 401 {
		t.Errorf("no key: %d", r.StatusCode)
	}
	r := e.do("POST", fedPrefix+"/v1/chat/completions", ilaas, fedChat("llama", "x"))
	if readAll(t, r); r.StatusCode != 200 {
		t.Errorf("the peer's key under the prefix: %d", r.StatusCode)
	}
	if r := e.do("GET", fedPrefix+"/v1/models", ilaas, ""); r.StatusCode != 200 {
		t.Errorf("models under the prefix: %d", r.StatusCode)
	}
	// A refused key is not an observation.
	if _, body := e.fedMetrics(t); !strings.Contains(body, `_count{model_name="llama"} 1.0`) {
		t.Errorf("403s were observed:\n%s", body)
	}
}

// Nothing but the inference API and the metrics is reachable under the prefix.
func TestFederationPrefixReachesNothingElse(t *testing.T) {
	e, up := newFedEnv(t, loopbackNets)
	_, ilaas := e.newKey("ilaas")
	paths := []string{
		"/admin/keys", "/admin/orgs", "/admin/pools", "/ui/", "/ui/login", "/healthz", "/scim/v2/Users",
		"/mcp", "/sso/login", "/auth/callback",
		"/v1/../admin/keys", "/../admin/keys", "/./admin/keys", "//admin/keys", "/v1//models",
		"/%2e%2e/admin/keys", "/v1/%2e%2e/admin/keys", "/%2fadmin/keys", "/v1/models/", "",
	}
	for _, p := range paths {
		for _, tok := range []string{master, ilaas, ""} {
			for _, m := range []string{"GET", "POST", "DELETE"} {
				req, err := http.NewRequest(m, e.gw.URL+fedPrefix+p, strings.NewReader("{}"))
				if err != nil {
					t.Fatal(err)
				}
				if tok != "" {
					req.Header.Set("Authorization", "Bearer "+tok)
				}
				resp, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body := readAll(t, resp)
				if resp.StatusCode != 404 && resp.StatusCode != 405 && !(p == "" && resp.StatusCode == 404) {
					t.Errorf("%s %s%s with %.6s: %d %.80s", m, fedPrefix, p, tok, resp.StatusCode, body)
				}
			}
		}
	}
	if up.count() != 0 {
		t.Errorf("a path under the prefix reached the upstream: %d", up.count())
	}
	// The prefix itself, and a sibling that merely starts with it.
	for _, p := range []string{fedPrefix, fedPrefix + "x/v1/models", "/fed"} {
		if r := e.do("GET", p, ilaas, ""); r.StatusCode != 404 {
			t.Errorf("%s: %d", p, r.StatusCode)
		}
	}
	// Root administration works as before.
	if r := e.do("GET", "/admin/keys", master, ""); r.StatusCode != 200 {
		t.Errorf("root admin: %d", r.StatusCode)
	}
}

// The metrics are for the listed networks only; the gateway's own /metrics is
// unchanged and does not carry the histogram.
func TestFederationMetricsNetworks(t *testing.T) {
	e, _ := newFedEnv(t, `["10.0.0.0/8"]`)
	if code, body := e.fedMetrics(t); code != 403 || strings.Contains(body, "vllm:") {
		t.Errorf("a network that is not listed: %d %s", code, body)
	}
	// Forwarded-for is not trusted without trusted_proxies.
	if r := e.do("GET", fedPrefix+"/metrics", "", "", "X-Forwarded-For", "10.1.2.3"); r.StatusCode != 403 {
		t.Errorf("X-Forwarded-For spoofing: %d", r.StatusCode)
	}
	root := e.do("GET", "/metrics", "", "")
	body := readAll(t, root)
	if root.StatusCode != 200 || !strings.Contains(body, "llmflow_") || strings.Contains(body, "vllm:time_to_first_token") {
		t.Errorf("root /metrics: %d (federation series in it, or lost): %.200s", root.StatusCode, body)
	}
	e2, _ := newFedEnv(t, loopbackNets)
	if code, _ := e2.fedMetrics(t); code != 200 {
		t.Errorf("listed network: %d", code)
	}
}

// No federation block: the prefix is not a route, the root is as before.
func TestFederationAbsent(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	if r := e.do("GET", fedPrefix+"/metrics", "", ""); r.StatusCode != 404 {
		t.Errorf("no block: %d", r.StatusCode)
	}
}

// FuzzFederationRouter: whatever the path, a request under the prefix is
// either refused (not clean) or lands on a pattern of the inference API or
// the metrics, never on the administration, the UI, SCIM, MCP, healthz or the
// gateway's own /metrics.
func FuzzFederationRouter(f *testing.F) {
	up := newFakeVLLM(f)
	cfg, err := config.Parse([]byte(fmt.Sprintf(fedHCL, up.URL(), loopbackNets)), "fed.hcl")
	if err != nil {
		f.Fatal(err)
	}
	srv, err := New(cfg, store.NewMemory(), Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		f.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { srv.Close(context.Background()) })
	fr := fedRoute{srv.feds[0], srv.fedMux(srv.feds[0])}
	allowed := regexp.MustCompile(`^(POST /(v1|v2)/[a-z_/]+|POST /(de)?tokenize|GET /v1/(models|me/usage)|GET /metrics)$`)
	for _, s := range []string{"/v1/chat/completions", "/admin/keys", "/../admin", "/v1/../admin", "//x", "/%2e%2e", "/metrics", "/ui/", "/healthz", ""} {
		f.Add(s, "GET")
		f.Add(s, "POST")
	}
	f.Fuzz(func(t *testing.T, tail, method string) {
		p := fedPrefix + tail
		rest, ok := fr.match(p)
		if !ok {
			return
		}
		if method == "" || strings.ContainsAny(method, " \r\n\x00") || !httpToken(method) {
			return
		}
		req := httptest.NewRequest("GET", "/", nil)
		req.Method = method
		req.URL.Path = rest
		if clean := strings.HasPrefix(rest, "/") && pathClean(rest); !clean {
			return // serveFed answers 404 before the mux
		}
		_, pattern := fr.mux.Handler(req)
		if pattern != "" && !allowed.MatchString(pattern) {
			t.Fatalf("%s %q reaches %q", method, rest, pattern)
		}
	})
}

func httpToken(s string) bool {
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func pathClean(p string) bool { return p == path.Clean(p) }

// BenchmarkFederationPath interleaves one request at the root and one under
// the federation prefix (same key organisation, same upstream) and reports the
// mean of each: the cost of the prefix mount (path match, context, writer
// wrapper, histogram observation). Run on one core:
//
//	go test -run '^$' -bench FederationPath -cpu 1 -benchtime 20000x ./gateway
func BenchmarkFederationPath(b *testing.B) {
	up := instantUpstream()
	defer up.Close()
	cfg, err := config.Parse([]byte(fmt.Sprintf(fedHCL, up.URL, loopbackNets)), "fed.hcl")
	if err != nil {
		b.Fatal(err)
	}
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		b.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer srv.Close(context.Background())
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()
	secret, id, h := store.GenerateKey()
	if err := st.CreateKey(context.Background(), store.Key{ID: id, Hash: h, Team: "ilaas"}); err != nil {
		b.Fatal(err)
	}
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
	body := `{"model":"llama","messages":[{"role":"user","content":"Say hi."}],"max_tokens":8}`
	for i := 0; i < 200; i++ {
		post(b, client, gw.URL+"/v1/chat/completions", secret, body)
		post(b, client, gw.URL+fedPrefix+"/v1/chat/completions", secret, body)
	}
	var root, fed time.Duration
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i%2 == 0 {
			root += post(b, client, gw.URL+"/v1/chat/completions", secret, body)
			fed += post(b, client, gw.URL+fedPrefix+"/v1/chat/completions", secret, body)
		} else {
			fed += post(b, client, gw.URL+fedPrefix+"/v1/chat/completions", secret, body)
			root += post(b, client, gw.URL+"/v1/chat/completions", secret, body)
		}
	}
	b.ReportMetric(float64(root.Microseconds())/float64(b.N), "root_us/req")
	b.ReportMetric(float64(fed.Microseconds())/float64(b.N), "fed_us/req")
}
