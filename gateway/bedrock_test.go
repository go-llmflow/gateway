package gateway

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/aws/eventstream"
	"github.com/go-llmflow/aws/sigv4"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// fakeBedrock answers like Bedrock's runtime endpoint and VERIFIES the
// request's SigV4 signature (or bearer key) with go-llmflow/aws: a request
// signed over other bytes, at another path, with other credentials or with a
// header changed after signing is refused with 403.
type fakeBedrock struct {
	srv     *httptest.Server
	region  string
	keys    map[string]sigv4.Credentials // access key id → credentials
	bearers map[string]string            // API key → who

	mu   sync.Mutex
	reqs []bedrockRec
	mode string
	// status answers an error instead (message in bad).
	status int
	bad    string
	// usage is what an answer reports, given the request's max_tokens.
	usage func(maxTokens int64) bedrockUsage
}

type bedrockUsage struct{ In, Out, Read, Write int64 }

type bedrockRec struct {
	Path, RawPath string
	Header        http.Header
	Body          map[string]any
	Raw           []byte
	Who           string
	SigOK         bool
}

const (
	akidAlpha  = "AKIDALPHA0123456789A"
	skAlpha    = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYalphaSECRETKEY"
	tokAlpha   = "FwoGZXIvYXdzEBYaDsessionTokenALPHAxxxxxxxxxxxxxxxxxxxx"
	bearerBeta = "bedrock-api-key-BETA-secret-value-0123456789"
	bedrockOne = `{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":50,"output_tokens":5}`
	// one answer: (10×2 + 100×0.1 + 50×2.5 + 5×10) / 1e6 USD.
	fakeBdrkCost = 205e-6
	bedrockModel = "eu.anthropic.claude-sonnet-4-5-20250929-v1:0"
)

func newFakeBedrock(t testing.TB) *fakeBedrock {
	f := &fakeBedrock{region: "eu-west-3",
		keys:    map[string]sigv4.Credentials{akidAlpha: {AccessKeyID: akidAlpha, SecretAccessKey: skAlpha, SessionToken: tokAlpha}},
		bearers: map[string]string{bearerBeta: "etabB"},
		usage:   func(int64) bedrockUsage { return bedrockUsage{10, 5, 100, 50} }}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// verify checks the signature as AWS does: it signs the same method, path,
// signed headers and body with the credentials of the access key id and
// compares the Authorization headers.
func (f *fakeBedrock) verify(r *http.Request, body []byte) (string, bool) {
	auth := r.Header.Get("Authorization")
	if tok, ok := strings.CutPrefix(auth, "Bearer "); ok {
		who, known := f.bearers[tok]
		return who, known
	}
	const pfx = "AWS4-HMAC-SHA256 Credential="
	rest, ok := strings.CutPrefix(auth, pfx)
	if !ok {
		return "", false
	}
	cred, rest, _ := strings.Cut(rest, ", SignedHeaders=")
	signed, _, _ := strings.Cut(rest, ", Signature=")
	akid, scope, _ := strings.Cut(cred, "/")
	c, known := f.keys[akid]
	if !known || !strings.HasSuffix(scope, "/"+f.region+"/bedrock/aws4_request") {
		return "", false
	}
	at, err := time.Parse("20060102T150405Z", r.Header.Get("X-Amz-Date"))
	if err != nil {
		return "", false
	}
	clone := &http.Request{Method: r.Method, Host: r.Host, URL: &url.URL{Path: r.URL.Path, RawPath: r.URL.RawPath}, Header: http.Header{}}
	for _, h := range strings.Split(signed, ";") {
		if h == "host" {
			continue
		}
		for _, v := range r.Header.Values(h) {
			clone.Header.Add(h, v)
		}
	}
	if err := (sigv4.Signer{Credentials: c, Region: f.region, Service: "bedrock"}).Sign(clone, body, at); err != nil {
		return "", false
	}
	return akid, clone.Header.Get("Authorization") == auth
}

func chunkEvent(ev string) []byte {
	pl, _ := json.Marshal(map[string]string{"bytes": base64.StdEncoding.EncodeToString([]byte(ev))})
	b, err := eventstream.Marshal(eventstream.Message{Headers: []eventstream.Header{
		{Name: ":event-type", Value: eventstream.String("chunk")},
		{Name: ":content-type", Value: eventstream.String("application/json")},
		{Name: ":message-type", Value: eventstream.String("event")}}, Payload: pl})
	if err != nil {
		panic(err)
	}
	return b
}

func exceptionEvent(kind string) []byte {
	b, err := eventstream.Marshal(eventstream.Message{Headers: []eventstream.Header{
		{Name: ":exception-type", Value: eventstream.String(kind)},
		{Name: ":content-type", Value: eventstream.String("application/json")},
		{Name: ":message-type", Value: eventstream.String("exception")}}, Payload: []byte(`{"message":"Too many requests"}`)})
	if err != nil {
		panic(err)
	}
	return b
}

// The events of the golden stream.
func bedrockEvents(u bedrockUsage) []string {
	return []string{
		fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_bdrk_1","type":"message","role":"assistant","content":[],"model":"claude","usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":1}}}`, u.In, u.Read, u.Write),
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`,
		`{"type":"content_block_stop","index":0}`,
		fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":%d}}`, u.Out),
		`{"type":"message_stop","amazon-bedrock-invocationMetrics":{"inputTokenCount":160,"outputTokenCount":5,"invocationLatency":10,"firstByteLatency":5}}`,
	}
}

func (f *fakeBedrock) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	who, ok := f.verify(r, raw)
	f.mu.Lock()
	f.reqs = append(f.reqs, bedrockRec{Path: r.URL.Path, RawPath: r.URL.EscapedPath(), Header: r.Header.Clone(), Body: body, Raw: raw, Who: who, SigOK: ok})
	mode, status, bad, usage := f.mode, f.status, f.bad, f.usage
	f.mu.Unlock()
	w.Header().Set("X-Amzn-Requestid", "req-bdrk-1")
	jsonErr := func(code int, msg string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
	if !ok {
		jsonErr(403, "The request signature we calculated does not match the signature you provided for access key "+akidAlpha+" "+skAlpha)
		return
	}
	if status != 0 {
		jsonErr(status, bad)
		return
	}
	stream := strings.HasSuffix(r.URL.Path, "/invoke-with-response-stream")
	if !stream && !strings.HasSuffix(r.URL.Path, "/invoke") || !strings.HasPrefix(r.URL.Path, "/model/") {
		jsonErr(404, "no such operation")
		return
	}
	if body["anthropic_version"] != config.BedrockVersion || body["model"] != nil || body["stream"] != nil || body["max_tokens"] == nil {
		jsonErr(400, "malformed Anthropic body: "+string(raw))
		return
	}
	mt, _ := body["max_tokens"].(float64)
	u := usage(int64(mt))
	if !stream && mode == "no-usage" {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_bdrk_1","type":"message","role":"assistant","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`)
		return
	}
	if !stream && mode == "cut-body" {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "1000")
		fmt.Fprint(w, `{"id":"msg_bdrk_1","type":"mess`)
		return
	}
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"msg_bdrk_1","type":"message","role":"assistant","model":"claude","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":%d}}`, u.In, u.Read, u.Write, u.Out)
		return
	}
	w.Header().Set("Content-Type", bedrockEventStream)
	w.Header().Set("X-Amzn-Bedrock-Content-Type", "application/json")
	evs := bedrockEvents(u)
	switch mode {
	case "cut-mid-message":
		w.Write(chunkEvent(evs[0]))
		w.Write(chunkEvent(evs[2])[:20])
	case "cut-at-boundary": // everything but the last two events
		for _, e := range evs[:4] {
			w.Write(chunkEvent(e))
		}
	case "exception":
		w.Write(chunkEvent(evs[0]))
		w.Write(chunkEvent(evs[2]))
		w.Write(exceptionEvent("throttlingException"))
	case "crc":
		w.Write(chunkEvent(evs[0]))
		bad := chunkEvent(evs[2])
		bad[len(bad)-1] ^= 0xff
		w.Write(bad)
		w.Write(chunkEvent(evs[4]))
		w.Write(chunkEvent(evs[5]))
	case "no-usage": // a complete-looking stream whose message_delta has no usage
		for i, e := range evs {
			if i == 4 {
				e = `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`
			}
			w.Write(chunkEvent(e))
		}
	default:
		for _, e := range evs {
			w.Write(chunkEvent(e))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}
}

func (f *fakeBedrock) set(mode string, status int, bad string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mode, f.status, f.bad = mode, status, bad
}

func (f *fakeBedrock) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func (f *fakeBedrock) last(t testing.TB) bedrockRec {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		t.Fatal("Bedrock received nothing")
	}
	return f.reqs[len(f.reqs)-1]
}

// bedrockHCL: etabA authenticates with SigV4 (and a session token), etabB
// with a Bedrock API key; each has its own credentials.
func bedrockHCL(endpoint, vllm, extra string) string {
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
pool "bedrock" { capacity = 4 }
model "llama" {
  pool     = "b300"
  key_file = "/unused"
  upstream "u" { base_url = "` + vllm + `" }
}
model "claude" {
  provider          = "bedrock"
  region            = "eu-west-3"
  model_id          = "` + bedrockModel + `"
  endpoint_override = "` + endpoint + `"
  pool              = "bedrock"
  allow_external    = true
  orgs              = ["etabA", "etabB"]
  anthropic_betas   = ["context-management-2025-06-27"]
  context_window    = 1000
  cost {
    unit                    = "USD"
    price_date              = "2026-10-31"
    input_per_mtok          = 2
    output_per_mtok         = 10
    cached_per_mtok         = 0.1
    cache_write_5m_per_mtok = 2.5
    cache_write_1h_per_mtok = 4
  }
  org_credentials "etabA" {
    access_key_id_file     = "/aws/alpha-id"
    secret_access_key_file = "/aws/alpha-secret"
    session_token_file     = "/aws/alpha-token"
  }
  org_credentials "etabB" {
    bearer_token_file = "/aws/beta-bearer"
  }
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

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func newBedrockEnv(t *testing.T, extra string) (*env, *fakeBedrock, *clock, *syncBuf) {
	t.Helper()
	up := newFakeBedrock(t)
	vllm := newFakeVLLM(t)
	cfg, err := config.Parse([]byte(bedrockHCL(up.srv.URL, vllm.URL(), extra)), "bedrock.hcl")
	if err != nil {
		t.Fatal(err)
	}
	keys := testKeys(cfg)
	keys["/aws/alpha-id"], keys["/aws/alpha-secret"], keys["/aws/alpha-token"] = akidAlpha, skAlpha, tokAlpha
	keys["/aws/beta-bearer"] = bearerBeta
	clk := &clock{t: time.Now().UTC().Truncate(time.Second)}
	logs := &syncBuf{}
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: keys, CacheSaltKey: testSaltKey}, WithClock(clk.now),
		WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	e := &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	t.Cleanup(func() { gw.Close(); srv.Close(context.Background()) })
	return e, up, clk, logs
}

func bdrkMsg(stream bool, max int) string {
	return fmt.Sprintf(`{"model":"claude","max_tokens":%d,"stream":%v,"priority":-5,"cache_salt":"mine","vllm_xargs":{"x":1},`+
		`"messages":[{"role":"user","content":"mail jean.dupont@example.fr token %s"}]}`, max, stream, ghToken)
}

// What reaches Bedrock: a request signed over the exact bytes sent, at the
// server's clock, for the organisation's own credentials; a body without
// model and stream, with the version and the allowed betas; nothing of what
// the client sent beyond the masked messages.
func TestBedrockEgress(t *testing.T) {
	e, up, clk, _ := newBedrockEnv(t, "")
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	_, keyC := e.newKey("etabC")
	resp := e.do("POST", "/v1/messages", keyA, bdrkMsg(false, 16),
		"X-Api-Key", "client-own-key", "Authorization-Extra", "x", "Anthropic-Version", "2023-06-01",
		"Anthropic-Beta", "context-management-2025-06-27, oauth-2025-04-20,unknown-beta-2026-01-01",
		"X-Vllm-Priority", "-100", "Traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	body := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(body, `"hello"`) {
		t.Fatalf("messages: %d %s", resp.StatusCode, body)
	}
	if resp.Header.Get("Request-Id") != "req-bdrk-1" {
		t.Errorf("request id %q", resp.Header.Get("Request-Id"))
	}
	got := up.last(t)
	if !got.SigOK || got.Who != akidAlpha {
		t.Fatalf("signature not verified (who %q)", got.Who)
	}
	if got.RawPath != "/model/eu.anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke" {
		t.Errorf("path %q", got.RawPath)
	}
	if want := clk.now().Format("20060102T150405Z"); got.Header.Get("X-Amz-Date") != want || got.Header.Get("X-Amz-Security-Token") != tokAlpha {
		t.Errorf("date %q, want the server's clock %q; token set %v", got.Header.Get("X-Amz-Date"), want, got.Header.Get("X-Amz-Security-Token") == tokAlpha)
	}
	for _, k := range []string{"X-Api-Key", "X-Vllm-Priority", "X-Request-Id", "Traceparent", "Anthropic-Version", "Anthropic-Beta", "Authorization-Extra"} {
		if v := got.Header.Get(k); v != "" {
			t.Errorf("%s reached Bedrock: %q", k, v)
		}
	}
	if got.Header.Get("Accept") != "application/json" {
		t.Errorf("accept %q", got.Header.Get("Accept"))
	}
	if got.Body["anthropic_version"] != "bedrock-2023-05-31" || fmt.Sprint(got.Body["anthropic_beta"]) != "[context-management-2025-06-27]" {
		t.Errorf("version %v betas %v", got.Body["anthropic_version"], got.Body["anthropic_beta"])
	}
	for _, f := range []string{"model", "stream", "priority", "cache_salt", "vllm_xargs"} {
		if _, ok := got.Body[f]; ok {
			t.Errorf("field %s reached Bedrock", f)
		}
	}
	text, _ := json.Marshal(got.Body["messages"])
	if strings.Contains(string(text), "jean.dupont@example.fr") || strings.Contains(string(text), ghToken) {
		t.Errorf("PII or secret left the site: %s", text)
	}
	// The signature covers the bytes: the same request replayed with a changed
	// body, at another time, or with another key does not verify.
	clk.add(time.Hour)
	resp = e.do("POST", "/v1/messages", keyB, bdrkMsg(false, 16))
	if readAll(t, resp); resp.StatusCode != 200 {
		t.Fatalf("etabB: %d", resp.StatusCode)
	}
	gb := up.last(t)
	if gb.Who != "etabB" || gb.Header.Get("Authorization") != "Bearer "+bearerBeta || gb.Header.Get("X-Amz-Date") != "" {
		t.Errorf("etabB must use its API key as a bearer, unsigned: who %q auth %q", gb.Who, gb.Header.Get("Authorization"))
	}
	// An organisation not allowed: the model does not exist for it.
	if resp := e.do("POST", "/v1/messages", keyC, bdrkMsg(false, 16)); readAll(t, resp) == "" || resp.StatusCode != 404 {
		t.Errorf("etabC: %d", resp.StatusCode)
	}
	// Only /v1/messages: the chat endpoint and token counting are refused.
	n := up.count()
	chat := `{"model":"claude","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	for _, p := range []string{"/v1/chat/completions", "/v1/messages/count_tokens"} {
		body := chat
		resp := e.do("POST", p, keyA, body)
		if out := readAll(t, resp); resp.StatusCode < 400 || up.count() != n {
			t.Errorf("%s: %d %s (provider calls %d → %d)", p, resp.StatusCode, out, n, up.count())
		}
	}
}

func recordsOf(e *env) []store.Record { e.srv.Close(context.Background()); return e.st.Records() }

// Usage and cost, non-stream and stream: the cache reads and writes priced
// apart, in USD; the stream re-emitted as Anthropic SSE, byte for byte.
func TestBedrockGolden(t *testing.T) {
	e, up, _, _ := newBedrockEnv(t, "")
	_, keyA := e.newKey("etabA")
	resp := e.do("POST", "/v1/messages", keyA, bdrkMsg(false, 16))
	if body := readAll(t, resp); resp.StatusCode != 200 || !strings.Contains(body, `"usage":`+bedrockOne) {
		t.Fatalf("non-stream: %d %s", resp.StatusCode, body)
	}
	resp = e.do("POST", "/v1/messages", keyA, bdrkMsg(true, 16))
	sse := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: %d %v", resp.StatusCode, resp.Header)
	}
	// The client's stream is the provider's events, one SSE event each (the
	// output guardrail filter re-encodes them and drops the provider's own
	// metrics): compared as JSON. The byte-exact conversion is
	// TestBedrockStreamGolden.
	evs := bedrockEvents(bedrockUsage{10, 5, 100, 50})
	blocks := strings.Split(strings.TrimSuffix(sse, "\n\n"), "\n\n")
	if len(blocks) != len(evs) {
		t.Fatalf("%d events, want %d:\n%s", len(blocks), len(evs), sse)
	}
	for i, blk := range blocks {
		ev, data, _ := strings.Cut(blk, "\ndata: ")
		var got, want map[string]any
		_ = json.Unmarshal([]byte(data), &got)
		_ = json.Unmarshal([]byte(evs[i]), &want)
		delete(want, "amazon-bedrock-invocationMetrics")
		delete(got, "amazon-bedrock-invocationMetrics")
		if ev != "event: "+fmt.Sprint(want["type"]) || fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("event %d: %q\n got %v\nwant %v", i, ev, got, want)
		}
	}
	if p := up.last(t).RawPath; !strings.HasSuffix(p, "/invoke-with-response-stream") || up.last(t).Header.Get("Accept") != bedrockEventStream {
		t.Errorf("stream request: %q accept %q", p, up.last(t).Header.Get("Accept"))
	}
	recs := recordsOf(e)
	if len(recs) != 2 {
		t.Fatalf("%d records", len(recs))
	}
	for _, r := range recs {
		if r.Unit != "USD" || math.Abs(r.Cost-fakeBdrkCost) > 1e-12 || r.PromptTokens != 160 || r.CachedTokens != 100 || r.CompletionTokens != 5 || r.Estimated {
			t.Errorf("record %+v, want USD %v, 160 prompt (100 read), 5 out", r, fakeBdrkCost)
		}
	}
}

// The seven ways a budget could overflow (#89), each refused before anything
// leaves the site.
func TestBedrockRefusals(t *testing.T) {
	e, up, _, _ := newBedrockEnv(t, "")
	_, keyA := e.newKey("etabA")
	e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":1,"unit":"USD"}`)
	e.srv.groups["claude"].cfg.External.Betas = []string{"*"}
	msg := func(extra string) string {
		return `{"model":"claude","max_tokens":16,` + extra + `"messages":[{"role":"user","content":"hi"}]}`
	}
	cases := []struct {
		item, name, body string
		hdr              []string
		want             string
	}{
		{"1 guardrailConfig", "body", msg(`"guardrailConfig":{"guardrailIdentifier":"g","guardrailVersion":"1"},`), nil, "guardrailConfig"},
		{"1 guardrailConfig", "converse-style field", msg(`"amazon-bedrock-guardrailConfig":{"tagSuffix":"x"},`), nil, "guardrailConfig"},
		{"1 guardrails", "header", msg(``), []string{"X-Amzn-Bedrock-GuardrailIdentifier", "abc123"}, "X-Amzn-Bedrock"},
		{"2 serviceTier", "body", msg(`"serviceTier":{"type":"priority"},`), nil, "serviceTier"},
		{"2 performanceConfig", "body", msg(`"performanceConfig":{"latency":"optimized"},`), nil, "performanceConfig"},
		{"2 service tier", "header", msg(``), []string{"X-Amzn-Bedrock-Service-Tier", "priority"}, "X-Amzn-Bedrock"},
		{"2 performance config", "header", msg(``), []string{"X-Amzn-Bedrock-PerformanceConfig-Latency", "optimized"}, "X-Amzn-Bedrock"},
		{"5 additionalModelRequestFields", "body", msg(`"additionalModelRequestFields":{"anthropic_beta":["context-1m-2025-08-07"]},`), nil, "additionalModelRequestFields"},
		{"5 context beta in the body", "betaUnbounded", msg(`"anthropic_beta":["context-1m-2025-08-07"],`), nil, "context-1m-2025-08-07"},
		{"5 compaction beta in the body", "betaUnbounded", msg(`"anthropic_beta":["compact-2026-01-12"],`), nil, "compact-2026-01-12"},
		{"5 compaction beta in the header", "betaUnbounded", msg(``), []string{"Anthropic-Beta", "compact-2026-01-12"}, "compact-2026-01-12"},
		{"7 promptVariables", "body", msg(`"promptVariables":{"x":{"text":"y"}},`), nil, "promptVariables"},
	}
	n := up.count()
	for _, c := range cases {
		resp := e.do("POST", "/v1/messages", keyA, c.body, c.hdr...)
		out := readAll(t, resp)
		if resp.StatusCode != 400 || !strings.Contains(out, c.want) || up.count() != n {
			t.Errorf("item %s (%s): %d %s (provider calls %d → %d)", c.item, c.name, resp.StatusCode, out, n, up.count())
		}
	}
	// A beta that cannot widen anything passes, in the body or the header, and
	// reaches Bedrock once, in the body.
	e.srv.groups["claude"].cfg.External.Betas = []string{"context-management-2025-06-27"}
	for _, c := range []struct {
		body string
		hdr  []string
	}{{msg(`"anthropic_beta":["context-management-2025-06-27"],`), nil}, {msg(``), []string{"Anthropic-Beta", "context-management-2025-06-27"}}} {
		if resp := e.do("POST", "/v1/messages", keyA, c.body, c.hdr...); readAll(t, resp) == "" || resp.StatusCode != 200 {
			t.Fatalf("benign beta: %d", resp.StatusCode)
		}
		if got := fmt.Sprint(up.last(t).Body["anthropic_beta"]); got != "[context-management-2025-06-27]" {
			t.Errorf("anthropic_beta %s", got)
		}
	}
}

// Items 3, 4, 6, 7 are the configuration's: the model ID is checked when the
// file is read (the client cannot choose it).
func TestBedrockModelIDs(t *testing.T) {
	good := []string{
		"anthropic.claude-sonnet-4-5-20250929-v1:0",
		"eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"global.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"arn:aws:bedrock:eu-west-3::foundation-model/anthropic.claude-3-5-sonnet-20241022-v2:0",
		"arn:aws:bedrock:eu-west-3:123456789012:inference-profile/eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"arn:aws:bedrock:eu-west-3:123456789012:application-inference-profile/abcd1234efgh",
	}
	for _, id := range good {
		if err := config.CheckBedrockModelID(id, "eu-west-3"); err != nil {
			t.Errorf("%s refused: %v", id, err)
		}
	}
	bad := []struct{ id, why string }{
		{"arn:aws:bedrock:eu-west-3:123456789012:default-prompt-router/anthropic.claude:1", "prompt router"},
		{"arn:aws:bedrock:eu-west-3:123456789012:prompt-router/abc", "prompt router"},
		{"arn:aws:bedrock:eu-west-3:123456789012:prompt/ABCDEFGHIJ:1", "Prompt management"},
		{"arn:aws:bedrock:eu-west-3:123456789012:provisioned-model/abcdefghijkl", "Provisioned Throughput"},
		{"arn:aws:bedrock:eu-west-3:123456789012:custom-model-deployment/abcdefghijkl", "custom"},
		{"arn:aws:bedrock:eu-west-3:123456789012:imported-model/abcdefghijkl", "imported"},
		{"arn:aws:sagemaker:eu-west-3:123456789012:endpoint/x", "Bedrock resource"},
		{"arn:aws:bedrock:us-east-1::foundation-model/anthropic.claude-3-5-sonnet-20241022-v2:0", "region"},
		{"amazon.titan-text-express-v1", "Anthropic"},
		{"meta.llama3-1-70b-instruct-v1:0", "Anthropic"},
		{"", ""}, {"anthropic.claude/../x", ""},
	}
	for _, c := range bad {
		err := config.CheckBedrockModelID(c.id, "eu-west-3")
		if err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%q: %v, want a refusal mentioning %q", c.id, err, c.why)
		}
	}
}

// Chaos: a stream that did not deliver its final usage is charged W, the
// worst case held before the request left, whatever it had reported.
// W = (1000 × 4 + 16 × 10) / 1e6 = 0.00416 USD (L × highest input price, the
// 1-hour cache write, + M × output).
func TestBedrockChaosChargesW(t *testing.T) {
	const w = 0.00416
	for _, mode := range []string{"cut-mid-message", "cut-at-boundary", "exception", "crc", "no-usage"} {
		t.Run(mode, func(t *testing.T) {
			e, up, _, _ := newBedrockEnv(t, "")
			_, keyA := e.newKey("etabA")
			e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":1,"unit":"USD"}`)
			up.set(mode, 0, "")
			resp := e.do("POST", "/v1/messages", keyA, bdrkMsg(true, 16))
			sse := readAll(t, resp)
			if mode != "no-usage" && !strings.Contains(sse, "event: error") {
				t.Errorf("the client was not told the stream failed:\n%s", sse)
			}
			recs := recordsOf(e)
			if len(recs) != 1 || math.Abs(recs[0].Cost-w) > 1e-12 || !recs[0].Estimated {
				t.Fatalf("%s: records %+v, want one at W = %v", mode, recs, w)
			}
		})
	}
	// A non-stream answer without usage, and a 200 whose body is cut.
	for _, mode := range []string{"no-usage", "cut-body"} {
		e, up, _, _ := newBedrockEnv(t, "")
		_, keyA := e.newKey("etabA")
		e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":1,"unit":"USD"}`)
		up.set(mode, 0, "")
		readAll(t, e.do("POST", "/v1/messages", keyA, bdrkMsg(false, 16)))
		recs := recordsOf(e)
		if len(recs) != 1 || math.Abs(recs[0].Cost-w) > 1e-12 || !recs[0].Estimated {
			t.Errorf("non-stream %s: records %+v, want one at W = %v", mode, recs, w)
		}
	}
}

// Provider errors: translated to the Anthropic shape, the credentials refused
// without the provider's message, a 429 backs the organisation off.
func TestBedrockErrors(t *testing.T) {
	e, up, _, logs := newBedrockEnv(t, "")
	_, keyA := e.newKey("etabA")
	up.set("", 429, "Too many requests, please wait before trying again.")
	resp := e.do("POST", "/v1/messages", keyA, bdrkMsg(false, 16))
	if body := readAll(t, resp); resp.StatusCode != 429 || !strings.Contains(body, `"rate_limit_error"`) || !strings.Contains(body, "Too many requests") {
		t.Fatalf("429: %d %s", resp.StatusCode, body)
	}
	n := up.count()
	if resp := e.do("POST", "/v1/messages", keyA, bdrkMsg(false, 16)); readAll(t, resp) == "" || resp.StatusCode != 429 || up.count() != n {
		t.Errorf("no back-off after a 429: %d, provider calls %d → %d", resp.StatusCode, n, up.count())
	}
	// A message that echoes a secret is redacted; a 403 says nothing of it.
	e2, up2, _, logs2 := newBedrockEnv(t, "")
	_, keyA2 := e2.newKey("etabA")
	up2.set("", 400, "validation failed for key "+akidAlpha+" and token "+tokAlpha)
	resp = e2.do("POST", "/v1/messages", keyA2, bdrkMsg(false, 16))
	body := readAll(t, resp)
	if resp.StatusCode != 400 || strings.Contains(body, akidAlpha) || strings.Contains(body, tokAlpha) || !strings.Contains(body, "[redacted]") {
		t.Errorf("400: %d %s", resp.StatusCode, body)
	}
	up2.set("", 403, "signature mismatch "+skAlpha)
	resp = e2.do("POST", "/v1/messages", keyA2, bdrkMsg(false, 16))
	if body := readAll(t, resp); resp.StatusCode != 403 || strings.Contains(body, skAlpha) || !strings.Contains(body, "refused the gateway's credentials") {
		t.Errorf("403: %d %s", resp.StatusCode, body)
	}
	_, _ = logs, logs2
}

// The secrets never reach an output: responses, logs, metrics, records, the
// audit chain.
func TestBedrockSecretsStayInside(t *testing.T) {
	e, up, _, logs := newBedrockEnv(t, "")
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	var seen strings.Builder
	use := func(resp *http.Response) { seen.WriteString(readAll(t, resp)); fmt.Fprint(&seen, resp.Header) }
	use(e.do("POST", "/v1/messages", keyA, bdrkMsg(false, 16)))
	use(e.do("POST", "/v1/messages", keyA, bdrkMsg(true, 16)))
	use(e.do("POST", "/v1/messages", keyB, bdrkMsg(false, 16)))
	use(e.do("POST", "/v1/messages", keyB, bdrkMsg(true, 16)))
	up.set("", 400, "bad request mentioning "+skAlpha+" and "+bearerBeta)
	use(e.do("POST", "/v1/messages", keyA, bdrkMsg(false, 16)))
	use(e.do("POST", "/v1/messages", keyB, bdrkMsg(false, 16)))
	up.set("", 403, "forbidden "+tokAlpha)
	use(e.do("POST", "/v1/messages", keyA, bdrkMsg(false, 16)))
	up.set("crc", 0, "")
	use(e.do("POST", "/v1/messages", keyA, bdrkMsg(true, 16)))
	use(e.do("GET", "/metrics", "", ""))
	use(e.do("GET", "/admin/models", master, ""))
	recs, _ := json.Marshal(recordsOf(e))
	seen.Write(recs)
	ents, _ := e.st.AuditRange(context.Background(), 0, 1000)
	au, _ := json.Marshal(ents)
	seen.Write(au)
	seen.WriteString(logs.String())
	for _, secret := range []string{skAlpha, tokAlpha, bearerBeta} {
		if strings.Contains(seen.String(), secret) {
			t.Errorf("the secret %q… reached an output", secret[:12])
		}
	}
	// The access key id is not a secret in AWS's sense but is not logged either.
	if strings.Contains(logs.String(), akidAlpha) {
		t.Errorf("the access key id reached the logs")
	}
	// The planted control: the same scan finds a secret when one is there.
	if !strings.Contains(seen.String()+skAlpha, skAlpha) {
		t.Fatal("the scan cannot see a planted secret")
	}
	// The credentials print redacted.
	c := sigv4.Credentials{AccessKeyID: akidAlpha, SecretAccessKey: skAlpha, SessionToken: tokAlpha}
	if s := fmt.Sprintf("%v %+v %#v %s", c, c, c, c); strings.Contains(s, skAlpha) || strings.Contains(s, tokAlpha) {
		t.Errorf("credentials print their secrets: %s", s)
	}
}

// A request with a changed body, path, time or key does not verify at the
// provider: the signature covers what is sent.
func TestFakeBedrockRejectsBadSignatures(t *testing.T) {
	f := newFakeBedrock(t)
	body := []byte(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":1,"messages":[]}`)
	sign := func(c sigv4.Credentials, path string, b []byte, at time.Time) *http.Request {
		req, _ := http.NewRequest("POST", f.srv.URL+path, bytes.NewReader(b))
		req.Header.Set("Content-Type", "application/json")
		if err := (sigv4.Signer{Credentials: c, Region: "eu-west-3", Service: "bedrock"}).Sign(req, body, at); err != nil {
			t.Fatal(err)
		}
		return req
	}
	good := sigv4.Credentials{AccessKeyID: akidAlpha, SecretAccessKey: skAlpha, SessionToken: tokAlpha}
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	path := "/model/x.anthropic.y-v1%3A0/invoke"
	do := func(req *http.Request) int {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := do(sign(good, path, body, at)); c == 403 {
		t.Fatal("a good signature was refused")
	}
	other := sigv4.Credentials{AccessKeyID: akidAlpha, SecretAccessKey: skAlpha + "x", SessionToken: tokAlpha}
	if c := do(sign(other, path, body, at)); c != 403 {
		t.Errorf("wrong secret: %d", c)
	}
	tampered := sign(good, path, body, at)
	tampered.Body = io.NopCloser(strings.NewReader(`{"anthropic_version":"bedrock-2023-05-31","max_tokens":999,"messages":[]}`))
	tampered.ContentLength = -1
	if c := do(tampered); c != 403 {
		t.Errorf("body changed after signing: %d", c)
	}
	moved := sign(good, path, body, at)
	moved.URL.Path, moved.URL.RawPath = "/model/other/invoke", ""
	if c := do(moved); c != 403 {
		t.Errorf("path changed after signing: %d", c)
	}
	late := sign(good, path, body, at)
	late.Header.Set("X-Amz-Date", at.Add(time.Hour).Format("20060102T150405Z"))
	if c := do(late); c != 403 {
		t.Errorf("date changed after signing: %d", c)
	}
}

// W against the worst usage Bedrock can report: the fake maximises usage
// within max_tokens and the context window, all of it priced as the dearest
// cache write, and the budget is never exceeded.
func TestBedrockBudgetNeverExceeded(t *testing.T) {
	const limit = 0.03
	for _, stream := range []bool{false, true} {
		e, up, _, _ := newBedrockEnv(t, "")
		_, keyA := e.newKey("etabA")
		// 1000-token window: output M, input the rest, all of it written to
		// the 1-hour cache (the dearest price) — the usage fields carry no
		// split, so it is priced as 5-minute writes: still ≤ W.
		up.usage = func(m int64) bedrockUsage { return bedrockUsage{Out: m, Write: 1000 - m} }
		e.setBudget(fmt.Sprintf(`{"kind":"org","name":"etabA","period":"day","max_cost":%v,"unit":"USD"}`, limit))
		ok, refused := 0, 0
		for i := 0; i < 40; i++ {
			m := 1 + (i*37)%200
			resp := e.do("POST", "/v1/messages", keyA, bdrkMsg(stream, m))
			readAll(t, resp)
			switch resp.StatusCode {
			case 200:
				ok++
			case 429:
				refused++
			default:
				t.Fatalf("status %d", resp.StatusCode)
			}
			e.waitRecords(ok)
			var spent float64
			for _, r := range e.st.Records() {
				spent += r.Cost
			}
			if spent > limit+1e-12 {
				t.Fatalf("stream=%v request %d: spent %v > budget %v", stream, i, spent, limit)
			}
		}
		if ok == 0 || refused == 0 {
			t.Errorf("stream=%v: %d served, %d refused: the budget must serve some and stop the rest", stream, ok, refused)
		}
		m := readAll(t, e.do("GET", "/metrics", "", ""))
		for _, line := range strings.Split(m, "\n") {
			if strings.HasPrefix(line, "llmflow_budget_hold_exceeded_total") {
				if f := strings.Fields(line); f[len(f)-1] != "0" {
					t.Errorf("a request cost more than its W: %s", line)
				}
			}
		}
	}
}

// Two organisations sharing one credential set share one prompt cache, as far
// as the documentation says: refused unless cache_isolation = "none".
func TestBedrockCacheIsolation(t *testing.T) {
	hcl := func(top, orgB string) string {
		return strings.Replace(bedrockHCL("http://127.0.0.1:1", "http://127.0.0.1:2", ""),
			`  org_credentials "etabB" {
    bearer_token_file = "/aws/beta-bearer"
  }`, orgB, 1) + top
	}
	share := `  org_credentials "etabB" {
    access_key_id_file     = "/aws/alpha-id"
    secret_access_key_file = "/aws/alpha-secret"
  }`
	if _, err := config.Parse([]byte(hcl("", share)), "x.hcl"); err == nil || !strings.Contains(err.Error(), "Shared AWS credentials") {
		t.Errorf("two organisations on one credential reference: %v", err)
	}
	if _, err := config.Parse([]byte(hcl("cache_isolation = \"none\"\n", share)), "x.hcl"); err != nil {
		t.Errorf("cache_isolation = none must accept it: %v", err)
	}
	// An organisation without credentials of its own, with a default: refused
	// while it shares the model with another.
	dflt := `  credentials {
    access_key_id_file     = "/aws/shared-id"
    secret_access_key_file = "/aws/shared-secret"
  }`
	if _, err := config.Parse([]byte(hcl("", dflt)), "x.hcl"); err == nil || !strings.Contains(err.Error(), "Shared AWS account") {
		t.Errorf("an organisation on the default credentials: %v", err)
	}
	if _, err := config.Parse([]byte(hcl("", "")), "x.hcl"); err == nil {
		t.Error("an organisation with no credentials at all was accepted")
	}
	// At run time two organisations whose files hold the same key are refused too.
	cfg, err := config.Parse([]byte(bedrockHCL("http://127.0.0.1:1", "http://127.0.0.1:2", "")), "x.hcl")
	if err != nil {
		t.Fatal(err)
	}
	keys := testKeys(cfg)
	keys["/aws/alpha-id"], keys["/aws/alpha-secret"], keys["/aws/alpha-token"] = akidAlpha, skAlpha, tokAlpha
	keys["/aws/beta-bearer"] = akidAlpha // the same value in another file
	if _, err := New(cfg, store.NewMemory(), Secrets{MasterKey: master, UpstreamKeys: keys, CacheSaltKey: testSaltKey}); err != nil {
		t.Errorf("a bearer and a SigV4 key are different credentials: %v", err)
	}
	cfg.CacheIsolation = "" // isolated
	cfg.ModelGroups[1].External.Bedrock.Orgs["etabB"] = cfg.ModelGroups[1].External.Bedrock.Orgs["etabA"]
	if _, err := New(cfg, store.NewMemory(), Secrets{MasterKey: master, UpstreamKeys: keys, CacheSaltKey: testSaltKey}); err == nil || !strings.Contains(err.Error(), "same AWS credentials") {
		t.Errorf("run time: %v", err)
	}
}

func TestBedrockConfig(t *testing.T) {
	cases := []struct{ name, extra, want string }{
		{"region", `region = "Paris"`, "Invalid region"},
		{"override http", `endpoint_override = "http://bedrock.example.com"`, "Invalid endpoint_override"},
		{"override userinfo", `endpoint_override = "https://u:p@example.com"`, "Invalid endpoint_override"},
		{"upstream block", `upstream "x" { base_url = "https://example.com" }`, "does not take upstream blocks"},
		{"anthropic versions", `anthropic_versions = ["2023-06-01"]`, "anthropic_versions"},
	}
	for _, c := range cases {
		hcl := strings.Replace(bedrockHCL("http://127.0.0.1:1", "http://127.0.0.1:2", c.extra), "region            = \"eu-west-3\"\n", "", 1)
		if c.name != "region" {
			hcl = strings.Replace(bedrockHCL("http://127.0.0.1:1", "http://127.0.0.1:2", c.extra), "endpoint_override = \"http://127.0.0.1:1\"", "", 1)
			if strings.HasPrefix(c.name, "override") {
				hcl = bedrockHCL("http://127.0.0.1:1", "http://127.0.0.1:2", c.extra)
				hcl = strings.Replace(hcl, `endpoint_override = "http://127.0.0.1:1"`, "", 1)
			}
		} else {
			hcl = strings.Replace(bedrockHCL("http://127.0.0.1:1", "http://127.0.0.1:2", ""), `region            = "eu-west-3"`, c.extra, 1)
		}
		if _, err := config.Parse([]byte(hcl), "x.hcl"); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// allow_external is mandatory, the credentials are required.
	hcl := strings.Replace(bedrockHCL("http://127.0.0.1:1", "http://127.0.0.1:2", ""), "allow_external    = true", "", 1)
	if _, err := config.Parse([]byte(hcl), "x.hcl"); err == nil || !strings.Contains(err.Error(), "allow_external") {
		t.Errorf("allow_external: %v", err)
	}
	if got := config.BedrockEndpoint("eu-west-3"); got != "https://bedrock-runtime.eu-west-3.amazonaws.com" {
		t.Errorf("endpoint %s", got)
	}
	if got := config.BedrockEndpoint("cn-north-1"); got != "https://bedrock-runtime.cn-north-1.amazonaws.com.cn" {
		t.Errorf("endpoint %s", got)
	}
	// The derived endpoint is https, never taken from a client.
	if ok := strings.HasPrefix(config.BedrockEndpoint("us-east-1"), "https://"); !ok {
		t.Error("not https")
	}
	_ = strconv.Itoa
}
