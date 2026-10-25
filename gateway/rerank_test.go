package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

func TestRerankProxied(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	_, key := e.newKey("etabA")
	resp := e.do("POST", "/v1/rerank", key, `{"model":"llama","query":"capital of France","documents":["Paris","Lyon"],"priority":-1,"request_id":"x"}`)
	out := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(out, `"relevance_score"`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	got := up.last()
	if got.Path != "/v1/rerank" || got.Body["cache_salt"] == nil || got.Body["priority"] != nil || got.Body["request_id"] != nil ||
		got.Body["model"] != "meta/llama" {
		t.Fatalf("%s %v", got.Path, got.Body)
	}
	r := waitRecord(t, e.st, func(r store.Record) bool { return r.PromptTokens == 30 })
	if r.Estimated || r.Cost <= 0 {
		t.Fatalf("%+v", r)
	}
	resp = e.do("POST", "/v2/rerank", key, `{"model":"llama","query":"q","documents":["a"]}`)
	if readAll(t, resp); resp.StatusCode != 200 || up.last().Path != "/v2/rerank" {
		t.Fatalf("v2: %d %s", resp.StatusCode, up.last().Path)
	}
}

func TestTokenizeIsFreeAndUnguarded(t *testing.T) {
	up, gup := newFakeVLLM(t), newFakeVLLM(t)
	cfg := guardConfig(t, up.URL(), gup.URL(), `
guardrail "secrets" {
  type  = "hide_secrets"
  teams = ["etabA"]
}`)
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	resp := e.do("POST", "/tokenize", key, `{"model":"llama","prompt":"count `+ghToken+`","chat_template":"{{ evil }}"}`)
	out := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(out, `"count":3`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	got := up.last()
	// Not masked (the count would be wrong, and no model runs), but the
	// client template never reaches vLLM, and tokenize has no cache_salt.
	if got.Path != "/tokenize" || !strings.Contains(got.Body["prompt"].(string), ghToken) || got.Body["chat_template"] != nil || got.Body["cache_salt"] != nil {
		t.Fatalf("%v", got.Body)
	}
	resp = e.do("POST", "/detokenize", key, `{"model":"llama","tokens":[1,2,3]}`)
	if out := readAll(t, resp); resp.StatusCode != 200 || !strings.Contains(out, `"prompt":"abc"`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	// Recorded with no cost and nothing estimated.
	r := waitRecord(t, e.st, func(r store.Record) bool { return r.Status == 200 })
	if r.Cost != 0 || r.Estimated || r.PromptTokens != 0 {
		t.Fatalf("%+v", r)
	}
	// Without a key: refused like every route.
	if resp := e.do("POST", "/tokenize", "", `{"model":"llama","prompt":"x"}`); resp.StatusCode != 401 {
		t.Fatalf("unauthenticated tokenize: %d", resp.StatusCode)
	}
}

func TestRerankGuarded(t *testing.T) {
	up, gup := newFakeVLLM(t), newFakeVLLM(t)
	cfg := guardConfig(t, up.URL(), gup.URL(), `
guardrail "pii" {
  type  = "pii"
  teams = ["etabA"]
}`)
	e := newEnv(t, cfg)
	_, key := e.newKey("etabA")
	resp := e.do("POST", "/v1/rerank", key, `{"model":"llama","query":"who is jean@example.fr","documents":["mail jean@example.fr","nothing"]}`)
	out := readAll(t, resp)
	if resp.StatusCode != 200 || strings.Contains(out, "jean@") || !strings.Contains(out, "[EMAIL]") || strings.Contains(out, `"extra"`) {
		t.Fatalf("%d %s", resp.StatusCode, out)
	}
	sent, _ := json.Marshal(up.last().Body)
	if strings.Contains(string(sent), "jean@") {
		t.Fatalf("vLLM got %s", sent)
	}
	// Document objects the guardrails cannot read are refused.
	resp = e.do("POST", "/v1/rerank", key, `{"model":"llama","query":"q","documents":[{"content":[{"type":"image_url"}]}]}`)
	if readAll(t, resp); resp.StatusCode != 400 {
		t.Fatalf("object documents: %d", resp.StatusCode)
	}
}
