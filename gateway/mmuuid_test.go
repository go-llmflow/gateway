package gateway

import (
	"encoding/json"
	"strings"
	"testing"
)

// Security audit 2026-10-26: a multimodal "uuid" names a vLLM media cache
// entry, so it must be scoped per tenant on EVERY route whose request vLLM
// parses with its chat content-part parser, not only on chat completions.
// vLLM's rerank parses query and documents with parse_chat_message_content_parts
// (vllm/entrypoints/pooling/scoring/utils.py), tokenize renders chat messages,
// and embeddings accept batched messages and chat messages in "input"
// (vllm/entrypoints/pooling/embed/protocol.py). An unscoped uuid lets one
// tenant read (probe) or poison another tenant's cached media.
func TestMultimodalUUIDScopedOnEveryRoute(t *testing.T) {
	const part = `{"type":"image_url","image_url":{"url":"http://img/1"},"uuid":"img-1"}`
	const escaped = `{"type":"image_url","image_url":{"url":"http://img/1"},"uuid":"img-1"}`
	msgs := `[{"role":"user","content":[{"type":"text","text":"x"},` + part + `]}]`
	for _, tc := range []struct{ name, path, body string }{
		{"chat", "/v1/chat/completions", `{"model":"llama","messages":` + msgs + `}`},
		{"chat escaped key", "/v1/chat/completions", `{"model":"llama","messages":[{"role":"user","content":[` + escaped + `]}]}`},
		{"embeddings messages", "/v1/embeddings", `{"model":"llama","messages":` + msgs + `}`},
		{"embeddings batched messages", "/v1/embeddings", `{"model":"llama","messages":[` + msgs + `,` + msgs + `]}`},
		{"embeddings chat input", "/v1/embeddings", `{"model":"llama","input":` + msgs + `}`},
		{"embeddings batched chat input", "/v1/embeddings", `{"model":"llama","input":[` + msgs + `]}`},
		{"rerank query", "/v1/rerank", `{"model":"llama","query":{"content":[` + part + `]},"documents":["a"]}`},
		{"rerank documents", "/v1/rerank", `{"model":"llama","query":"q","documents":[{"content":[` + part + `]},"b"]}`},
		{"rerank v2 document object", "/v2/rerank", `{"model":"llama","query":"q","documents":{"content":[` + escaped + `]}}`},
		{"tokenize messages", "/tokenize", `{"model":"llama","messages":` + msgs + `}`},
		{"responses input", "/v1/responses", `{"model":"llama","input":` + msgs + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := newFakeVLLM(t)
			e := newEnv(t, testConfig(up.URL()))
			_, ka := e.newKey("etabA")
			_, kb := e.newKey("etabB")
			sent := func(key string) string {
				t.Helper()
				resp := e.do("POST", tc.path, key, tc.body)
				if out := readAll(t, resp); resp.StatusCode != 200 {
					t.Fatalf("%d %s", resp.StatusCode, out)
				}
				b, _ := json.Marshal(up.last().Body)
				return string(b)
			}
			a, b := sent(ka), sent(kb)
			if strings.Contains(a, `"img-1"`) || strings.Contains(b, `"img-1"`) {
				t.Fatalf("the client's uuid reached vLLM unscoped:\n%s", a)
			}
			if !strings.Contains(a, `"uuid":"llmf-`) {
				t.Fatalf("no scoped uuid in %s", a)
			}
			if a == b {
				t.Fatal("two organisations sent the same scoped uuid")
			}
		})
	}
}
