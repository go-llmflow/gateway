package openai

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Usage is the token usage of a request (CompletionUsage).
type Usage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	// AudioSeconds is the duration of transcribed audio, when vLLM reports
	// it (TranscriptionUsageAudio) instead of tokens.
	AudioSeconds float64 `json:"-"`
}

// Cached returns the number of cached prompt tokens.
func (u Usage) Cached() int64 {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

// Valid reports whether the counts are sane (non-negative, cached within
// prompt). An upstream is trusted, but a wrong count must not become a
// negative charge.
func (u Usage) Valid() bool {
	return u.PromptTokens >= 0 && u.CompletionTokens >= 0 && u.Cached() >= 0 && u.Cached() <= u.PromptTokens &&
		u.AudioSeconds >= 0 && u.AudioSeconds <= maxAudioSeconds
}

// maxAudioSeconds bounds a reported duration (a week): beyond it the count
// is wrong, and must not become a charge.
const maxAudioSeconds = 7 * 24 * 3600

type usageEnvelope struct {
	Usage   *Usage            `json:"usage"`
	Choices []json.RawMessage `json:"choices"`
}

// ParseUsage reads the usage object of a non-streaming response.
func ParseUsage(body []byte) (Usage, bool) {
	var env usageEnvelope
	if err := json.Unmarshal(body, &env); err != nil || env.Usage == nil || !env.Usage.Valid() {
		return Usage{}, false
	}
	return *env.Usage, true
}

// maxEvent bounds the bytes held for one SSE event when the usage chunk
// may have to be dropped. Longer events are passed through unexamined.
const maxEvent = 1 << 20

var (
	dataPrefix = []byte("data:")
	usageKey   = []byte(`"usage"`)
)

// StreamResult is what CopySSE learnt from a stream.
type StreamResult struct {
	// Usage is the usage chunk's content, valid when HaveUsage.
	Usage     Usage
	HaveUsage bool
	// Deltas counts the non-empty choices[].delta.content values forwarded
	// to the client. vLLM emits about one token per delta, so it is the
	// estimate of completion tokens when the stream ends before the usage
	// chunk (which comes last).
	Deltas int64
}

var (
	contentKey   = []byte(`"content":"`)
	emptyContent = []byte(`"content":""`)
)

// countDeltas counts non-empty "content" string values in one event.
// "reasoning_content" does not match: the key must start with a quote.
func countDeltas(event []byte) int64 {
	return int64(bytes.Count(event, contentKey) - bytes.Count(event, emptyContent))
}

// CopySSE copies a server-sent event stream from src to dst event by event
// (an event ends with a blank line), calling flush after each one so
// tokens reach the client as soon as vLLM produces them; nothing beyond
// one event is buffered. It returns the usage carried by the stream, if
// any, and the number of content deltas forwarded. When dropUsage is true
// the usage-only chunk (empty choices, non-null usage), which the gateway
// requested on the client's behalf, is not forwarded.
func CopySSE(dst io.Writer, flush func(), src io.Reader, dropUsage bool) (StreamResult, error) {
	return CopySSEFor(Chat, dst, flush, src, dropUsage)
}

// CopySSEFor is CopySSE for the event stream of ep: a Responses stream
// carries its usage in the final response.completed (or .incomplete,
// .failed) event, and its text in response.output_text.delta events.
func CopySSEFor(ep Endpoint, dst io.Writer, flush func(), src io.Reader, dropUsage bool) (StreamResult, error) {
	inspectEvent, countDeltas := inspectEvent, countDeltas
	switch ep {
	case Responses:
		inspectEvent = func(ev []byte) (Usage, bool, bool) {
			u, ok := inspectResponsesEvent(ev)
			return u, ok, false
		}
		countDeltas = func(ev []byte) int64 { return int64(bytes.Count(ev, responsesDelta)) }
	case Messages:
		t := &messagesUsageTracker{}
		inspectEvent = func(ev []byte) (Usage, bool, bool) {
			u, ok := t.inspect(ev)
			return u, ok, false
		}
		countDeltas = func(ev []byte) int64 { return int64(bytes.Count(ev, messagesDelta)) }
	}
	br := bufio.NewReaderSize(src, 64<<10)
	var (
		event    []byte
		overflow bool // event too long to hold: being streamed through
		res      StreamResult
	)
	write := func(b []byte) error {
		if len(b) == 0 {
			return nil
		}
		_, err := dst.Write(b)
		return err
	}
	endEvent := func() error {
		if !overflow {
			u, ok, isUsageOnly := inspectEvent(event)
			if ok {
				res.Usage, res.HaveUsage = u, true
			}
			if !(dropUsage && isUsageOnly) {
				if err := write(event); err != nil {
					return err
				}
				res.Deltas += countDeltas(event)
			}
		}
		event, overflow = event[:0], false
		if flush != nil {
			flush()
		}
		return nil
	}
	for {
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			blank := len(bytes.TrimRight(line, "\r\n")) == 0 && line[len(line)-1] == '\n'
			switch {
			case overflow:
				if werr := write(line); werr != nil {
					return res, werr
				}
			case len(event)+len(line) > maxEvent:
				overflow = true
				if werr := write(event); werr != nil {
					return res, werr
				}
				if werr := write(line); werr != nil {
					return res, werr
				}
				event = event[:0]
			default:
				event = append(event, line...)
			}
			if blank {
				if werr := endEvent(); werr != nil {
					return res, werr
				}
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			// Forward a trailing unterminated event as is.
			if !overflow && len(event) > 0 {
				if u, ok, _ := inspectEvent(event); ok {
					res.Usage, res.HaveUsage = u, true
				}
				if werr := write(event); werr != nil {
					return res, werr
				}
				res.Deltas += countDeltas(event)
				if flush != nil {
					flush()
				}
			}
			if err == io.EOF {
				return res, nil
			}
			return res, err
		}
	}
}

// inspectEvent looks for a usage object in the data lines of one event and
// reports whether the event is a usage-only chunk.
func inspectEvent(event []byte) (Usage, bool, bool) {
	if !bytes.Contains(event, usageKey) {
		return Usage{}, false, false
	}
	var data []byte
	for _, line := range bytes.Split(event, []byte("\n")) {
		line = bytes.TrimRight(line, "\r")
		if !bytes.HasPrefix(line, dataPrefix) {
			continue
		}
		v := bytes.TrimPrefix(line[len(dataPrefix):], []byte(" "))
		data = append(data, v...)
	}
	var env usageEnvelope
	if err := json.Unmarshal(data, &env); err != nil || env.Usage == nil || !env.Usage.Valid() {
		return Usage{}, false, false
	}
	return *env.Usage, true, len(env.Choices) == 0
}
