package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

func (e *env) responses(key, body string) (int, string) {
	e.t.Helper()
	resp := e.do("POST", "/v1/responses", key, body)
	return resp.StatusCode, readAll(e.t, resp)
}

// waitRecord waits for a spend record matching ok.
func waitRecord(t *testing.T, st *store.Memory, ok func(store.Record) bool) store.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, r := range st.Records() {
			if ok(r) {
				return r
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching record in %+v", st.Records())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResponsesProxied(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA")

	code, out := e.responses(key, `{"model":"llama","input":"hello","store":true,"priority":-10,"max_output_tokens":32}`)
	if code != 200 || !strings.Contains(out, `"output_text"`) {
		t.Fatalf("%d %s", code, out)
	}
	got := up.last()
	if got.Path != "/v1/responses" || got.Body["store"] != false || got.Body["model"] != "meta/llama" || got.Body["cache_salt"] == nil ||
		got.Body["priority"] != nil || got.Header.Get("X-Vllm-Priority") == "" {
		t.Fatalf("upstream got %s %v", got.Path, got.Body)
	}
	r := waitRecord(t, e.st, func(r store.Record) bool { return !r.Stream })
	if r.PromptTokens != 100 || r.CompletionTokens != 20 || r.CachedTokens != 40 || r.Estimated || r.Cost <= 0 {
		t.Fatalf("%+v", r)
	}

	code, out = e.responses(key, `{"model":"llama","input":"hello","stream":true}`)
	if code != 200 || !strings.Contains(out, "response.output_text.delta") || !strings.Contains(out, "response.completed") {
		t.Fatalf("%d %s", code, out)
	}
	r = waitRecord(t, e.st, func(r store.Record) bool { return r.Stream })
	if r.PromptTokens != 100 || r.Estimated {
		t.Fatalf("stream usage: %+v", r)
	}

	// Stateful features are refused; stored responses are not reachable.
	if code, out := e.responses(key, `{"model":"llama","input":"x","previous_response_id":"resp_1"}`); code != 400 || !strings.Contains(out, "stored responses") {
		t.Fatalf("%d %s", code, out)
	}
	if resp := e.do("GET", "/v1/responses/resp_1", key, ""); resp.StatusCode == 200 {
		t.Fatal("GET /v1/responses/{id} is routed")
	}
	if code, _ := e.responses(key, `{"model":"nope","input":"x"}`); code != 404 {
		t.Fatalf("unknown model: %d", code)
	}
}

func TestResponsesGuarded(t *testing.T) {
	up, gup := newFakeVLLM(t), newFakeVLLM(t)
	cfg := guardConfig(t, up.URL(), gup.URL(), `
guardrail "secrets" {
  type  = "hide_secrets"
  teams = ["etabA"]
}`)
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	up.setReply("here: ", ghToken)
	body, _ := json.Marshal(map[string]any{"model": "llama", "instructions": "use " + ghToken,
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "token " + ghToken}}}}})
	code, out := e.responses(key, string(body))
	if code != 200 || strings.Contains(out, ghToken) || !strings.Contains(out, "here: [TOKEN]") || strings.Contains(out, "thinking") {
		t.Fatalf("%d %s", code, out)
	}
	sent, _ := json.Marshal(up.last().Body)
	if strings.Contains(string(sent), ghToken) || strings.Count(string(sent), "[TOKEN]") != 2 {
		t.Fatalf("vLLM got %s", sent)
	}
	// A stream cannot be rewritten under post-call guardrails yet: refused,
	// before vLLM is called.
	n := up.count()
	if code, out := e.responses(key, `{"model":"llama","input":"x","stream":true}`); code != 400 || !strings.Contains(out, "stream") || up.count() != n {
		t.Fatalf("%d %s", code, out)
	}
	// Input the guardrails cannot read is refused.
	if code, _ := e.responses(key, `{"model":"llama","input":[{"type":"reasoning","summary":[]}]}`); code != 400 {
		t.Fatalf("unreadable input: %d", code)
	}
}
