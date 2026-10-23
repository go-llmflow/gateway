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
		`"kv_transfer_params":{"remote_host":"10.0.0.9"},"ec_transfer_params":{},"vllm_xargs":{"a":1},` +
		`"priority":-5,"messages":[{"role":"user","content":"hi"}],"max_tokens":64}`
	out, req, err := Rewrite([]byte(in), resolveAll)
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
	out, req, err := Rewrite([]byte(`{"model":"m","stream":true,"stream_options":{"continuous_usage_stats":false}}`), resolveAll)
	if err != nil {
		t.Fatal(err)
	}
	so := decode(t, out)["stream_options"].(map[string]any)
	if so["include_usage"] != true || so["continuous_usage_stats"] != false || req.ClientWantsUsage || !req.Stream {
		t.Fatalf("%v %+v", so, req)
	}
	_, req, err = Rewrite([]byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true},"max_completion_tokens":5,"max_tokens":9}`), resolveAll)
	if err != nil || !req.ClientWantsUsage || req.MaxTokens != 5 {
		t.Fatalf("%+v %v", req, err)
	}
	out, _, err = Rewrite([]byte(`{"model":"m","stream":false,"stream_options":null}`), resolveAll)
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
	} {
		if out, _, err := Rewrite([]byte(in), resolveAll); err == nil {
			t.Errorf("accepted %q -> %s", in, out)
		}
	}
}

func FuzzRewrite(f *testing.F) {
	for _, s := range []string{
		`{"model":"m","priority":-1}`,
		`{"model":"m","stream":true,"stream_options":{"include_usage":false}}`,
		`{"model":"m","priority":1,"priority":2}`,
		`{"model":"m","priority":3,"extra_body":{"priority":0}}`,
		`{"model":"m","messages":[{"role":"user","content":"\u0000"}]}`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, in []byte) {
		out, req, err := Rewrite(in, resolveAll)
		if err != nil {
			return
		}
		m := decode(t, out)
		for _, k := range StrippedFields {
			if _, ok := m[k]; ok {
				t.Fatalf("%s survived in %s", k, out)
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
		out2, _, err := Rewrite(out, func(string) (string, error) { return "up-" + req.Model, nil })
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
	u, ok, err := CopySSE(&out, func() { out.flushes++ }, strings.NewReader(stream), false)
	if err != nil || !ok || u.PromptTokens != 7 || u.CompletionTokens != 2 {
		t.Fatalf("%+v %v %v", u, ok, err)
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
	u, ok, err := CopySSE(&out, nil, strings.NewReader(strings.ReplaceAll(stream, "\n", "\r\n")), true)
	if err != nil || !ok || u.TotalTokens != 9 {
		t.Fatalf("%+v %v %v", u, ok, err)
	}
	if strings.Contains(out.String(), "prompt_tokens") || !strings.Contains(out.String(), "[DONE]") ||
		!strings.Contains(out.String(), `"content":"b"`) {
		t.Fatalf("%q", out.String())
	}
}

func TestCopySSEUnterminatedAndLong(t *testing.T) {
	var out bytes.Buffer
	tail := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}"
	u, ok, err := CopySSE(&out, func() {}, strings.NewReader(tail), true)
	if err != nil || !ok || u.TotalTokens != 2 || out.String() != tail {
		t.Fatalf("%+v %v %v %q", u, ok, err, out.String())
	}
	long := "data: " + strings.Repeat("x", 3*maxEvent) + "\n\n" + stream
	out.Reset()
	u, ok, err = CopySSE(&out, nil, strings.NewReader(long), false)
	if err != nil || !ok || u.TotalTokens != 9 || out.String() != long {
		t.Fatalf("long event: %v %v len %d", ok, err, out.Len())
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
	if _, _, err := CopySSE(&failWriter{}, nil, strings.NewReader(stream), false); err == nil {
		t.Fatal("write error swallowed")
	}
	long := "data: " + strings.Repeat("x", 2*maxEvent) + "\n\n"
	for i := 0; i < 3; i++ {
		if _, _, err := CopySSE(&failWriter{after: i}, nil, strings.NewReader(long), false); err == nil {
			t.Fatalf("write error swallowed in overflow (after %d)", i)
		}
	}
	if _, _, err := CopySSE(&failWriter{}, nil, strings.NewReader("data: x"), false); err == nil {
		t.Fatal("trailing write error swallowed")
	}
	if _, _, err := CopySSE(io.Discard, nil, errReader{strings.NewReader("data: x\n\ndata: y")}, false); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("read error: %v", err)
	}
}

func FuzzCopySSE(f *testing.F) {
	f.Add([]byte(stream), false)
	f.Add([]byte(strings.ReplaceAll(stream, "\n", "\r\n")), true)
	f.Add([]byte("data: {\"usage\":{\"prompt_tokens\":1}}\n\n:comment\n\n"), true)
	f.Fuzz(func(t *testing.T, in []byte, drop bool) {
		var out bytes.Buffer
		_, _, err := CopySSE(&out, nil, bytes.NewReader(in), drop)
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
