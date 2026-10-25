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
	// The system prompt outside messages: the Responses API's
	// instructions, the Anthropic Messages API's system (a string or text
	// blocks).
	for _, key := range []string{"instructions", "system"} {
		raw, ok := top[key]
		if !ok || isNull(raw) {
			continue
		}
		nv, out, ch, err := mapContent(raw, "system", f)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, out...)
		if ch {
			top[key], changed = nv, true
		}
	}
	// Rerank: the query (a string or content parts) and the documents (a
	// string or a list of strings; document objects are items mapItems
	// refuses, as it cannot read them).
	if raw, ok := top["query"]; ok && !isNull(raw) {
		nv, out, ch, err := mapContent(raw, "user", f)
		if err != nil {
			return nil, nil, err
		}
		msgs = append(msgs, out...)
		if ch {
			top["query"], changed = nv, true
		}
	}
	for _, key := range []string{"prompt", "input", "documents"} {
		raw, ok := top[key]
		if !ok || isNull(raw) {
			continue
		}
		if key != "prompt" && isItemList(raw) {
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
		if p == nil {
			return nil, nil, false, errors.New("guard: a content part is not an object")
		}
		var typ string
		_ = json.Unmarshal(p["type"], &typ)
		field, partRole, nested := "", role, false
		switch typ {
		case "text", "input_text", "output_text":
			field = "text"
		case "refusal":
			// A refusal is text too (Responses output, chat parts).
			field = "refusal"
		case "thinking":
			// Anthropic thinking blocks sent back in the history.
			field = "thinking"
		case "tool_result":
			// Anthropic: a string or a list of blocks.
			field, partRole, nested = "content", "tool", true
		case "tool_use":
			// Anthropic: the call's input is a JSON object; every string
			// in it is text.
			if in, ok := p["input"]; ok && !isNull(in) {
				nv, msgs, ch, err := mapJSONStrings(in, "tool_call", f, 0)
				if err != nil {
					return nil, nil, false, err
				}
				out = append(out, msgs...)
				if ch {
					p["input"], changed = nv, true
				}
			}
			continue
		case "image", "image_url", "input_image", "input_audio", "audio_url", "video_url", "file", "input_file", "redacted_thinking":
			// Media and opaque blobs: no text to read.
			continue
		default:
			// A part the guards do not know may carry text (an Anthropic
			// "document" with a text source does): refused, not passed.
			return nil, nil, false, errors.New("guard: content parts of type " + strconvQuote(typ) + " cannot be inspected")
		}
		raw, ok := p[field]
		if !ok || isNull(raw) {
			continue
		}
		var nv json.RawMessage
		var msgs []Message
		var ch bool
		if nested {
			var err error
			if nv, msgs, ch, err = mapContent(raw, partRole, f); err != nil {
				return nil, nil, false, err
			}
		} else {
			var text string
			if err := json.Unmarshal(raw, &text); err != nil {
				return nil, nil, false, errors.New("guard: a " + typ + " part has no " + field + " string")
			}
			nt := f(partRole, text)
			msgs = []Message{{partRole, nt}}
			if nt != text {
				nv, _ = json.Marshal(nt)
				ch = true
			}
		}
		out = append(out, msgs...)
		if ch {
			p[field], changed = nv, true
		}
	}
	if !changed {
		return raw, out, false, nil
	}
	b, err := json.Marshal(parts)
	return b, out, true, err
}

// maxJSONDepth bounds the nesting mapJSONStrings walks.
const maxJSONDepth = 32

// mapJSONStrings calls f on every string value of a JSON value (an
// Anthropic tool_use input), keys excepted.
func mapJSONStrings(raw json.RawMessage, role string, f func(string, string) string, depth int) (json.RawMessage, []Message, bool, error) {
	if depth > maxJSONDepth {
		return nil, nil, false, errors.New("guard: a tool input is nested too deeply")
	}
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return raw, nil, false, nil
	}
	switch b[0] {
	case '"':
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return nil, nil, false, err
		}
		ns := f(role, s)
		if ns == s {
			return raw, []Message{{role, s}}, false, nil
		}
		nb, err := json.Marshal(ns)
		return nb, []Message{{role, ns}}, true, err
	case '{':
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(b, &obj); err != nil {
			return nil, nil, false, err
		}
		var out []Message
		changed := false
		for k, v := range obj {
			nv, msgs, ch, err := mapJSONStrings(v, role, f, depth+1)
			if err != nil {
				return nil, nil, false, err
			}
			out = append(out, msgs...)
			if ch {
				obj[k], changed = nv, true
			}
		}
		if !changed {
			return raw, out, false, nil
		}
		nb, err := json.Marshal(obj)
		return nb, out, true, err
	case '[':
		var list []json.RawMessage
		if err := json.Unmarshal(b, &list); err != nil {
			return nil, nil, false, err
		}
		var out []Message
		changed := false
		for i, v := range list {
			nv, msgs, ch, err := mapJSONStrings(v, role, f, depth+1)
			if err != nil {
				return nil, nil, false, err
			}
			out = append(out, msgs...)
			if ch {
				list[i], changed = nv, true
			}
		}
		if !changed {
			return raw, out, false, nil
		}
		nb, err := json.Marshal(list)
		return nb, out, true, err
	}
	return raw, nil, false, nil
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
	var typ string
	_ = json.Unmarshal(top["type"], &typ)
	if _, chat := top["choices"]; !chat && typ == "message" {
		return mapMessagesOutput(top, strip, f)
	}
	if _, ok := top["results"]; ok {
		if _, chat := top["choices"]; !chat {
			return mapRerankOutput(top, strip, f)
		}
	}
	if _, ok := top["text"]; ok {
		if _, chat := top["choices"]; !chat {
			return mapTranscriptOutput(top, strip, f)
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

// The fields of a guarded Anthropic message and of its blocks.
var (
	messagesFields = []string{"id", "type", "role", "model", "content", "stop_reason", "usage"}
	textBlock      = []string{"type", "text"}
	toolUseBlock   = []string{"type", "id", "name", "input"}
)

// mapMessagesOutput is MapResponse for an Anthropic Messages answer: text
// blocks and every string of tool_use inputs go through f. With strip,
// thinking and any other block are removed, and only known fields stay.
func mapMessagesOutput(top map[string]json.RawMessage, strip bool, f func(string) string) ([]byte, []string, error) {
	if strip {
		keepOnly(top, messagesFields)
	}
	var blocks []map[string]json.RawMessage
	if raw := top["content"]; raw != nil && !isNull(raw) {
		if err := json.Unmarshal(raw, &blocks); err != nil {
			return nil, nil, errors.New("guard: content is not a list of blocks")
		}
	}
	g := func(_, s string) string { return f(s) }
	var texts []string
	kept := blocks[:0]
	for _, b := range blocks {
		if b == nil {
			return nil, nil, errors.New("guard: a content block is not an object")
		}
		var typ string
		_ = json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text":
			if strip {
				keepOnly(b, textBlock)
			}
			var s string
			if err := json.Unmarshal(b["text"], &s); err != nil {
				return nil, nil, errors.New("guard: a text block has no text string")
			}
			ns := f(s)
			texts = append(texts, ns)
			b["text"], _ = json.Marshal(ns)
		case "tool_use":
			if strip {
				keepOnly(b, toolUseBlock)
			}
			if in, ok := b["input"]; ok && !isNull(in) {
				nv, msgs, _, err := mapJSONStrings(in, "tool_call", g, 0)
				if err != nil {
					return nil, nil, err
				}
				for _, m := range msgs {
					texts = append(texts, m.Content)
				}
				b["input"] = nv
			}
		default:
			if strip {
				continue
			}
		}
		kept = append(kept, b)
	}
	cb, err := json.Marshal(kept)
	if err != nil {
		return nil, nil, err
	}
	top["content"] = cb
	out, err := json.Marshal(top)
	return out, texts, err
}

// mapTranscriptOutput is MapResponse for a JSON transcription: the text
// goes through f. With strip, the segments and words (which repeat the
// text piece by piece) and every other field go, but the usage, duration
// and language.
func mapTranscriptOutput(top map[string]json.RawMessage, strip bool, f func(string) string) ([]byte, []string, error) {
	var s string
	if err := json.Unmarshal(top["text"], &s); err != nil {
		return nil, nil, errors.New("guard: a transcription text is not a string")
	}
	if strip {
		for k, v := range top {
			switch k {
			case "text", "usage":
			case "duration":
				var n float64
				if json.Unmarshal(v, &n) != nil {
					delete(top, k)
				}
			case "language":
				var l string
				if json.Unmarshal(v, &l) != nil || !metaRE.MatchString(l) {
					delete(top, k)
				}
			default:
				delete(top, k)
			}
		}
		if u, ok := top["usage"]; ok && !wellTyped("usage", u) {
			// {type: "duration", seconds}: a name and a number.
			var d struct {
				Type    string  `json:"type"`
				Seconds float64 `json:"seconds"`
			}
			if json.Unmarshal(u, &d) != nil || !metaRE.MatchString(d.Type) {
				delete(top, "usage")
			} else {
				top["usage"], _ = json.Marshal(d)
			}
		}
	}
	ns := f(s)
	top["text"], _ = json.Marshal(ns)
	out, err := json.Marshal(top)
	return out, []string{ns}, err
}

// mapRerankOutput is MapResponse for a rerank answer: the documents it
// echoes go through f (they were masked on the way in already; this keeps
// it so whatever the upstream returns). With strip, a result keeps only
// index, relevance_score and document.text.
func mapRerankOutput(top map[string]json.RawMessage, strip bool, f func(string) string) ([]byte, []string, error) {
	if strip {
		keepOnly(top, []string{"id", "model", "usage", "results"})
	}
	var results []map[string]json.RawMessage
	if raw := top["results"]; !isNull(raw) {
		if err := json.Unmarshal(raw, &results); err != nil {
			return nil, nil, errors.New("guard: results is not a list of objects")
		}
	}
	var texts []string
	for _, r := range results {
		if r == nil {
			return nil, nil, errors.New("guard: a result is not an object")
		}
		if strip {
			for k := range r {
				if k != "index" && k != "relevance_score" && k != "document" {
					delete(r, k)
				}
			}
			for _, k := range []string{"index", "relevance_score"} {
				var n float64
				if v, ok := r[k]; ok && json.Unmarshal(v, &n) != nil {
					delete(r, k)
				}
			}
		}
		d, ok := r["document"]
		if !ok || isNull(d) {
			continue
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(d, &doc); err != nil || doc == nil {
			if strip {
				delete(r, "document")
			}
			continue
		}
		if strip {
			keepOnly(doc, []string{"text"})
		}
		if t, ok := doc["text"]; ok {
			var s string
			if err := json.Unmarshal(t, &s); err != nil {
				return nil, nil, errors.New("guard: a document text is not a string")
			}
			ns := f(s)
			texts = append(texts, ns)
			doc["text"], _ = json.Marshal(ns)
		}
		r["document"], _ = json.Marshal(doc)
	}
	rb, err := json.Marshal(results)
	if err != nil {
		return nil, nil, err
	}
	top["results"] = rb
	out, err := json.Marshal(top)
	return out, texts, err
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
