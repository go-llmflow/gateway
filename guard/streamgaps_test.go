package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/go-gitsafe/gitsafe/redact"
)

// maskJSONWith is a MaskJSON over the test secret finder: it masks the text as
// it is (the gateway's own goes through every JSON layer).
func maskJSONWith(in string) (string, error) {
	return Apply(in, Combine(in, []Finder{secretFinder}), Placeholder), nil
}

func gapFilter(dst *bytes.Buffer) *StreamFilter {
	return &StreamFilter{Dst: dst, MaskJSON: maskJSONWith,
		NewHoldback: func() *Holdback { return &Holdback{Window: 16, Finders: []Finder{secretFinder}, Repl: Placeholder} },
		BlockEvent:  func(error) []byte { return []byte(`{"error":{"type":"guardrail_violation"}}`) }}
}

func delta(d string, finish string) string {
	if finish == "" {
		finish = "null"
	}
	return `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":` + d + `,"finish_reason":` + finish + `}]}` + "\n\n"
}

// toolCalls collects the tool calls of an output: per index, the id, name and
// the concatenated arguments, and how many chunks carried tool calls.
func toolCalls(t *testing.T, out string) (calls map[int][3]string, chunks int, finishAfter bool) {
	t.Helper()
	calls = map[int][3]string{}
	sawTool := false
	for _, ev := range strings.Split(out, "\n\n") {
		data, ok := eventData([]byte(ev))
		if !ok || string(data) == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					ToolCalls []struct {
						Index    int
						ID, Type string
						Function struct{ Name, Arguments string }
					} `json:"tool_calls"`
				}
				FinishReason *string `json:"finish_reason"`
			}
		}
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatalf("event %q: %v", data, err)
		}
		for _, ch := range c.Choices {
			if len(ch.Delta.ToolCalls) > 0 {
				chunks++
				sawTool = true
			}
			for _, tc := range ch.Delta.ToolCalls {
				v := calls[tc.Index]
				v[0] += tc.ID
				v[1] += tc.Function.Name
				v[2] += tc.Function.Arguments
				calls[tc.Index] = v
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				finishAfter = sawTool
			}
		}
	}
	return
}

func TestStreamFilterInspectsToolCalls(t *testing.T) {
	var dst bytes.Buffer
	f := gapFilter(&dst)
	var spans []Span
	f.Spans = func(s []Span) error { spans = append(spans, s...); return nil }
	for _, ev := range []string{
		delta(`{"role":"assistant"}`, ""),
		delta(`{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"run_cmd","arguments":""}}]}`, ""),
		delta(`{"tool_calls":[{"index":0,"function":{"arguments":"{\"token\":\"SEC"}}]}`, ""),
		delta(`{"tool_calls":[{"index":0,"function":{"arguments":"RET12345\",\"n\":1}"}}]}`, ""),
		delta(`{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"other","arguments":"{}"}}]}`, ""),
		delta(`{}`, `"tool_calls"`),
		"data: [DONE]\n\n",
	} {
		if _, err := f.Write([]byte(ev)); err != nil {
			t.Fatalf("%v\n%s", err, dst.String())
		}
	}
	out := dst.String()
	if strings.Contains(out, "SECRET") || strings.Contains(out, "SEC") && strings.Contains(out, "RET12345") {
		t.Fatalf("a secret went out: %s", out)
	}
	calls, chunks, finishAfter := toolCalls(t, out)
	if chunks != 1 || !finishAfter {
		t.Fatalf("%d tool chunks (want 1, before the finish): %s", chunks, out)
	}
	if got := calls[0]; got[0] != "call_1" || got[1] != "run_cmd" || got[2] != `{"token":"[TOKEN]","n":1}` {
		t.Fatalf("call 0 = %q", got)
	}
	if got := calls[1]; got[0] != "call_2" || got[1] != "other" || got[2] != "{}" {
		t.Fatalf("call 1 = %q", got)
	}
	if !json.Valid([]byte(calls[0][2])) || len(spans) != 0 && spans[0].Category == "" {
		t.Fatalf("masked arguments %q", calls[0][2])
	}
	if !strings.HasSuffix(out, "data: [DONE]\n\n") {
		t.Fatalf("no [DONE]: %s", out)
	}
}

func TestStreamFilterToolCallsFlushWithoutFinish(t *testing.T) {
	var dst bytes.Buffer
	f := gapFilter(&dst)
	_, _ = f.Write([]byte(delta(`{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":"{\"k\":\"SECRET99999\"}"}}]}`, "")))
	if dst.Len() != 0 {
		t.Fatalf("tool calls went out before the choice ended: %s", dst.String())
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	calls, chunks, _ := toolCalls(t, dst.String())
	if chunks != 1 || calls[0][2] != `{"k":"[TOKEN]"}` {
		t.Fatalf("%v %d\n%s", calls, chunks, dst.String())
	}
}

func TestStreamFilterToolCallsBlock(t *testing.T) {
	bad := func() *StreamFilter {
		var dst bytes.Buffer
		f := gapFilter(&dst)
		f.MaskJSON = func(string) (string, error) { return "", errors.New("blocked by policy") }
		return f
	}
	f := bad()
	_, _ = f.Write([]byte(delta(`{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"{}"}}]}`, "")))
	if _, err := f.Write([]byte(delta(`{}`, `"tool_calls"`))); !errors.Is(err, ErrBlocked) {
		t.Fatalf("a blocked argument: %v", err)
	}
	for name, ev := range map[string]string{
		"unknown field":  `{"tool_calls":[{"index":0,"extra":"text nobody read"}]}`,
		"unknown fn key": `{"tool_calls":[{"index":0,"function":{"x":"y"}}]}`,
		"odd name":       `{"tool_calls":[{"index":0,"function":{"name":"SECRET 1234 free text"}}]}`,
		"odd id":         `{"tool_calls":[{"index":0,"id":"a b"}]}`,
		"bad index":      `{"tool_calls":[{"index":-1}]}`,
		"not a list":     `{"tool_calls":"x"}`,
		"not an object":  `{"tool_calls":[1]}`,
	} {
		var dst bytes.Buffer
		f := gapFilter(&dst)
		if _, err := f.Write([]byte(delta(ev, ""))); !errors.Is(err, ErrBlocked) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// A stream of tool arguments too long to inspect.
	var dst bytes.Buffer
	f = gapFilter(&dst)
	chunk := delta(`{"tool_calls":[{"index":0,"function":{"arguments":"`+strings.Repeat("a", 200<<10)+`"}}]}`, "")
	var err error
	for i := 0; i < 8 && err == nil; i++ {
		_, err = f.Write([]byte(chunk))
	}
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("1 MiB of arguments: %v", err)
	}
}

func TestStreamFilterInspectsReasoning(t *testing.T) {
	var dst bytes.Buffer
	f := gapFilter(&dst)
	for _, ev := range []string{
		delta(`{"role":"assistant"}`, ""),
		delta(`{"reasoning_content":"let me think about SEC"}`, ""),
		delta(`{"reasoning_content":"RET12345 and more thoughts here"}`, ""),
		delta(`{"content":"the answer"}`, ""),
		delta(`{"reasoning_content":" last"}`, `"stop"`),
		"data: [DONE]\n\n",
	} {
		if _, err := f.Write([]byte(ev)); err != nil {
			t.Fatalf("%v\n%s", err, dst.String())
		}
	}
	out := dst.String()
	var reasoning, content strings.Builder
	for _, ev := range strings.Split(out, "\n\n") {
		data, ok := eventData([]byte(ev))
		if !ok || string(data) == "[DONE]" {
			continue
		}
		var c struct {
			Choices []struct {
				Delta struct {
					Content          string `json:"content"`
					ReasoningContent string `json:"reasoning_content"`
				}
			}
		}
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatal(err)
		}
		for _, ch := range c.Choices {
			reasoning.WriteString(ch.Delta.ReasoningContent)
			content.WriteString(ch.Delta.Content)
		}
	}
	if strings.Contains(out, "SECRET") || strings.Contains(out, "SEC") && strings.Contains(out, "RET12345") {
		t.Fatalf("a secret went out in the reasoning: %s", out)
	}
	if reasoning.String() != "let me think about [TOKEN] and more thoughts here last" || content.String() != "the answer" {
		t.Fatalf("reasoning %q content %q\n%s", reasoning.String(), content.String(), out)
	}
}

// A key written with escapes is the same key: it must not slip past the
// cheap test that skips decoding.
func TestStreamFilterEscapedReasoningKey(t *testing.T) {
	var dst bytes.Buffer
	f := gapFilter(&dst)
	_, _ = f.Write([]byte(delta(`{"reasoning_content":"SECRET1234567 here"}`, `"stop"`)))
	if strings.Contains(dst.String(), "SECRET") {
		t.Fatalf("a secret went out under an escaped key: %s", dst.String())
	}
	if !strings.Contains(dst.String(), "[TOKEN]") {
		t.Fatalf("the reasoning was dropped, not inspected: %s", dst.String())
	}
}

func TestStreamFilterReasoningStrippedWithoutMasking(t *testing.T) {
	var dst bytes.Buffer
	f := &StreamFilter{Dst: &dst, BlockEvent: func(error) []byte { return []byte(`{"error":{}}`) }}
	_, _ = f.Write([]byte(delta(`{"reasoning_content":"uninspected"}`, "")))
	_, _ = f.Write([]byte(delta(`{"content":"ok"}`, `"stop"`)))
	if strings.Contains(dst.String(), "uninspected") {
		t.Fatalf("uninspected reasoning went out: %s", dst.String())
	}
}

// userinfo is a URL whose user part is 400 bytes: the `://` that opens it is
// far outside a 16-byte window by the time the `@` arrives.
func userinfoURL() string {
	return "see https://" + strings.Repeat("u", 400) + ":pa55w0rd1234@host.example/x and go on"
}

func redactFinder(text string) []Span {
	var out []Span
	for _, sp := range redact.New().Find(text) {
		out = append(out, Span{Start: sp.Start, End: sp.End, Guard: "secrets", Category: string(sp.Kind), Action: "mask"})
	}
	return out
}

func pushAll(h *Holdback, s string, n int) string {
	var out strings.Builder
	for len(s) > 0 {
		k := min(n, len(s))
		r, _ := h.Push(s[:k])
		out.WriteString(r)
		s = s[k:]
	}
	r, _ := h.Flush()
	out.WriteString(r)
	return out.String()
}

func TestHoldbackHoldsAnOpenURLUserinfo(t *testing.T) {
	text := userinfoURL()
	// The gap: with the window alone, the start of the userinfo has left the
	// window when the `@` comes, and the user part is released.
	plain := &Holdback{Window: 16, Finders: []Finder{redactFinder}, Repl: Placeholder}
	if out := pushAll(plain, text, 7); !strings.Contains(out, strings.Repeat("u", 100)) {
		t.Fatalf("the control no longer shows the gap: %q", out)
	}
	// Closed: the userinfo is held until it ends, and masked.
	h := &Holdback{Window: 16, Finders: []Finder{redactFinder}, Repl: Placeholder, Hold: HoldURLUserinfo}
	out := pushAll(h, text, 7)
	if strings.Contains(out, "uuuu") || strings.Contains(out, "pa55w0rd") || !strings.Contains(out, "host.example/x and go on") {
		t.Fatalf("%q", out)
	}
	// A URL without userinfo, and text without a URL, is not held: the first
	// bytes come out while the rest is still arriving.
	h = &Holdback{Window: 16, Finders: []Finder{redactFinder}, Repl: Placeholder, Hold: HoldURLUserinfo}
	var got string
	for _, p := range []string{"plain text with https://example.org/path ", strings.Repeat("x", 100)} {
		r, _ := h.Push(p)
		got += r
	}
	if !strings.HasPrefix(got, "plain text with https://example.org/path") {
		t.Fatalf("a URL with a path was held: %q", got)
	}
	// An opening that never closes is bounded by MaxHold, not held for ever.
	h = &Holdback{Window: 16, Finders: []Finder{redactFinder}, Repl: Placeholder, Hold: HoldURLUserinfo}
	var rel int
	for i := 0; i < 20; i++ {
		r, _ := h.Push("://" + strings.Repeat("a", 8000))
		rel += len(r)
	}
	if h.Held() > MaxHold+8003 {
		t.Fatalf("held %d bytes", h.Held())
	}
}

func TestHoldURLUserinfo(t *testing.T) {
	const window = 16
	for in, want := range map[string]int{
		"":                                    -1,
		"no url":                              -1,
		"a://":                                1,
		"a://user":                            1,
		"a://user:pw":                         1,
		"a://user:pw@":                        1,  // the span ends inside the window: a cut could land in it
		"a://u:p@" + strings.Repeat("x", 30):  -1, // the span ended well behind the window
		"a://u:p@h/" + strings.Repeat("x", 9): 1,  // ended, but only 10 bytes behind
		"a://host/path":                       -1,
		"a://b c":                             -1,
		"a://x b://y":                         7, // the last opening
		"a://x/ b://y":                        8,
		"https://h.example/p?q=a:b":           -1,
	} {
		if got := HoldURLUserinfo(in, window); got != want {
			t.Errorf("%q: %d, want %d", in, got, want)
		}
	}
}

// FuzzHoldbackURL: whatever the user part's length, the chunking and the
// window, the password of a URL's userinfo never comes out.
func FuzzHoldbackURL(f *testing.F) {
	f.Add(uint16(400), uint8(7), uint8(16), "see ")
	f.Add(uint16(1), uint8(1), uint8(8), "")
	f.Add(uint16(3000), uint8(200), uint8(255), "a://b ")
	f.Fuzz(func(t *testing.T, userLen uint16, chunk, window uint8, lead string) {
		// The window must hold what begins a match ("://" is three bytes: the rule
		// documented on Holdback).
		if chunk == 0 || window < 8 {
			return
		}
		text := lead + "https://" + strings.Repeat("u", int(userLen%4000)+1) + ":pa55w0rd1234@host.example/x tail"
		h := &Holdback{Window: int(window), Finders: []Finder{redactFinder}, Repl: Placeholder, Hold: HoldURLUserinfo}
		out := pushAll(h, text, int(chunk))
		if strings.Contains(out, "pa55w0rd1234") {
			t.Fatalf("password released: %q", out[:min(len(out), 200)])
		}
	})
}

func FuzzStreamFilterTools(f *testing.F) {
	f.Add([]byte(delta(`{"tool_calls":[{"index":0,"id":"c","function":{"name":"f","arguments":"{\"k\":\"SECRET99999\"}"}}]}`, "") + delta(`{}`, `"tool_calls"`) + "data: [DONE]\n\n"))
	f.Add([]byte(delta(`{"reasoning_content":"SECRET1234 x"}`, `"stop"`)))
	f.Add([]byte(delta(`{"tool_calls":[{"index":3,"function":{"arguments":"SECR"}},{"index":3,"function":{"arguments":"ET12345"}}]}`, "")))
	f.Fuzz(func(t *testing.T, in []byte) {
		var dst bytes.Buffer
		sf := gapFilter(&dst)
		_, err := sf.Write(in)
		if err == nil {
			err = sf.Close()
		}
		for _, ev := range strings.Split(dst.String(), "\n\n") {
			data, ok := eventData([]byte(ev))
			if !ok || string(bytes.TrimSpace(data)) == "[DONE]" {
				continue
			}
			var v any
			if json.Unmarshal(data, &v) != nil {
				t.Fatalf("event %q is not JSON", data)
			}
			if s := secretIn(v, ""); s != "" {
				t.Fatalf("secret released in %s (err %v): %q", s, err, dst.String())
			}
		}
	})
}
