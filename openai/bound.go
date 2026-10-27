package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
)

// What bounds the usage of a request (hard budgets, #87).
//
// References opened for this file (2026-10-27), vLLM v0.32.0:
//   - vllm/entrypoints/serve/utils/api_utils.py get_max_tokens: an input
//     longer than max_model_len is refused; the output is at most
//     min(max_tokens, max_model_len - input_length);
//   - vllm/renderers/params.py: max_tokens above max_model_len is refused
//     ("cannot be greater than max_total_tokens"); the input bound is the
//     whole context (max_output_tokens is not subtracted from it);
//   - vllm/entrypoints/openai/chat_completion/protocol.py: max_completion_tokens
//     wins over max_tokens when both are set; n (default 1); the usage of a
//     choice set counts the prompt once and the completions of every choice
//     (serving.py: num_generated_tokens = sum over outputs);
//   - vllm/entrypoints/openai/completion/protocol.py and serving.py: max_tokens
//     defaults to 16 (also when null), prompt may be a list (one request per
//     prompt, usage summed over prompts), prompt_embeds likewise;
//   - vllm/entrypoints/openai/responses/serving.py: built-in tools make the
//     server loop, each turn with its own max_output_tokens: such a request
//     has no bound;
//   - vllm/entrypoints/pooling/embed/protocol.py: input (a string, a list of
//     strings or token lists, one chat conversation, or batched
//     conversations) and messages; vllm/entrypoints/pooling/scoring/serving.py:
//     the usage of a rerank is the sum of its prompts' lengths.

// Shape is what bounds the usage of one request, read from the body vLLM
// receives (the rewritten one).
type Shape struct {
	// Free: the endpoint is never charged (tokenize, detokenize, count).
	Free bool
	// Generates: the endpoint produces output tokens, at most MaxTokens per
	// choice; a pooling endpoint (embeddings, rerank, classify) does not.
	Generates bool
	// Inputs is the number of independent prompts (each at most
	// max_model_len tokens); at least 1.
	Inputs int64
	// Choices is n (chat, completions); at least 1.
	Choices int64
	// MaxTokens is the client's output bound as vLLM reads it, valid when
	// HasMaxTokens (a completion without one defaults to 16, as in vLLM).
	MaxTokens    int64
	HasMaxTokens bool
	// Unbounded, when not empty, says why the usage of the request has no
	// bound the gateway can compute (a budget then refuses it).
	Unbounded string
}

// Errors of ShapeOf.
var (
	ErrBadN = errors.New(`"n" must be a positive integer`)
)

// maxShapeInputs bounds the inputs counted: more is refused (a budget could
// never hold them anyway).
const maxShapeInputs = 1 << 20

// ShapeOf reads the shape of a rewritten request body for ep.
func ShapeOf(ep Endpoint, body []byte) (Shape, error) {
	sh := Shape{Inputs: 1, Choices: 1}
	switch ep {
	case Tokenize, Detokenize, CountTokens:
		sh.Free = true
		return sh, nil
	case Transcriptions:
		// Priced per duration, which nothing in the request bounds.
		sh.Generates = true
		sh.Unbounded = "a transcription's cost depends on the audio's duration, which the gateway cannot bound before it runs"
		return sh, nil
	}
	m, err := decodeObject(body)
	if err != nil {
		return sh, err
	}
	switch ep {
	case Chat:
		sh.Generates = true
		if sh.Choices, err = choices(m); err != nil {
			return sh, err
		}
		// max_completion_tokens wins over max_tokens (vLLM to_sampling_params).
		for _, k := range []string{"max_completion_tokens", "max_tokens"} {
			if n, ok, err := intField(m, k); err != nil {
				return sh, err
			} else if ok {
				sh.MaxTokens, sh.HasMaxTokens = n, true
				break
			}
		}
	case Completions:
		sh.Generates = true
		if sh.Choices, err = choices(m); err != nil {
			return sh, err
		}
		n, ok, err := intField(m, "max_tokens")
		if err != nil {
			return sh, err
		}
		if !ok {
			n = 16 // CompletionRequest.max_tokens default, null included
		}
		sh.MaxTokens, sh.HasMaxTokens = n, true
		sh.Inputs = max(1, countInputs(m["prompt"])+countInputs(m["prompt_embeds"]))
	case Responses:
		sh.Generates = true
		if n, ok, err := intField(m, "max_output_tokens"); err != nil {
			return sh, err
		} else if ok {
			sh.MaxTokens, sh.HasMaxTokens = n, true
		}
		if t := serverSideTool(m["tools"], responsesTool); t != "" {
			sh.Unbounded = "the built-in tool " + t + " makes the server generate again after each call, each time up to max_output_tokens"
		}
	case Messages:
		sh.Generates = true
		if n, ok, err := intField(m, "max_tokens"); err != nil {
			return sh, err
		} else if ok {
			sh.MaxTokens, sh.HasMaxTokens = n, true
		}
		if t := serverSideTool(m["tools"], anthropicClientTool); t != "" {
			sh.Unbounded = "the server tool " + t + " is priced per use"
		}
		for _, f := range []string{"mcp_servers", "container"} {
			if raw, ok := m[f]; ok && !isNull(raw) && !bytes.Equal(bytes.TrimSpace(raw), []byte("[]")) {
				sh.Unbounded = "\"" + f + "\" runs tools on the provider's side, priced per use"
			}
		}
	case Embeddings:
		sh.Inputs = max(1, countInputs(m["input"])+countInputs(m["messages"]))
	case Rerank:
		// The query and every document: at most one prompt each.
		sh.Inputs = 1 + max(1, countDocuments(m["documents"]))
	default:
		return sh, fmt.Errorf("no bound for endpoint %d", ep)
	}
	if sh.Inputs > maxShapeInputs {
		return sh, fmt.Errorf("too many inputs (%d, at most %d)", sh.Inputs, maxShapeInputs)
	}
	return sh, nil
}

// ClassifyShape is the shape of a pooling request (vLLM /classify, a guard
// classifier): its inputs, nothing generated.
func ClassifyShape(body []byte) (Shape, error) {
	m, err := decodeObject(body)
	if err != nil {
		return Shape{}, err
	}
	return Shape{Inputs: max(1, countInputs(m["input"])+countInputs(m["messages"])), Choices: 1}, nil
}

// choices reads n (and best_of, kept by vLLM's batch chat request): absent
// or null is 1. A value vLLM would coerce (a string, 2.0) is refused rather
// than read differently.
func choices(m map[string]json.RawMessage) (int64, error) {
	n := int64(1)
	for _, k := range []string{"n", "best_of"} {
		v, ok, err := intField(m, k)
		if err != nil {
			return 0, ErrBadN
		}
		if ok {
			if v < 1 {
				return 0, ErrBadN
			}
			n = max(n, v)
		}
	}
	return n, nil
}

// intField reads a non-negative JSON integer; absent or null is not ok.
func intField(m map[string]json.RawMessage, k string) (int64, bool, error) {
	raw, ok := m[k]
	if !ok || isNull(raw) {
		return 0, false, nil
	}
	// A JSON number only: encoding/json also decodes a numeric string into
	// a json.Number, which vLLM would coerce too.
	if t := bytes.TrimSpace(raw); len(t) == 0 || !(t[0] >= '0' && t[0] <= '9') {
		return 0, false, fmt.Errorf("%q must be a non-negative integer", k)
	}
	var num json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&num); err != nil {
		return 0, false, fmt.Errorf("%q must be a non-negative integer", k)
	}
	v, err := num.Int64()
	if err != nil || v < 0 || strings.ContainsAny(num.String(), ".eE") {
		return 0, false, fmt.Errorf("%q must be a non-negative integer", k)
	}
	return v, true, nil
}

// countInputs counts the prompts a pooling or completion input holds: a
// string or an object is one, a list of numbers (token ids) or of chat
// messages is one, any other list is one per element; absent or null is 0.
func countInputs(raw json.RawMessage) int64 {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || isNull(t) {
		return 0
	}
	if t[0] != '[' {
		return 1
	}
	var items []json.RawMessage
	if err := json.Unmarshal(t, &items); err != nil {
		return 1 // vLLM refuses it
	}
	if len(items) == 0 {
		return 1
	}
	numbers, messages := true, true
	for _, it := range items {
		it = bytes.TrimSpace(it)
		if len(it) == 0 || !(it[0] == '-' || it[0] >= '0' && it[0] <= '9') {
			numbers = false
		}
		if !isMessage(it) {
			messages = false
		}
	}
	if numbers || messages {
		return 1
	}
	return int64(len(items))
}

// isMessage reports a chat message: an object whose "role" is a string
// (vLLM _is_chat_message).
func isMessage(raw json.RawMessage) bool {
	if len(raw) == 0 || raw[0] != '{' {
		return false
	}
	var o struct {
		Role json.RawMessage `json:"role"`
	}
	if json.Unmarshal(raw, &o) != nil {
		return false
	}
	r := bytes.TrimSpace(o.Role)
	return len(r) > 0 && r[0] == '"'
}

// countDocuments counts rerank documents: a list (one each), an object
// with a content list (one per part), anything else one.
func countDocuments(raw json.RawMessage) int64 {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || isNull(t) {
		return 0
	}
	switch t[0] {
	case '[':
		var items []json.RawMessage
		if json.Unmarshal(t, &items) != nil {
			return 1
		}
		return int64(len(items))
	case '{':
		var o struct {
			Content []json.RawMessage `json:"content"`
		}
		if json.Unmarshal(t, &o) != nil {
			return 1
		}
		return max(1, int64(len(o.Content)))
	}
	return 1
}

// responsesTool reports a Responses tool the client runs itself (a
// function): every other type is built in and runs on the server.
func responsesTool(typ string) bool { return typ == "function" }

// anthropicClientTool reports a Messages tool the client runs itself: a
// custom tool (no type, or "custom"), or one of Anthropic's client tools
// (bash, text editor, computer use, memory). Server tools (web search, web
// fetch, code execution…) are priced per use.
func anthropicClientTool(typ string) bool {
	if typ == "" || typ == "custom" {
		return true
	}
	for _, p := range []string{"bash_", "text_editor_", "computer_", "memory_"} {
		if strings.HasPrefix(typ, p) {
			return true
		}
	}
	return false
}

// serverSideTool returns the type of the first tool that client does not
// accept, or "" (a tool list that does not parse is left to the server to
// refuse; its entries cannot then run).
func serverSideTool(raw json.RawMessage, client func(string) bool) string {
	if len(raw) == 0 || isNull(raw) {
		return ""
	}
	var tools []json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return "(unreadable tools)"
	}
	for _, t := range tools {
		var o map[string]json.RawMessage
		if json.Unmarshal(t, &o) != nil || o == nil {
			return "(unreadable tool)"
		}
		typ := ""
		if r, ok := o["type"]; ok && !isNull(r) {
			if json.Unmarshal(r, &typ) != nil {
				return "(unreadable tool type)"
			}
		}
		if !client(typ) {
			if len(typ) > 64 {
				typ = typ[:64]
			}
			return fmt.Sprintf("%q", typ)
		}
	}
	return ""
}

// SetMaxTokens writes the output bound m into a rewritten body, in every
// field vLLM (or the provider) may read for ep, and n when the body has
// one, as the integers the gateway counted.
func SetMaxTokens(ep Endpoint, body []byte, m int64) ([]byte, error) {
	if m < 0 || m > math.MaxInt32 {
		return nil, fmt.Errorf("max_tokens %d out of range", m)
	}
	o, err := decodeObject(body)
	if err != nil {
		return nil, err
	}
	v, _ := json.Marshal(m)
	switch ep {
	case Chat:
		o["max_completion_tokens"], o["max_tokens"] = v, v
	case Completions, Messages:
		o["max_tokens"] = v
	case Responses:
		o["max_output_tokens"] = v
	default:
		return body, nil
	}
	if ep == Chat || ep == Completions {
		// n and best_of as the integers the gateway read (ShapeOf refused
		// anything else), so vLLM cannot read them differently.
		for _, k := range []string{"n", "best_of"} {
			if n, ok, err := intField(o, k); err != nil {
				return nil, ErrBadN
			} else if ok {
				o[k], _ = json.Marshal(n)
			}
		}
	}
	return json.Marshal(o)
}
