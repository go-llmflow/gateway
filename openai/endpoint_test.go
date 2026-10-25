package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRewriteResponses(t *testing.T) {
	out, req, err := RewriteFor(Responses, []byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_image","uuid":"u1"}]}],"stream":true,"max_output_tokens":64,"priority":-5,"cache_salt":"theirs","store":true,"background":false,"previous_response_id":null}`),
		resolveAll, Isolation{CacheSalt: "ours", ScopeUUID: func(u string) string { return "t/" + u }})
	if err != nil {
		t.Fatal(err)
	}
	b := decode(t, out)
	if b["model"] != "up-m" || b["store"] != false || b["cache_salt"] != "ours" || b["priority"] != nil || b["stream_options"] != nil {
		t.Fatalf("%s", out)
	}
	if !strings.Contains(string(out), `"uuid":"t/u1"`) {
		t.Fatalf("uuid not scoped: %s", out)
	}
	if !req.Stream || req.MaxTokens != 64 || !req.ClientWantsUsage || req.Model != "m" {
		t.Fatalf("%+v", req)
	}
	for _, bad := range []string{
		`{"model":"m","input":"x","previous_response_id":"resp_1"}`,
		`{"model":"m","input":"x","background":true}`,
		`{"model":"m","input":"x","conversation":"conv_1"}`,
		`{"model":"m","input":"x","prompt":{"id":"p"}}`,
	} {
		if _, _, err := RewriteFor(Responses, []byte(bad), resolveAll, Isolation{}); !errors.Is(err, ErrStateful) {
			t.Errorf("%s: %v", bad, err)
		}
	}
	for _, bad := range []string{`[]`, `{"input":"x"}`, `{"model":"m","max_output_tokens":-1}`, `{"model":"m","stream":"yes"}`, `{"model":"m"} {}`} {
		if _, _, err := RewriteFor(Responses, []byte(bad), resolveAll, Isolation{}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestResponsesUsage(t *testing.T) {
	u, ok := ParseUsageFor(Responses, []byte(`{"id":"r","output":[],"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"input_tokens_per_turn":[]},"output_tokens":20,"total_tokens":120}}`))
	if !ok || u.PromptTokens != 100 || u.CompletionTokens != 20 || u.Cached() != 40 {
		t.Fatalf("%+v %v", u, ok)
	}
	if _, ok := ParseUsageFor(Responses, []byte(`{"usage":{"input_tokens":1,"input_tokens_details":{"cached_tokens":5},"output_tokens":0}}`)); ok {
		t.Fatal("cached above input accepted")
	}
	stream := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"usage\":null}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"he\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"llo\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"total_tokens\":9}}}\n\n"
	var dst bytes.Buffer
	res, err := CopySSEFor(Responses, &dst, nil, strings.NewReader(stream), false)
	if err != nil || !res.HaveUsage || res.Usage.PromptTokens != 7 || res.Usage.CompletionTokens != 2 || res.Deltas != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if dst.String() != stream {
		t.Fatal("the stream was not passed through unchanged")
	}
}

func TestRewritePlain(t *testing.T) {
	out, _, err := RewriteFor(Rerank, []byte(`{"model":"m","query":"q","documents":["a"],"priority":1,"cache_salt":"x"}`), resolveAll, Isolation{CacheSalt: "ours"})
	if b := decode(t, out); err != nil || b["cache_salt"] != "ours" || b["priority"] != nil || b["model"] != "up-m" {
		t.Fatalf("%s %v", out, err)
	}
	out, _, err = RewriteFor(Tokenize, []byte(`{"model":"m","messages":[],"chat_template":"{{x}}"}`), resolveAll, Isolation{CacheSalt: "ours"})
	if b := decode(t, out); err != nil || b["cache_salt"] != nil || b["chat_template"] != nil {
		t.Fatalf("%s %v", out, err)
	}
	if u, ok := ParseUsageFor(Rerank, []byte(`{"usage":{"total_tokens":12}}`)); !ok || u.PromptTokens != 12 {
		t.Fatalf("%+v %v", u, ok)
	}
	if u, ok := ParseUsageFor(Tokenize, []byte(`{"count":3}`)); !ok || u.PromptTokens != 0 {
		t.Fatalf("%+v %v", u, ok)
	}
	if Tokenize.Metered() || Detokenize.Guarded() || !Rerank.Guarded() {
		t.Fatal("endpoint flags")
	}
}

func FuzzRewriteResponses(f *testing.F) {
	f.Add([]byte(`{"model":"m","input":"hi","store":true,"priority":3}`))
	f.Add([]byte(`{"model":"m","input":[{"role":"user","content":[{"type":"input_text","text":"x","uuid":"u"}]}],"stream":true}`))
	f.Add([]byte(`{"model":"m","previous_response_id":false,"background":null}`))
	f.Fuzz(func(t *testing.T, in []byte) {
		out, _, err := RewriteFor(Responses, in, resolveAll, Isolation{CacheSalt: "salt", ScopeUUID: func(u string) string { return "s:" + u }})
		if err != nil {
			return
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(out, &m); err != nil {
			t.Fatalf("invalid JSON out: %v", err)
		}
		if string(m["store"]) != "false" || string(m["cache_salt"]) != `"salt"` {
			t.Fatalf("store/cache_salt not enforced: %s", out)
		}
		for _, f := range StrippedFields {
			if f != "cache_salt" && m[f] != nil {
				t.Fatalf("%s survived: %s", f, out)
			}
		}
		for _, f := range statefulFields {
			if v := m[f]; v != nil && !isNull(v) && string(v) != "false" {
				t.Fatalf("%s survived: %s", f, out)
			}
		}
	})
}
