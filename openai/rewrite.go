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
	// TextOut: the client asked a transcription as plain text; the gateway
	// asked vLLM for JSON (which carries the usage) and answers the text.
	TextOut bool
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
	// Chat and completions carry messages; embeddings also accept batched
	// messages and chat messages in "input" (vLLM pooling/embed/protocol.py).
	if err := scopeFields(m, iso.ScopeUUID, "messages", "input"); err != nil {
		return nil, req, err
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

// scopeFields scopes the multimodal uuids of the named top-level members
// (nothing when scope is nil).
func scopeFields(m map[string]json.RawMessage, scope func(string) string, fields ...string) error {
	if scope == nil {
		return nil
	}
	for _, f := range fields {
		raw, ok := m[f]
		if !ok {
			continue
		}
		scoped, changed, err := scopeIn(raw, scope, 0)
		if err != nil {
			return err
		}
		if changed {
			m[f] = scoped
		}
	}
	return nil
}

// scopeUUIDs rewrites the content parts' uuids of a list of messages.
func scopeUUIDs(raw json.RawMessage, scope func(string) string) (json.RawMessage, error) {
	out, _, err := scopeIn(raw, scope, 0)
	return out, err
}

// maxScopeDepth bounds the arrays walked to reach content parts: a list of
// messages, a list of lists (batched messages), a list of score inputs.
const maxScopeDepth = 3

// scopeIn rewrites through scope the "uuid" of every content part reachable
// as value.content[] where value is the given object or, recursively, an
// element of the given array: the shapes of chat messages, batched
// messages, and vLLM's score inputs ({"content": [parts]}). What is changed
// is re-encoded from its parsed form, like the body, so duplicate or escaped
// keys cannot survive. A uuid that is not a string (nor null) is refused.
func scopeIn(raw json.RawMessage, scope func(string) string, depth int) (json.RawMessage, bool, error) {
	t := bytes.TrimLeft(raw, " \t\r\n")
	if len(t) == 0 {
		return raw, false, nil
	}
	switch t[0] {
	case '[':
		if depth >= maxScopeDepth {
			return raw, false, nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return raw, false, nil // vLLM will reject it; nothing to scope
		}
		changed := false
		for i, it := range items {
			out, c, err := scopeIn(it, scope, depth+1)
			if err != nil {
				return nil, false, err
			}
			if c {
				items[i], changed = out, true
			}
		}
		if !changed {
			return raw, false, nil
		}
		out, err := json.Marshal(items)
		return out, err == nil, err
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return raw, false, nil
		}
		var parts []json.RawMessage
		if err := json.Unmarshal(obj["content"], &parts); err != nil {
			return raw, false, nil // string content carries no uuid
		}
		changed := false
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
				return nil, false, ErrBadUUID
			}
			part["uuid"], _ = json.Marshal(scope(s))
			parts[j], _ = json.Marshal(part)
			changed = true
		}
		if !changed {
			return raw, false, nil
		}
		obj["content"], _ = json.Marshal(parts)
		out, err := json.Marshal(obj)
		return out, err == nil, err
	}
	return raw, false, nil
}
