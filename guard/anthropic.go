package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
)

// AnthropicStreamFilter rewrites an Anthropic Messages event stream (as
// vLLM v0.32.0 emits it: message_start, content_block_start/delta/stop,
// message_delta, message_stop, ping, error) on its way to the client, the
// way StreamFilter does for OpenAI chunks:
//
//   - text blocks (text_delta) and tool_use blocks (input_json_delta, the
//     JSON of the call's input as it is generated) go through a Holdback
//     each and through the gate; a masked span inside the JSON text is
//     replaced by a placeholder with no quote or backslash, so the JSON
//     stays well formed;
//   - thinking and redacted_thinking blocks are removed (not inspected), and
//     the blocks that remain are renumbered from 0 so the client's content
//     list has no hole;
//   - only known events and fields pass; an unknown event, or one that
//     cannot be read, stops the stream.
type AnthropicStreamFilter struct {
	Dst         io.Writer
	NewHoldback func() *Holdback
	Spans       func(spans []Span) error
	CheckEvery  int
	Check       func(block int, completion string, final bool) error
	BlockEvent  func(err error) []byte

	pending []byte
	blocks  map[int]*anthropicBlock
	next    int // the next client index
	blocked bool
}

type anthropicBlock struct {
	kind  string // "text", "json", "drop"
	index int    // client index
	ch    choiceState
}

// Write accepts SSE bytes.
func (f *AnthropicStreamFilter) Write(p []byte) (int, error) {
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
		if err := f.event(f.pending[:end+sep]); err != nil {
			return len(p), err
		}
		f.pending = f.pending[end+sep:]
	}
	if len(f.pending) > maxPending {
		return len(p), f.block(errors.New("guard: stream event too long to inspect"))
	}
	return len(p), nil
}

// Close ends the blocks still open: the stream ended without their stop.
func (f *AnthropicStreamFilter) Close() error {
	if f.blocked {
		return ErrBlocked
	}
	if len(bytes.TrimSpace(f.pending)) > 0 {
		if err := f.event(append(f.pending, '\n', '\n')); err != nil {
			return err
		}
		f.pending = nil
	}
	idx := make([]int, 0, len(f.blocks))
	for i := range f.blocks {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		if err := f.stopBlock(i); err != nil {
			return err
		}
	}
	return nil
}

func (f *AnthropicStreamFilter) block(err error) error {
	f.blocked = true
	if f.BlockEvent != nil {
		ev := append([]byte("event: error\ndata: "), f.BlockEvent(err)...)
		_, _ = f.Dst.Write(append(ev, '\n', '\n'))
	}
	return ErrBlocked
}

func (f *AnthropicStreamFilter) emit(typ string, ev map[string]any) error {
	ev["type"] = typ
	b, err := json.Marshal(ev)
	if err != nil {
		return f.block(err)
	}
	out := append([]byte("event: "+typ+"\ndata: "), b...)
	_, err = f.Dst.Write(append(out, '\n', '\n'))
	return err
}

func (f *AnthropicStreamFilter) event(ev []byte) error {
	data, ok := eventData(ev)
	if !ok {
		_, err := f.Dst.Write(ev) // comments, keep-alives
		return err
	}
	var e map[string]json.RawMessage
	if err := json.Unmarshal(data, &e); err != nil || e == nil {
		return f.block(errors.New("guard: unreadable stream event"))
	}
	var typ string
	_ = json.Unmarshal(e["type"], &typ)
	switch typ {
	case "ping", "message_stop":
		return f.emit(typ, map[string]any{})
	case "message_start":
		var m struct {
			Message map[string]json.RawMessage `json:"message"`
		}
		if json.Unmarshal(data, &m) != nil || m.Message == nil {
			return f.block(errors.New("guard: unreadable message_start"))
		}
		keepOnly(m.Message, []string{"id", "type", "role", "model", "usage"})
		m.Message["content"] = json.RawMessage("[]")
		return f.emit(typ, map[string]any{"message": m.Message})
	case "message_delta":
		var m struct {
			Delta map[string]json.RawMessage `json:"delta"`
			Usage json.RawMessage            `json:"usage"`
		}
		if json.Unmarshal(data, &m) != nil {
			return f.block(errors.New("guard: unreadable message_delta"))
		}
		out := map[string]any{}
		if m.Delta != nil {
			keepOnly(m.Delta, []string{"stop_reason"})
			out["delta"] = m.Delta
		}
		if m.Usage != nil && wellTyped("usage", m.Usage) {
			out["usage"] = m.Usage
		}
		return f.emit(typ, out)
	case "content_block_start":
		return f.startBlock(data)
	case "content_block_delta":
		return f.deltaBlock(data)
	case "content_block_stop":
		var m struct {
			Index *int `json:"index"`
		}
		if json.Unmarshal(data, &m) != nil || m.Index == nil {
			return f.block(errors.New("guard: unreadable content_block_stop"))
		}
		return f.stopBlock(*m.Index)
	case "error":
		// An upstream error's message may quote content: only its type
		// goes on.
		var m struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &m)
		if !metaRE.MatchString(m.Error.Type) {
			m.Error.Type = "api_error"
		}
		return f.emit(typ, map[string]any{"error": map[string]any{"type": m.Error.Type, "message": "the model server reported an error"}})
	}
	return f.block(errors.New("guard: unknown stream event"))
}

func (f *AnthropicStreamFilter) startBlock(data []byte) error {
	var m struct {
		Index *int                       `json:"index"`
		Block map[string]json.RawMessage `json:"content_block"`
	}
	if json.Unmarshal(data, &m) != nil || m.Index == nil || m.Block == nil {
		return f.block(errors.New("guard: unreadable content_block_start"))
	}
	if f.blocks == nil {
		f.blocks = map[int]*anthropicBlock{}
	}
	if f.blocks[*m.Index] != nil {
		return f.block(errors.New("guard: a content block started twice"))
	}
	var typ string
	_ = json.Unmarshal(m.Block["type"], &typ)
	b := &anthropicBlock{}
	var start map[string]any
	switch typ {
	case "text":
		b.kind = "text"
		start = map[string]any{"type": "text", "text": ""}
	case "tool_use":
		b.kind = "json"
		keepOnly(m.Block, []string{"type", "id", "name"})
		start = map[string]any{"type": "tool_use", "id": m.Block["id"], "name": m.Block["name"], "input": map[string]any{}}
	case "thinking", "redacted_thinking":
		b.kind = "drop"
		f.blocks[*m.Index] = b
		return nil
	default:
		return f.block(errors.New("guard: content blocks of this type cannot be inspected"))
	}
	if f.NewHoldback != nil {
		b.ch.hb = f.NewHoldback()
	}
	b.index = f.next
	f.next++
	f.blocks[*m.Index] = b
	return f.emit("content_block_start", map[string]any{"index": b.index, "content_block": start})
}

func (f *AnthropicStreamFilter) deltaBlock(data []byte) error {
	var m struct {
		Index *int `json:"index"`
		Delta struct {
			Type        string  `json:"type"`
			Text        *string `json:"text"`
			PartialJSON *string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(data, &m) != nil || m.Index == nil {
		return f.block(errors.New("guard: unreadable content_block_delta"))
	}
	b := f.blocks[*m.Index]
	if b == nil {
		return f.block(errors.New("guard: a delta for a block that never started"))
	}
	var text string
	switch {
	case b.kind == "drop":
		return nil
	case b.kind == "text" && m.Delta.Type == "text_delta" && m.Delta.Text != nil:
		text = *m.Delta.Text
	case b.kind == "json" && m.Delta.Type == "input_json_delta" && m.Delta.PartialJSON != nil:
		text = *m.Delta.PartialJSON
	default:
		return f.block(errors.New("guard: a delta that does not match its block"))
	}
	out, err := f.pass(b, text, false)
	if err != nil || out == "" {
		return err
	}
	return f.emitDelta(b, out)
}

func (f *AnthropicStreamFilter) emitDelta(b *anthropicBlock, out string) error {
	delta := map[string]any{"type": "text_delta", "text": out}
	if b.kind == "json" {
		delta = map[string]any{"type": "input_json_delta", "partial_json": out}
	}
	return f.emit("content_block_delta", map[string]any{"index": b.index, "delta": delta})
}

// stopBlock flushes a block and passes its stop.
func (f *AnthropicStreamFilter) stopBlock(i int) error {
	b := f.blocks[i]
	if b == nil {
		return f.block(errors.New("guard: a stop for a block that never started"))
	}
	delete(f.blocks, i)
	if b.kind == "drop" {
		return nil
	}
	out, err := f.pass(b, "", true)
	if err != nil {
		return err
	}
	if out != "" {
		if err := f.emitDelta(b, out); err != nil {
			return err
		}
	}
	return f.emit("content_block_stop", map[string]any{"index": b.index})
}

// pass is StreamFilter.pass for one block: its holdback, then its gate.
func (f *AnthropicStreamFilter) pass(b *anthropicBlock, text string, final bool) (string, error) {
	st := &b.ch
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
	if f.CheckEvery <= 0 || f.Check == nil {
		return released, nil
	}
	st.gate = append(st.gate, released...)
	if len(st.gate) < f.CheckEvery && !final {
		return "", nil
	}
	if err := f.Check(b.index, string(st.sent)+string(st.gate), final); err != nil {
		return "", f.block(err)
	}
	out := string(st.gate)
	st.sent = append(st.sent, st.gate...)
	st.gate = st.gate[:0]
	return out, nil
}
