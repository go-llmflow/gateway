package openai

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// The Anthropic Messages API, as vLLM v0.32.0 serves it
// (vllm/entrypoints/anthropic/protocol.py and serving.py, opened
// 2026-10-25): the request has model, messages, max_tokens (required),
// system, stream, and vLLM's extras (cache_salt, kv_transfer_params,
// ec_transfer_params, vllm_xargs…); the usage is AnthropicUsage —
// input_tokens EXCLUDES the cached prompt ("input_tokens = prompt_tokens -
// cache_read - cache_creation"), cache_read_input_tokens and
// cache_creation_input_tokens carry the rest. A stream reports usage in
// message_start (message.usage) and in its final message_delta (usage on
// the event), the latter with the output count.

// messagesStripped are removed from a Messages request on top of
// StrippedFields: engine-internal fields of vLLM's Anthropic model.
var messagesStripped = []string{"mm_payload", "return_mm_kwargs"}

func rewriteMessages(body []byte, resolve func(string) (string, error), iso Isolation) ([]byte, Request, error) {
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
	for _, f := range append(StrippedFields, messagesStripped...) {
		if _, ok := m[f]; ok {
			delete(m, f)
			req.Stripped = append(req.Stripped, f)
		}
	}
	if iso.CacheSalt != "" {
		m["cache_salt"], _ = json.Marshal(iso.CacheSalt)
	}
	if raw, ok := m["stream"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &req.Stream); err != nil {
			return nil, req, fmt.Errorf(`"stream" must be a boolean`)
		}
	}
	if raw, ok := m["max_tokens"]; ok && !isNull(raw) {
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
			return nil, req, fmt.Errorf(`"max_tokens" must be a non-negative integer`)
		}
		req.MaxTokens = n
	}
	req.ClientWantsUsage = true
	out, err := json.Marshal(m)
	if err != nil {
		return nil, req, fmt.Errorf("re-encoding the request: %w", err)
	}
	return out, req, nil
}

// anthropicUsage is AnthropicUsage.
type anthropicUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// usage converts to the gateway's Usage: the prompt is every input token,
// cached or not.
func (a anthropicUsage) usage() Usage {
	prompt := a.InputTokens + a.CacheReadInputTokens + a.CacheCreationInputTokens
	return Usage{PromptTokens: prompt, CompletionTokens: a.OutputTokens, TotalTokens: prompt + a.OutputTokens,
		PromptTokensDetails: &struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{a.CacheReadInputTokens}}
}

func (a anthropicUsage) valid() bool {
	return a.InputTokens >= 0 && a.OutputTokens >= 0 && a.CacheReadInputTokens >= 0 && a.CacheCreationInputTokens >= 0
}

func parseMessagesUsage(body []byte) (Usage, bool) {
	var env struct {
		Usage *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil || !env.Usage.valid() {
		return Usage{}, false
	}
	u := env.Usage.usage()
	return u, u.Valid()
}

// messagesUsageTracker merges the usage of a Messages stream: the input
// counts from message_start, the output count from the last message_delta
// (cumulative). Each event that carries usage refines it.
type messagesUsageTracker struct {
	start, last anthropicUsage
	seen        bool
}

func (t *messagesUsageTracker) inspect(event []byte) (Usage, bool) {
	if !bytes.Contains(event, usageKey) {
		return Usage{}, false
	}
	var env struct {
		Type    string          `json:"type"`
		Usage   *anthropicUsage `json:"usage"`
		Message *struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
	}
	if err := json.Unmarshal(eventPayload(event), &env); err != nil {
		return Usage{}, false
	}
	switch {
	case env.Type == "message_start" && env.Message != nil && env.Message.Usage != nil && env.Message.Usage.valid():
		t.start = *env.Message.Usage
	case env.Type == "message_delta" && env.Usage != nil && env.Usage.valid():
		t.last = *env.Usage
	default:
		return Usage{}, false
	}
	t.seen = true
	merged := t.last
	if merged.InputTokens+merged.CacheReadInputTokens+merged.CacheCreationInputTokens == 0 {
		merged.InputTokens, merged.CacheReadInputTokens, merged.CacheCreationInputTokens =
			t.start.InputTokens, t.start.CacheReadInputTokens, t.start.CacheCreationInputTokens
	}
	merged.OutputTokens = max(merged.OutputTokens, t.start.OutputTokens)
	u := merged.usage()
	return u, u.Valid()
}

var messagesDelta = []byte(`"text_delta"`)
