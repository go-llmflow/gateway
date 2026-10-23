package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func resolveAll(m string) (string, error) {
	if m == "unknown" {
		return "", errors.New("unknown model")
	}
	return "up-" + m, nil
}

func decode(t testing.TB, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("output is not JSON: %v: %s", err, b)
	}
	return m
}

func TestRewriteStripsClientPriority(t *testing.T) {
	in := `{"model":"llama","priority":-100,"extra_body":{"priority":0},"request_id":"x",` +
		`"kv_transfer_params":{"remote_host":"10.0.0.9"},"ec_transfer_params":{},"vllm_xargs":{"a":1},"cache_salt":"guess",` +
		`"priority":-5,"messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	out, req, err := Rewrite([]byte(in), resolveAll, Isolation{})
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	for _, f := range StrippedFields {
		if _, ok := m[f]; ok {
			t.Fatalf("%s survived: %s", f, out)
		}
	}
	if bytes.Contains(out, []byte("priority")) {
		t.Fatalf("priority text survived: %s", out)
	}
	if m["model"] != "up-llama" || req.Model != "llama" || req.MaxTokens != 64 || req.Stream {
		t.Fatalf("%+v %s", req, out)
	}
	if len(req.Stripped) != len(StrippedFields) {
		t.Fatalf("stripped %v", req.Stripped)
	}
}

func TestRewriteStreamUsage(t *testing.T) {
	out, req, err := Rewrite([]byte(`{"model":"m","stream":true,"stream_options":{"continuous_usage_stats":false}}`), resolveAll, Isolation{})
	if err != nil {
		t.Fatal(err)
	}
	so := decode(t, out)["stream_options"].(map[string]any)
	if so["include_usage"] != true || so["continuous_usage_stats"] != false || req.ClientWantsUsage || !req.Stream {
		t.Fatalf("%v %+v", so, req)
	}
	_, req, err = Rewrite([]byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"max_completion_tokens":5,"max_tokens":9}`), resolveAll, Isolation{})
	if err != nil || !req.ClientWantsUsage || req.MaxTokens != 5 {
		t.Fatalf("%+v %v", req, err)
	}
	out, _, err = Rewrite([]byte(`{"model":"m","stream":false,"stream_options":null}`), resolveAll, Isolation{})
	if err != nil || bytes.Contains(out, []byte("include_usage")) {
		t.Fatalf("non-stream got include_usage: %s %v", out, err)
	}
}

func TestRewriteRejects(t *testing.T) {
	for _, in := range []string{
		``, `null`, `[]`, `"x"`, `{"model":1}`, `{"model":""}`, `{}`, `{"model":"m"} {}`, `{"model":"m"}x`,
		`{"model":"m","stream":"yes"}`, `{"model":"m","max_tokens":-1}`, `{"model":"m","max_tokens":1.5}`,
		`{"model":"m","stream":true,"stream_options":[]}`,
		`{"model":"m","stream":true,"stream_options":{"include_usage":"y"}}`,
		`{"model":"unknown"}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","uuid":7}]}]}`,
	} {
		iso := Isolation{ScopeUUID: func(s string) string { return "S:" + s }}
		if out, _, err := Rewrite([]byte(in), resolveAll, iso); err == nil {
			t.Errorf("accepted %q -> %s", in, out)
		}
	}
}

// The gateway's salt replaces any client salt, and multimodal uuids are
// scoped, wherever they sit in the content parts.
func TestRewriteCacheIsolation(t *testing.T) {
	in := `{"model":"m","cache_salt":"other-tenant","cache_salt":"again","messages":[` +
		`{"role":"system","content":"plain text"},` +
		`{"role":"user","content":[{"type":"text","text":"x"},{"type":"image_url","image_url":{"url":"http://i"},"uuid":"img-1"},` +
		`{"type":"input_audio","uuid":null},{"type":"video_url","video_url":null,"uuid":"vid-2"}]}]}`
	iso := Isolation{CacheSalt: "SALT", ScopeUUID: func(s string) string { return "S:" + s }}
	out, req, err := Rewrite([]byte(in), resolveAll, iso)
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, out)
	if m["cache_salt"] != "SALT" || bytes.Contains(out, []byte("other-tenant")) || bytes.Contains(out, []byte("again")) {
		t.Fatalf("salt: %s", out)
	}
	if !bytes.Contains(out, []byte(`"uuid":"S:img-1"`)) || !bytes.Contains(out, []byte(`"uuid":"S:vid-2"`)) ||
		!bytes.Contains(out, []byte(`"uuid":null`)) || len(req.Stripped) != 1 {
		t.Fatalf("uuids: %s %v", out, req.Stripped)
	}
	// Without isolation the client salt is still removed and nothing is set.
	out, _, err = Rewrite([]byte(in), resolveAll, Isolation{})
	if err != nil || bytes.Contains(out, []byte("cache_salt")) || !bytes.Contains(out, []byte(`"uuid":"img-1"`)) {
		t.Fatalf("no isolation: %s %v", out, err)
	}
	// Messages that are not an array are left to vLLM to reject.
	if _, _, err := Rewrite([]byte(`{"model":"m","messages":"x"}`), resolveAll, iso); err != nil {
		t.Fatal(err)
	}
}

func FuzzRewrite(f *testing.F) {
	for _, s := range []string{
		`{"model":"m","priority":-1}`,
		`{"model":"m","stream":true,"stream_options":{"include_usage":false}}`,
		`{"model":"m","priority":1,"priority":2}`,
		`{"model":"m","priority":3,"extra_body":{"priority":0}}`,
		`{"model":"m","messages":[{"role":"user","content":"\u0000"}]}`,
		`{"model":"m","cache_salt":"x","messages":[{"role":"user","content":[{"type":"image_url","uuid":"u"}]}]}`,
	} {
		f.Add([]byte(s))
	}
	iso := Isolation{CacheSalt: "SALT", ScopeUUID: func(s string) string { return "S:" + s }}
	f.Fuzz(func(t *testing.T, in []byte) {
		out, req, err := Rewrite(in, resolveAll, iso)
		if err != nil {
			return
		}
		m := decode(t, out)
		for _, k := range StrippedFields {
			if _, ok := m[k]; ok && k != "cache_salt" {
				t.Fatalf("%s survived in %s", k, out)
			}
		}
		if m["cache_salt"] != "SALT" {
			t.Fatalf("cache_salt %v in %s", m["cache_salt"], out)
		}
		// Every multimodal uuid in the content parts is scoped.
		if msgs, ok := m["messages"].([]any); ok {
			for _, mm := range msgs {
				msg, _ := mm.(map[string]any)
				parts, _ := msg["content"].([]any)
				for _, p := range parts {
					part, _ := p.(map[string]any)
					if u, ok := part["uuid"]; ok && u != nil {
						if s, _ := u.(string); !strings.HasPrefix(s, "S:") {
							t.Fatalf("unscoped uuid %v in %s", u, out)
						}
					}
				}
			}
		}
		if m["model"] != "up-"+req.Model {
			t.Fatalf("model %v", m["model"])
		}
		if req.Stream {
			so, ok := m["stream_options"].(map[string]any)
			if !ok || so["include_usage"] != true {
				t.Fatalf("stream without include_usage: %s", out)
			}
		}
		// Idempotent on its own output (modulo the model mapping).
		out2, _, err := Rewrite(out, func(string) (string, error) { return "up-" + req.Model, nil }, Isolation{CacheSalt: "SALT"})
		if err != nil || !bytes.Equal(out, out2) {
			t.Fatalf("not idempotent: %v\n%s\n%s", err, out, out2)
		}
	})
}

func TestParseUsage(t *testing.T) {
	u, ok := ParseUsage([]byte(`{"choices":[{}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15,"prompt_tokens_details":{"cached_tokens":4}}}`))
	if !ok || u.PromptTokens != 10 || u.CompletionTokens != 5 || u.Cached() != 4 {
		t.Fatalf("%+v %v", u, ok)
	}
	for _, bad := range []string{`{}`, `x`, `{"usage":null}`, `{"usage":{"prompt_tokens":-1}}`,
		`{"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`} {
		if _, ok := ParseUsage([]byte(bad)); ok {
			t.Errorf("accepted %s", bad)
		}
	}
	if (Usage{}).Cached() != 0 {
		t.Fatal("Cached of nil details")
	}
}

const stream = "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}],\"usage\":null}\n\n" +
	"data: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n" +
	"data: [DONE]\n\n"

type flushCounter struct {
	bytes.Buffer
	flushes int
}

func TestCopySSEPassThrough(t *testing.T) {
	var out flushCounter
	r, err := CopySSE(&out, func() { out.flushes++ }, strings.NewReader(stream), false)
	u, ok := r.Usage, r.HaveUsage
	if err != nil || !ok || u.PromptTokens != 7 || u.CompletionTokens != 2 || r.Deltas != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	if out.String() != stream {
		t.Fatalf("not byte-identical:\n%q", out.String())
	}
	if out.flushes != 4 {
		t.Fatalf("flushes %d, want one per event", out.flushes)
	}
}

func TestCopySSEDropsInjectedUsage(t *testing.T) {
	var out bytes.Buffer
	r, err := CopySSE(&out, nil, strings.NewReader(strings.ReplaceAll(stream, "\n", "\r\n")), true)
	if err != nil || !r.HaveUsage || r.Usage.TotalTokens != 9 {
		t.Fatalf("%+v %v", r, err)
	}
	if strings.Contains(out.String(), "prompt_tokens") || !strings.Contains(out.String(), "[DONE]") ||
		!strings.Contains(out.String(), `"content":"b"`) {
		t.Fatalf("%q", out.String())
	}
}

func TestCopySSEUnterminatedAndLong(t *testing.T) {
	var out bytes.Buffer
	tail := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}"
	r, err := CopySSE(&out, func() {}, strings.NewReader(tail), true)
	if err != nil || !r.HaveUsage || r.Usage.TotalTokens != 2 || out.String() != tail {
		t.Fatalf("%+v %v %q", r, err, out.String())
	}
	long := "data: " + strings.Repeat("x", 3*maxEvent) + "\n\n" + stream
	out.Reset()
	r, err = CopySSE(&out, nil, strings.NewReader(long), false)
	if err != nil || !r.HaveUsage || r.Usage.TotalTokens != 9 || out.String() != long {
		t.Fatalf("long event: %+v %v len %d", r, err, out.Len())
	}
}

// Deltas counts the content forwarded before a stream is cut, so that
// work done by vLLM can be charged when the usage chunk never arrives.
func TestCopySSECountsDeltas(t *testing.T) {
	in := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n" + // empty: not a token
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"}}]}\n\n" + // not content
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}},{\"delta\":{\"content\":\"b\"}}]}\n\n" + // n=2
		"data: {\"choices\":[{\"delta\":{\"content\":\"c\"}}]}" // trailing, unterminated
	r, err := CopySSE(io.Discard, nil, strings.NewReader(in), false)
	if err != nil || r.Deltas != 3 || r.HaveUsage {
		t.Fatalf("%+v %v", r, err)
	}
	// A client gone after two events: only what was forwarded counts.
	r, err = CopySSE(&failWriter{after: 3}, nil, strings.NewReader(in), false)
	if err == nil || r.Deltas != 2 {
		t.Fatalf("after write failure: %+v %v", r, err)
	}
}

type failWriter struct{ after int }

func (w *failWriter) Write(b []byte) (int, error) {
	if w.after <= 0 {
		return 0, io.ErrClosedPipe
	}
	w.after--
	return len(b), nil
}

type errReader struct{ r io.Reader }

func (e errReader) Read(b []byte) (int, error) {
	n, err := e.r.Read(b)
	if err == io.EOF {
		return n, io.ErrUnexpectedEOF
	}
	return n, err
}

func TestCopySSEErrors(t *testing.T) {
	if _, err := CopySSE(&failWriter{}, nil, strings.NewReader(stream), false); err == nil {
		t.Fatal("write error swallowed")
	}
	long := "data: " + strings.Repeat("x", 2*maxEvent) + "\n\n"
	for i := 0; i < 3; i++ {
		if _, err := CopySSE(&failWriter{after: i}, nil, strings.NewReader(long), false); err == nil {
			t.Fatalf("write error swallowed in overflow (after %d)", i)
		}
	}
	if _, err := CopySSE(&failWriter{}, nil, strings.NewReader("data: x"), false); err == nil {
		t.Fatal("trailing write error swallowed")
	}
	if _, err := CopySSE(io.Discard, nil, errReader{strings.NewReader("data: x\n\ndata: y")}, false); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error: %v", err)
	}
}

func FuzzCopySSE(f *testing.F) {
	f.Add([]byte(stream), false)
	f.Add([]byte(strings.ReplaceAll(stream, "\n", "\r\n")), true)
	f.Add([]byte("data: {\"usage\":{\"prompt_tokens\":1}}\n\n:comment\n\n"), true)
	f.Fuzz(func(t *testing.T, in []byte, drop bool) {
		var out bytes.Buffer
		_, err := CopySSE(&out, nil, bytes.NewReader(in), drop)
		if err != nil {
			t.Fatal(err)
		}
		if !drop && !bytes.Equal(out.Bytes(), in) {
			t.Fatalf("pass-through altered the stream")
		}
		if out.Len() > len(in) {
			t.Fatalf("output longer than input")
		}
	})
}
