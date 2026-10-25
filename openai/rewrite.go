// Package openai rewrites OpenAI-compatible request bodies before they are
// sent to vLLM and extracts token usage from the responses.
//
// References opened for this package (2026-10-23):
//   - OpenAI chat completions, stream_options.include_usage and the
//     CompletionUsage object, as generated from OpenAI's OpenAPI spec in
//     openai-python (src/openai/types/chat/chat_completion_stream_options_param.py,
//     src/openai/types/completion_usage.py): with include_usage "an
//     additional chunk will be streamed before the data: [DONE] message",
//     whose choices "will always be an empty array";
//   - vLLM's ChatCompletionRequest (vllm/entrypoints/openai/chat_completion/protocol.py):
//     priority, request_id, kv_transfer_params, ec_transfer_params,
//     vllm_xargs.
package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// StrippedFields are removed from every client body. A client must not
// choose its own scheduling priority (priority), smuggle one through a
// wrapper some SDKs or gateways interpret (extra_body), pick the engine's
// request identifier (request_id), or pass engine-internal parameters
// (kv_transfer_params and ec_transfer_params for disaggregated serving,
// vllm_xargs for custom extensions).
var StrippedFields = []string{
	"priority",
	"extra_body",
	"request_id",
	"kv_transfer_params",
	"ec_transfer_params",
	"vllm_xargs",
	// The prefix-cache salt is the gateway's: a client salt would let a
	// caller join another tenant's cache (vLLM security guide: prefix-cache
	// reuse is a timing side channel, CVE-2025-46570).
	"cache_salt",
	// A client-supplied Jinja chat template (vLLM's chat and tokenize
	// requests accept one; vLLM refuses it unless the server trusts request
	// templates). Defence in depth: it never reaches vLLM.
	"chat_template",
}

// Isolation is the per-tenant cache scoping the gateway applies (vLLM
// security guide: "a gateway that authenticates callers and scopes
// client-supplied cache identifiers" — cache_salt and multimodal uuids —
// per tenant).
type Isolation struct {
	// CacheSalt, when not empty, is set as the body's cache_salt.
	CacheSalt string
	// ScopeUUID, when not nil, replaces every multimodal "uuid" of the
	// message content parts by a tenant-scoped value, so two tenants
	// presenting the same uuid never share a media cache entry.
	ScopeUUID func(uuid string) string
}

// Request describes a rewritten request.
type Request struct {
	// Model is the client-facing model name.
	Model string
	// Stream is true for an SSE response.
	Stream bool
	// ClientWantsUsage is true when the client itself asked for the usage
	// chunk; when false the gateway drops the chunk it asked for.
	ClientWantsUsage bool
	// MaxTokens is max_completion_tokens or max_tokens, 0 when absent.
	MaxTokens int64
	// Stripped lists the fields removed from the body.
	Stripped []string
}

// Errors returned by Rewrite.
var (
	ErrNotObject = errors.New("request body must be a JSON object")
	ErrNoModel   = errors.New(`"model" must be a non-empty string`)
)

// Rewrite parses a request body, removes StrippedFields, replaces the model
// with the upstream model name returned by resolve and, for streams, sets
// stream_options.include_usage. The body is re-encoded from its parsed
// form, so duplicate keys and escaped key spellings ("priority")
// cannot survive: what vLLM parses is exactly what the gateway checked.
func Rewrite(body []byte, resolve func(model string) (string, error), iso Isolation) ([]byte, Request, error) {
	var req Request
	var m map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&m); err != nil || m == nil {
		return nil, req, ErrNotObject
	}
	if dec.More() {
		return nil, req, ErrNotObject
	}
	if _, err := dec.Token(); err == nil {
		return nil, req, ErrNotObject
	}

	if err := json.Unmarshal(m["model"], &req.Model); err != nil || req.Model == "" {
		return nil, req, ErrNoModel
	}
	upstream, err := resolve(req.Model)
	if err != nil {
		return nil, req, err
	}
	um, _ := json.Marshal(upstream)
	m["model"] = um

	for _, f := range StrippedFields {
		if _, ok := m[f]; ok {
			delete(m, f)
			req.Stripped = append(req.Stripped, f)
		}
	}
	if iso.CacheSalt != "" {
		salt, _ := json.Marshal(iso.CacheSalt)
		m["cache_salt"] = salt
	}
	if iso.ScopeUUID != nil {
		if raw, ok := m["messages"]; ok {
			scoped, err := scopeUUIDs(raw, iso.ScopeUUID)
			if err != nil {
				return nil, req, err
			}
			m["messages"] = scoped
		}
	}

	if raw, ok := m["stream"]; ok && !isNull(raw) {
		if err := json.Unmarshal(raw, &req.Stream); err != nil {
			return nil, req, fmt.Errorf(`"stream" must be a boolean`)
		}
	}
	for _, k := range []string{"max_completion_tokens", "max_tokens"} {
		if raw, ok := m[k]; ok && !isNull(raw) {
			var n int64
			if err := json.Unmarshal(raw, &n); err != nil || n < 0 {
				return nil, req, fmt.Errorf("%q must be a non-negative integer", k)
			}
			if req.MaxTokens == 0 {
				req.MaxTokens = n
			}
		}
	}

	if req.Stream {
		opts := map[string]json.RawMessage{}
		if raw, ok := m["stream_options"]; ok && !isNull(raw) {
			if err := json.Unmarshal(raw, &opts); err != nil || opts == nil {
				return nil, req, fmt.Errorf(`"stream_options" must be an object`)
			}
		}
		if raw, ok := opts["include_usage"]; ok && !isNull(raw) {
			if err := json.Unmarshal(raw, &req.ClientWantsUsage); err != nil {
				return nil, req, fmt.Errorf(`"stream_options.include_usage" must be a boolean`)
			}
		}
		opts["include_usage"] = json.RawMessage("true")
		enc, err := json.Marshal(opts)
		if err != nil {
			return nil, req, err
		}
		m["stream_options"] = enc
	}

	out, err := json.Marshal(m)
	if err != nil {
		return nil, req, fmt.Errorf("re-encoding the request: %w", err)
	}
	return out, req, nil
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }

// ErrBadUUID reports a multimodal uuid that is not a string.
var ErrBadUUID = errors.New(`a content part "uuid" must be a string`)

// scopeUUIDs rewrites messages[].content[].uuid through scope. Messages
// and parts are re-encoded from their parsed form, like the body.
func scopeUUIDs(raw json.RawMessage, scope func(string) string) (json.RawMessage, error) {
	var msgs []json.RawMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		return raw, nil // not an array: vLLM will reject it; nothing to scope
	}
	changed := false
	for i, mr := range msgs {
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(mr, &msg); err != nil || msg == nil {
			continue
		}
		var parts []json.RawMessage
		if err := json.Unmarshal(msg["content"], &parts); err != nil {
			continue // string content carries no uuid
		}
		partChanged := false
		for j, pr := range parts {
			var part map[string]json.RawMessage
			if err := json.Unmarshal(pr, &part); err != nil || part == nil {
				continue
			}
			u, ok := part["uuid"]
			if !ok || isNull(u) {
				continue
			}
			var s string
			if err := json.Unmarshal(u, &s); err != nil {
				return nil, ErrBadUUID
			}
			part["uuid"], _ = json.Marshal(scope(s))
			parts[j], _ = json.Marshal(part)
			partChanged = true
		}
		if partChanged {
			msg["content"], _ = json.Marshal(parts)
			msgs[i], _ = json.Marshal(msg)
			changed = true
		}
	}
	if !changed {
		return raw, nil
	}
	return json.Marshal(msgs)
}
