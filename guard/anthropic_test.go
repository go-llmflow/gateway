package guard

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func aev(typ, data string) string { return "event: " + typ + "\ndata: " + data + "\n\n" }

func newAnthropicFilter(dst *bytes.Buffer) *AnthropicStreamFilter {
	return &AnthropicStreamFilter{Dst: dst,
		NewHoldback: func() *Holdback { return &Holdback{Window: 16, Finders: []Finder{secretFinder}, Repl: Placeholder} },
		BlockEvent: func(error) []byte {
			return []byte(`{"type":"error","error":{"type":"guardrail_violation","message":"stopped"}}`)
		},
	}
}

// anthropicOut reassembles the client's view: the text of each block by
// client index, the events seen, whether an error came.
func anthropicOut(t *testing.T, out string) (map[int]string, []string, bool) {
	text := map[int]string{}
	var types []string
	errored := false
	for _, ev := range strings.Split(out, "\n\n") {
		data, ok := eventData([]byte(ev))
		if !ok {
			continue
		}
		var e struct {
			Type  string
			Index int
			Delta struct {
				Type, Text  string
				PartialJSON string `json:"partial_json"`
			}
		}
		if err := json.Unmarshal(data, &e); err != nil {
			t.Fatalf("event %q is not JSON", data)
		}
		types = append(types, e.Type)
		if e.Type == "error" {
			errored = true
		}
		if e.Type == "content_block_delta" {
			text[e.Index] += e.Delta.Text + e.Delta.PartialJSON
		}
	}
	return text, types, errored
}

func TestAnthropicFilterMasksAndRenumbers(t *testing.T) {
	var dst bytes.Buffer
	f := newAnthropicFilter(&dst)
	for _, ev := range []string{
		aev("message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"x","content":[],"usage":{"input_tokens":3},"extra":"SECRET99999"}}`),
		aev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`),
		aev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"SECRET55555 plans"}}`),
		aev("content_block_stop", `{"type":"content_block_stop","index":0}`),
		aev("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		aev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"key SEC"}}`),
		aev("content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"RET12345 ok"}}`),
		aev("content_block_stop", `{"type":"content_block_stop","index":1}`),
		aev("content_block_start", `{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"sh","input":{}}}`),
		aev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"env\": \"SECRET"}}`),
		aev("content_block_delta", `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"777777\"}"}}`),
		aev("content_block_stop", `{"type":"content_block_stop","index":2}`),
		aev("ping", `{"type":"ping"}`),
		aev("message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":"SECRET00000"},"usage":{"output_tokens":9}}`),
		aev("message_stop", `{"type":"message_stop"}`),
	} {
		if _, err := f.Write([]byte(ev)); err != nil {
			t.Fatalf("%v at %s", err, ev)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	out := dst.String()
	text, types, errored := anthropicOut(t, out)
	if errored || strings.Contains(out, "SECRET") || strings.Contains(out, "plans") {
		t.Fatalf("%s", out)
	}
	if text[0] != "key [TOKEN] ok" || text[1] != `{"env": "[TOKEN]"}` || len(text) != 2 {
		t.Fatalf("%v\n%s", text, out)
	}
	var tool map[string]any
	if err := json.Unmarshal([]byte(text[1]), &tool); err != nil {
		t.Fatalf("masked tool input is not JSON: %v", err)
	}
	if !strings.Contains(out, `"stop_reason":"tool_use"`) || !strings.Contains(out, `"output_tokens":9`) || types[len(types)-1] != "message_stop" {
		t.Fatalf("%s", out)
	}
}

// TestAnthropicFilterEscapedToolInput: a token written with \u escapes and
// split across input_json_delta fragments is masked, since the tool input
// is held whole and read through every JSON layer.
func TestAnthropicFilterEscapedToolInput(t *testing.T) {
	deep := Deep(func(_, s string) string { return Apply(s, Combine(s, []Finder{secretFinder}), Placeholder) })
	// "SECRET12345" with S, E and 3 escaped, cut in the middle of escapes.
	parts := []string{`{"env": "\u00`, `53ECRET123`, `45", "n": 1}`}
	events := []string{aev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"t","name":"sh","input":{}}}`)}
	for _, p := range parts {
		events = append(events, aev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":`+quoted(p)+`}}`))
	}
	events = append(events, aev("content_block_stop", `{"type":"content_block_stop","index":0}`))
	run := func(mask bool) string {
		var dst bytes.Buffer
		f := newAnthropicFilter(&dst)
		if mask {
			f.MaskJSON = func(in string) (string, error) { return deep("", in), nil }
		}
		for _, ev := range events {
			if _, err := f.Write([]byte(ev)); err != nil {
				t.Fatal(err)
			}
		}
		text, _, _ := anthropicOut(t, dst.String())
		return text[0]
	}
	// The control: read fragment by fragment, the decoded input leaks.
	if dec, _ := unescapeJSON(run(false)); !strings.Contains(dec, "SECRET12345") {
		t.Fatalf("the control does not reproduce the bypass: %q", dec)
	}
	var input map[string]any
	got := run(true)
	if err := json.Unmarshal([]byte(got), &input); err != nil || input["env"] != "[TOKEN]" || input["n"] != float64(1) {
		t.Fatalf("%q %v", got, err)
	}
}

func TestAnthropicFilterRefuses(t *testing.T) {
	for _, evs := range [][]string{
		{aev("bogus", `{"type":"bogus"}`)},
		{"data: not json\n\n"},
		{aev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"x"}}`)},
		{aev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"document"}}`)},
		{aev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text"}}`),
			aev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"x"}}`)},
	} {
		var dst bytes.Buffer
		f := newAnthropicFilter(&dst)
		var err error
		for _, ev := range evs {
			if _, err = f.Write([]byte(ev)); err != nil {
				break
			}
		}
		if !errors.Is(err, ErrBlocked) {
			t.Errorf("%q: %v", evs, err)
		}
	}
}

func TestAnthropicFilterGate(t *testing.T) {
	var dst bytes.Buffer
	f := newAnthropicFilter(&dst)
	f.NewHoldback = nil
	f.CheckEvery = 8
	f.Check = func(_ int, c string, _ bool) error {
		if strings.Contains(c, "BAD") {
			return errors.New("unsafe")
		}
		return nil
	}
	_, _ = f.Write([]byte(aev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)))
	_, _ = f.Write([]byte(aev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"fine text"}}`)))
	// " BAD" is under CheckEvery: held, and judged when the block ends.
	if _, err := f.Write([]byte(aev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" BAD"}}`))); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); !errors.Is(err, ErrBlocked) {
		t.Fatal(err)
	}
	text, _, errored := anthropicOut(t, dst.String())
	if text[0] != "fine text" || !errored {
		t.Fatalf("%v %v", text, errored)
	}
}

func FuzzAnthropicStreamFilter(f *testing.F) {
	f.Add([]byte(aev("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`) +
		aev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"SECRET1234"}}`) +
		aev("content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"5678"}}`)))
	f.Add([]byte(aev("message_start", `{"type":"message_start","message":{"id":"SECRET11111","usage":{"input_tokens":1}}}`) +
		aev("error", `{"type":"error","error":{"type":"x","message":"SECRET22222"}}`)))
	f.Add([]byte(aev("content_block_start", `{"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"a","name":"SECRET33333"}}`) +
		aev("content_block_delta", `{"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"SECRET44444"}}`)))
	f.Fuzz(func(t *testing.T, in []byte) {
		var dst bytes.Buffer
		af := newAnthropicFilter(&dst)
		_, err := af.Write(in)
		if err == nil {
			_ = af.Close()
		}
		for _, ev := range strings.Split(dst.String(), "\n\n") {
			data, ok := eventData([]byte(ev))
			if !ok {
				continue
			}
			var v any
			if json.Unmarshal(data, &v) != nil {
				t.Fatalf("event %q is not JSON", data)
			}
			if k := secretIn(v, ""); k != "" {
				t.Fatalf("secret released in %s: %s", k, dst.String())
			}
		}
	})
}
