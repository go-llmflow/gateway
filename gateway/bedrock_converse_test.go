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
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/aws/eventstream"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/openai"
	"github.com/go-llmflow/gateway/store"
)

const converseModel = "eu.mistral.mistral-large-2407-v1:0"

// converseFrame is one ConverseStream event message: ":event-type" names the
// member (messageStart, contentBlockDelta, messageStop, metadata …) and the
// payload is that member's JSON (API_runtime_ConverseStream.html).
func converseFrame(typ, payload string) []byte {
	b, err := eventstream.Marshal(eventstream.Message{Headers: []eventstream.Header{
		{Name: ":event-type", Value: eventstream.String(typ)},
		{Name: ":content-type", Value: eventstream.String("application/json")},
		{Name: ":message-type", Value: eventstream.String("event")}}, Payload: []byte(payload)})
	if err != nil {
		panic(err)
	}
	return b
}

// converseEvents are the events of a golden answer: "hel" "lo", end_turn, and
// the usage in the metadata event with the cache writes split by lifetime.
func converseEvents(u bedrockUsage) [][2]string {
	return [][2]string{
		{"messageStart", `{"role":"assistant"}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"hel"}}`},
		{"contentBlockDelta", `{"contentBlockIndex":0,"delta":{"text":"lo"}}`},
		{"contentBlockStop", `{"contentBlockIndex":0}`},
		{"messageStop", `{"stopReason":"end_turn"}`},
		{"metadata", fmt.Sprintf(`{"usage":%s,"metrics":{"latencyMs":3}}`, converseUsageJSON(u))},
	}
}

// converseUsageJSON: inputTokens excludes the cache counts; the writes are
// split 3/5 five-minute and 2/5 one-hour (cacheDetails).
func converseUsageJSON(u bedrockUsage) string {
	w1h := u.Write * 2 / 5
	return fmt.Sprintf(`{"inputTokens":%d,"outputTokens":%d,"totalTokens":%d,"cacheReadInputTokens":%d,"cacheWriteInputTokens":%d,"cacheDetails":[{"inputTokens":%d,"ttl":"5m"},{"inputTokens":%d,"ttl":"1h"}]}`,
		u.In, u.Out, u.In+u.Out+u.Read+u.Write, u.Read, u.Write, u.Write-w1h, w1h)
}

func converseStreamBytes(u bedrockUsage) []byte {
	var b bytes.Buffer
	for _, e := range converseEvents(u) {
		b.Write(converseFrame(e[0], e[1]))
	}
	return b.Bytes()
}

// serveConverse answers /converse and /converse-stream like Bedrock, after the
// signature was verified by serve.
func (f *fakeBedrock) serveConverse(w http.ResponseWriter, r *http.Request, raw []byte, body map[string]any, mode string, usage func(int64) bedrockUsage, jsonErr func(int, string)) {
	stream := strings.HasSuffix(r.URL.Path, "/converse-stream")
	inf, _ := body["inferenceConfig"].(map[string]any)
	msgs, _ := body["messages"].([]any)
	if body["model"] != nil || body["stream"] != nil || len(msgs) == 0 || msgs[0].(map[string]any)["role"] != "user" {
		jsonErr(400, "malformed Converse body: "+string(raw))
		return
	}
	last := ""
	for _, m := range msgs {
		role := m.(map[string]any)["role"].(string)
		if role == last {
			jsonErr(400, "roles must alternate: "+string(raw))
			return
		}
		last = role
	}
	mt, _ := inf["maxTokens"].(float64)
	u := usage(int64(mt))
	if !stream {
		if mode == "no-split" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},"stopReason":"end_turn","usage":{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheWriteInputTokens":%d}}`, u.In, u.Out, u.Read, u.Write)
			return
		}
		if mode == "no-usage" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},"stopReason":"end_turn"}`)
			return
		}
		if mode == "cut-body" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "1000")
			fmt.Fprint(w, `{"output":{"message":`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"output":{"message":{"role":"assistant","content":[{"text":"hello"}]}},"stopReason":"end_turn","usage":%s,"metrics":{"latencyMs":5}}`, converseUsageJSON(u))
		return
	}
	w.Header().Set("Content-Type", bedrockEventStream)
	evs := converseEvents(u)
	frame := func(i int) []byte { return converseFrame(evs[i][0], evs[i][1]) }
	switch mode {
	case "cut-mid-message":
		w.Write(frame(0))
		w.Write(frame(1)[:20])
	case "cut-at-boundary": // everything but the metadata event
		for i := 0; i < len(evs)-1; i++ {
			w.Write(frame(i))
		}
	case "exception":
		w.Write(frame(0))
		w.Write(frame(1))
		w.Write(exceptionEvent("throttlingException"))
	case "crc":
		w.Write(frame(0))
		bad := frame(1)
		bad[len(bad)-1] ^= 0xff
		w.Write(bad)
		for i := 2; i < len(evs); i++ {
			w.Write(frame(i))
		}
	case "no-split": // usage without cacheDetails
		for i := 0; i < len(evs)-1; i++ {
			w.Write(frame(i))
		}
		w.Write(converseFrame("metadata", fmt.Sprintf(`{"usage":{"inputTokens":%d,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheWriteInputTokens":%d}}`, u.In, u.Out, u.Read, u.Write)))
	case "no-usage": // a metadata event without usage
		for i := 0; i < len(evs)-1; i++ {
			w.Write(frame(i))
		}
		w.Write(converseFrame("metadata", `{"metrics":{"latencyMs":3}}`))
	default:
		for i := range evs {
			w.Write(frame(i))
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
		}
	}
}

// The Converse model: a Mistral model next to the Claude one, same pool, same
// prices, the same organisations with their own credentials.
func converseModelHCL(extra string) string {
	return `
model "mistral" {
  provider          = "bedrock"
  region            = "eu-west-3"
  model_id          = "` + converseModel + `"
  endpoint_override = "ENDPOINT"
  pool              = "bedrock"
  allow_external    = true
  orgs              = ["etabA", "etabB"]
  context_window    = 1000
  cost {
    unit                    = "USD"
    price_date              = "2026-11-01"
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
`
}

func newConverseEnv(t *testing.T, extra string) (*env, *fakeBedrock, *clock, *syncBuf) {
	t.Helper()
	up := newFakeBedrock(t)
	vllm := newFakeVLLM(t)
	src := bedrockHCL(up.srv.URL, vllm.URL(), "") + strings.Replace(converseModelHCL(extra), "ENDPOINT", up.srv.URL, 1)
	cfg, err := config.Parse([]byte(src), "converse.hcl")
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

func chatMsg(stream bool, max int, extra string) string {
	return fmt.Sprintf(`{"model":"mistral","max_tokens":%d,"stream":%v,%s"priority":-5,"cache_salt":"mine",`+
		`"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"mail jean.dupont@example.fr token %s"},{"role":"user","content":"and more"}]}`,
		max, stream, extra, ghToken)
}

// What reaches Bedrock, and what comes back: SigV4 for the organisation, the
// Converse body (system apart, consecutive user messages merged, maxTokens),
// PII and secrets masked, nothing of the client's engine fields; the answer as
// an OpenAI chat completion, its usage read from the Converse fields with the
// cache writes priced by lifetime: (10×2 + 100×0.1 + 30×2.5 + 20×4 + 5×10) /
// 1e6 = 235e-6 USD.
func TestConverseGolden(t *testing.T) {
	e, up, _, _ := newConverseEnv(t, "")
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	resp := e.do("POST", "/v1/chat/completions", keyA, chatMsg(false, 16, ""), "X-Api-Key", "client", "X-Vllm-Priority", "-100")
	body := readAll(t, resp)
	if resp.StatusCode != 200 {
		t.Fatalf("chat: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Object  string
		Model   string
		Choices []struct {
			Message      struct{ Role, Content string }
			FinishReason string `json:"finish_reason"`
		}
		Usage struct {
			Prompt, Completion, Total int64
			Details                   struct{ Cached int64 } `json:"prompt_tokens_details"`
		}
	}
	// the usage names are OpenAI's
	var raw map[string]any
	_ = json.Unmarshal([]byte(body), &raw)
	_ = json.Unmarshal([]byte(body), &out)
	u, _ := raw["usage"].(map[string]any)
	d, _ := u["prompt_tokens_details"].(map[string]any)
	if out.Object != "chat.completion" || out.Model != "mistral" || len(out.Choices) != 1 || out.Choices[0].Message.Content != "hello" ||
		out.Choices[0].Message.Role != "assistant" || out.Choices[0].FinishReason != "stop" ||
		u["prompt_tokens"] != 160.0 || u["completion_tokens"] != 5.0 || u["total_tokens"] != 165.0 || d["cached_tokens"] != 100.0 {
		t.Fatalf("answer: %s", body)
	}
	got := up.last(t)
	if !got.SigOK || got.Who != akidAlpha || got.RawPath != "/model/eu.mistral.mistral-large-2407-v1%3A0/converse" {
		t.Fatalf("who %q ok %v path %q", got.Who, got.SigOK, got.RawPath)
	}
	for _, k := range []string{"X-Api-Key", "X-Vllm-Priority", "Anthropic-Version", "X-Request-Id"} {
		if v := got.Header.Get(k); v != "" {
			t.Errorf("%s reached Bedrock: %q", k, v)
		}
	}
	for _, f := range []string{"model", "stream", "priority", "cache_salt", "max_tokens"} {
		if _, ok := got.Body[f]; ok {
			t.Errorf("field %s reached Bedrock", f)
		}
	}
	if sys, _ := json.Marshal(got.Body["system"]); string(sys) != `[{"text":"be brief"}]` {
		t.Errorf("system %s", sys)
	}
	msgs, _ := got.Body["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages %v: the two user messages must merge", got.Body["messages"])
	}
	m0, _ := json.Marshal(msgs[0])
	if strings.Contains(string(m0), "jean.dupont@example.fr") || strings.Contains(string(m0), ghToken) || !strings.Contains(string(m0), "and more") {
		t.Errorf("message %s", m0)
	}
	if inf, _ := json.Marshal(got.Body["inferenceConfig"]); string(inf) != `{"maxTokens":16}` {
		t.Errorf("inferenceConfig %s", inf)
	}
	// An organisation with an API key: bearer, unsigned.
	if resp := e.do("POST", "/v1/chat/completions", keyB, chatMsg(false, 16, "")); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("etabB: %d", resp.StatusCode)
	}
	if g := up.last(t); g.Who != "etabB" || g.Header.Get("Authorization") != "Bearer "+bearerBeta {
		t.Errorf("etabB: who %q", g.Who)
	}
	for _, r := range recordsOf(e) {
		if r.Model != "mistral" || r.Unit != "USD" || math.Abs(r.Cost-235e-6) > 1e-12 || r.PromptTokens != 160 || r.CachedTokens != 100 || r.CompletionTokens != 5 || r.Estimated {
			t.Errorf("record %+v, want USD 235e-6 with the cache writes by lifetime", r)
		}
	}
}

// The stream, as OpenAI chunks: role, two content deltas, the finish, the
// usage when the client asked for it, [DONE].
func TestConverseStreamGoldenEndToEnd(t *testing.T) {
	e, up, _, _ := newConverseEnv(t, "")
	_, keyA := e.newKey("etabA")
	for _, withUsage := range []bool{false, true} {
		extra := ""
		if withUsage {
			extra = `"stream_options":{"include_usage":true},`
		}
		resp := e.do("POST", "/v1/chat/completions", keyA, chatMsg(true, 16, extra))
		sse := readAll(t, resp)
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("stream: %d %v", resp.StatusCode, resp.Header)
		}
		if p := up.last(t).RawPath; !strings.HasSuffix(p, "/converse-stream") || up.last(t).Header.Get("Accept") != bedrockEventStream {
			t.Errorf("request %q accept %q", p, up.last(t).Header.Get("Accept"))
		}
		var text strings.Builder
		var finish string
		var usage map[string]any
		done := false
		for _, blk := range strings.Split(strings.TrimSpace(sse), "\n\n") {
			data, ok := strings.CutPrefix(blk, "data: ")
			if !ok {
				t.Fatalf("block %q", blk)
			}
			if data == "[DONE]" {
				done = true
				continue
			}
			var ch struct {
				Choices []struct {
					Delta        struct{ Content string }
					FinishReason *string `json:"finish_reason"`
				}
				Usage map[string]any
			}
			if err := json.Unmarshal([]byte(data), &ch); err != nil {
				t.Fatalf("%q: %v", data, err)
			}
			for _, c := range ch.Choices {
				text.WriteString(c.Delta.Content)
				if c.FinishReason != nil {
					finish = *c.FinishReason
				}
			}
			if ch.Usage != nil {
				usage = ch.Usage
			}
		}
		if text.String() != "hello" || finish != "stop" || !done {
			t.Errorf("usage=%v: text %q finish %q done %v:\n%s", withUsage, text.String(), finish, done, sse)
		}
		if withUsage != (usage != nil) || usage != nil && (usage["prompt_tokens"] != 160.0 || usage["completion_tokens"] != 5.0) {
			t.Errorf("usage=%v: usage chunk %v", withUsage, usage)
		}
	}
	for _, r := range recordsOf(e) {
		if math.Abs(r.Cost-235e-6) > 1e-12 || r.Estimated || r.PromptTokens != 160 {
			t.Errorf("record %+v", r)
		}
	}
}

// The byte-exact conversion of the events.
func TestConverseStreamGolden(t *testing.T) {
	c := newConverseSSE(bytes.NewReader(converseStreamBytes(bedrockUsage{10, 5, 100, 50})), "mistral", "x", time.Unix(1700000000, 0), true)
	got := readAllSSE(t, c)
	chunk := func(delta, finish string) string {
		return `data: {"choices":[{"delta":` + delta + `,"finish_reason":` + finish + `,"index":0}],"created":1700000000,"id":"chatcmpl-x","model":"mistral","object":"chat.completion.chunk"}` + "\n\n"
	}
	want := chunk(`{"content":"","role":"assistant"}`, "null") + chunk(`{"content":"hel"}`, "null") + chunk(`{"content":"lo"}`, "null") +
		chunk(`{}`, `"stop"`) +
		`data: {"choices":[],"created":1700000000,"id":"chatcmpl-x","model":"mistral","object":"chat.completion.chunk","usage":{"prompt_tokens":160,"completion_tokens":5,"total_tokens":165,"prompt_tokens_details":{"cached_tokens":100}}}` + "\n\n" +
		"data: [DONE]\n\n"
	if got != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
	if !c.Complete() {
		t.Error("a whole stream is not complete")
	}
	if u, ok := c.Usage(); !ok || u.CacheWrite5m != 30 || u.CacheWrite1h != 20 || u.Cached() != 100 || u.PromptTokens != 160 {
		t.Errorf("usage %+v %v", u, ok)
	}
}

// translateConverse: usage mapping tables from the documented shape.
func TestConverseUsageMapping(t *testing.T) {
	cases := []struct {
		name, json                 string
		ok                         bool
		prompt, completion, cached int64
		write5m, write1h           int64
		finish                     string
	}{
		{"plain", `{"output":{"message":{"role":"assistant","content":[{"text":"a"},{"text":"b"}]}},"stopReason":"end_turn","usage":{"inputTokens":30,"outputTokens":628,"totalTokens":658}}`, true, 30, 628, 0, 0, 0, "stop"},
		{"cache", `{"output":{"message":{"content":[{"text":"x"}]}},"stopReason":"max_tokens","usage":{"inputTokens":10,"outputTokens":5,"totalTokens":165,"cacheReadInputTokens":100,"cacheWriteInputTokens":50}}`, true, 160, 5, 100, 50, 0, "length"},
		{"split", `{"output":{"message":{"content":[]}},"stopReason":"guardrail_intervened","usage":{"inputTokens":0,"outputTokens":1,"cacheWriteInputTokens":50,"cacheDetails":[{"inputTokens":30,"ttl":"5m"},{"inputTokens":20,"ttl":"1h"}]}}`, true, 50, 1, 0, 30, 20, "content_filter"},
		{"split that does not add up", `{"output":{"message":{"content":[]}},"usage":{"inputTokens":0,"outputTokens":1,"cacheWriteInputTokens":50,"cacheDetails":[{"inputTokens":10,"ttl":"1h"}]}}`, true, 50, 1, 0, 50, 0, "stop"},
		{"window", `{"output":{"message":{"content":[]}},"stopReason":"model_context_window_exceeded","usage":{"inputTokens":1,"outputTokens":1}}`, true, 1, 1, 0, 0, 0, "length"},
		{"no usage", `{"output":{"message":{"content":[]}},"stopReason":"end_turn"}`, false, 0, 0, 0, 0, 0, ""},
		{"negative", `{"usage":{"inputTokens":-1,"outputTokens":1}}`, false, 0, 0, 0, 0, 0, ""},
		{"missing output tokens", `{"usage":{"inputTokens":1}}`, false, 0, 0, 0, 0, 0, ""},
		{"not json", `x`, false, 0, 0, 0, 0, 0, ""},
	}
	for _, c := range cases {
		body, u, ok := translateConverse([]byte(c.json), "m", "id", time.Unix(5, 0))
		if ok != c.ok {
			t.Errorf("%s: ok=%v", c.name, ok)
			continue
		}
		if !ok {
			continue
		}
		if u.PromptTokens != c.prompt || u.CompletionTokens != c.completion || u.Cached() != c.cached || u.CacheWrite5m+u.CacheWriteUnknown != c.write5m || u.CacheWrite1h != c.write1h {
			t.Errorf("%s: %+v", c.name, u)
		}
		var o struct {
			Choices []struct {
				FinishReason string `json:"finish_reason"`
			}
		}
		_ = json.Unmarshal(body, &o)
		if o.Choices[0].FinishReason != c.finish {
			t.Errorf("%s: finish %q, want %q", c.name, o.Choices[0].FinishReason, c.finish)
		}
	}
}

// The request mapping, case by case.
func TestConverseRequestMapping(t *testing.T) {
	png := base64.StdEncoding.EncodeToString([]byte("\x89PNG-bytes"))
	cases := []struct{ name, in, want string }{
		{"text", `{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":9}`,
			`{"inferenceConfig":{"maxTokens":9},"messages":[{"content":[{"text":"hi"}],"role":"user"}]}`},
		{"max_completion_tokens wins", `{"messages":[{"role":"user","content":"hi"}],"max_tokens":9,"max_completion_tokens":7}`,
			`{"inferenceConfig":{"maxTokens":7},"messages":[{"content":[{"text":"hi"}],"role":"user"}]}`},
		{"system and developer, alternation", `{"messages":[{"role":"system","content":"s1"},{"role":"developer","content":"s2"},{"role":"user","content":"u"},{"role":"assistant","content":"a"},{"role":"user","content":"u2"}]}`,
			`{"messages":[{"content":[{"text":"u"}],"role":"user"},{"content":[{"text":"a"}],"role":"assistant"},{"content":[{"text":"u2"}],"role":"user"}],"system":[{"text":"s1"},{"text":"s2"}]}`},
		{"parts and image", `{"messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"data:image/jpg;base64,` + png + `"}}]}]}`,
			`{"messages":[{"content":[{"text":"look"},{"image":{"format":"jpeg","source":{"bytes":"` + png + `"}}}],"role":"user"}]}`},
		{"sampling and stop", `{"messages":[{"role":"user","content":"x"}],"temperature":0.5,"top_p":0.9,"stop":["a","b"]}`,
			`{"inferenceConfig":{"stopSequences":["a","b"],"temperature":0.5,"topP":0.9},"messages":[{"content":[{"text":"x"}],"role":"user"}]}`},
		{"stop string", `{"messages":[{"role":"user","content":"x"}],"stop":"END"}`,
			`{"inferenceConfig":{"stopSequences":["END"]},"messages":[{"content":[{"text":"x"}],"role":"user"}]}`},
	}
	for _, c := range cases {
		out, err := converseRequest([]byte(c.in))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		var a, b any
		_ = json.Unmarshal(out, &a)
		_ = json.Unmarshal([]byte(c.want), &b)
		if fmt.Sprint(a) != fmt.Sprint(b) {
			t.Errorf("%s:\n got %s\nwant %s", c.name, out, c.want)
		}
	}
	bad := map[string]string{
		"tools":           `{"messages":[{"role":"user","content":"x"}],"tools":[{"type":"function"}]}`,
		"tool_calls":      `{"messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"1"}]},{"role":"user","content":"x"}]}`,
		"tool role":       `{"messages":[{"role":"user","content":"x"},{"role":"tool","tool_call_id":"1","content":"r"}]}`,
		"remote image":    `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"https://example.com/a.png"}}]}]}`,
		"bad image":       `{"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/bmp;base64,AAAA"}}]}]}`,
		"audio part":      `{"messages":[{"role":"user","content":[{"type":"input_audio"}]}]}`,
		"response_format": `{"messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_object"}}`,
		"seed":            `{"messages":[{"role":"user","content":"x"}],"seed":1}`,
		"empty":           `{"messages":[]}`,
		"no user":         `{"messages":[{"role":"system","content":"s"}]}`,
		"role":            `{"messages":[{"role":"function","content":"x"}]}`,
		"max_tokens 0":    `{"messages":[{"role":"user","content":"x"}],"max_tokens":0}`,
		"null body":       `null`,
		"array body":      `[]`,
		"guardrailConfig": `{"messages":[{"role":"user","content":"x"}],"guardrailConfig":{"guardrailIdentifier":"g"}}`,
	}
	for name, in := range bad {
		if _, err := converseRequest([]byte(in)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The refusals of #89 for Converse, each before anything leaves the site.
func TestConverseRefusals(t *testing.T) {
	e, up, _, _ := newConverseEnv(t, "")
	_, keyA := e.newKey("etabA")
	e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":1,"unit":"USD"}`)
	msg := func(extra string) string {
		return `{"model":"mistral","max_tokens":16,` + extra + `"messages":[{"role":"user","content":"hi"}]}`
	}
	cases := []struct {
		name, body string
		hdr        []string
		want       string
	}{
		{"guardrailConfig", msg(`"guardrailConfig":{"guardrailIdentifier":"g","guardrailVersion":"1","trace":"enabled"},`), nil, "guardrailConfig"},
		{"guardrail header", msg(``), []string{"X-Amzn-Bedrock-GuardrailIdentifier", "abc"}, "X-Amzn-Bedrock"},
		{"serviceTier", msg(`"serviceTier":{"type":"priority"},`), nil, "serviceTier"},
		{"service tier header", msg(``), []string{"X-Amzn-Bedrock-Service-Tier", "priority"}, "X-Amzn-Bedrock"},
		{"performanceConfig", msg(`"performanceConfig":{"latency":"optimized"},`), nil, "performanceConfig"},
		{"promptVariables", msg(`"promptVariables":{"x":{"text":"y"}},`), nil, "promptVariables"},
		{"additionalModelRequestFields", msg(`"additionalModelRequestFields":{"top_k":200},`), nil, "additionalModelRequestFields"},
		{"requestMetadata", msg(`"requestMetadata":{"a":"b"},`), nil, "requestMetadata"},
		{"outputConfig", msg(`"outputConfig":{"textFormat":{"type":"json_schema"}},`), nil, "outputConfig"},
		{"toolConfig", msg(`"toolConfig":{"tools":[]},`), nil, "toolConfig"},
		{"inferenceConfig (Converse's own)", msg(`"inferenceConfig":{"maxTokens":99999},`), nil, "inferenceConfig"},
		{"tools", msg(`"tools":[{"type":"function","function":{"name":"f"}}],`), nil, "cannot be mapped"},
		{"n above 1", msg(`"n":3,`), nil, `must be 1`},
		{"response_format", msg(`"response_format":{"type":"json_object"},`), nil, "cannot be mapped"},
	}
	n := up.count()
	for _, c := range cases {
		resp := e.do("POST", "/v1/chat/completions", keyA, c.body, c.hdr...)
		out := readAll(t, resp)
		if resp.StatusCode != 400 || !strings.Contains(out, c.want) || up.count() != n {
			t.Errorf("%s: %d %s (provider calls %d → %d)", c.name, resp.StatusCode, out, n, up.count())
		}
	}
	// A tool message and a remote image come through the messages.
	for name, b := range map[string]string{
		"tool message": `{"model":"mistral","max_tokens":16,"messages":[{"role":"user","content":"hi"},{"role":"tool","tool_call_id":"1","content":"r"}]}`,
		"remote image": `{"model":"mistral","max_tokens":16,"messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"http://169.254.169.254/x.png"}}]}]}`,
	} {
		resp := e.do("POST", "/v1/chat/completions", keyA, b)
		if out := readAll(t, resp); resp.StatusCode != 400 || up.count() != n {
			t.Errorf("%s: %d %s", name, resp.StatusCode, out)
		}
	}
	// The Anthropic route is not this model's.
	resp := e.do("POST", "/v1/messages", keyA, `{"model":"mistral","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if out := readAll(t, resp); resp.StatusCode < 400 || up.count() != n {
		t.Errorf("/v1/messages on a Converse model: %d %s", resp.StatusCode, out)
	}
	// And the Converse route is not the Claude model's.
	resp = e.do("POST", "/v1/chat/completions", keyA, `{"model":"claude","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	if out := readAll(t, resp); resp.StatusCode < 400 || up.count() != n {
		t.Errorf("/v1/chat/completions on an InvokeModel model: %d %s", resp.StatusCode, out)
	}
}

// Without a context window a budgeted request cannot be bounded: refused.
func TestConverseNeedsContextWindowUnderBudget(t *testing.T) {
	up := newFakeBedrock(t)
	vllm := newFakeVLLM(t)
	m := strings.Replace(converseModelHCL(""), "  context_window    = 1000\n", "", 1)
	src := bedrockHCL(up.srv.URL, vllm.URL(), "") + strings.Replace(m, "ENDPOINT", up.srv.URL, 1)
	cfg, err := config.Parse([]byte(src), "x.hcl")
	if err != nil {
		t.Fatal(err)
	}
	keys := testKeys(cfg)
	keys["/aws/alpha-id"], keys["/aws/alpha-secret"], keys["/aws/alpha-token"], keys["/aws/beta-bearer"] = akidAlpha, skAlpha, tokAlpha, bearerBeta
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: keys, CacheSaltKey: testSaltKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	e := &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	t.Cleanup(func() { gw.Close(); srv.Close(context.Background()) })
	_, keyA := e.newKey("etabA")
	e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":1,"unit":"USD"}`)
	resp := e.do("POST", "/v1/chat/completions", keyA, chatMsg(false, 16, ""))
	if out := readAll(t, resp); resp.StatusCode != 503 || !strings.Contains(out, "context length") || up.count() != 0 {
		t.Fatalf("%d %s (provider calls %d)", resp.StatusCode, out, up.count())
	}
}

// The model IDs Converse refuses (check-config), and accepts.
func TestConverseModelIDs(t *testing.T) {
	good := []string{
		"eu.mistral.mistral-large-2407-v1:0", "meta.llama3-1-70b-instruct-v1:0", "us.amazon.nova-pro-v1:0", "eu.anthropic.claude-sonnet-4-5-20250929-v1:0",
		"arn:aws:bedrock:eu-west-3::foundation-model/mistral.mistral-large-2407-v1:0",
		"arn:aws:bedrock:eu-west-3:123456789012:inference-profile/eu.amazon.nova-pro-v1:0",
	}
	for _, id := range good {
		if err := config.CheckBedrockModelIDFor(id, "eu-west-3", config.APIConverse); err != nil {
			t.Errorf("%s refused: %v", id, err)
		}
	}
	bad := []struct{ id, why string }{
		{"arn:aws:bedrock:eu-west-3:123456789012:default-prompt-router/meta.llama:1", "prompt router"},
		{"arn:aws:bedrock:eu-west-3:123456789012:prompt-router/abc", "prompt router"},
		{"arn:aws:bedrock:eu-west-3:123456789012:provisioned-model/abcdefghijkl", "Provisioned Throughput"},
		{"arn:aws:bedrock:eu-west-3:123456789012:custom-model-deployment/abcdefghijkl", "custom"},
		{"arn:aws:bedrock:eu-west-3:123456789012:imported-model/abcdefghijkl", "imported"},
		{"arn:aws:sagemaker:eu-west-3:123456789012:endpoint/my-endpoint", "Bedrock resource"},
		{"arn:aws:bedrock:eu-west-3:123456789012:prompt/ABCDEFGHIJ:1", "Prompt management"},
		{"arn:aws:bedrock:us-east-1::foundation-model/mistral.m:0", "region"},
		{"mistral", "<vendor>"}, {"", ""}, {"a b.c", ""},
	}
	for _, c := range bad {
		err := config.CheckBedrockModelIDFor(c.id, "eu-west-3", config.APIConverse)
		if err == nil || !strings.Contains(err.Error(), c.why) {
			t.Errorf("%q: %v, want a refusal mentioning %q", c.id, err, c.why)
		}
	}
	// InvokeModel still takes Anthropic only, and says how to get the others.
	if err := config.CheckBedrockModelIDFor("meta.llama3-1-70b-instruct-v1:0", "eu-west-3", config.APIInvoke); err == nil || !strings.Contains(err.Error(), "converse") {
		t.Errorf("invoke with a Llama ID: %v", err)
	}
}

// api is inferred from the ID, explicit when given, and the options of each
// are kept apart.
func TestConverseConfig(t *testing.T) {
	base := bedrockHCL("http://127.0.0.1:1", "http://127.0.0.1:2", "")
	parse := func(model string) (*config.Config, error) {
		return config.Parse([]byte(base+strings.Replace(model, "ENDPOINT", "http://127.0.0.1:1", 1)), "x.hcl")
	}
	c, err := parse(converseModelHCL(""))
	if err != nil {
		t.Fatal(err)
	}
	var api string
	for _, g := range c.ModelGroups {
		if g.Name == "mistral" {
			api = g.External.Bedrock.API
		}
		if g.Name == "claude" && g.External.Bedrock.API != config.APIInvoke {
			t.Errorf("claude: api %q", g.External.Bedrock.API)
		}
	}
	if api != config.APIConverse {
		t.Errorf("mistral: api %q", api)
	}
	bad := map[string]string{
		"anthropic_betas on converse": strings.Replace(converseModelHCL(""), "context_window", "anthropic_betas = [\"context-management-2025-06-27\"]\n  context_window", 1),
		"api = nonsense":              converseModelHCL(`api = "chat"`),
		"invoke with a Mistral ID":    converseModelHCL(`api = "invoke"`),
		"prompt router":               strings.Replace(converseModelHCL(""), converseModel, "arn:aws:bedrock:eu-west-3:123456789012:default-prompt-router/meta.llama:1", 1),
		"provisioned":                 strings.Replace(converseModelHCL(""), converseModel, "arn:aws:bedrock:eu-west-3:123456789012:provisioned-model/abcdefghijkl", 1),
		"sagemaker":                   strings.Replace(converseModelHCL(""), converseModel, "arn:aws:sagemaker:eu-west-3:123456789012:endpoint/x", 1),
		"application profile":         strings.Replace(converseModelHCL(""), converseModel, "arn:aws:bedrock:eu-west-3:123456789012:application-inference-profile/abcd1234efgh", 1),
	}
	for name, m := range bad {
		if _, err := parse(m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// An application inference profile names no vendor: api says which.
	ok := strings.Replace(converseModelHCL(`api = "converse"`), converseModel, "arn:aws:bedrock:eu-west-3:123456789012:application-inference-profile/abcd1234efgh", 1)
	if _, err := parse(ok); err != nil {
		t.Errorf("application profile with api: %v", err)
	}
}

// Chaos: a Converse answer that did not deliver its usage is charged W =
// (1000 × 4 + 16 × 10) / 1e6, the worst case held before the request left.
func TestConverseChaosChargesW(t *testing.T) {
	const w = 0.00416
	for _, mode := range []string{"cut-mid-message", "cut-at-boundary", "exception", "crc", "no-usage"} {
		t.Run(mode, func(t *testing.T) {
			e, up, _, _ := newConverseEnv(t, "")
			_, keyA := e.newKey("etabA")
			e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":1,"unit":"USD"}`)
			up.set(mode, 0, "")
			resp := e.do("POST", "/v1/chat/completions", keyA, chatMsg(true, 16, ""))
			sse := readAll(t, resp)
			// A failed stream has no [DONE]: an OpenAI client reads that as a failure.
			if strings.Contains(sse, "[DONE]") {
				t.Errorf("a failed stream ends with [DONE]:\n%s", sse)
			}
			recs := recordsOf(e)
			if len(recs) != 1 || math.Abs(recs[0].Cost-w) > 1e-12 || !recs[0].Estimated {
				t.Fatalf("%s: records %+v, want one at W = %v", mode, recs, w)
			}
		})
	}
	for _, mode := range []string{"no-usage", "cut-body"} {
		e, up, _, _ := newConverseEnv(t, "")
		_, keyA := e.newKey("etabA")
		e.setBudget(`{"kind":"org","name":"etabA","period":"day","max_cost":1,"unit":"USD"}`)
		up.set(mode, 0, "")
		readAll(t, e.do("POST", "/v1/chat/completions", keyA, chatMsg(false, 16, "")))
		recs := recordsOf(e)
		if len(recs) != 1 || math.Abs(recs[0].Cost-w) > 1e-12 || !recs[0].Estimated {
			t.Errorf("non-stream %s: records %+v, want one at W = %v", mode, recs, w)
		}
	}
}

// W against the worst usage a Converse model can report: the fake maximises
// usage within maxTokens and the context window, the rest written to a cache,
// and the budget is never exceeded.
func TestConverseBudgetNeverExceeded(t *testing.T) {
	const limit = 0.03
	for _, stream := range []bool{false, true} {
		e, up, _, _ := newConverseEnv(t, "")
		_, keyA := e.newKey("etabA")
		up.usage = func(m int64) bedrockUsage { return bedrockUsage{Out: m, Write: 1000 - m} }
		e.setBudget(fmt.Sprintf(`{"kind":"org","name":"etabA","period":"day","max_cost":%v,"unit":"USD"}`, limit))
		ok, refused := 0, 0
		for i := 0; i < 40; i++ {
			m := 1 + (i*37)%200
			resp := e.do("POST", "/v1/chat/completions", keyA, chatMsg(stream, m, ""))
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
			t.Errorf("stream=%v: %d served, %d refused", stream, ok, refused)
		}
		for _, line := range strings.Split(readAll(t, e.do("GET", "/metrics", "", "")), "\n") {
			if strings.HasPrefix(line, "llmflow_budget_hold_exceeded_total") {
				if f := strings.Fields(line); f[len(f)-1] != "0" {
					t.Errorf("a request cost more than its W: %s", line)
				}
			}
		}
	}
}

// The secrets never reach an output on the Converse path either.
func TestConverseSecretsStayInside(t *testing.T) {
	e, up, _, logs := newConverseEnv(t, "")
	_, keyA := e.newKey("etabA")
	_, keyB := e.newKey("etabB")
	var seen strings.Builder
	use := func(resp *http.Response) { seen.WriteString(readAll(t, resp)); fmt.Fprint(&seen, resp.Header) }
	for _, k := range []string{keyA, keyB} {
		use(e.do("POST", "/v1/chat/completions", k, chatMsg(false, 16, "")))
		use(e.do("POST", "/v1/chat/completions", k, chatMsg(true, 16, "")))
	}
	up.set("", 400, "bad request mentioning "+skAlpha+" and "+bearerBeta+" and "+tokAlpha)
	use(e.do("POST", "/v1/chat/completions", keyA, chatMsg(false, 16, "")))
	use(e.do("POST", "/v1/chat/completions", keyB, chatMsg(true, 16, "")))
	up.set("", 403, "forbidden "+skAlpha)
	use(e.do("POST", "/v1/chat/completions", keyA, chatMsg(false, 16, "")))
	up.set("crc", 0, "")
	use(e.do("POST", "/v1/chat/completions", keyA, chatMsg(true, 16, "")))
	use(e.do("GET", "/metrics", "", ""))
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
	if !strings.Contains(seen.String()+skAlpha, skAlpha) {
		t.Fatal("the scan cannot see a planted secret")
	}
}

// FuzzConverseMapping: whatever a client sends, a body that passes the ingress
// and the mapping is a Converse body with no refused option, no model and no
// stream, only text and image blocks, alternating roles and a positive
// maxTokens when one was given.
func FuzzConverseMapping(f *testing.F) {
	for _, s := range []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":9}`,
		`{"messages":[{"role":"system","content":"s"},{"role":"user","content":[{"type":"text","text":"a"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`,
		`{"messages":[{"role":"user","content":"x"}],"guardrailConfig":{}}`,
		`{"messages":[{"role":"user","content":"x"}],"tools":[{}]}`,
		`{"messages":[{"role":"assistant","content":"a"},{"role":"user","content":"u"}],"stop":["a"],"temperature":0.2}`,
		`null`, `[]`, `{`, `{"messages":null}`, `{"messages":[{"role":"user","content":null}]}`,
		`{"messages":[{"role":"user","content":"x"}],"max_tokens":-1}`, `{"messages":[{"role":"user","content":"x"}],"stop":[1]}`,
	} {
		f.Add([]byte(s), "Content-Type", "x")
	}
	f.Add([]byte(`{"messages":[{"role":"user","content":"x"}]}`), "X-Amzn-Bedrock-Service-Tier", "priority")
	f.Fuzz(func(t *testing.T, body []byte, hk, hv string) {
		h := http.Header{}
		if hk != "" && !strings.ContainsAny(hk, " \t\r\n:") {
			h[hk] = []string{hv}
		}
		in, err := converseIngress(h, body)
		if err != nil {
			return
		}
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "x-amzn-bedrock-") {
				t.Fatalf("header %q accepted", k)
			}
		}
		out, err := converseRequest(in)
		if err != nil {
			return
		}
		var m map[string]json.RawMessage
		if json.Unmarshal(out, &m) != nil {
			t.Fatalf("not an object: %q", out)
		}
		for k := range m {
			if k != "messages" && k != "system" && k != "inferenceConfig" {
				t.Fatalf("field %q in a Converse body: %s", k, out)
			}
		}
		for _, f := range append([]string{"model", "stream"}, bedrockRefusedFields...) {
			if _, ok := m[f]; ok {
				t.Fatalf("field %s survived: %s", f, out)
			}
		}
		var msgs []struct {
			Role    string
			Content []map[string]json.RawMessage
		}
		if json.Unmarshal(m["messages"], &msgs) != nil || len(msgs) == 0 || msgs[0].Role != "user" {
			t.Fatalf("messages: %s", out)
		}
		for i, msg := range msgs {
			if i > 0 && msg.Role == msgs[i-1].Role || len(msg.Content) == 0 {
				t.Fatalf("roles or empty content: %s", out)
			}
			for _, blk := range msg.Content {
				_, text := blk["text"]
				_, img := blk["image"]
				if len(blk) != 1 || !text && !img {
					t.Fatalf("a block that is not text or image: %s", out)
				}
			}
		}
		var inf struct{ MaxTokens *int64 }
		if raw, ok := m["inferenceConfig"]; ok {
			if json.Unmarshal(raw, &inf) != nil || inf.MaxTokens != nil && *inf.MaxTokens < 1 {
				t.Fatalf("inferenceConfig: %s", raw)
			}
		}
	})
}

// FuzzConverseStream feeds random frames through the ConverseStream path: it
// must never panic, always end, always emit well-formed SSE, and never charge
// less than the usage a complete stream delivered (cache writes by lifetime
// included) nor less than W for an incomplete one.
func FuzzConverseStream(f *testing.F) {
	f.Add([]byte{}, uint8(0), uint32(10), uint32(5), uint32(100), uint32(50))
	f.Add(converseStreamBytes(bedrockUsage{10, 5, 100, 50}), uint8(0), uint32(10), uint32(5), uint32(100), uint32(50))
	f.Add([]byte("garbage"), uint8(1), uint32(1), uint32(1), uint32(1), uint32(1))
	f.Add([]byte{0, 0, 0, 16}, uint8(2), uint32(0), uint32(0), uint32(0), uint32(0))
	f.Add([]byte(`{"usage":{"inputTokens":1,"outputTokens":999999999999}}`), uint8(3), uint32(7), uint32(7), uint32(7), uint32(7))
	f.Add([]byte(`{"usage":{"inputTokens":-5,"outputTokens":1}}`), uint8(3), uint32(7), uint32(7), uint32(7), uint32(7))
	const w = int64(1) << 60
	f.Fuzz(func(t *testing.T, data []byte, mode uint8, in, out, cr, cw uint32) {
		u := bedrockUsage{In: int64(in), Out: int64(out), Read: int64(cr), Write: int64(cw)}
		var src []byte
		switch mode % 5 {
		case 0:
			src = data
		case 1:
			src = append(converseStreamBytes(u), data...)
		case 2:
			src = converseStreamBytes(u)
			if len(data) > 0 {
				src[int(data[0])*7%len(src)] ^= 0x80 | data[len(data)-1]
			}
		case 3: // the data as the metadata event between a start and a stop
			src = append(append(converseFrame("messageStart", `{"role":"assistant"}`), converseFrame("messageStop", `{"stopReason":"end_turn"}`)...), converseFrame("metadata", string(data))...)
		case 4:
			src = converseStreamBytes(u)
		}
		c := newConverseSSE(bytes.NewReader(src), "m", "x", time.Unix(1, 0), true)
		sse, err := io.ReadAll(c)
		if err != nil {
			t.Fatalf("the converter fails with %v instead of ending", err)
		}
		for _, blk := range strings.Split(strings.TrimSuffix(string(sse), "\n\n"), "\n\n") {
			if blk == "" {
				continue
			}
			d, ok := strings.CutPrefix(blk, "data: ")
			if !ok || d != "[DONE]" && !json.Valid([]byte(d)) || strings.ContainsAny(d, "\n\r") {
				t.Fatalf("malformed SSE block %q", blk)
			}
		}
		st, _ := openai.CopySSEFor(openai.Chat, io.Discard, nil, bytes.NewReader(sse), false)
		st.Usage, st.HaveUsage = c.Usage()
		cost, atW := settleBedrockStream(c.Complete(), st, bdrkPrices, w)
		if !c.Complete() {
			if cost != w || !atW {
				t.Fatalf("an incomplete stream was charged %d, want W", cost)
			}
			return
		}
		if !st.HaveUsage {
			t.Fatal("complete, but no usage")
		}
		if want := bdrkPrices.costNano(st.Usage); cost != want {
			t.Fatalf("charged %d, usage costs %d", cost, want)
		}
		if mode%5 == 4 { // the independent oracle: writes split 3/5 and 2/5
			prompt := u.In + u.Read + u.Write
			w1h := u.Write * 2 / 5
			oracle := bdrkPrices.costNano(openai.Usage{PromptTokens: prompt, CompletionTokens: u.Out, TotalTokens: prompt + u.Out,
				PromptTokensDetails: &struct {
					CachedTokens int64 `json:"cached_tokens"`
				}{u.Read}, CacheWrite5m: u.Write - w1h, CacheWrite1h: w1h})
			if cost != oracle {
				t.Fatalf("complete stream of %+v charged %d, oracle %d", u, cost, oracle)
			}
		}
	})
}
