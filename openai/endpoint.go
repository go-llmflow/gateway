package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// Endpoint is the API a request is for. The rewrite and the usage
// extraction differ per endpoint; everything else (auth, fair share,
// guardrails, accounting) is shared.
//
// References opened for the Responses API (2026-10-25):
//   - openai-python src/openai/types/responses/response_create_params.py
//     (input, instructions, previous_response_id, store, background,
//     conversation, prompt, max_output_tokens), response_usage.py
//     (input_tokens, input_tokens_details.cached_tokens, output_tokens) and
//     response_completed_event.py (the final event carries the response);
//   - vLLM v0.32.0 vllm/entrypoints/openai/responses/protocol.py
//     (ResponsesRequest: store defaults to TRUE, previous_response_id,
//     background, cache_salt, priority, request_id).
type Endpoint int

const (
	// Chat is /v1/chat/completions; Completions and Embeddings share its
	// rewrite.
	Chat Endpoint = iota
	Completions
	Embeddings
	// Responses is /v1/responses (OpenAI Responses API).
	Responses
	// Messages is /v1/messages (Anthropic Messages API, served by vLLM
	// itself: vllm/entrypoints/anthropic, v0.32.0, converts it to a chat
	// completion with the same raw request, so X-Vllm-Priority applies).
	Messages
	// Rerank is /v1/rerank (vLLM's Jina/Cohere-style rerank for score
	// models).
	Rerank
	// Tokenize and Detokenize are vLLM's /tokenize and /detokenize: no
	// generation, nothing to charge.
	Tokenize
	Detokenize
)

// Metered reports whether the endpoint's work is charged: when usage is
// missing, the gateway estimates it for a metered endpoint only.
func (ep Endpoint) Metered() bool { return ep != Tokenize && ep != Detokenize }

// Guarded reports whether the guardrails apply. Tokenization sends the text
// to vLLM's API server only (no model runs) and returns token ids; masking
// it would only make the count wrong.
func (ep Endpoint) Guarded() bool { return ep != Tokenize && ep != Detokenize }

// ErrStateful refuses Responses features that keep state in vLLM.
//
// vLLM stores every response in the memory of the instance that produced
// it (store defaults to true) and lets any caller who knows its id read it
// back (GET /v1/responses/{id}), continue it (previous_response_id) or
// cancel it. Behind the gateway that state would be shared by every tenant
// and live on one upstream among several, so the gateway serves the
// Responses API statelessly: store is forced to false, and the fields that
// refer to stored state are refused rather than silently ignored.
var ErrStateful = errors.New(`stored responses are not served: previous_response_id, background, conversation and stored prompts are refused, and "store" is forced to false (send the whole conversation in "input")`)

// statefulFields are refused in a Responses request when not null/false.
var statefulFields = []string{"previous_response_id", "background", "conversation", "prompt"}

// RewriteFor is Rewrite for one endpoint.
func RewriteFor(ep Endpoint, body []byte, resolve func(model string) (string, error), iso Isolation) ([]byte, Request, error) {
	switch ep {
	case Responses:
	case Messages:
		return rewriteMessages(body, resolve, iso)
	case Rerank:
		return rewritePlain(body, resolve, iso.CacheSalt)
	case Tokenize, Detokenize:
		// No cache_salt field there (vllm/entrypoints/serve/tokenize).
		return rewritePlain(body, resolve, "")
	default:
		return Rewrite(body, resolve, iso)
	}
	var req Request
	m, err := decodeObject(body)
	if err != nil {
		return nil, req, err
	}
	if err := json.Unmarshal(m["model"], &req.Model); err != nil || req.Model == "" {
		return nil, req, ErrNoModel
	}
	for _, f := range statefulFields {
		raw, ok := m[f]
		if !ok || isNull(raw) || bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
			continue
		}
		return nil, req, ErrStateful
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
	m["store"] = json.RawMessage("false")
	if iso.CacheSalt != "" {
		m["cache_salt"], _ = json.Marshal(iso.CacheSalt)
	}
	if iso.ScopeUUID != nil {
		if raw, ok := m["input"]; ok {
			// Input items have the shape of chat messages: content parts
			// may carry a multimodal uuid.
			scoped, err := scopeUUIDs(raw, iso.ScopeUUID)
			if err != nil {
				return nil, req, err
			}
			m["input"] = scoped
		}
	}
	if raw, ok := m["stream"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &req.Stream); err != nil {
			return nil, req, fmt.Errorf(`"stream" must be a boolean`)
		}
	}
	if raw, ok := m["max_output_tokens"]; ok && !isNull(raw) {
		var n int64
		if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
			return nil, req, fmt.Errorf(`"max_output_tokens" must be a non-negative integer`)
		}
		req.MaxTokens = n
	}
	// The usage of a Responses stream is in its last event, which the
	// client receives anyway: nothing to request or to drop.
	req.ClientWantsUsage = true
	out, err := json.Marshal(m)
	if err != nil {
		return nil, req, fmt.Errorf("re-encoding the request: %w", err)
	}
	return out, req, nil
}

// decodeObject decodes exactly one JSON object.
func decodeObject(body []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, ErrNotObject
	}
	if _, err := dec.Token(); err == nil {
		return nil, ErrNotObject
	}
	return m, nil
}

// responsesUsage is the usage object of the Responses API.
type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	TotalTokens        int64 `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func (r responsesUsage) usage() Usage {
	u := Usage{PromptTokens: r.InputTokens, CompletionTokens: r.OutputTokens, TotalTokens: r.TotalTokens}
	if r.InputTokensDetails != nil {
		u.PromptTokensDetails = &struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{r.InputTokensDetails.CachedTokens}
	}
	return u
}

// ParseUsageFor reads the usage of a non-streamed answer of ep.
func ParseUsageFor(ep Endpoint, body []byte) (Usage, bool) {
	switch ep {
	case Responses:
	case Messages:
		return parseMessagesUsage(body)
	case Rerank:
		return parseRerankUsage(body)
	case Tokenize, Detokenize:
		return Usage{}, true
	default:
		return ParseUsage(body)
	}
	var env struct {
		Usage *responsesUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return Usage{}, false
	}
	u := env.Usage.usage()
	return u, u.Valid()
}

// inspectResponsesEvent reads the usage of the response carried by a
// Responses stream event (response.completed, response.incomplete,
// response.failed carry the whole response).
func inspectResponsesEvent(event []byte) (Usage, bool) {
	if !bytes.Contains(event, usageKey) {
		return Usage{}, false
	}
	var env struct {
		Response *struct {
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(eventPayload(event), &env); err != nil || env.Response == nil || env.Response.Usage == nil {
		return Usage{}, false
	}
	u := env.Response.Usage.usage()
	return u, u.Valid()
}

var responsesDelta = []byte(`"response.output_text.delta"`)

// eventPayload joins the data lines of an SSE event.
func eventPayload(event []byte) []byte {
	var data []byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, dataPrefix) {
			continue
		}
		data = append(data, bytes.TrimPrefix(line[len(dataPrefix):], []byte(" "))...)
	}
	return data
}
