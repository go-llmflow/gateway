package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// fakeAnthropic answers like the Anthropic Messages API and records what
// it receives. Its usage carries every cache field
// (platform.claude.com/docs/en/build-with-claude/prompt-caching).
type fakeAnthropic struct {
	srv  *httptest.Server
	mu   sync.Mutex
	reqs []recorded
	// status/retryAfter: answer with this error instead; workspace maps an
	// API key to the anthropic-workspace-id answered (key itself when unset).
	status     int
	retryAfter string
	workspace  map[string]string
}

const (
	keyAlpha = "sk-ant-alpha-test-key"
	keyBeta  = "sk-ant-beta-test-key"
	// usage of every answer: 10 uncached, 100 read, 30 + 20 written, 5 out.
	fakeAnthUsage = `{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":50,` +
		`"cache_creation":{"ephemeral_5m_input_tokens":30,"ephemeral_1h_input_tokens":20},"output_tokens":5}`
)

func newFakeAnthropic(t testing.TB) *fakeAnthropic {
	f := &fakeAnthropic{workspace: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAnthropic) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.reqs = append(f.reqs, recorded{Header: r.Header.Clone(), Body: body, Path: r.URL.Path})
	status, ra := f.status, f.retryAfter
	ws := f.workspace[r.Header.Get("X-Api-Key")]
	f.mu.Unlock()
	if ws == "" {
		ws = "wrkspc_" + r.Header.Get("X-Api-Key")
	}
	w.Header().Set("Anthropic-Workspace-Id", ws)
	w.Header().Set("Request-Id", "req_test")
	w.Header().Set("Anthropic-Ratelimit-Tokens-Remaining", "12345")
	if status != 0 {
		if ra != "" {
			w.Header().Set("Retry-After", ra)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"},"request_id":"req_test"}`)
		return
	}
	if stream, _ := body["stream"].(bool); stream {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"model\":\"m\",\"usage\":%s}}\n\n", strings.Replace(fakeAnthUsage, `"output_tokens":5`, `"output_tokens":1`, 1))
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprintf(w, `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":%s}`, fakeAnthUsage)
}

func (f *fakeAnthropic) set(status int, retryAfter string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.retryAfter = status, retryAfter
}

func (f *fakeAnthropic) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeAnthropic) last(t *testing.T) recorded {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("the provider received nothing")
	}
	return f.reqs[len(f.reqs)-1]
}

// Test prices, USD per MTok: input 2, output 10, 5-minute writes 2.5,
// 1-hour writes 4, reads 0.1. One answer costs
// (10×2 + 100×0.1 + 30×2.5 + 20×4 + 5×10) / 1e6 = 235e-6 USD.
const fakeAnthCost = 235e-6

func anthropicHCL(upstream, vllm, extra string) string {
	return `
master_key_file = "/unused"
cache_salt_key_file = "/unused"
budget_refresh  = "1h"
key_cache_ttl   = "1h"
health {
  interval = "1h"
  cooldown = "1h"
}
org "etabA" {
  share = 0.5
  team "etabA" {}
}
org "etabB" {
  share = 0.25
  team "etabB" {}
}
org "etabC" {
  share = 0.25
  team "etabC" {}
}
pool "b300" { capacity = 4 }
pool "anthropic" { capacity = 4 }
model "llama" {
  pool     = "b300"
  key_file = "/unused"
  upstream "u" { base_url = "` + vllm + `" }
}
model "claude" {
  provider       = "anthropic"
  upstream_model = "claude-sonnet-5-5"
  pool           = "anthropic"
  allow_external = true
  orgs           = ["etabA", "etabB"]
  anthropic_betas = ["context-management-2025-06-27"]
  cost {
    unit                    = "USD"
    price_date              = "2026-10-26"
    input_per_mtok          = 2
    output_per_mtok         = 10
    cached_per_mtok         = 0.1
    cache_write_5m_per_mtok = 2.5
    cache_write_1h_per_mtok = 4
  }
  upstream "api" { base_url = "` + upstream + `" }
  org_key "etabA" { key_file = "/keys/alpha" }
  org_key "etabB" { key_file = "/keys/beta" }
` + extra + `
}
guardrail "pii" {
  type = "pii"
  orgs = ["etabA", "etabB"]
}
guardrail "secrets" {
  type = "hide_secrets"
  orgs = ["etabA", "etabB"]
}
`
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func newAnthropicEnv(t *testing.T, extra string) (*env, *fakeAnthropic, *fakeVLLM, *clock) {
	t.Helper()
	up := newFakeAnthropic(t)
	vllm := newFakeVLLM(t)
	cfg, err := config.Parse([]byte(anthropicHCL(up.srv.URL, vllm.URL(), extra)), "anth.hcl")
	if err != nil {
		t.Fatal(err)
	}
	keys := testKeys(cfg)
	keys["/keys/alpha"], keys["/keys/beta"] = keyAlpha, keyBeta
	clk := &clock{t: time.Now()}
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: keys, CacheSaltKey: testSaltKey}, WithClock(clk.now))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	e := &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	t.Cleanup(func() { gw.Close(); srv.Close(context.Background()) })
	return e, up, vllm, clk
}

func anthMsg(stream bool) string {
	return fmt.Sprintf(`{"model":"claude","max_tokens":16,"stream":%v,"priority":-5,"cache_salt":"mine","vllm_xargs":{"x":1},`+
		`"messages":[{"role":"user","content":"mail jean.dupont@example.fr token %s"}]}`, stream, ghToken)
}

// What reaches the provider: the organisation's key, the allow-listed
// headers, a body without engine fields, with PII and secrets masked.
func TestAnthropicEgress(t *testing.T) {
	e, up, _, _ := newAnthropicEnv(t, "")
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	_, keyC := e.newKey("etabC")
	resp := e.do("POST", "/v1/messages", keyA, anthMsg(false),
		"X-Api-Key", "client-own-anthropic-key", "Anthropic-Version", "2023-06-01",
		"Anthropic-Beta", "context-management-2025-06-27, oauth-2025-04-20,unknown-beta-2026-01-01",
		"X-Vllm-Priority", "-100", "Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	body := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `"hello"`) {
		t.Fatalf("messages: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Request-Id") != "req_test" || resp.Header.Get("Anthropic-Ratelimit-Tokens-Remaining") != "" {
		t.Errorf("response headers: request-id %q, rate-limit header %q (the shared organisation's usage must not leak)",
			resp.Header.Get("Request-Id"), resp.Header.Get("Anthropic-Ratelimit-Tokens-Remaining"))
	}
	got := up.last(t)
	h := got.Header
	if h.Get("X-Api-Key") != keyAlpha {
		t.Errorf("x-api-key %q, want etabA's key", h.Get("X-Api-Key"))
	}
	for _, k := range []string{"Authorization", "X-Vllm-Priority", "X-Request-Id", "Traceparent"} {
		if v := h.Get(k); v != "" {
			t.Errorf("%s reached the provider: %q", k, v)
		}
	}
	if h.Get("Anthropic-Version") != "2023-06-01" || h.Get("Anthropic-Beta") != "context-management-2025-06-27" {
		t.Errorf("anthropic headers: version %q beta %q", h.Get("Anthropic-Version"), h.Get("Anthropic-Beta"))
	}
	if got.Path != "/v1/messages" || got.Body["model"] != "claude-sonnet-5-5" {
		t.Errorf("path %q model %v", got.Path, got.Body["model"])
	}
	for _, f := range []string{"priority", "cache_salt", "vllm_xargs"} {
		if _, ok := got.Body[f]; ok {
			t.Errorf("field %s reached the provider", f)
		}
	}
	text, _ := json.Marshal(got.Body["messages"])
	if strings.Contains(string(text), "jean.dupont@example.fr") || strings.Contains(string(text), ghToken) {
		t.Errorf("PII or secret left the site: %s", text)
	}

	// Another organisation, authenticated the Anthropic way (x-api-key, as
	// Claude Code does): its own key upstream. An unknown version falls
	// back to the first allowed one.
	if resp := e.do("POST", "/v1/messages", "", anthMsg(false), "X-Api-Key", keyB, "Anthropic-Version", "1999-01-01"); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("x-api-key credential: %d", resp.StatusCode)
	}
	if got := up.last(t).Header; got.Get("X-Api-Key") != keyBeta || got.Get("Anthropic-Version") != "2023-06-01" || got.Get("Anthropic-Beta") != "" {
		t.Errorf("etabB: key %q version %q beta %q", got.Get("X-Api-Key"), got.Get("Anthropic-Version"), got.Get("Anthropic-Beta"))
	}
	// An organisation not allowed: the model does not exist for it.
	n := up.count()
	if resp := e.do("POST", "/v1/messages", keyC, anthMsg(false)); resp.StatusCode != 404 {
		t.Errorf("etabC: %d, want 404", resp.StatusCode)
	}
	if list := readAll(t, e.do("GET", "/v1/models", keyC, "")); strings.Contains(list, "claude") {
		t.Errorf("etabC sees the model: %s", list)
	}
	if list := readAll(t, e.do("GET", "/v1/models", keyA, "")); !strings.Contains(list, `"claude"`) {
		t.Errorf("etabA does not see the model: %s", list)
	}
	// Only /v1/messages: no OpenAI-compatibility route.
	resp = e.do("POST", "/v1/chat/completions", keyA, `{"model":"claude","messages":[{"role":"user","content":"hi"}]}`)
	if body := readAll(t, resp); resp.StatusCode != 400 || !strings.Contains(body, "/v1/messages only") {
		t.Errorf("chat completions on an Anthropic model: %d %s", resp.StatusCode, body)
	}
	if up.count() != n {
		t.Error("a refused request reached the provider")
	}
}

// Accounting: the cache reads and both kinds of writes priced apart, in
// USD, never in GPU-hours; a stream is charged from its events.
func TestAnthropicAccounting(t *testing.T) {
	e, _, _, _ := newAnthropicEnv(t, "")
	_, keyA := e.newKey("etabA")
	for _, stream := range []bool{false, true} {
		resp := e.do("POST", "/v1/messages", keyA, anthMsg(stream))
		if body := readAll(t, resp); resp.StatusCode != 200 || !strings.Contains(body, "hello") {
			t.Fatalf("stream=%v: %d %s", stream, resp.StatusCode, body)
		}
	}
	e.srv.Close(context.Background())
	recs := e.st.Records()
	if len(recs) != 2 {
		t.Fatalf("%d records", len(recs))
	}
	for _, r := range recs {
		if r.Unit != "USD" || math.Abs(r.Cost-fakeAnthCost) > 1e-12 || r.PromptTokens != 160 || r.CachedTokens != 100 || r.CompletionTokens != 5 || r.Estimated {
			t.Errorf("record %+v, want USD %v, 160 prompt (100 read), 5 out", r, fakeAnthCost)
		}
	}
	metrics := readAll(t, e.do("GET", "/metrics", "", ""))
	if !strings.Contains(metrics, `llmflow_cost_total{team="etabA",pool="anthropic",unit="USD"}`) || strings.Contains(metrics, `llmflow_gpu_hours_total{team="etabA",pool="anthropic"}`) {
		t.Errorf("cost metrics:\n%s", metrics)
	}
}

// A USD budget stops the Anthropic model; a GPU-hours budget does not.
func TestAnthropicBudgetInUSD(t *testing.T) {
	e, up, _, _ := newAnthropicEnv(t, "")
	_, keyA := e.newKey("etabA")
	e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":0.000001,"unit":"gpu_hours"}`)
	for i := 0; i < 2; i++ {
		if resp := e.do("POST", "/v1/messages", keyA, anthMsg(false)); readAll(t, resp) == "" || resp.StatusCode != 200 {
			t.Fatalf("a GPU-hours budget refused a USD request: %d", resp.StatusCode)
		}
	}
	e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":0.0003,"unit":"USD"}`)
	n := up.count()
	resp := e.do("POST", "/v1/messages", keyA, anthMsg(false))
	if readAll(t, resp); resp.StatusCode != 429 || up.count() != n {
		t.Fatalf("over the USD budget (2 × %v > 0.0003): %d, provider calls %d → %d", fakeAnthCost, resp.StatusCode, n, up.count())
	}
}

// 429/529 with retry-after: the organisation waits, the others do not.
func TestAnthropicRetryAfter(t *testing.T) {
	e, up, _, clk := newAnthropicEnv(t, "")
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	up.set(429, "30")
	resp := e.do("POST", "/v1/messages", keyA, anthMsg(false))
	readAll(t, resp)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "30" {
		t.Fatalf("provider 429: %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
	up.set(0, "")
	n := up.count()
	clk.add(10 * time.Second)
	resp = e.do("POST", "/v1/messages", keyA, anthMsg(false))
	readAll(t, resp)
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "20" || up.count() != n {
		t.Fatalf("inside the wait: %d retry-after %q, provider calls %d → %d", resp.StatusCode, resp.Header.Get("Retry-After"), n, up.count())
	}
	if resp := e.do("POST", "/v1/messages", keyB, anthMsg(false)); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("another organisation waited too: %d", resp.StatusCode)
	}
	clk.add(21 * time.Second)
	if resp := e.do("POST", "/v1/messages", keyA, anthMsg(false)); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("after the wait: %d", resp.StatusCode)
	}
	// 529 without retry-after: the default back-off.
	up.set(statusOverloaded, "")
	resp = e.do("POST", "/v1/messages", keyB, anthMsg(false))
	if readAll(t, resp); resp.StatusCode != statusOverloaded {
		t.Fatalf("529 passed through: %d", resp.StatusCode)
	}
	up.set(0, "")
	resp = e.do("POST", "/v1/messages", keyB, anthMsg(false))
	if readAll(t, resp); resp.StatusCode != 429 || resp.Header.Get("Retry-After") != "5" {
		t.Fatalf("after a 529: %d retry-after %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

// With a local fallback, a 429/529 moves the request to vLLM; the local
// model gets its cache_salt and is charged in GPU-hours.
func TestAnthropicFallbackToLocal(t *testing.T) {
	e, up, vllm, _ := newAnthropicEnv(t, `  fallbacks   = ["llama"]
  fallback_on = ["unavailable"]`)
	_, keyA := e.newKey("etabA")
	up.set(statusOverloaded, "2")
	resp := e.do("POST", "/v1/messages", keyA, anthMsg(false))
	body := readAll(t, resp)
	if resp.StatusCode != 200 || resp.Header.Get("X-Llmflow-Model") != "llama" {
		t.Fatalf("fallback: %d model %q %s", resp.StatusCode, resp.Header.Get("X-Llmflow-Model"), body)
	}
	got := vllm.last()
	if got.Path != "/v1/messages" || got.Body["cache_salt"] == nil || got.Header.Get("X-Api-Key") != "" {
		t.Errorf("at vLLM: path %q cache_salt %v x-api-key %q", got.Path, got.Body["cache_salt"], got.Header.Get("X-Api-Key"))
	}
	// During the wait the provider is not asked at all.
	up.set(0, "")
	n := up.count()
	if resp := e.do("POST", "/v1/messages", keyA, anthMsg(false)); readAll(t, resp) == "" || resp.StatusCode != 200 || up.count() != n {
		t.Fatalf("during the back-off: %d, provider calls %d → %d", resp.StatusCode, n, up.count())
	}
	e.srv.Close(context.Background())
	for _, r := range e.st.Records() {
		if r.Model == "llama" && r.Unit != "" {
			t.Errorf("local model charged in %q", r.Unit)
		}
	}
}

// Two organisations answered from one workspace share a prompt cache:
// both are refused from then on.
func TestAnthropicSharedWorkspace(t *testing.T) {
	e, up, _, _ := newAnthropicEnv(t, "")
	up.workspace[keyAlpha], up.workspace[keyBeta] = "wrkspc_same", "wrkspc_same"
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	if resp := e.do("POST", "/v1/messages", keyA, anthMsg(false)); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	readAll(t, e.do("POST", "/v1/messages", keyB, anthMsg(false)))
	n := up.count()
	for _, k := range []string{keyA, keyB} {
		resp := e.do("POST", "/v1/messages", k, anthMsg(false))
		if body := readAll(t, resp); resp.StatusCode != 503 || !strings.Contains(body, "workspace is shared") {
			t.Errorf("after a shared workspace: %d %s", resp.StatusCode, body)
		}
	}
	if up.count() != n {
		t.Error("a refused organisation still reached the provider")
	}
}

func TestAnthropicConfig(t *testing.T) {
	ok := anthropicHCL("https://api.anthropic.com", "http://127.0.0.1:1", "")
	if _, err := config.Parse([]byte(ok), "a.hcl"); err != nil {
		t.Fatalf("valid configuration refused: %v", err)
	}
	for name, tc := range map[string]struct{ from, to, want string }{
		"no allow_external":     {"allow_external = true", "allow_external = false", "allow_external = true"},
		"no allow-list":         {`orgs           = ["etabA", "etabB"]`, "", "No allow-list"},
		"plain http":            {"https://api.anthropic.com", "http://api.anthropic.com", "over https"},
		"shared key":            {`org_key "etabB" { key_file = "/keys/beta" }`, "", "Shared provider workspace"},
		"same key file":         {`"/keys/beta"`, `"/keys/alpha"`, "same key_file"},
		"oauth beta":            {`["context-management-2025-06-27"]`, `["oauth-2025-04-20"]`, "OAuth beta refused"},
		"no unit":               {`unit                    = "USD"`, "", "Missing cost unit"},
		"no guardrail":          {`type = "pii"`, `type = "hide_secrets"`, "needs a pre-stage pii guardrail"},
		"half the cache":        {"cache_write_1h_per_mtok = 4", "", "Incomplete cache prices"},
		"unknown provider":      {`provider       = "anthropic"`, `provider       = "openai"`, "Unknown provider"},
		"external in vLLM pool": {`pool           = "anthropic"`, `pool           = "b300"`, "Mixed pool"},
		"settings on vLLM":      {"model \"llama\" {", "model \"llama\" {\n  orgs = [\"etabA\"]", "Unexpected external settings"},
		"external fallback":     {"model \"llama\" {", "model \"llama\" {\n  fallbacks = [\"claude\"]", "External fallback"},
	} {
		src := strings.Replace(ok, tc.from, tc.to, 1)
		if src == ok {
			t.Fatalf("%s: the replacement did not apply", name)
		}
		if _, err := config.Parse([]byte(src), "a.hcl"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	// One organisation, or cache_isolation none: one key is enough.
	one := strings.Replace(strings.Replace(ok, `org_key "etabB" { key_file = "/keys/beta" }`, "", 1), `orgs           = ["etabA", "etabB"]`, `orgs = ["etabA"]`, 1)
	if _, err := config.Parse([]byte(one), "a.hcl"); err != nil {
		t.Errorf("one organisation with its key: %v", err)
	}
	none := strings.Replace(strings.Replace(ok, `org_key "etabB" { key_file = "/keys/beta" }`, "", 1), `budget_refresh  = "1h"`, "budget_refresh = \"1h\"\ncache_isolation = \"none\"", 1)
	none = strings.Replace(none, `upstream "api" { base_url = "https://api.anthropic.com" }`, `key_file = "/keys/default"`+"\n"+`  upstream "api" { base_url = "https://api.anthropic.com" }`, 1)
	if c, err := config.Parse([]byte(none), "a.hcl"); err != nil {
		t.Errorf("cache_isolation none with a shared default key: %v", err)
	} else if kf := strings.Join(c.KeyFiles(), ","); !strings.Contains(kf, "/keys/alpha") || !strings.Contains(kf, "/keys/default") {
		t.Errorf("key files %s", kf)
	}
	off := strings.Replace(ok, `type = "pii"`, `type = "hide_secrets"`, 1)
	off = strings.Replace(off, `budget_refresh  = "1h"`, "budget_refresh = \"1h\"\nexternal_requires_guardrails = false", 1)
	if _, err := config.Parse([]byte(off), "a.hcl"); err != nil {
		t.Errorf("external_requires_guardrails = false: %v", err)
	}
}

// FuzzAnthropicHeaders: whatever a client sends as anthropic-version and
// anthropic-beta, the provider gets an allowed version, only allow-listed
// betas, never an OAuth one, and no header value a client chose freely.
func FuzzAnthropicHeaders(f *testing.F) {
	f.Add("2023-06-01", "context-management-2025-06-27,oauth-2025-04-20")
	f.Add("x\r\nX-Api-Key: stolen", "a\r\nb")
	f.Add("", " ,,OAuth-2025-04-20 , context-management-2025-06-27")
	e := &extRuntime{cfg: &config.External{Versions: []string{"2023-06-01", "2026-10-21"},
		Betas: []string{"*"}}, keys: map[string]string{"o": "k"}}
	f.Fuzz(func(t *testing.T, version, betas string) {
		r := httptest.NewRequest("POST", "/v1/messages", nil)
		r.Header["Anthropic-Version"] = []string{version}
		r.Header["Anthropic-Beta"] = []string{betas}
		h := http.Header{}
		e.headers(h, clientEgress(r, "o"), "application/json", "")
		if v := h.Get("Anthropic-Version"); v != "2023-06-01" && v != "2026-10-21" {
			t.Fatalf("version %q", v)
		}
		if h.Get("X-Api-Key") != "k" {
			t.Fatalf("key %q", h.Get("X-Api-Key"))
		}
		for _, b := range strings.Split(h.Get("Anthropic-Beta"), ",") {
			if b == "" && h.Get("Anthropic-Beta") == "" {
				continue
			}
			if !e.cfg.AllowsBeta(b) || strings.HasPrefix(strings.ToLower(b), "oauth") || strings.ContainsAny(b, "\r\n ") {
				t.Fatalf("beta %q forwarded", b)
			}
		}
	})
}
