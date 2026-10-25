package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
)

var secretRE = regexp.MustCompile(`SECRET[0-9]{4,}`)

// secretFinder is a context-free finder: a match does not depend on what
// precedes it, so masking a text in pieces must give exactly what masking
// it whole gives.
func secretFinder(text string) []Span {
	var out []Span
	for _, l := range secretRE.FindAllStringIndex(text, -1) {
		out = append(out, Span{Start: l[0], End: l[1], Guard: "s", Category: "token", Action: "mask"})
	}
	return out
}

func upper(_, s string) string { return strings.ToUpper(s) }

func TestMapRequestChat(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"system","content":"sys"},` +
		`{"role":"user","content":[{"type":"text","text":"part"},{"type":"image_url","image_url":{"url":"x"}}]},` +
		`{"role":"assistant","content":null,"tool_calls":[{"id":"1","type":"function","function":{"name":"f","arguments":"{\"a\":1}"}}]},` +
		`{"role":"tool","content":"result"}],"temperature":0.5}`
	out, msgs, err := MapRequest([]byte(body), upper)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Model       string
		Temperature float64
		Messages    []struct {
			Content   any
			ToolCalls []struct{ Function struct{ Arguments string } } `json:"tool_calls"`
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.Model != "m" || got.Temperature != 0.5 || got.Messages[0].Content != "SYS" || got.Messages[3].Content != "RESULT" ||
		got.Messages[2].ToolCalls[0].Function.Arguments != `{"A":1}` {
		t.Fatalf("%s", out)
	}
	parts := got.Messages[1].Content.([]any)
	if parts[0].(map[string]any)["text"] != "PART" || parts[1].(map[string]any)["image_url"] == nil {
		t.Fatalf("parts %v", parts)
	}
	roles := []string{}
	for _, m := range msgs {
		roles = append(roles, m.Role+"="+m.Content)
	}
	if strings.Join(roles, ",") != `system=SYS,user=PART,tool_call={"A":1},tool=RESULT` {
		t.Fatalf("%v", roles)
	}
	// Unchanged texts: the body is returned byte for byte.
	same, _, err := MapRequest([]byte(body), func(_, s string) string { return s })
	if err != nil || !bytes.Equal(same, []byte(body)) {
		t.Fatalf("identity changed the body: %s %v", same, err)
	}
}

func TestMapRequestCompletionsAndEmbeddings(t *testing.T) {
	out, msgs, err := MapRequest([]byte(`{"prompt":["a","b"],"input":"c"}`), upper)
	if err != nil || string(out) != `{"input":"C","prompt":["A","B"]}` || len(msgs) != 3 {
		t.Fatalf("%s %v %v", out, msgs, err)
	}
	// Token arrays are not text.
	out, msgs, err = MapRequest([]byte(`{"input":[1,2,3]}`), upper)
	if err != nil || string(out) != `{"input":[1,2,3]}` || len(msgs) != 0 {
		t.Fatalf("%s %v %v", out, msgs, err)
	}
	for _, bad := range []string{`[]`, `"x"`, `{"messages":"x"}`, `{"messages":[{"content":5}]}`} {
		if _, _, err := MapRequest([]byte(bad), upper); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestMapResponse(t *testing.T) {
	body := `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hello","reasoning_content":"think"},"logprobs":{"content":[{"token":"hello"}]}}],"usage":{"prompt_tokens":1}}`
	out, texts, err := MapResponse([]byte(body), true, strings.ToUpper)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "hello") || strings.Contains(string(out), "think") || !strings.Contains(string(out), `"HELLO"`) ||
		!strings.Contains(string(out), `"prompt_tokens":1`) || len(texts) != 1 {
		t.Fatalf("%s", out)
	}
	out, _, err = MapResponse([]byte(`{"choices":[{"text":"abc"}]}`), false, strings.ToUpper)
	if err != nil || string(out) != `{"choices":[{"text":"ABC"}]}` {
		t.Fatalf("%s %v", out, err)
	}
}

func TestMapRequestResponses(t *testing.T) {
	body := `{"model":"m","instructions":"be brief","input":[` +
		`{"role":"user","content":"plain"},` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"part"},{"type":"input_image","image_url":"x"}]},` +
		`{"type":"function_call","call_id":"c","name":"f","arguments":"{\"q\":1}"},` +
		`{"type":"function_call_output","call_id":"c","output":"result"}]}`
	out, msgs, err := MapRequest([]byte(body), upper)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"BE BRIEF"`, `"PLAIN"`, `"PART"`, `{\"Q\":1}`, `"RESULT"`, `"image_url":"x"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("%s missing from %s", want, out)
		}
	}
	var roles []string
	for _, m := range msgs {
		roles = append(roles, m.Role)
	}
	if strings.Join(roles, ",") != "system,user,user,tool_call,tool" {
		t.Fatalf("%v", roles)
	}
	// A string input is a user prompt, as before.
	if _, msgs, err := MapRequest([]byte(`{"input":"hi"}`), upper); err != nil || msgs[0].Content != "HI" {
		t.Fatal(msgs, err)
	}
	// An item the guards cannot read is refused, not passed.
	if _, _, err := MapRequest([]byte(`{"input":[{"type":"reasoning","summary":[{"text":"x"}]}]}`), upper); err == nil {
		t.Fatal("reasoning input item accepted")
	}
}

func TestMapResponseResponses(t *testing.T) {
	body := `{"id":"resp_1","object":"response","created_at":1,"model":"m","status":"completed","instructions":"echo",` +
		`"output":[{"type":"reasoning","summary":[{"type":"summary_text","text":"secret thoughts"}]},` +
		`{"type":"message","id":"msg","role":"assistant","status":"completed","content":[{"type":"output_text","text":"hello","annotations":[],"logprobs":[{"token":"hello"}]}]},` +
		`{"type":"function_call","call_id":"c","name":"f","arguments":"{\"a\":\"b\"}"}],` +
		`"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5,"input_tokens_details":{"cached_tokens":0,"input_tokens_per_turn":[3]}}}`
	out, texts, err := MapResponse([]byte(body), true, strings.ToUpper)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "hello") || !strings.Contains(s, `"HELLO"`) || strings.Contains(s, "thoughts") || strings.Contains(s, "echo") ||
		strings.Contains(s, "logprobs") || !strings.Contains(s, `{\"A\":\"B\"}`) || !strings.Contains(s, `"input_tokens":3`) || len(texts) != 2 {
		t.Fatalf("%s %q", s, texts)
	}
}

func TestMapRequestMessages(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"system":[{"type":"text","text":"sys"}],"messages":[` +
		`{"role":"user","content":"plain"},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"hmm","signature":"sig"},{"type":"tool_use","id":"t1","name":"run","input":{"cmd":"ls","args":["a",{"deep":"b"}],"n":3}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":"out"}]},{"type":"image","source":{"data":"zz"}}]}]}`
	out, msgs, err := MapRequest([]byte(body), upper)
	if err != nil {
		t.Fatal(err)
	}
	// Keys of a tool input are text too (a secret can be a key).
	for _, want := range []string{`"SYS"`, `"PLAIN"`, `"HMM"`, `"signature":"sig"`, `"CMD":"LS"`, `"A"`, `"DEEP":"B"`, `"N":3`, `"OUT"`, `"data":"zz"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("%s missing from %s", want, out)
		}
	}
	if len(msgs) != 11 { // sys, plain, hmm, ls, a, b, out, and the keys cmd, args, deep, n
		t.Fatalf("%d texts: %v", len(msgs), msgs)
	}
	if _, _, err := MapRequest([]byte(`{"messages":[{"role":"user","content":[{"type":"document","source":{"type":"text","data":"x"}}]}]}`), upper); err == nil {
		t.Fatal("a document block was not refused")
	}
	deep := strings.Repeat(`{"a":`, maxJSONDepth+2) + `"x"` + strings.Repeat("}", maxJSONDepth+2)
	if _, _, err := MapRequest([]byte(`{"messages":[{"role":"assistant","content":[{"type":"tool_use","input":`+deep+`}]}]}`), upper); err == nil {
		t.Fatal("a too deep tool input was not refused")
	}
}

func TestMapResponseMessages(t *testing.T) {
	body := `{"id":"msg_1","type":"message","role":"assistant","model":"m","stop_reason":"end_turn","stop_sequence":null,` +
		`"content":[{"type":"thinking","thinking":"secret thoughts"},{"type":"text","text":"hello","citations":[{"cited_text":"x"}]},` +
		`{"type":"tool_use","id":"t","name":"f","input":{"q":"find"}}],"usage":{"input_tokens":3,"output_tokens":2}}`
	out, texts, err := MapResponse([]byte(body), true, strings.ToUpper)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if strings.Contains(s, "thoughts") || strings.Contains(s, "cited_text") || !strings.Contains(s, `"HELLO"`) || !strings.Contains(s, `"Q":"FIND"`) ||
		!strings.Contains(s, `"stop_reason":"end_turn"`) || !strings.Contains(s, `"input_tokens":3`) || len(texts) != 3 {
		t.Fatalf("%s %q", s, texts)
	}
}

func TestCombine(t *testing.T) {
	a := func(string) []Span { return []Span{{Start: 0, End: 5, Category: "a", Action: "log"}} }
	b := func(string) []Span {
		return []Span{{Start: 3, End: 8, Category: "b", Action: "block"}, {Start: 10, End: 12, Action: "mask"}}
	}
	got := Combine("0123456789abc", []Finder{a, b})
	if len(got) != 2 || got[0].End != 8 || got[0].Action != "block" || got[0].Category != "b" || got[1].Start != 10 {
		t.Fatalf("%+v", got)
	}
}

func TestHoldbackSplitSecret(t *testing.T) {
	h := &Holdback{Window: 8, Finders: []Finder{secretFinder}, Repl: Placeholder}
	var out strings.Builder
	for _, c := range []string{"abc SEC", "RET12", "345 and", " more text after it"} {
		o, _ := h.Push(c)
		out.WriteString(o)
	}
	o, _ := h.Flush()
	out.WriteString(o)
	if got := out.String(); got != "abc [TOKEN] and more text after it" {
		t.Fatalf("%q", got)
	}
}

func TestHoldbackReleasesAsItGoes(t *testing.T) {
	h := &Holdback{Window: 4, Finders: []Finder{secretFinder}, Repl: Placeholder}
	o, _ := h.Push("0123456789")
	if o != "012345" || h.Held() != 4 {
		t.Fatalf("%q held %d", o, h.Held())
	}
	// An open span is held whole, however long it grows.
	o, _ = h.Push("SECRET" + strings.Repeat("1", 100))
	if o != "6789" {
		t.Fatalf("%q", o)
	}
	o, _ = h.Push(" x")
	if o != "" {
		t.Fatalf("%q", o)
	}
	o, _ = h.Push("yzzzzz")
	if o != "[TOKEN] xyz" {
		t.Fatalf("%q", o)
	}
}

func TestHoldbackRuneBoundary(t *testing.T) {
	h := &Holdback{Window: 2, Finders: []Finder{secretFinder}, Repl: Placeholder}
	o, _ := h.Push("ééé") // 6 bytes
	if o != "éé" {
		t.Fatalf("%q", o)
	}
}

// TestHoldbackUTF8BackoffLeak is the review's reproduction: a span ending
// right at the cut, followed by a stray continuation byte, was released
// unmasked when the rune alignment moved the cut into it.
func TestHoldbackUTF8BackoffLeak(t *testing.T) {
	text := "SECRET12345" + "\x80" + "abcdefghi"
	h := &Holdback{Window: 10, Finders: []Finder{secretFinder}, Repl: Placeholder}
	var out strings.Builder
	o, _ := h.Push(text)
	out.WriteString(o)
	o, _ = h.Flush()
	out.WriteString(o)
	want := Apply(text, Combine(text, []Finder{secretFinder}), Placeholder)
	if out.String() != want {
		t.Fatalf("streamed %q\nwhole    %q", out.String(), want)
	}
}

func TestHoldbackMaxHold(t *testing.T) {
	h := &Holdback{Window: 16, Finders: []Finder{secretFinder}, Repl: Placeholder}
	o, _ := h.Push("SECRET" + strings.Repeat("1", MaxHold+10))
	// Released masked as far as the span reached, never a byte of it in
	// clear.
	if o != "[TOKEN]" || h.Held() != 0 {
		t.Fatalf("%q held %d", o, h.Held())
	}
}

func TestCutHelpers(t *testing.T) {
	spans := []Span{{Start: 2, End: 5}, {Start: 5, End: 9}}
	// 5 ends one span and starts the next: a safe cut.
	if c := cutDown("abcdefghij", 7, spans); c != 5 {
		t.Fatalf("down %d", c)
	}
	if c := cutUp("abcdefghij", 3, spans); c != 5 {
		t.Fatalf("up %d", c)
	}
	if c := cutDown("aé", 2, nil); c != 1 {
		t.Fatalf("rune %d", c)
	}
}

// FuzzHoldback: masking a text in arbitrary pieces gives what masking it
// whole gives (with a finder whose matches do not depend on what precedes).
func FuzzHoldback(f *testing.F) {
	f.Add("abc SECRET12345 def SECRET9999999 x", uint8(3), uint8(8))
	f.Add("SECRET1SECRET12345678", uint8(1), uint8(1))
	f.Add("ééé SECRET0000 ü", uint8(5), uint8(16))
	// Invalid UTF-8 around a secret: a stray continuation byte after a span
	// once moved the cut into it.
	f.Add("SECRET12345\x80abcdefghi", uint8(30), uint8(0))
	f.Add("\xffSECRET1234\xbf", uint8(2), uint8(3))
	f.Add("SECRET12345\x80\x80\x80SECRET99999\xbfz", uint8(4), uint8(1))
	f.Fuzz(func(t *testing.T, text string, step, window uint8) {
		n := int(step%31) + 1
		// The window covers the longest partial match, "SECRET123" (see
		// Holdback).
		h := &Holdback{Window: int(window%64) + 10, Finders: []Finder{secretFinder}, Repl: Placeholder}
		var out strings.Builder
		for i := 0; i < len(text); i += n {
			o, _ := h.Push(text[i:min(i+n, len(text))])
			out.WriteString(o)
		}
		o, _ := h.Flush()
		out.WriteString(o)
		want := Apply(text, Combine(text, []Finder{secretFinder}), Placeholder)
		if out.String() != want {
			t.Fatalf("pieces of %d, window %d:\n got %q\nwant %q", n, h.Window, out.String(), want)
		}
	})
}

// sse builds a chat chunk event.
func sse(content string, finish bool) string {
	c, _ := json.Marshal(content)
	fr := "null"
	if finish {
		fr = `"stop"`
	}
	return `data: {"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":` + string(c) + `},"finish_reason":` + fr + `}]}` + "\n\n"
}

// streamText returns the concatenated delta contents of an SSE output and
// whether it ended with an error event.
func streamText(t *testing.T, out string) (string, bool) {
	var text strings.Builder
	errored := false
	for _, ev := range strings.Split(out, "\n\n") {
		data, ok := eventData([]byte(ev))
		if !ok || string(data) == "[DONE]" {
			continue
		}
		var c struct {
			Error   any
			Choices []struct {
				Delta struct{ Content string }
				Text  string
			}
		}
		if err := json.Unmarshal(data, &c); err != nil {
			t.Fatalf("event %q is not JSON: %v", data, err)
		}
		if c.Error != nil {
			errored = true
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content + ch.Text)
		}
	}
	return text.String(), errored
}

func TestStreamFilterMasks(t *testing.T) {
	var dst bytes.Buffer
	f := &StreamFilter{Dst: &dst, NewHoldback: func() *Holdback {
		return &Holdback{Window: 16, Finders: []Finder{secretFinder}, Repl: Placeholder}
	}}
	var spans []Span
	f.Spans = func(s []Span) error { spans = append(spans, s...); return nil }
	for _, ev := range []string{
		`data: {"id":"x","choices":[{"index":0,"delta":{"role":"assistant"},"logprobs":{"content":[1]}}]}` + "\n\n",
		sse("the key is SEC", false), sse("RET1234", false), sse("5678, keep it", false), sse("", true),
		`data: {"id":"x","choices":[],"usage":{"prompt_tokens":3}}` + "\n\n", "data: [DONE]\n\n",
	} {
		if _, err := f.Write([]byte(ev)); err != nil {
			t.Fatal(err)
		}
	}
	text, errored := streamText(t, dst.String())
	if text != "the key is [TOKEN], keep it" || errored || len(spans) != 1 {
		t.Fatalf("%q %v %+v\n%s", text, errored, spans, dst.String())
	}
	out := dst.String()
	if !strings.Contains(out, `"role":"assistant"`) || !strings.Contains(out, `"prompt_tokens":3`) || strings.Contains(out, `"content":[1]`) ||
		!strings.HasSuffix(out, "data: [DONE]\n\n") || strings.Contains(out, "SECRET") {
		t.Fatalf("%s", out)
	}
}

func TestStreamFilterFlushesWithoutFinish(t *testing.T) {
	var dst bytes.Buffer
	f := &StreamFilter{Dst: &dst, NewHoldback: func() *Holdback {
		return &Holdback{Window: 64, Finders: []Finder{secretFinder}, Repl: Placeholder}
	}}
	_, _ = f.Write([]byte(sse("short SECRET1234", false)))
	if dst.Len() != 0 {
		// Everything is still held.
		if text, _ := streamText(t, dst.String()); text != "" {
			t.Fatalf("released %q", text)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if text, _ := streamText(t, dst.String()); text != "short [TOKEN]" {
		t.Fatalf("%q", text)
	}
}

func TestStreamFilterBlocks(t *testing.T) {
	var dst bytes.Buffer
	f := &StreamFilter{Dst: &dst,
		NewHoldback: func() *Holdback {
			return &Holdback{Window: 4, Finders: []Finder{func(s string) []Span {
				sp := secretFinder(s)
				for i := range sp {
					sp[i].Action = "block"
				}
				return sp
			}}, Repl: Placeholder}
		},
		Spans: func(s []Span) error {
			for _, x := range s {
				if x.Action == "block" {
					return errors.New("blocked")
				}
			}
			return nil
		},
		BlockEvent: func(error) []byte { return []byte(`{"error":{"type":"guardrail_violation"}}`) },
	}
	_, err := f.Write([]byte(sse("hello SECRET12345 more text here", false)))
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err %v", err)
	}
	text, errored := streamText(t, dst.String())
	if strings.Contains(text, "SECRET") || !errored || !strings.HasSuffix(dst.String(), "data: [DONE]\n\n") {
		t.Fatalf("%q %v %s", text, errored, dst.String())
	}
	if _, err := f.Write([]byte(sse("x", false))); !errors.Is(err, ErrBlocked) {
		t.Fatal("wrote after a block")
	}
}

func TestStreamFilterGate(t *testing.T) {
	var dst bytes.Buffer
	var checks []string
	f := &StreamFilter{Dst: &dst, CheckEvery: 10,
		Check: func(_ int, completion string, final bool) error {
			checks = append(checks, completion)
			if strings.Contains(completion, "BAD") {
				return errors.New("unsafe")
			}
			return nil
		},
		BlockEvent: func(error) []byte { return []byte(`{"error":{}}`) },
	}
	for _, c := range []string{"0123", "45678", "9abc", "def"} {
		if _, err := f.Write([]byte(sse(c, false))); err != nil {
			t.Fatal(err)
		}
	}
	text, _ := streamText(t, dst.String())
	// Released in checked bursts of at least 10 bytes: "0123456789abc" was
	// checked at 13 bytes, "def" is waiting.
	if text != "0123456789abc" || len(checks) != 1 {
		t.Fatalf("%q %q", text, checks)
	}
	_, err := f.Write([]byte(sse(" BAD stuff", false)))
	if !errors.Is(err, ErrBlocked) {
		t.Fatal(err)
	}
	if text, errored := streamText(t, dst.String()); strings.Contains(text, "BAD") || strings.Contains(text, "def") || !errored {
		t.Fatalf("%q", text)
	}
}

func TestStreamFilterRefusesWhatItCannotRead(t *testing.T) {
	for _, ev := range []string{
		"data: not json\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"x"}}]}}]}` + "\n\n",
		`data: {"choices":"x"}` + "\n\n",
	} {
		var dst bytes.Buffer
		f := &StreamFilter{Dst: &dst, BlockEvent: func(error) []byte { return []byte(`{"error":{}}`) }}
		if _, err := f.Write([]byte(ev)); !errors.Is(err, ErrBlocked) {
			t.Errorf("%q: %v", ev, err)
		}
	}
}

func FuzzStreamFilter(f *testing.F) {
	f.Add([]byte(sse("hello SECRET1234", false) + sse("5", true) + "data: [DONE]\n\n"))
	f.Add([]byte(": ping\n\n" + `data: {"choices":[{"index":2,"text":"SECRET99999"}]}` + "\n\n"))
	f.Add([]byte("data: [DONE]\r\n\r\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		var dst bytes.Buffer
		sf := &StreamFilter{Dst: &dst,
			NewHoldback: func() *Holdback { return &Holdback{Window: 16, Finders: []Finder{secretFinder}, Repl: Placeholder} },
			CheckEvery:  32, Check: func(int, string, bool) error { return nil },
			BlockEvent: func(error) []byte { return []byte(`{"error":{}}`) },
		}
		_, err := sf.Write(in)
		if err == nil {
			err = sf.Close()
		}
		// Whatever came in, no secret goes out in a text: every string of
		// every event, except the identifiers wellTyped confines to a name.
		out := dst.String()
		for _, ev := range strings.Split(out, "\n\n") {
			data, ok := eventData([]byte(ev))
			if !ok || string(bytes.TrimSpace(data)) == "[DONE]" {
				continue
			}
			var v any
			if json.Unmarshal(data, &v) != nil {
				t.Fatalf("event %q is not JSON", data)
			}
			if s := secretIn(v, ""); s != "" {
				t.Fatalf("secret released in %s (err %v): %q", s, err, out)
			}
		}
	})
}

// secretIn returns the key of a string holding a secret, outside the
// identifier fields.
func secretIn(v any, key string) string {
	switch x := v.(type) {
	case string:
		switch key {
		case "id", "object", "model", "system_fingerprint", "service_tier", "finish_reason", "role",
			"status", "type", "call_id", "name", "stop_reason":
			// The identifiers wellTyped confines to a short name.
			return ""
		}
		if secretRE.MatchString(x) {
			return key
		}
	case []any:
		for _, e := range x {
			if s := secretIn(e, key); s != "" {
				return s
			}
		}
	case map[string]any:
		for k, e := range x {
			if s := secretIn(e, k); s != "" {
				return s
			}
		}
	}
	return ""
}

func TestParseLlamaGuard(t *testing.T) {
	for in, want := range map[string]string{
		"safe":             "false []",
		"\n\nsafe\n":       "false []",
		"unsafe\nS1":       "true [S1]",
		"unsafe\nS1, S10 ": "true [S1 S10]",
		"unsafe":           "true [unsafe]",
	} {
		v, err := ParseLlamaGuard(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got := strings.TrimSpace(strings.Replace(strings.Trim(jsonish(v), " "), "  ", " ", -1)); got != want {
			t.Errorf("%q: %s, want %s", in, got, want)
		}
	}
	for _, bad := range []string{"", "maybe", "unsafe\nviolence", "Safe"} {
		if _, err := ParseLlamaGuard(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func jsonish(v Verdict) string {
	b := "false"
	if v.Flagged {
		b = "true"
	}
	return b + " [" + strings.Join(v.Categories, " ") + "]"
}

func TestLlamaGuardConversation(t *testing.T) {
	msgs := []Message{{"system", "s"}, {"assistant", "greeting"}, {"user", "a"}, {"user", "b"}, {"tool", "t"}, {"assistant", "c"}, {"user", "d"}}
	turns := LlamaGuardConversation(msgs, "answer", true)
	var got []string
	for _, m := range turns {
		got = append(got, m.Role+":"+m.Content)
	}
	if strings.Join(got, "|") != "user:a\n\nb|assistant:c|user:d|assistant:answer" {
		t.Fatalf("%q", got)
	}
	long := []Message{{"user", strings.Repeat("x", MaxInput)}, {"assistant", "y"}, {"user", strings.Repeat("z", 100)}}
	turns = LlamaGuardConversation(long, "", false)
	total := 0
	for _, m := range turns {
		total += len(m.Content)
	}
	if turns[0].Role != "user" || total > MaxInput {
		t.Fatalf("%d bytes, first %s", total, turns[0].Role)
	}
}

func FuzzParseGuardAnswers(f *testing.F) {
	f.Add([]byte("unsafe\nS1,S2"))
	f.Add([]byte(`{"data":[{"index":0,"label":"MALICIOUS","probs":[0.01,0.99]}]}`))
	f.Add([]byte(`{"action":"block","categories":["pii"]}`))
	f.Add([]byte(`{"choices":[{"message":{"content":"safe"}}]}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		if v, err := ParseLlamaGuard(string(in)); err == nil && v.Flagged && len(v.Categories) == 0 {
			t.Fatal("flagged without a category")
		}
		if v, err := ParsePromptGuard(in, 1, 0.5); err == nil && v.Flagged && v.Categories[0] != "injection" {
			t.Fatal("prompt guard category")
		}
		if v, err := ParseHTTP(in); err == nil && v.Flagged {
			if v.Action != "block" && v.Action != "log" || len(v.Categories) == 0 || len(v.Categories) > 16 {
				t.Fatalf("%+v", v)
			}
			for _, c := range v.Categories {
				if !categoryName.MatchString(c) {
					t.Fatalf("category %q", c)
				}
			}
		}
		_, _ = ChatAnswer(in)
	})
}

func TestParsePromptGuard(t *testing.T) {
	v, err := ParsePromptGuard([]byte(`{"data":[{"index":0,"label":"BENIGN","probs":[0.9,0.1]},{"index":1,"label":"MALICIOUS","probs":[0.05,0.95]}]}`), 2, 0.9)
	if err != nil || !v.Flagged {
		t.Fatalf("%+v %v", v, err)
	}
	v, err = ParsePromptGuard([]byte(`{"data":[{"index":0,"label":"MALICIOUS","probs":[0.3,0.7]}]}`), 1, 0.9)
	if err != nil || v.Flagged {
		t.Fatalf("below threshold: %+v %v", v, err)
	}
	if _, err := ParsePromptGuard([]byte(`{"data":[]}`), 1, 0.9); err == nil {
		t.Fatal("missing segment accepted")
	}
	if _, err := ParsePromptGuard([]byte(`{"data":[{"label":"MALICIOUS","probs":[2]}]}`), 1, 0.9); err == nil {
		t.Fatal("probability 2 accepted")
	}
	segs, ok := PromptGuardInput([]Message{{"system", "x"}, {"user", strings.Repeat("é", SegmentBytes)}, {"tool", "t"}})
	if !ok || len(segs) != 3 || segs[2] != "t" {
		t.Fatalf("%d %v", len(segs), ok)
	}
	for _, s := range segs {
		if !utf8Valid(s) || len(s) > SegmentBytes {
			t.Fatalf("segment of %d bytes", len(s))
		}
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "\uFFFD") == s }

func TestParseHTTP(t *testing.T) {
	if v, err := ParseHTTP([]byte(`{"action":"allow"}`)); err != nil || v.Flagged {
		t.Fatal(v, err)
	}
	if v, err := ParseHTTP([]byte(`{"action":"block","categories":["pii.email"]}`)); err != nil || !v.Flagged || v.Action != "block" {
		t.Fatal(v, err)
	}
	for _, bad := range []string{`{"action":"mask"}`, `{"action":"block","categories":["a b"]}`, `nope`} {
		if _, err := ParseHTTP([]byte(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func FuzzMapRequest(f *testing.F) {
	f.Add([]byte(`{"messages":[{"role":"user","content":"SECRET12345"}]}`))
	f.Add([]byte(`{"messages":[{"role":"user","content":[{"type":"text","text":"SECRET55555"}]}],"prompt":["x"]}`))
	f.Add([]byte(`{"input":["SECRET1111",[1,2]]}`))
	f.Add([]byte(`{"query":"SECRET12345","documents":["SECRET99999",{"content":[]}]}`))
	f.Add([]byte(`{"system":[{"type":"text","text":"SECRET4444"}],"messages":[{"role":"assistant","content":[{"type":"tool_use","input":{"k":"SECRET12345"}}]},{"role":"user","content":[{"type":"tool_result","content":"SECRET98765"}]}]}`))
	f.Add([]byte(`{"instructions":"SECRET0000","input":[{"role":"user","content":[{"type":"input_text","text":"SECRET12345"}]},{"type":"function_call_output","output":"SECRET9999"}]}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		mask := func(_, s string) string { return Apply(s, secretFinder(s), Placeholder) }
		out, msgs, err := MapRequest(in, mask)
		if err != nil {
			return
		}
		if !json.Valid(out) {
			t.Fatalf("invalid JSON out of %q", in)
		}
		for _, m := range msgs {
			if secretRE.MatchString(m.Content) {
				t.Fatalf("unmasked text %q", m.Content)
			}
		}
		// What the guards saw is what goes out: mapping again finds the
		// same texts.
		_, again, err := MapRequest(out, func(_, s string) string { return s })
		if err != nil || len(again) != len(msgs) {
			t.Fatalf("remap: %v, %d texts then %d", err, len(msgs), len(again))
		}
	})
}

// FuzzMapResponse: a guarded answer (chat or Responses) holds no secret in
// any string but the identifiers.
func FuzzMapResponse(f *testing.F) {
	f.Add([]byte(`{"choices":[{"message":{"content":"SECRET12345","reasoning_content":"SECRET1111"}}]}`))
	f.Add([]byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"SECRET12345"}]},{"type":"reasoning","summary":[{"text":"SECRET1234"}]}],"instructions":"SECRET0000"}`))
	f.Add([]byte(`{"output":[{"type":"function_call","arguments":"SECRET5555"}],"error":{"message":"SECRET7777"}}`))
	f.Add([]byte(`{"id":"r","results":[{"index":0,"relevance_score":0.5,"document":{"text":"SECRET12345","extra":"SECRET0000"},"x":"SECRET1111"}]}`))
	f.Add([]byte(`{"type":"message","content":[{"type":"text","text":"SECRET12345"},{"type":"tool_use","input":{"a":["SECRET99999"]}},{"type":"thinking","thinking":"SECRET1111"}],"stop_sequence":"SECRET2222"}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		out, _, err := MapResponse(in, true, func(s string) string { return Apply(s, secretFinder(s), Placeholder) })
		if err != nil {
			return
		}
		var v any
		if json.Unmarshal(out, &v) != nil {
			t.Fatalf("invalid JSON out of %q", in)
		}
		if k := secretIn(v, ""); k != "" {
			t.Fatalf("secret left in %s: %s", k, out)
		}
	})
}
