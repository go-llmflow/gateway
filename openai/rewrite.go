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
func Rewrite(body []byte, resolve func(model string) (string, error)) ([]byte, Request, error) {
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
