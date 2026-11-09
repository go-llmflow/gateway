package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"slices"
	"sort"
)

// ErrBlocked ends a stream a guardrail refused; the error event has been
// written.
var ErrBlocked = errors.New("guard: stream blocked by a guardrail")

// maxPending bounds an event being assembled (the gateway's own SSE copy
// hands over whole events; this protects against one that never ends).
const maxPending = 1 << 20

// StreamFilter rewrites an OpenAI SSE stream (chat.completion.chunk or
// text_completion events) on its way to the client. The content of each
// choice goes through a Holdback (masking guards) and then a gate
// (classifying guards), and the events are rewritten to carry what was
// released. It is an io.Writer for whole events, as openai.CopySSE writes
// them.
//
// The gate judges the completion every CheckEvery new bytes and releases
// those bytes only once Check has accepted them: a classifier can refuse
// text before the client has seen it, at the price of delivering the text
// in bursts of CheckEvery bytes, each after one classifier call.
type StreamFilter struct {
	Dst io.Writer
	// NewHoldback returns the Holdback of one choice (nil: no masking).
	NewHoldback func() *Holdback
	// Spans is told what each release masked or flagged; an error blocks.
	Spans func(spans []Span) error
	// CheckEvery and Check: the gate (CheckEvery 0: no gate). Check gets the
	// choice's completion so far, and final once it has ended.
	CheckEvery int
	Check      func(choice int, completion string, final bool) error
	// BlockEvent is the data of the event written when the stream is
	// blocked.
	BlockEvent func(err error) []byte
	// MaskJSON, when set, lets streamed tool calls through: the arguments of
	// each call are held until the choice ends, masked whole (every JSON
	// layer, as for a non-streamed answer) and sent in one chunk. Without it
	// a stream that carries tool calls is stopped.
	MaskJSON func(input string) (string, error)

	pending []byte
	choices map[int]*choiceState
	env     map[string]json.RawMessage // the last chunk, to build a final one
	blocked bool
}

type choiceState struct {
	hb       *Holdback
	gate     []byte // released by the holdback, not yet checked
	sent     []byte // checked and sent
	finished bool

	// Reasoning text (reasoning_content, or reasoning) has its own holdback:
	// it is masked like the content, and sent under the field it came in.
	rhb    *Holdback
	rfield string
	// Tool calls held until the choice ends (MaskJSON), in the order they
	// first appeared.
	tools     map[int]*toolAcc
	toolOrder []int
	toolBytes int
}

type toolAcc struct {
	id, typ, name string
	args          []byte
}

// reasoningKeys are the delta fields that carry a model's reasoning text.
var reasoningKeys = []string{"reasoning_content", "reasoning"}

// Write accepts SSE bytes; it returns ErrBlocked once a guardrail refused.
func (f *StreamFilter) Write(p []byte) (int, error) {
	if f.blocked {
		return 0, ErrBlocked
	}
	f.pending = append(f.pending, p...)
	for {
		i := bytes.Index(f.pending, []byte("\n\n"))
		j := bytes.Index(f.pending, []byte("\r\n\r\n"))
		end, sep := i, 2
		if j >= 0 && (i < 0 || j < i) {
			end, sep = j, 4
		}
		if end < 0 {
			break
		}
		ev := f.pending[:end+sep]
		if err := f.event(ev); err != nil {
			return len(p), err
		}
		f.pending = f.pending[end+sep:]
	}
	if len(f.pending) > maxPending {
		return len(p), f.block(errors.New("guard: stream event too long to inspect"))
	}
	return len(p), nil
}

// Close releases what is held: the stream ended without [DONE].
func (f *StreamFilter) Close() error {
	if f.blocked {
		return ErrBlocked
	}
	if len(bytes.TrimSpace(f.pending)) > 0 {
		if err := f.event(append(f.pending, '\n', '\n')); err != nil {
			return err
		}
		f.pending = nil
	}
	return f.flushAll()
}

func (f *StreamFilter) block(err error) error {
	f.blocked = true
	if f.BlockEvent != nil {
		_, _ = f.Dst.Write(append(append([]byte("data: "), f.BlockEvent(err)...), "\n\ndata: [DONE]\n\n"...))
	}
	return ErrBlocked
}

func (f *StreamFilter) state(i int) *choiceState {
	if f.choices == nil {
		f.choices = map[int]*choiceState{}
	}
	c := f.choices[i]
	if c == nil {
		c = &choiceState{}
		if f.NewHoldback != nil {
			c.hb = f.NewHoldback()
			c.rhb = f.NewHoldback()
		}
		f.choices[i] = c
	}
	return c
}

// eventData returns the joined data lines of an event and whether it had
// any.
func eventData(ev []byte) ([]byte, bool) {
	var data []byte
	found := false
	for _, line := range bytes.Split(ev, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		if found {
			data = append(data, '\n')
		}
		data = append(data, bytes.TrimPrefix(line[5:], []byte(" "))...)
		found = true
	}
	return data, found
}

func (f *StreamFilter) event(ev []byte) error {
	data, ok := eventData(ev)
	if !ok {
		// Comments and keep-alives carry no text.
		_, err := f.Dst.Write(ev)
		return err
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("[DONE]")) {
		if err := f.flushAll(); err != nil {
			return err
		}
		_, err := f.Dst.Write(ev)
		return err
	}
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal(data, &chunk); err != nil || chunk == nil {
		// Text the filter cannot read cannot be let through unread.
		return f.block(errors.New("guard: unreadable stream event"))
	}
	// Only the fields the filter knows go out: anything else could carry
	// text it never inspected (FuzzStreamFilter released a secret from an
	// unknown key).
	keepOnly(chunk, chunkFields)
	f.env = chunk
	raw, ok := chunk["choices"]
	if !ok || isNull(raw) {
		return f.emit(chunk)
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &choices); err != nil {
		return f.block(errors.New("guard: unreadable stream choices"))
	}
	keep := false
	var toolChunks []int // choices that end in this chunk with tool calls held
	for _, c := range choices {
		if c == nil {
			return f.block(errors.New("guard: unreadable stream choice"))
		}
		keepOnly(c, choiceFields)
		var idx int
		if err := json.Unmarshal(c["index"], &idx); c["index"] != nil && err != nil {
			return f.block(errors.New("guard: unreadable choice index"))
		}
		st := f.state(idx)
		if lp, ok := c["logprobs"]; ok && !isNull(lp) {
			c["logprobs"] = json.RawMessage("null")
		}
		fields := deltaFields
		if st.rhb != nil {
			fields = deltaFieldsReasoning
		}
		if err := keepOnlyIn(c, "delta", fields); err != nil {
			return f.block(err)
		}
		finished := c["finish_reason"] != nil && !isNull(c["finish_reason"])
		if f.MaskJSON != nil {
			if err := f.takeTools(c, st); err != nil {
				return f.block(err)
			}
		}
		if err := f.passReasoning(c, st, finished); err != nil {
			return err
		}
		text, field, err := choiceText(c)
		if err != nil {
			return f.block(err)
		}
		if finished && len(st.toolOrder) > 0 {
			toolChunks = append(toolChunks, idx)
		}
		out, err := f.pass(idx, st, text, finished)
		if err != nil {
			return err
		}
		if field != "" || out != "" {
			if err := setChoiceText(c, field, out); err != nil {
				return f.block(err)
			}
		}
		if out != "" || finished || hasOther(c, field) {
			keep = true
		}
	}
	// The tool calls of a choice go out, masked, just before the chunk that
	// ends it.
	for _, idx := range toolChunks {
		if err := f.emitTools(idx, f.choices[idx], chunk); err != nil {
			return err
		}
	}
	if !keep && len(choices) > 0 {
		return nil
	}
	b, err := json.Marshal(choices)
	if err != nil {
		return f.block(err)
	}
	chunk["choices"] = b
	return f.emit(chunk)
}

// mayHave reports whether the raw JSON object d may hold the key name: its
// bytes are there, or any backslash is (a key can be written with escapes).
func mayHave(d []byte, name string) bool {
	return bytes.Contains(d, []byte(name)) || bytes.IndexByte(d, '\\') >= 0
}

// maxToolHold bounds the tool calls held for one choice.
const maxToolHold = 1 << 20

// takeTools removes the tool-call deltas of a choice from the chunk and keeps
// them in st until the choice ends.
func (f *StreamFilter) takeTools(c map[string]json.RawMessage, st *choiceState) error {
	d, ok := c["delta"]
	if !ok || isNull(d) || !mayHave(d, "tool_calls") {
		return nil // (the common delta: no decoding for nothing)
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(d, &delta); err != nil || delta == nil {
		return errors.New("guard: a stream delta is not an object")
	}
	raw, ok := delta["tool_calls"]
	if !ok || isNull(raw) {
		return nil
	}
	var calls []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &calls); err != nil {
		return errors.New("guard: streamed tool_calls is not a list of objects")
	}
	for pos, call := range calls {
		if call == nil {
			return errors.New("guard: a streamed tool call is not an object")
		}
		idx := pos
		if ib, ok := call["index"]; ok && !isNull(ib) {
			if err := json.Unmarshal(ib, &idx); err != nil || idx < 0 || idx > 1023 {
				return errors.New("guard: a streamed tool call has a bad index")
			}
		}
		acc := st.tools[idx]
		if acc == nil {
			if st.tools == nil {
				st.tools = map[int]*toolAcc{}
			}
			if len(st.tools) >= 128 {
				return errors.New("guard: too many streamed tool calls to inspect")
			}
			acc = &toolAcc{}
			st.tools[idx] = acc
			st.toolOrder = append(st.toolOrder, idx)
		}
		for k, v := range call {
			switch k {
			case "index":
			case "id", "type":
				var s string
				if err := json.Unmarshal(v, &s); err != nil || !metaRE.MatchString(s) {
					return errors.New("guard: a streamed tool call has a malformed " + k)
				}
				if k == "id" {
					acc.id += s
				} else {
					acc.typ += s
				}
			case "function":
				var fn map[string]json.RawMessage
				if err := json.Unmarshal(v, &fn); err != nil || fn == nil {
					return errors.New("guard: a streamed tool call has a malformed function")
				}
				for fk, fv := range fn {
					var s string
					if isNull(fv) {
						continue
					}
					if err := json.Unmarshal(fv, &s); err != nil {
						return errors.New("guard: a streamed tool call has a malformed function " + fk)
					}
					switch fk {
					case "name":
						if !metaRE.MatchString(s) {
							return errors.New("guard: a streamed tool call has a malformed name")
						}
						acc.name += s
					case "arguments":
						acc.args = append(acc.args, s...)
					default:
						return errors.New("guard: a streamed tool call has an unknown function field")
					}
					if st.toolBytes += len(s); st.toolBytes > maxToolHold {
						return errors.New("guard: streamed tool calls too long to inspect")
					}
				}
			default:
				// Anything else could carry text nobody read.
				return errors.New("guard: a streamed tool call has an unknown field")
			}
		}
	}
	delete(delta, "tool_calls")
	b, err := json.Marshal(delta)
	c["delta"] = b
	return err
}

// emitTools sends the held tool calls of a choice, their arguments masked
// whole, in one chunk built on env, and forgets them.
func (f *StreamFilter) emitTools(idx int, st *choiceState, env map[string]json.RawMessage) error {
	if len(st.toolOrder) == 0 {
		return nil
	}
	var calls []map[string]any
	for _, ti := range st.toolOrder {
		acc := st.tools[ti]
		args := string(acc.args)
		if args != "" {
			masked, err := f.MaskJSON(args)
			if err != nil {
				return f.block(err)
			}
			args = masked
		}
		fn := map[string]any{"arguments": args}
		if acc.name != "" {
			fn["name"] = acc.name
		}
		call := map[string]any{"index": ti, "function": fn}
		if acc.id != "" {
			call["id"] = acc.id
		}
		if acc.typ != "" {
			call["type"] = acc.typ
		}
		calls = append(calls, call)
	}
	st.tools, st.toolOrder, st.toolBytes = nil, nil, 0
	chunk := map[string]json.RawMessage{}
	for k, v := range env {
		if k != "choices" && k != "usage" {
			chunk[k] = v
		}
	}
	cb, err := json.Marshal([]map[string]any{{"index": idx, "delta": map[string]any{"tool_calls": calls}, "finish_reason": nil}})
	if err != nil {
		return f.block(err)
	}
	chunk["choices"] = cb
	return f.emit(chunk)
}

// passReasoning runs the reasoning text of a delta through the choice's
// reasoning holdback, and puts back what may be sent now (nothing: the field is
// removed). At the end of the choice what is held is released with it.
func (f *StreamFilter) passReasoning(c map[string]json.RawMessage, st *choiceState, final bool) error {
	if st.rhb == nil {
		return nil
	}
	d, ok := c["delta"]
	if !ok || isNull(d) {
		return nil
	}
	// (The common delta needs no decoding. A key written with escapes could
	// hide the name from this test, so any backslash decodes.)
	if !mayHave(d, "reasoning") && !(final && st.rhb.Held() > 0) {
		return nil
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(d, &delta); err != nil || delta == nil {
		return errors.New("guard: a stream delta is not an object")
	}
	var text string
	seen := ""
	for _, k := range reasoningKeys {
		v, ok := delta[k]
		if !ok {
			continue
		}
		delete(delta, k)
		if isNull(v) {
			continue
		}
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return f.block(errors.New("guard: a reasoning text is not a string"))
		}
		if seen != "" && seen != k {
			return f.block(errors.New("guard: reasoning under two names"))
		}
		seen, text = k, text+s
	}
	if seen != "" {
		st.rfield = seen
	}
	if seen == "" && !(final && st.rhb.Held() > 0) {
		return nil
	}
	out, spans := st.rhb.Push(text)
	if final {
		rest, more := st.rhb.Flush()
		out += rest
		spans = append(spans, more...)
	}
	if len(spans) > 0 && f.Spans != nil {
		if err := f.Spans(spans); err != nil {
			return f.block(err)
		}
	}
	if out != "" {
		b, _ := json.Marshal(out)
		delta[st.rfield] = b
	}
	b, err := json.Marshal(delta)
	c["delta"] = b
	return err
}

// pass runs one choice's new text through its holdback and gate and
// returns what may be sent now.
func (f *StreamFilter) pass(idx int, st *choiceState, text string, final bool) (string, error) {
	released := text
	if st.hb != nil {
		var spans []Span
		released, spans = st.hb.Push(text)
		if final {
			rest, more := st.hb.Flush()
			released += rest
			spans = append(spans, more...)
		}
		if len(spans) > 0 && f.Spans != nil {
			if err := f.Spans(spans); err != nil {
				return "", f.block(err)
			}
		}
	}
	if final {
		st.finished = true
	}
	if f.CheckEvery <= 0 || f.Check == nil {
		return released, nil
	}
	st.gate = append(st.gate, released...)
	if len(st.gate) < f.CheckEvery && !final {
		return "", nil
	}
	if err := f.Check(idx, string(st.sent)+string(st.gate), final); err != nil {
		return "", f.block(err)
	}
	out := string(st.gate)
	st.sent = append(st.sent, st.gate...)
	st.gate = st.gate[:0]
	return out, nil
}

// flushAll ends every choice not finished yet, in one final chunk.
func (f *StreamFilter) flushAll() error {
	idx := make([]int, 0, len(f.choices))
	for i, st := range f.choices {
		if !st.finished {
			idx = append(idx, i)
		}
	}
	if len(idx) == 0 {
		return nil
	}
	sort.Ints(idx)
	var choices []map[string]json.RawMessage
	for _, i := range idx {
		st := f.choices[i]
		ib, _ := json.Marshal(i)
		c := map[string]json.RawMessage{"index": ib, "delta": json.RawMessage("{}")}
		if err := f.passReasoning(c, st, true); err != nil {
			return err
		}
		out, err := f.pass(i, st, "", true)
		if err != nil {
			return err
		}
		if len(st.toolOrder) > 0 && f.env != nil {
			if err := f.emitTools(i, st, f.env); err != nil {
				return err
			}
		}
		if out == "" && string(c["delta"]) == "{}" {
			continue
		}
		if out != "" {
			if err := setChoiceText(c, "", out); err != nil {
				return err
			}
		}
		choices = append(choices, c)
	}
	if len(choices) == 0 || f.env == nil {
		return nil
	}
	chunk := map[string]json.RawMessage{}
	for k, v := range f.env {
		chunk[k] = v
	}
	delete(chunk, "usage")
	b, _ := json.Marshal(choices)
	chunk["choices"] = b
	return f.emit(chunk)
}

func (f *StreamFilter) emit(chunk map[string]json.RawMessage) error {
	b, err := json.Marshal(chunk)
	if err != nil {
		return f.block(err)
	}
	_, err = f.Dst.Write(append(append([]byte("data: "), b...), '\n', '\n'))
	return err
}

// choiceText returns the text of a streamed choice and where it is:
// "delta" (chat: delta.content) or "text" (completions); "" when it has
// none. Tool-call argument deltas are refused: they would bypass the
// masking.
func choiceText(c map[string]json.RawMessage) (string, string, error) {
	if t, ok := c["text"]; ok && !isNull(t) {
		var s string
		if err := json.Unmarshal(t, &s); err != nil {
			return "", "", errors.New("guard: a stream text is not a string")
		}
		return s, "text", nil
	}
	d, ok := c["delta"]
	if !ok || isNull(d) {
		return "", "", nil
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(d, &delta); err != nil {
		return "", "", errors.New("guard: a stream delta is not an object")
	}
	if tc, ok := delta["tool_calls"]; ok && !isNull(tc) {
		return "", "", errors.New("guard: streamed tool calls cannot be inspected; ask without stream")
	}
	cnt, ok := delta["content"]
	if !ok || isNull(cnt) {
		return "", "delta", nil
	}
	var s string
	if err := json.Unmarshal(cnt, &s); err != nil {
		return "", "", errors.New("guard: a delta content is not a string")
	}
	return s, "delta", nil
}

func setChoiceText(c map[string]json.RawMessage, field, text string) error {
	tb, err := json.Marshal(text)
	if err != nil {
		return err
	}
	if field == "text" {
		c["text"] = tb
		return nil
	}
	delta := map[string]json.RawMessage{}
	if d, ok := c["delta"]; ok && !isNull(d) {
		if err := json.Unmarshal(d, &delta); err != nil {
			return err
		}
	}
	delta["content"] = tb
	db, err := json.Marshal(delta)
	c["delta"] = db
	return err
}

// The fields that pass a guarded answer, at each level. Everything else
// (a refusal, audio, a field added by a later version) is text the guards did
// not inspect, so it is removed rather than let through. Reasoning text
// (reasoning_content, reasoning) is inspected: always in a whole answer, and in
// a stream when masking guards give it a holdback (deltaFieldsReasoning).
var (
	chunkFields          = []string{"id", "object", "created", "model", "choices", "usage", "system_fingerprint", "service_tier", "prompt_logprobs"}
	choiceFields         = []string{"index", "delta", "text", "message", "finish_reason", "stop_reason", "logprobs"}
	deltaFields          = []string{"role", "content", "tool_calls"}
	deltaFieldsReasoning = []string{"role", "content", "tool_calls", "reasoning_content", "reasoning"}
	messageFields        = []string{"role", "content", "tool_calls", "reasoning_content", "reasoning"}
)

func keepOnly(m map[string]json.RawMessage, allowed []string) {
	for k, v := range m {
		if !slices.Contains(allowed, k) || !wellTyped(k, v) {
			delete(m, k)
		}
	}
	if p, ok := m["prompt_logprobs"]; ok && !isNull(p) {
		m["prompt_logprobs"] = json.RawMessage("null")
	}
}

// metaRE is what an identifier field (id, model, object…) may hold.
var metaRE = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]{0,256}$`)

// wellTyped checks the metadata fields: identifiers are short names,
// created is a number, usage holds numbers only. A value of another shape
// is dropped: it could be text in a place the guards do not read.
func wellTyped(k string, v json.RawMessage) bool {
	switch k {
	case "id", "object", "model", "system_fingerprint", "service_tier", "finish_reason", "role",
		"status", "type", "call_id", "name":
		if isNull(v) {
			return true
		}
		var s string
		return json.Unmarshal(v, &s) == nil && metaRE.MatchString(s)
	case "created", "created_at", "index":
		var n float64
		return json.Unmarshal(v, &n) == nil
	case "stop_reason":
		// vLLM's stop_reason is a token id or the stop string matched,
		// Anthropic's a name (end_turn, tool_use…): a number or a short
		// name passes, free text goes.
		var n float64
		var s string
		return isNull(v) || json.Unmarshal(v, &n) == nil || json.Unmarshal(v, &s) == nil && metaRE.MatchString(s)
	case "usage":
		var u any
		return json.Unmarshal(v, &u) == nil && numericTree(u, 0)
	}
	return true
}

func numericTree(v any, depth int) bool {
	switch x := v.(type) {
	case nil, float64:
		return true
	case []any:
		// Responses usage has per-turn lists of counts.
		for _, e := range x {
			if _, ok := e.(float64); !ok {
				return false
			}
		}
		return true
	case map[string]any:
		if depth > 2 {
			return false
		}
		for _, e := range x {
			if !numericTree(e, depth+1) {
				return false
			}
		}
		return true
	}
	return false
}

// keepOnlyIn applies keepOnly to the object c[field], if any.
func keepOnlyIn(c map[string]json.RawMessage, field string, allowed []string) error {
	d, ok := c[field]
	if !ok || isNull(d) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(d, &obj); err != nil || obj == nil {
		return errors.New("guard: a " + field + " is not an object")
	}
	n := len(obj)
	keepOnly(obj, allowed)
	if len(obj) == n {
		return nil
	}
	b, err := json.Marshal(obj)
	c[field] = b
	return err
}

// hasOther reports whether a choice carries something besides its text (a
// role, a refusal…) that must still reach the client.
func hasOther(c map[string]json.RawMessage, field string) bool {
	if field != "delta" {
		return false
	}
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(c["delta"], &delta); err != nil {
		return false
	}
	for k, v := range delta {
		if k != "content" && !isNull(v) {
			return true
		}
	}
	return false
}
