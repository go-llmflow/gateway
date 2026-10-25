package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

func (e *env) messages(key, body string, hdr ...string) (int, string) {
	e.t.Helper()
	resp := e.do("POST", "/v1/messages", key, body, hdr...)
	return resp.StatusCode, readAll(e.t, resp)
}

func TestMessagesProxied(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA")

	code, out := e.messages(key, `{"model":"llama","max_tokens":64,"messages":[{"role":"user","content":"hello"}],"priority":-3}`)
	if code != 200 || !strings.Contains(out, `"type":"message"`) {
		t.Fatalf("%d %s", code, out)
	}
	got := up.last()
	if got.Path != "/v1/messages" || got.Body["model"] != "meta/llama" || got.Body["cache_salt"] == nil || got.Body["priority"] != nil ||
		got.Header.Get("X-Vllm-Priority") == "" {
		t.Fatalf("upstream got %s %v", got.Path, got.Body)
	}
	// input_tokens excludes the cached prompt: charged as 100 prompt
	// tokens of which 40 cached.
	r := waitRecord(t, e.st, func(r store.Record) bool { return !r.Stream })
	if r.PromptTokens != 100 || r.CachedTokens != 40 || r.CompletionTokens != 20 || r.Estimated {
		t.Fatalf("%+v", r)
	}
	code, out = e.messages(key, `{"model":"llama","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	if code != 200 || !strings.Contains(out, "message_stop") {
		t.Fatalf("%d %s", code, out)
	}
	r = waitRecord(t, e.st, func(r store.Record) bool { return r.Stream })
	if r.PromptTokens != 100 || r.CompletionTokens != 20 || r.Estimated {
		t.Fatalf("stream usage: %+v", r)
	}
	// Errors carry the Anthropic shape too.
	code, out = e.messages(key, `{"model":"nope","max_tokens":1,"messages":[]}`)
	var er struct {
		Type  string
		Error struct{ Type, Message string }
	}
	if code != 404 || json.Unmarshal([]byte(out), &er) != nil || er.Type != "error" || er.Error.Message == "" {
		t.Fatalf("%d %s", code, out)
	}
}

func TestMessagesGuarded(t *testing.T) {
	up, gup := newFakeVLLM(t), newFakeVLLM(t)
	cfg := guardConfig(t, up.URL(), gup.URL(), `
guardrail "secrets" {
  type  = "hide_secrets"
  teams = ["etabA"]
}`)
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	up.setReply("found ", ghToken)
	body, _ := json.Marshal(map[string]any{"model": "llama", "max_tokens": 64, "system": "key " + ghToken,
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": "t", "name": "sh", "input": map[string]any{"env": ghToken}}}},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": "t", "content": "ok " + ghToken}}},
		}})
	code, out := e.messages(key, string(body))
	if code != 200 || strings.Contains(out, ghToken) || !strings.Contains(out, "found [TOKEN]") || strings.Contains(out, "pondering") {
		t.Fatalf("%d %s", code, out)
	}
	sent, _ := json.Marshal(up.last().Body)
	if strings.Contains(string(sent), ghToken) || strings.Count(string(sent), "[TOKEN]") != 3 {
		t.Fatalf("vLLM got %s", sent)
	}
	// Streamed: the token split over deltas is masked by the Anthropic
	// stream filter.
	up.setReply("found ", ghToken[:12], ghToken[12:], " done")
	code, out = e.messages(key, `{"model":"llama","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"x"}]}`)
	if code != 200 || strings.Contains(out, ghToken[12:]) || !strings.Contains(out, "[TOKEN]") || !strings.Contains(out, "message_stop") ||
		!strings.Contains(out, `"output_tokens":20`) {
		t.Fatalf("%d %s", code, out)
	}
}
