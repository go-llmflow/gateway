// Package guard holds the guardrail machinery that does not depend on the
// server: where the text of a request and of a response lies in the OpenAI
// JSON, the bounded hold-back that masks a stream, the SSE rewriter, and the
// parsers of what guard models and guard services answer.
package guard

import (
	"bytes"
	"encoding/json"
	"errors"
)

// Message is one piece of conversation text with its role: "system",
// "user", "assistant", "tool", or "tool_call" for the arguments of a call
// the assistant made, and "prompt"/"input" for the completions and
// embeddings endpoints.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

var errNotObject = errors.New("guard: the body is not a JSON object")

// MapRequest calls f on every text of an OpenAI request body (chat
// messages, including text parts and tool-call arguments; the completions
// prompt; the embeddings input) and returns the body with the texts f
// changed. It also returns the texts, after f, as messages. A body whose
// texts f leaves alone is returned as is.
func MapRequest(body []byte, f func(role, text string) string) ([]byte, []Message, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, nil, errNotObject
	}
	var msgs []Message
	changed := false
	if raw, ok := top["messages"]; ok && !isNull(raw) {
		var list []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &list); err != nil {
			return nil, nil, errors.New("guard: messages is not a list of objects")
		}
		for _, m := range list {
			var role string
			_ = json.Unmarshal(m["role"], &role)
			if c, ok := m["content"]; ok {
				nc, out, ch, err := mapContent(c, role, f)
				if err != nil {
					return nil, nil, err
				}
				msgs = append(msgs, out...)
				if ch {
					m["content"], changed = nc, true
				}
			}
			if tc, ok := m["tool_calls"]; ok && !isNull(tc) {
				ntc, out, ch, err := mapToolCalls(tc, f)
				if err != nil {
					return nil, nil, err
				}
				msgs = append(msgs, out...)
				if ch {
					m["tool_calls"], changed = ntc, true
				}
			}
		}
		if changed {
			b, err := json.Marshal(list)
			if err != nil {
				return nil, nil, err
			}
			top["messages"] = b
		}
	}
	for _, key := range []string{"prompt", "input"} {
		raw, ok := top[key]
		if !ok || isNull(raw) {
			continue
		}
		nv, out, ch, err := mapStrings(raw, key, f)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, out...)
		if ch {
			top[key], changed = nv, true
		}
	}
	if !changed {
		return body, msgs, nil
	}
	b, err := json.Marshal(top)
	return b, msgs, err
}

// mapContent handles a message content: a string, or a list of parts of
// which those of type "text" carry text.
func mapContent(raw json.RawMessage, role string, f func(string, string) string) (json.RawMessage, []Message, bool, error) {
	if isNull(raw) {
		return raw, nil, false, nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		ns := f(role, s)
		if ns == s {
			return raw, []Message{{role, s}}, false, nil
		}
		b, err := json.Marshal(ns)
		return b, []Message{{role, ns}}, true, err
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, nil, false, errors.New("guard: content is neither a string nor a list of parts")
	}
	var out []Message
	changed := false
	for _, p := range parts {
		var typ, text string
		_ = json.Unmarshal(p["type"], &typ)
		if typ != "text" {
			continue
		}
		if err := json.Unmarshal(p["text"], &text); err != nil {
			return nil, nil, false, errors.New("guard: a text part has no text string")
		}
		nt := f(role, text)
		out = append(out, Message{role, nt})
		if nt != text {
			b, err := json.Marshal(nt)
			if err != nil {
				return nil, nil, false, err
			}
			p["text"], changed = b, true
		}
	}
	if !changed {
		return raw, out, false, nil
	}
	b, err := json.Marshal(parts)
	return b, out, true, err
}

// mapToolCalls handles the function arguments of an assistant's calls.
func mapToolCalls(raw json.RawMessage, f func(string, string) string) (json.RawMessage, []Message, bool, error) {
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil, nil, false, errors.New("guard: tool_calls is not a list of objects")
	}
	var out []Message
	changed := false
	for _, c := range calls {
		var fn map[string]json.RawMessage
		if err := json.Unmarshal(c["function"], &fn); err != nil || fn == nil {
			continue
		}
		var args string
		if err := json.Unmarshal(fn["arguments"], &args); err != nil {
			continue
		}
		na := f("tool_call", args)
		out = append(out, Message{"tool_call", na})
		if na != args {
			b, _ := json.Marshal(na)
			fn["arguments"] = b
			nf, err := json.Marshal(fn)
			if err != nil {
				return nil, nil, false, err
			}
			c["function"], changed = nf, true
		}
	}
	if !changed {
		return raw, out, false, nil
	}
	b, err := json.Marshal(calls)
	return b, out, true, err
}

// mapStrings handles a string or a list of strings (token arrays are left
// alone: they are not text).
func mapStrings(raw json.RawMessage, role string, f func(string, string) string) (json.RawMessage, []Message, bool, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		ns := f(role, s)
		if ns == s {
			return raw, []Message{{role, s}}, false, nil
		}
		b, err := json.Marshal(ns)
		return b, []Message{{role, ns}}, true, err
	}
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil {
		return raw, nil, false, nil
	}
	var out []Message
	changed := false
	for i, item := range list {
		var s string
		if err := json.Unmarshal(item, &s); err != nil {
			continue
		}
		ns := f(role, s)
		out = append(out, Message{role, ns})
		if ns != s {
			b, err := json.Marshal(ns)
			if err != nil {
				return nil, nil, false, err
			}
			list[i], changed = b, true
		}
	}
	if !changed {
		return raw, out, false, nil
	}
	b, err := json.Marshal(list)
	return b, out, true, err
}

// MapResponse calls f on every text of a non-streamed OpenAI response
// (choices[].message.content and tool-call arguments, choices[].text) and
// returns the body with the texts f changed, and the texts after f. When
// strip is set, only the fields the guards know are kept (as in the stream
// filter) and log probabilities are removed: they repeat the text token by
// token, so a masked answer would still carry what was masked.
func MapResponse(body []byte, strip bool, f func(text string) string) ([]byte, []string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, nil, errNotObject
	}
	changed := false
	if strip {
		n := len(top)
		keepOnly(top, chunkFields)
		changed = len(top) != n
	}
	raw, ok := top["choices"]
	if !ok || isNull(raw) {
		if changed {
			b, err := json.Marshal(top)
			return b, nil, err
		}
		return body, nil, nil
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &choices); err != nil {
		return nil, nil, errors.New("guard: choices is not a list of objects")
	}
	var texts []string
	g := func(_, s string) string { return f(s) }
	for _, c := range choices {
		if c == nil {
			return nil, nil, errors.New("guard: a choice is not an object")
		}
		if strip {
			n := len(c)
			keepOnly(c, choiceFields)
			if lp, ok := c["logprobs"]; ok && !isNull(lp) {
				c["logprobs"] = json.RawMessage("null")
			}
			before := string(c["message"])
			if err := keepOnlyIn(c, "message", messageFields); err != nil {
				return nil, nil, err
			}
			changed = changed || len(c) != n || string(c["message"]) != before || string(c["logprobs"]) == "null"
		}
		if t, ok := c["text"]; ok {
			nt, out, ch, err := mapStrings(t, "assistant", g)
			if err != nil {
				return nil, nil, err
			}
			for _, m := range out {
				texts = append(texts, m.Content)
			}
			if ch {
				c["text"], changed = nt, true
			}
		}
		var msg map[string]json.RawMessage
		if err := json.Unmarshal(c["message"], &msg); err != nil || msg == nil {
			continue
		}
		mch := false
		if mc, ok := msg["content"]; ok {
			nc, out, ch, err := mapContent(mc, "assistant", g)
			if err != nil {
				return nil, nil, err
			}
			for _, m := range out {
				texts = append(texts, m.Content)
			}
			if ch {
				msg["content"], mch = nc, true
			}
		}
		if tc, ok := msg["tool_calls"]; ok && !isNull(tc) {
			ntc, out, ch, err := mapToolCalls(tc, g)
			if err != nil {
				return nil, nil, err
			}
			for _, m := range out {
				texts = append(texts, m.Content)
			}
			if ch {
				msg["tool_calls"], mch = ntc, true
			}
		}
		if mch {
			b, err := json.Marshal(msg)
			if err != nil {
				return nil, nil, err
			}
			c["message"], changed = b, true
		}
	}
	if !changed {
		return body, texts, nil
	}
	b, err := json.Marshal(choices)
	if err != nil {
		return nil, nil, err
	}
	top["choices"] = b
	out, err := json.Marshal(top)
	return out, texts, err
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }
