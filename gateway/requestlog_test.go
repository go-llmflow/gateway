package gateway

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
)

// requestLogEnv runs a gateway whose requests are logged per cfg.Logging
// to a file export, sends what send sends, and returns the records each
// logging block wrote, raw, by block name.
func requestLogEnv(t *testing.T, logging []config.Logging, send func(e *env)) map[string][]string {
	t.Helper()
	ctx := context.Background()
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Exports = []config.Export{{Name: "local", Kind: "file", Directory: t.TempDir(), QueueSize: 16}}
	cfg.Logging = logging
	e := newEnv(t, cfg)
	send(e)
	e.gw.Close() // waits for the handlers, and so for their records
	for _, l := range e.srv.uniqueRequestLogs() {
		l.flush()
	}
	e.srv.exports["local"].Close(ctx)

	sink := e.srv.exports["local"].Sink()
	keys, err := sink.List(ctx, "requests/")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, k := range keys {
		parts := strings.Split(k, "/")
		if len(parts) != 6 || !strings.HasSuffix(k, ".jsonl.gz") {
			t.Fatalf("object key %q: want requests/<name>/YYYY/MM/DD/<n>.jsonl.gz", k)
		}
		gz, err := sink.Get(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		zr, err := gzip.NewReader(bytes.NewReader(gz))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := io.ReadAll(zr)
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
			out[parts[1]] = append(out[parts[1]], line)
		}
	}
	return out
}

func logging(name string, content bool, teams ...string) config.Logging {
	return config.Logging{Name: name, Teams: teams, Destination: "local", Content: content,
		MaxContentBytes: 1 << 20, BatchRecords: 100, BatchInterval: time.Hour}
}

const streamChat = `{"model":"llama","stream":true,"messages":[{"role":"user","content":"hi"}]}`

func TestRequestLogMetadataOnlyByDefault(t *testing.T) {
	got := requestLogEnv(t, []config.Logging{logging("meta", false, "etabA")}, func(e *env) {
		_, a := e.newKey("etabA")
		_, b := e.newKey("etabB")
		for _, body := range []string{chat, streamChat} {
			if c := e.chatWith(a, body); c != 200 {
				t.Fatalf("etabA: %d", c)
			}
		}
		if c := e.chatWith(b, chat); c != 200 {
			t.Fatalf("etabB: %d", c)
		}
	})
	if len(got) != 1 || len(got["meta"]) != 2 {
		t.Fatalf("records %v: want 2 of etabA in meta, none of etabB", got)
	}
	for _, line := range got["meta"] {
		var r map[string]any
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r["Team"] != "etabA" || r["request_id"] == "" || r["PromptTokens"] != float64(100) {
			t.Errorf("record %s: want etabA's spend metadata", line)
		}
		// No prompt and no completion: neither the message the client
		// sent nor the fake's answer may appear anywhere.
		if _, ok := r["content"]; ok || strings.Contains(line, "messages") || strings.Contains(line, `"t0`) {
			t.Errorf("metadata-only record holds content: %s", line)
		}
	}
}

func TestNoRequestLogWithoutConfig(t *testing.T) {
	got := requestLogEnv(t, nil, func(e *env) {
		_, a := e.newKey("etabA")
		e.chatWith(a, chat)
	})
	if len(got) != 0 {
		t.Fatalf("logged %v with no logging block", got)
	}
}

func TestRequestLogContentOptIn(t *testing.T) {
	l := logging("full", true, "etabA")
	l.MaxContentBytes = 64
	got := requestLogEnv(t, []config.Logging{l}, func(e *env) {
		_, a := e.newKey("etabA")
		e.chatWith(a, chat)
		e.chatWith(a, streamChat)
	})
	if len(got["full"]) != 2 {
		t.Fatalf("records %v", got)
	}
	var plain, stream struct {
		Stream  bool
		Content contentPart `json:"content"`
	}
	for _, line := range got["full"] {
		var r struct {
			Stream  bool
			Content contentPart `json:"content"`
		}
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			t.Fatal(err)
		}
		if r.Stream {
			stream = r
		} else {
			plain = r
		}
	}
	// The request is kept as the JSON the client sent (it fits in 64 bytes).
	var req map[string]any
	if err := json.Unmarshal(plain.Content.Request, &req); err != nil || req["model"] != "llama" || plain.Content.RequestTruncated {
		t.Errorf("request content %s (truncated %v): want the client's JSON", plain.Content.Request, plain.Content.RequestTruncated)
	}
	// The answer is longer than 64 bytes: cut, and said to be.
	if !plain.Content.ResponseTruncated || len(plain.Content.Response) != 64 || !strings.HasPrefix(plain.Content.Response, `{"id":"c"`) {
		t.Errorf("response content %q (truncated %v)", plain.Content.Response, plain.Content.ResponseTruncated)
	}
	// A stream is captured through the SSE copy.
	if !strings.HasPrefix(stream.Content.Response, "data: ") || !strings.Contains(stream.Content.Response, `"t0"`) || !stream.Content.ResponseTruncated {
		t.Errorf("stream content %q (truncated %v)", stream.Content.Response, stream.Content.ResponseTruncated)
	}
	// The request of the stream is longer than 64 bytes: cut, kept as a
	// JSON string since the cut is no longer JSON.
	var s string
	if err := json.Unmarshal(stream.Content.Request, &s); err != nil || !stream.Content.RequestTruncated || len(s) != 64 {
		t.Errorf("cut request %s (truncated %v)", stream.Content.Request, stream.Content.RequestTruncated)
	}
}

func TestRequestLogBatching(t *testing.T) {
	l := logging("b", false, "etabA")
	l.BatchRecords = 2
	got := requestLogEnv(t, []config.Logging{l}, func(e *env) {
		_, a := e.newKey("etabA")
		for range 5 {
			e.chatWith(a, chat)
		}
	})
	if len(got["b"]) != 5 {
		t.Fatalf("%d records, want 5", len(got["b"]))
	}
}
