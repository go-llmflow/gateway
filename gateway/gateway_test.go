package gateway

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

const (
	master      = "master-secret-for-tests"
	internalKey = "internal-vllm-key"
)

type env struct {
	t   *testing.T
	srv *Server
	gw  *httptest.Server
	st  *store.Memory
	cfg *config.Config
}

func testConfig(upstreams ...string) *config.Config {
	var ups strings.Builder
	for i, u := range upstreams {
		fmt.Fprintf(&ups, "  upstream \"u%d\" { base_url = %q }\n", i, u)
	}
	c, err := config.Parse([]byte(`
master_key_file = "/unused"
budget_refresh  = "1h"
key_cache_ttl   = "1h"
health {
  interval = "1h"
  cooldown = "1h"
}
limits { max_body_bytes = 4096 }
org "etabA" {
  share = 0.75
  team "etabA" {}
}
org "etabB" {
  share = 0.25
  team "etabB" {}
}
pool "b300" { capacity = 4 }
model "llama" {
  upstream_model = "meta/llama"
  pool           = "b300"
  key_file       = "/unused"
  cost {
    input_per_mtok  = 1000
    output_per_mtok = 5000
    cached_per_mtok = 100
  }
`+ups.String()+`}
model "llama-batch" {
  upstream_model = "meta/llama"
  pool           = "b300"
  class          = "batch"
  key_file       = "/unused"
`+ups.String()+`}
`), "test.hcl")
	if err != nil {
		panic(err)
	}
	return c
}

// testKeys maps every key file of the configuration to the internal key.
func testKeys(cfg *config.Config) map[string]string {
	keys := map[string]string{}
	for _, f := range cfg.KeyFiles() {
		keys[f] = internalKey
	}
	return keys
}

func newEnv(t *testing.T, cfg *config.Config, opts ...Option) *env {
	t.Helper()
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg)}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	e := &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	t.Cleanup(func() { gw.Close(); srv.Close(context.Background()) })
	return e
}

func (e *env) do(method, path, token, body string, hdr ...string) *http.Response {
	e.t.Helper()
	req, err := http.NewRequest(method, e.gw.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func readAll(t testing.TB, r *http.Response) string {
	t.Helper()
	defer r.Body.Close()
	b, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// newKey creates a key through the admin API.
func (e *env) newKey(team string, limits ...int) (id, secret string) {
	e.t.Helper()
	l := []int{0, 0, 0}
	copy(l, limits)
	resp := e.do("POST", "/admin/keys", master, fmt.Sprintf(`{"team":%q,"name":"t","rpm":%d,"tpm":%d,"max_inflight":%d}`, team, l[0], l[1], l[2]))
	body := readAll(e.t, resp)
	if resp.StatusCode != http.StatusCreated {
		e.t.Fatalf("create key: %d %s", resp.StatusCode, body)
	}
	var out struct{ ID, Key string }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		e.t.Fatal(err)
	}
	return out.ID, out.Key
}

const chat = `{"model":"llama","messages":[{"role":"user","content":"hi"}]}`

func TestClientCannotChooseItsPriority(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA")
	body := `{"model":"llama","priority":-100,"extra_body":{"priority":-100},"priority":-7,` +
		`"request_id":"steal","vllm_xargs":{"a":1},"messages":[{"role":"user","content":"hi"}]}`
	resp := e.do("POST", "/v1/chat/completions", key, body,
		"X-Vllm-Priority", "-50", "x-pass-X-Vllm-Priority", "-50", "x-pass-Authorization", "Bearer evil",
		"X-Forwarded-Host", "evil.example")
	out := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(out, `"content":"hi"`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	got := up.last()
	if p := got.Header.Values("X-Vllm-Priority"); len(p) != 1 || p[0] != "0" {
		t.Fatalf("upstream priority header %v, want [0]", p)
	}
	if got.Header.Get("Authorization") != "Bearer "+internalKey {
		t.Fatalf("upstream Authorization %q", got.Header.Get("Authorization"))
	}
	for k := range got.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-pass") || strings.HasPrefix(lk, "x-forwarded") {
			t.Fatalf("client header %s forwarded", k)
		}
	}
	for _, f := range []string{"priority", "extra_body", "request_id", "vllm_xargs"} {
		if _, ok := got.Body[f]; ok {
			t.Fatalf("%s reached vLLM: %v", f, got.Body)
		}
	}
	if got.Body["model"] != "meta/llama" {
		t.Fatalf("model %v", got.Body["model"])
	}
	if resp.Header.Get("X-Llmflow-Priority") != "0" || resp.Header.Get("X-Request-Id") == "" {
		t.Fatalf("response headers %v", resp.Header)
	}
}

func TestBorrowingPriorityAndBatchClass(t *testing.T) {
	up := newFakeVLLM(t)
	up.gate = make(chan struct{})
	e := newEnv(t, testConfig(up.URL()))
	// Registered after newEnv, so it runs first: a failed assertion must
	// not leave requests hanging and the test server waiting for them.
	release := sync.OnceFunc(func() { close(up.gate) })
	t.Cleanup(release)
	_, key := e.newKey("etabB") // share 0.25 of C=4: one reserved slot

	var wg sync.WaitGroup
	results := make(chan string, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp := e.do("POST", "/v1/chat/completions", key, chat)
			readAll(t, resp)
			results <- resp.Header.Get("X-Llmflow-Priority")
		}()
		// Wait until the request is in flight so the order is fixed.
		deadline := time.Now().Add(5 * time.Second)
		for up.count() < i+1 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
	}
	st := e.srv.pools["b300"].Snapshot()
	if st[1].Team != "etabB" || st[1].InFlight != 2 || st[1].Borrowing != 1 {
		t.Fatalf("snapshot %+v", st[1])
	}
	resp := e.do("GET", "/metrics", "", "")
	m := readAll(t, resp)
	if !strings.Contains(m, `llmflow_inflight_borrowing{pool="b300",org="etabB"} 1`) ||
		!strings.Contains(m, `llmflow_borrowed_fraction{pool="b300",org="etabB"} 0.5`) {
		t.Fatalf("metrics:\n%s", m)
	}
	release()
	wg.Wait()
	close(results)
	var got []string
	for p := range results {
		got = append(got, p)
	}
	if !(got[0] == "0" && got[1] == "10" || got[0] == "10" && got[1] == "0") {
		t.Fatalf("priorities %v, want one within share (0) and one borrowing (10)", got)
	}
	up.gate = nil
	resp = e.do("POST", "/v1/chat/completions", key, strings.Replace(chat, `"llama"`, `"llama-batch"`, 1))
	readAll(t, resp)
	if resp.Header.Get("X-Llmflow-Priority") != "1" {
		t.Fatalf("batch within share priority %q", resp.Header.Get("X-Llmflow-Priority"))
	}
}

func TestStreamingUsageCaptureAndAccounting(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	keyID, key := e.newKey("etabA")
	resp := e.do("POST", "/v1/chat/completions", key, `{"model":"llama","stream":true,"messages":[]}`)
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	sc := bufio.NewScanner(resp.Body)
	var lines []string
	for sc.Scan() {
		if sc.Text() != "" {
			lines = append(lines, sc.Text())
		}
	}
	resp.Body.Close()
	if len(lines) != 4 || lines[3] != "data: [DONE]" || strings.Contains(strings.Join(lines, ""), "prompt_tokens") {
		t.Fatalf("client stream %q", lines)
	}
	if so, _ := up.last().Body["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Fatalf("include_usage not injected: %v", up.last().Body)
	}
	// A client that asked for usage gets it.
	resp = e.do("POST", "/v1/chat/completions", key, `{"model":"llama","stream":true,"stream_options":{"include_usage":true}}`)
	if out := readAll(t, resp); !strings.Contains(out, `"prompt_tokens":100`) {
		t.Fatalf("usage chunk missing: %s", out)
	}
	e.srv.Close(context.Background())
	recs, _ := e.st.Usage(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	if len(recs) != 1 || recs[0].Requests != 2 || recs[0].PromptTokens != 200 || recs[0].CompletionTokens != 40 || recs[0].CachedTokens != 80 {
		t.Fatalf("usage %+v", recs)
	}
	// (60 uncached × 1000 + 40 cached × 100 + 20 out × 5000) / 1e6 = 0.164 GPU-h per request.
	if g := recs[0].GPUHours; g < 0.3279 || g > 0.3281 {
		t.Fatalf("gpu hours %v", g)
	}
	ks, _ := e.st.ListKeys(context.Background(), "etabA")
	if len(ks) != 1 || ks[0].ID != keyID {
		t.Fatalf("keys %+v", ks)
	}
}

func TestBudgetDeniesAndFailsClosed(t *testing.T) {
	up := newFakeVLLM(t)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := newEnv(t, testConfig(up.URL()), WithClock(clock))
	_, key := e.newKey("etabA")
	resp := e.do("PUT", "/admin/budgets/etabA", master, `{"period":"day","max_cost":0.2}`)
	if out := readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("set budget: %d %s", resp.StatusCode, out)
	}
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	if readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("first request %d", resp.StatusCode)
	}
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	if readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("second request (0.164 < 0.2) %d", resp.StatusCode)
	}
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	if out := readAll(t, resp); resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(out, "insufficient_quota") {
		t.Fatalf("over budget: %d %s", resp.StatusCode, out)
	}
	hits := up.count()

	// Store down: once the budget state is older than 3 refresh periods,
	// every request is refused with 503.
	e.st.SetFail(errors.New("connection refused"))
	if err := e.srv.budget.refresh(context.Background()); err == nil {
		t.Fatal("refresh should fail")
	}
	mu.Lock()
	now = now.Add(4 * time.Hour)
	mu.Unlock()
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	if out := readAll(t, resp); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("fail-closed: %d %s", resp.StatusCode, out)
	}
	if resp := e.do("GET", "/healthz", "", ""); readAll(t, resp) == "" || resp.StatusCode != 503 {
		t.Fatalf("healthz %d", resp.StatusCode)
	}
	if up.count() != hits {
		t.Fatal("a refused request reached vLLM")
	}
	e.st.SetFail(nil)
	if err := e.srv.budget.refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	resp = e.do("DELETE", "/admin/budgets/etabA", master, "")
	readAll(t, resp)
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	if readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("after recovery and budget removal: %d", resp.StatusCode)
	}
}

func TestFailOpenWhenConfigured(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	f := false
	cfg.Store.FailClosed = &f
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	// The key is resolved (and cached) while the store is up: an unknown
	// key can never be authenticated without the store.
	readAll(t, e.do("GET", "/v1/models", key, ""))
	e.st.SetFail(errors.New("down"))
	_ = e.srv.budget.refresh(context.Background())
	resp := e.do("POST", "/v1/chat/completions", key, chat)
	if readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("fail-open: %d", resp.StatusCode)
	}
	// Accounting failures are counted, not fatal.
	e.srv.Close(context.Background())
	if e.srv.rec.failed.Load() != 1 {
		t.Fatalf("failed records %d", e.srv.rec.failed.Load())
	}
}

func TestRetryAndCooldown(t *testing.T) {
	bad := newFakeVLLM(t)
	bad.status = http.StatusInternalServerError
	good := newFakeVLLM(t)
	closed := httptest.NewServer(http.NotFoundHandler())
	closedURL := closed.URL
	closed.Close()
	cfg := testConfig(closedURL, bad.URL(), good.URL())
	cfg.Retries = 2
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	resp := e.do("POST", "/v1/chat/completions", key, chat)
	if out := readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	if good.count() != 1 {
		t.Fatalf("good upstream hits %d", good.count())
	}
	// Both failing upstreams are cooling down: the next request goes
	// straight to the good one.
	bh := bad.count()
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	readAll(t, resp)
	if bad.count() != bh || good.count() != 2 {
		t.Fatalf("cooldown ignored: bad %d good %d", bad.count(), good.count())
	}
	if m := readAll(t, e.do("GET", "/metrics", "", "")); !strings.Contains(m, `llmflow_upstream_retries_total{group="llama"}`) {
		t.Fatalf("retry metric missing")
	}
}

func TestAllUpstreamsFail(t *testing.T) {
	bad := newFakeVLLM(t)
	bad.status = http.StatusServiceUnavailable
	e := newEnv(t, testConfig(bad.URL()))
	_, key := e.newKey("etabA")
	resp := e.do("POST", "/v1/chat/completions", key, chat)
	if readAll(t, resp); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("single upstream 5xx is passed through: %d", resp.StatusCode)
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	u := closed.URL
	closed.Close()
	e2 := newEnv(t, testConfig(u))
	_, key2 := e2.newKey("etabA")
	resp = e2.do("POST", "/v1/chat/completions", key2, chat)
	if readAll(t, resp); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("unreachable: %d", resp.StatusCode)
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	var leaked sync.Mutex
	leakedAuth := ""
	sink := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked.Lock()
		leakedAuth = r.Header.Get("Authorization")
		leaked.Unlock()
	}))
	defer sink.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, sink.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	e := newEnv(t, testConfig(redir.URL))
	_, key := e.newKey("etabA")
	resp := e.do("POST", "/v1/chat/completions", key, chat)
	readAll(t, resp)
	leaked.Lock()
	defer leaked.Unlock()
	if leakedAuth != "" || resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("redirect followed: status %d, auth %q", resp.StatusCode, leakedAuth)
	}
}

func TestAuthAndRevocation(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	for _, tok := range []string{"", "llmf-nope"} {
		resp := e.do("POST", "/v1/chat/completions", tok, chat)
		if readAll(t, resp); resp.StatusCode != 401 {
			t.Fatalf("token %q: %d", tok, resp.StatusCode)
		}
	}
	id, key := e.newKey("etabA")
	resp := e.do("GET", "/v1/models", key, "")
	if out := readAll(t, resp); !strings.Contains(out, `"id":"llama"`) || !strings.Contains(out, `"id":"llama-batch"`) {
		t.Fatalf("models %s", out)
	}
	resp = e.do("DELETE", "/admin/keys/"+id, master, "")
	if readAll(t, resp); resp.StatusCode != 204 {
		t.Fatalf("revoke %d", resp.StatusCode)
	}
	resp = e.do("POST", "/v1/chat/completions", key, chat)
	if readAll(t, resp); resp.StatusCode != 401 {
		t.Fatalf("revoked key accepted: %d", resp.StatusCode)
	}
	_, keyB := e.newKey("etabB")
	resp = e.do("DELETE", "/admin/teams/etabB", master, "")
	if readAll(t, resp); resp.StatusCode != 204 {
		t.Fatalf("disable team %d", resp.StatusCode)
	}
	resp = e.do("GET", "/v1/models", keyB, "")
	if readAll(t, resp); resp.StatusCode != 401 {
		t.Fatalf("key of disabled team accepted: %d", resp.StatusCode)
	}
	// Store down and key not cached: 503, not 401.
	e.st.SetFail(errors.New("down"))
	resp = e.do("GET", "/v1/models", "llmf-other", "")
	if readAll(t, resp); resp.StatusCode != 503 {
		t.Fatalf("store down: %d", resp.StatusCode)
	}
}

func TestAdminAPI(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	for _, tok := range []string{"", "wrong", master + "x"} {
		resp := e.do("GET", "/admin/teams", tok, "")
		if readAll(t, resp); resp.StatusCode != 401 {
			t.Fatalf("admin with %q: %d", tok, resp.StatusCode)
		}
	}
	resp := e.do("POST", "/admin/orgs", master, `{"name":"orgC"}`)
	if readAll(t, resp); resp.StatusCode != 201 {
		t.Fatalf("create org %d", resp.StatusCode)
	}
	resp = e.do("POST", "/admin/teams", master, `{"name":"etabC","org":"orgC"}`)
	if readAll(t, resp); resp.StatusCode != 201 {
		t.Fatalf("create team %d", resp.StatusCode)
	}
	for _, bad := range []string{`{"name":"a b","org":"orgC"}`, `{"name":"x","org":"orgC","extra":1}`, `nope`, `{"name":"x"}`} {
		resp := e.do("POST", "/admin/teams", master, bad)
		if readAll(t, resp); resp.StatusCode != 400 {
			t.Fatalf("bad team %s: %d", bad, resp.StatusCode)
		}
	}
	for _, bad := range []string{`{"name":"a b"}`, `{}`} {
		resp := e.do("POST", "/admin/orgs", master, bad)
		if readAll(t, resp); resp.StatusCode != 400 {
			t.Fatalf("bad org %s: %d", bad, resp.StatusCode)
		}
	}
	resp = e.do("POST", "/admin/teams", master, `{"name":"x","org":"nope"}`)
	if readAll(t, resp); resp.StatusCode != 404 {
		t.Fatalf("team in unknown org %d", resp.StatusCode)
	}
	resp = e.do("POST", "/admin/teams", master, `{"name":"etabC","org":"orgC"}`)
	if readAll(t, resp); resp.StatusCode != 409 {
		t.Fatalf("duplicate team %d", resp.StatusCode)
	}
	resp = e.do("POST", "/admin/teams", master, `{"name":"etabC","org":"etabA"}`)
	if readAll(t, resp); resp.StatusCode != 409 {
		t.Fatalf("team moved to another org %d", resp.StatusCode)
	}
	out := readAll(t, e.do("GET", "/admin/orgs", master, ""))
	if !strings.Contains(out, `"name":"etabA"`) || !strings.Contains(out, `"share":0.75`) || !strings.Contains(out, `"name":"orgC"`) {
		t.Fatalf("orgs %s", out)
	}
	out = readAll(t, e.do("GET", "/admin/teams?org=orgC", master, ""))
	if !strings.Contains(out, `"name":"etabC"`) || strings.Contains(out, `"name":"etabA"`) {
		t.Fatalf("teams of orgC %s", out)
	}
	// Projects.
	resp = e.do("POST", "/admin/teams/etabC/projects", master, `{"name":"eval"}`)
	if readAll(t, resp); resp.StatusCode != 201 {
		t.Fatalf("create project %d", resp.StatusCode)
	}
	resp = e.do("POST", "/admin/teams/etabC/projects", master, `{"name":"a/b"}`)
	if readAll(t, resp); resp.StatusCode != 400 {
		t.Fatalf("bad project %d", resp.StatusCode)
	}
	if out := readAll(t, e.do("GET", "/admin/teams/etabC/projects", master, "")); !strings.Contains(out, `"name":"eval"`) {
		t.Fatalf("projects %s", out)
	}
	if resp := e.do("GET", "/admin/teams/nobody/projects", master, ""); readAll(t, resp) == "" || resp.StatusCode != 404 {
		t.Fatalf("projects of unknown team %d", resp.StatusCode)
	}
	resp = e.do("POST", "/admin/keys", master, `{"team":"etabC","project":"eval"}`)
	projKey := readAll(t, resp)
	if resp.StatusCode != 201 || !strings.Contains(projKey, `"project":"eval"`) {
		t.Fatalf("project key %d %s", resp.StatusCode, projKey)
	}
	resp = e.do("POST", "/admin/keys", master, `{"team":"etabC","project":"nope"}`)
	if readAll(t, resp); resp.StatusCode != 404 {
		t.Fatalf("key in unknown project %d", resp.StatusCode)
	}
	_, secret := e.newKey("etabC")
	out = readAll(t, e.do("GET", "/admin/keys?team=etabC", master, ""))
	if strings.Contains(out, secret) || strings.Contains(out, "hash") || !strings.Contains(out, `"team":"etabC"`) {
		t.Fatalf("key listing leaks or misses: %s", out)
	}
	for _, bad := range []string{`{"team":"nobody"}`, `{"team":"etabC","rpm":-1}`} {
		resp := e.do("POST", "/admin/keys", master, bad)
		if readAll(t, resp); resp.StatusCode != 404 && resp.StatusCode != 400 {
			t.Fatalf("bad key %s: %d", bad, resp.StatusCode)
		}
	}
	for _, bad := range []string{`{"period":"year","max_cost":1}`, `{"period":"day","max_cost":-1}`} {
		resp := e.do("PUT", "/admin/budgets/etabC", master, bad)
		if readAll(t, resp); resp.StatusCode != 400 {
			t.Fatalf("bad budget %s: %d", bad, resp.StatusCode)
		}
	}
	resp = e.do("PUT", "/admin/budgets/nobody", master, `{"period":"day","max_cost":1}`)
	if readAll(t, resp); resp.StatusCode != 404 {
		t.Fatalf("budget for unknown team: %d", resp.StatusCode)
	}
	resp = e.do("PUT", "/admin/budgets/etabC", master, `{"period":"month","max_cost":100}`)
	readAll(t, resp)
	if out := readAll(t, e.do("GET", "/admin/budgets", master, "")); !strings.Contains(out, `"period":"month"`) {
		t.Fatalf("budgets %s", out)
	}
	resp = e.do("POST", "/v1/chat/completions", secret, chat)
	readAll(t, resp)
	e.srv.Close(context.Background())
	from := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	to := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	out = readAll(t, e.do("GET", "/admin/usage.csv?from="+from+"&to="+to, master, ""))
	if !strings.HasPrefix(out, "from,to,team,pool,requests") || !strings.Contains(out, ",etabC,b300,1,100,20,40,0.164000") {
		t.Fatalf("csv:\n%s", out)
	}
	resp = e.do("GET", "/admin/usage.csv?from=bad", master, "")
	if readAll(t, resp); resp.StatusCode != 400 {
		t.Fatalf("bad range %d", resp.StatusCode)
	}
	out = readAll(t, e.do("GET", "/admin/fairshare", master, ""))
	if !strings.Contains(out, `"b300"`) || !strings.Contains(out, `"org":"orgC"`) {
		t.Fatalf("fairshare %s", out)
	}
	// Disabling the project, then the organisation, revokes their keys.
	var pk struct{ Key string }
	if err := json.Unmarshal([]byte(projKey), &pk); err != nil {
		t.Fatal(err)
	}
	if resp := e.do("GET", "/v1/models", pk.Key, ""); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("project key %d", resp.StatusCode)
	}
	if resp := e.do("DELETE", "/admin/teams/etabC/projects/eval", master, ""); readAll(t, resp) != "" || resp.StatusCode != 204 {
		t.Fatalf("disable project %d", resp.StatusCode)
	}
	if resp := e.do("GET", "/v1/models", pk.Key, ""); readAll(t, resp) == "" || resp.StatusCode != 401 {
		t.Fatalf("key of disabled project %d", resp.StatusCode)
	}
	if resp := e.do("DELETE", "/admin/orgs/orgC", master, ""); readAll(t, resp) != "" || resp.StatusCode != 204 {
		t.Fatalf("disable org %d", resp.StatusCode)
	}
	if resp := e.do("GET", "/v1/models", secret, ""); readAll(t, resp) == "" || resp.StatusCode != 401 {
		t.Fatalf("key of disabled org %d", resp.StatusCode)
	}
	for _, p := range []string{"/admin/keys/k_none", "/admin/teams/nobody", "/admin/budgets/nobody", "/admin/orgs/nobody", "/admin/teams/etabA/projects/nobody"} {
		resp := e.do("DELETE", p, master, "")
		if readAll(t, resp); resp.StatusCode != 404 {
			t.Fatalf("DELETE %s: %d", p, resp.StatusCode)
		}
	}
	e.st.SetFail(errors.New("down"))
	for _, p := range []string{"/admin/teams", "/admin/keys", "/admin/budgets"} {
		resp := e.do("GET", p, master, "")
		if out := readAll(t, resp); resp.StatusCode != 503 || strings.Contains(out, "down") {
			t.Fatalf("GET %s with store down: %d %s", p, resp.StatusCode, out)
		}
	}
}

func TestKeyLimits(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA", 2, 0, 0) // 2 requests per minute
	for i, want := range []int{200, 200, 429} {
		resp := e.do("POST", "/v1/chat/completions", key, chat)
		if out := readAll(t, resp); resp.StatusCode != want {
			t.Fatalf("request %d: %d %s", i, resp.StatusCode, out)
		}
	}
	_, key = e.newKey("etabA", 0, 100, 0) // 100 tokens per minute; one request uses 120
	for i, want := range []int{200, 429} {
		resp := e.do("POST", "/v1/chat/completions", key, chat)
		if readAll(t, resp); resp.StatusCode != want {
			t.Fatalf("tpm request %d: %d", i, resp.StatusCode)
		}
	}
	up.gate = make(chan struct{})
	_, key = e.newKey("etabA", 0, 0, 1) // one in flight
	done := make(chan int)
	go func() {
		resp := e.do("POST", "/v1/chat/completions", key, chat)
		readAll(t, resp)
		done <- resp.StatusCode
	}()
	deadline := time.Now().Add(5 * time.Second)
	for up.count() < 6 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	resp := e.do("POST", "/v1/chat/completions", key, chat)
	if readAll(t, resp); resp.StatusCode != 429 {
		t.Fatalf("second in flight: %d", resp.StatusCode)
	}
	close(up.gate)
	if s := <-done; s != 200 {
		t.Fatalf("first in flight: %d", s)
	}
}

func TestRequestValidation(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA")
	cases := map[string]int{
		`{"model":"nope"}`:               404,
		`not json`:                       400,
		`{"model":"llama","stream":"x"}`: 400,
		`{"model":"llama","pad":"` + strings.Repeat("x", 5000) + `"}`: 413,
	}
	for body, want := range cases {
		resp := e.do("POST", "/v1/chat/completions", key, body)
		if readAll(t, resp); resp.StatusCode != want {
			t.Errorf("%.40s: %d, want %d", body, resp.StatusCode, want)
		}
	}
	if up.count() != 0 {
		t.Fatal("invalid requests reached vLLM")
	}
}

func TestMetricsTokenAndFormat(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, MetricsToken: "mt", UpstreamKeys: testKeys(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	defer gw.Close()
	e := &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	if resp := e.do("GET", "/metrics", "", ""); readAll(t, resp) == "" || resp.StatusCode != 401 {
		t.Fatalf("metrics without token: %d", resp.StatusCode)
	}
	resp := e.do("GET", "/metrics", "mt", "")
	out := readAll(t, resp)
	if resp.Header.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("content type %q", resp.Header.Get("Content-Type"))
	}
	for _, want := range []string{
		"# TYPE llmflow_inflight gauge", `llmflow_reserved{pool="b300",org="etabA"} 3`,
		`llmflow_overhead_seconds_bucket{le="+Inf"} 0`, "llmflow_overhead_seconds_count 0",
		`llmflow_upstream_healthy{group="llama",upstream="http://127.0.0.1:`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if _, err := New(cfg, st, Secrets{}); err == nil {
		t.Fatal("empty master key accepted")
	}
	if _, err := New(cfg, st, Secrets{MasterKey: "m"}); err == nil {
		t.Fatal("missing upstream key accepted")
	}
}

func TestHealthChecks(t *testing.T) {
	up := newFakeVLLM(t)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer down.Close()
	e := newEnv(t, testConfig(down.URL, up.URL()))
	e.srv.probeAll(context.Background())
	g := e.srv.groups["llama"]
	if g.upstreams[0].healthy.Load() || !g.upstreams[1].healthy.Load() {
		t.Fatal("health verdicts wrong")
	}
	if u := g.pick(map[*upstream]bool{}, time.Now()); u != g.upstreams[1] {
		t.Fatal("picked the unhealthy upstream")
	}
	// All unavailable: fall back rather than fail.
	g.upstreams[1].healthy.Store(false)
	if u := g.pick(map[*upstream]bool{}, time.Now()); u == nil {
		t.Fatal("no fallback")
	}
	if redactURL("http://h:1/x?y") != "http://h:1" || redactURL("%") != "invalid" {
		t.Fatal("redactURL")
	}
}

func TestRecorderDropsWhenFull(t *testing.T) {
	r := newRecorder(store.NewMemory(), 1, 10, nil)
	r.record(store.Record{})
	r.record(store.Record{})
	if r.dropped.Load() != 1 {
		t.Fatalf("dropped %d", r.dropped.Load())
	}
	r.start()
	r.close(context.Background())
	r.close(context.Background())
	r.record(store.Record{})
	if r.dropped.Load() != 2 || r.written.Load() != 1 {
		t.Fatalf("after close: dropped %d written %d", r.dropped.Load(), r.written.Load())
	}
}

func TestWindow(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var w window
	w.roll(t0)
	w.cur = 60
	if v := w.value(t0.Add(90 * time.Second)); v != 30 {
		t.Fatalf("half overlap %v", v)
	}
	if v := w.value(t0.Add(10 * time.Minute)); v != 0 {
		t.Fatalf("stale %v", v)
	}
	l := newLimiter(time.Now)
	l.release("unknown", 5)
}
