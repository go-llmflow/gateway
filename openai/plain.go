package openai

import (
	"encoding/json"
	"fmt"
)

// rewritePlain rewrites a request of a non-generating endpoint (rerank,
// tokenize, detokenize): the model is resolved, the client's engine fields
// are stripped, and the tenant's cache_salt is set where the endpoint has
// one (vLLM v0.32.0 RerankRequest does; the tokenize requests do not).
//
// References opened (2026-10-25): vLLM docs "Cohere Rerank API"
// (models/pooling_models/scoring) — query, documents, top_n, priority,
// cache_salt, request_id; response results[] {index, relevance_score,
// document}, usage.total_tokens; vllm/entrypoints/serve/tokenize/protocol.py
// — TokenizeCompletionRequest {prompt}, TokenizeChatRequest {messages,
// chat_template, chat_template_kwargs…}, DetokenizeRequest {tokens}.
func rewritePlain(body []byte, resolve func(string) (string, error), salt string) ([]byte, Request, error) {
	var req Request
	m, err := decodeObject(body)
	if err != nil {
		return nil, req, err
	}
	if err := json.Unmarshal(m["model"], &req.Model); err != nil || req.Model == "" {
		return nil, req, ErrNoModel
	}
	upstream, err := resolve(req.Model)
	if err != nil {
		return nil, req, err
	}
	m["model"], _ = json.Marshal(upstream)
	for _, f := range StrippedFields {
		if _, ok := m[f]; ok {
			delete(m, f)
			req.Stripped = append(req.Stripped, f)
		}
	}
	if salt != "" {
		m["cache_salt"], _ = json.Marshal(salt)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil, req, fmt.Errorf("re-encoding the request: %w", err)
	}
	return out, req, nil
}

// parseRerankUsage reads vLLM's rerank usage: total_tokens, the tokens of
// the query and documents scored (prompt_tokens when present).
func parseRerankUsage(body []byte) (Usage, bool) {
	var env struct {
		Usage *struct {
			PromptTokens int64 `json:"prompt_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Usage{}, false
	}
	p := env.Usage.PromptTokens
	if p == 0 {
		p = env.Usage.TotalTokens
	}
	u := Usage{PromptTokens: p, TotalTokens: p}
	return u, u.Valid() && env.Usage.TotalTokens >= 0
}
