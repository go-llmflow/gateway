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
	if raw, ok := top["instructions"]; ok && !isNull(raw) {
		// The Responses API's system prompt.
		nv, out, ch, err := mapContent(raw, "system", f)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, out...)
		if ch {
			top["instructions"], changed = nv, true
		}
	}
	for _, key := range []string{"prompt", "input"} {
		raw, ok := top[key]
		if !ok || isNull(raw) {
			continue
		}
		if key == "input" && isItemList(raw) {
			nv, out, ch, err := mapItems(raw, f)
			if err != nil {
				return nil, nil, err
			}
			msgs = append(msgs, out...)
			if ch {
				top[key], changed = nv, true
			}
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
		switch typ {
		case "text", "input_text", "output_text":
		case "refusal":
			// A refusal is text too (Responses output, chat parts).
			if _, ok := p["refusal"]; ok {
				var r string
				if err := json.Unmarshal(p["refusal"], &r); err != nil {
					return nil, nil, false, errors.New("guard: a refusal part has no refusal string")
				}
				nr := f(role, r)
				out = append(out, Message{role, nr})
				if nr != r {
					b, _ := json.Marshal(nr)
					p["refusal"], changed = b, true
				}
			}
			continue
		default:
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

// isItemList reports whether raw is a list whose first element is an
// object: Responses API input items rather than embeddings strings.
func isItemList(raw json.RawMessage) bool {
	var list []json.RawMessage
	if err := json.Unmarshal(raw, &list); err != nil || len(list) == 0 {
		return false
	}
	b := bytes.TrimSpace(list[0])
	return len(b) > 0 && b[0] == '{'
}

// mapItems handles Responses API input (or output) items: messages (with
// string or part content), function calls (arguments) and their outputs.
// An item of another type is refused: its text, if any, would go through
// uninspected.
func mapItems(raw json.RawMessage, f func(string, string) string) (json.RawMessage, []Message, bool, error) {
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, nil, false, errors.New("guard: input is not a list of items")
	}
	var out []Message
	changed := false
	for _, it := range items {
		if it == nil {
			return nil, nil, false, errors.New("guard: an input item is not an object")
		}
		var typ, role string
		_ = json.Unmarshal(it["type"], &typ)
		_ = json.Unmarshal(it["role"], &role)
		field := ""
		switch {
		case typ == "message" || typ == "" && role != "":
			field = "content"
			if role == "" {
				role = "user"
			}
		case typ == "function_call":
			field, role = "arguments", "tool_call"
		case typ == "function_call_output":
			field, role = "output", "tool"
		default:
			return nil, nil, false, errors.New("guard: input items of type " + strconvQuote(typ) + " cannot be inspected")
		}
		raw, ok := it[field]
		if !ok || isNull(raw) {
			continue
		}
		nv, msgs, ch, err := mapContent(raw, role, f)
		if err != nil {
			return nil, nil, false, err
		}
		out = append(out, msgs...)
		if ch {
			it[field], changed = nv, true
		}
	}
	if !changed {
		return raw, out, false, nil
	}
	b, err := json.Marshal(items)
	return b, out, true, err
}

// strconvQuote quotes a short item type for an error message (the type is
// a name, not content; it is cut to stay one).
func strconvQuote(s string) string {
	if len(s) > 32 {
		s = s[:32]
	}
	b, _ := json.Marshal(s)
	return string(b)
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
	if _, ok := top["output"]; ok {
		if _, chat := top["choices"]; !chat {
			return mapResponsesOutput(top, strip, f)
		}
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
			var parts []map[string]json.RawMessage
			if strip && json.Unmarshal(mc, &parts) == nil {
				n := len(parts)
				parts = textPartsOnly(parts)
				b, _ := json.Marshal(parts)
				mch = mch || len(parts) != n || string(b) != string(mc)
				mc = b
				msg["content"] = b
			}
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

// The fields of a guarded Responses answer, and of its output items.
var (
	responsesFields = []string{"id", "object", "created_at", "model", "status", "output", "usage", "incomplete_details"}
	messageItem     = []string{"id", "type", "role", "status", "content"}
	callItem        = []string{"id", "type", "call_id", "name", "arguments", "status"}
	textPart        = []string{"type", "text", "refusal"}
)

// mapResponsesOutput is MapResponse for a Responses API answer: the text
// of output messages and the arguments of function calls go through f.
// With strip, only those items and the known fields stay; reasoning items
// and anything else are removed (not inspected, so not let through).
func mapResponsesOutput(top map[string]json.RawMessage, strip bool, f func(string) string) ([]byte, []string, error) {
	if strip {
		keepOnly(top, responsesFields)
		if d, ok := top["incomplete_details"]; ok && !isNull(d) {
			top["incomplete_details"] = keepReason(d)
		}
	}
	var items []map[string]json.RawMessage
	if raw := top["output"]; !isNull(raw) {
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, nil, errors.New("guard: output is not a list of items")
		}
	}
	g := func(_, s string) string { return f(s) }
	var texts []string
	kept := items[:0]
	for _, it := range items {
		if it == nil {
			return nil, nil, errors.New("guard: an output item is not an object")
		}
		var typ string
		_ = json.Unmarshal(it["type"], &typ)
		switch typ {
		case "message":
			if strip {
				keepOnly(it, messageItem)
			}
			var parts []map[string]json.RawMessage
			if c := it["content"]; c != nil && !isNull(c) {
				if err := json.Unmarshal(c, &parts); err != nil {
					return nil, nil, errors.New("guard: an output message content is not a list of parts")
				}
			}
			if strip {
				parts = textPartsOnly(parts)
			}
			for _, p := range parts {
				if p == nil {
					return nil, nil, errors.New("guard: an output part is not an object")
				}
			}
			b, _ := json.Marshal(parts)
			nc, out, _, err := mapContent(b, "assistant", g)
			if err != nil {
				return nil, nil, err
			}
			for _, m := range out {
				texts = append(texts, m.Content)
			}
			it["content"] = nc
		case "function_call":
			if strip {
				keepOnly(it, callItem)
			}
			if a, ok := it["arguments"]; ok && !isNull(a) {
				na, out, _, err := mapContent(a, "tool_call", g)
				if err != nil {
					return nil, nil, err
				}
				for _, m := range out {
					texts = append(texts, m.Content)
				}
				it["arguments"] = na
			}
		default:
			if strip {
				continue
			}
		}
		kept = append(kept, it)
	}
	ob, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, err
	}
	top["output"] = ob
	b, err := json.Marshal(top)
	return b, texts, err
}

// textPartsOnly keeps the parts of an answer the guards read (text,
// output_text, refusal), with only their known fields; any other part —
// no type, an unknown one — would go out uninspected, so it goes.
// FuzzMapResponse found a part without a type carrying text through.
func textPartsOnly(parts []map[string]json.RawMessage) []map[string]json.RawMessage {
	kept := parts[:0]
	for _, p := range parts {
		if p == nil {
			continue
		}
		var typ string
		_ = json.Unmarshal(p["type"], &typ)
		switch typ {
		case "text", "output_text", "refusal":
			keepOnly(p, textPart)
			kept = append(kept, p)
		}
	}
	return kept
}

// keepReason keeps only a string "reason" of incomplete_details.
func keepReason(raw json.RawMessage) json.RawMessage {
	var d struct {
		Reason string `json:"reason"`
	}
	if json.Unmarshal(raw, &d) != nil || !metaRE.MatchString(d.Reason) {
		return json.RawMessage("null")
	}
	b, _ := json.Marshal(d)
	return b
}

func isNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }
