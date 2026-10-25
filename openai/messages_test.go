package openai

import (
	"bytes"
	"strings"
	"testing"
)

func TestRewriteMessages(t *testing.T) {
	out, req, err := RewriteFor(Messages, []byte(`{"model":"m","max_tokens":256,"stream":true,"system":"s","messages":[{"role":"user","content":"hi"}],"priority":-1,"cache_salt":"x","vllm_xargs":{"a":1},"mm_payload":"raw"}`),
		resolveAll, Isolation{CacheSalt: "ours"})
	if err != nil {
		t.Fatal(err)
	}
	b := decode(t, out)
	if b["model"] != "up-m" || b["cache_salt"] != "ours" || b["priority"] != nil || b["vllm_xargs"] != nil || b["mm_payload"] != nil ||
		b["stream_options"] != nil || b["system"] != "s" {
		t.Fatalf("%s", out)
	}
	if !req.Stream || req.MaxTokens != 256 || !req.ClientWantsUsage {
		t.Fatalf("%+v", req)
	}
	for _, bad := range []string{`{"max_tokens":1}`, `{"model":"m","max_tokens":-1}`, `{"model":"m","stream":1}`, `[1]`} {
		if _, _, err := RewriteFor(Messages, []byte(bad), resolveAll, Isolation{}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestMessagesUsage(t *testing.T) {
	// vLLM's AnthropicUsage: input_tokens excludes the cached prompt.
	u, ok := ParseUsageFor(Messages, []byte(`{"id":"msg","type":"message","content":[],"usage":{"input_tokens":60,"cache_read_input_tokens":40,"output_tokens":20}}`))
	if !ok || u.PromptTokens != 100 || u.Cached() != 40 || u.CompletionTokens != 20 {
		t.Fatalf("%+v %v", u, ok)
	}
	if _, ok := ParseUsageFor(Messages, []byte(`{"usage":{"input_tokens":-1,"output_tokens":1}}`)); ok {
		t.Fatal("negative usage accepted")
	}
	stream := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":60,\"cache_read_input_tokens\":40,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"he\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"llo\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	var dst bytes.Buffer
	res, err := CopySSEFor(Messages, &dst, nil, strings.NewReader(stream), false)
	if err != nil || !res.HaveUsage || res.Usage.PromptTokens != 100 || res.Usage.Cached() != 40 || res.Usage.CompletionTokens != 20 || res.Deltas != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if dst.String() != stream {
		t.Fatal("the stream was changed")
	}
}

func FuzzMessagesStream(f *testing.F) {
	f.Add([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":5}}}\n\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":3}}\n\n"))
	f.Add([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":2,\"cache_read_input_tokens\":9,\"output_tokens\":1}}\n\n"))
	f.Fuzz(func(t *testing.T, in []byte) {
		var dst bytes.Buffer
		res, err := CopySSEFor(Messages, &dst, nil, bytes.NewReader(in), false)
		if err == nil && !bytes.Equal(dst.Bytes(), in) {
			t.Fatal("the stream was changed")
		}
		if res.HaveUsage && !res.Usage.Valid() {
			t.Fatalf("invalid usage kept: %+v", res.Usage)
		}
		out, _, err := RewriteFor(Messages, in, resolveAll, Isolation{CacheSalt: "s"})
		if err != nil {
			return
		}
		m, err := decodeObject(out)
		if err != nil || string(m["cache_salt"]) != `"s"` {
			t.Fatalf("cache_salt not enforced: %s", out)
		}
		for _, f := range append(StrippedFields, messagesStripped...) {
			if f != "cache_salt" && m[f] != nil {
				t.Fatalf("%s survived: %s", f, out)
			}
		}
	})
}
