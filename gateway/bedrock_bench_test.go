package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/aws/sigv4"
	"github.com/go-llmflow/gateway/config"
)

// The per-request overhead Bedrock adds: preparing the body, signing it with
// SigV4, and decoding the event stream into SSE. Measure at a load of 4 or
// less (sysctl -n vm.loadavg); the numbers are in the README.

func benchBody(n int) []byte {
	return []byte(`{"model":"claude","max_tokens":1024,"stream":true,"messages":[{"role":"user","content":"` + strings.Repeat("x", n) + `"}]}`)
}

// BenchmarkBedrockPrepare: the wire body and the SigV4 signature of a request
// with a 4 KiB prompt.
func BenchmarkBedrockPrepare(b *testing.B) {
	rt := &bedrockRuntime{cfg: &config.Bedrock{Region: "eu-west-3", ModelID: bedrockModel},
		orgs: map[string]awsCred{"o": {Credentials: sigv4.Credentials{AccessKeyID: akidAlpha, SecretAccessKey: skAlpha, SessionToken: tokAlpha}}}}
	ext := &config.External{Betas: []string{"*"}}
	body := benchBody(4096)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		wire, err := rt.wireBody(body, ext, []string{"context-management-2025-06-27"})
		if err != nil {
			b.Fatal(err)
		}
		req, _ := http.NewRequest("POST", rt.target("https://bedrock-runtime.eu-west-3.amazonaws.com", true), bytes.NewReader(wire))
		req.Header.Set("Accept", bedrockEventStream)
		if err := rt.prepare(req, wire, "o", now); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBedrockSign: the SigV4 signature alone, over a 4 KiB body.
func BenchmarkBedrockSign(b *testing.B) {
	s := sigv4.Signer{Credentials: sigv4.Credentials{AccessKeyID: akidAlpha, SecretAccessKey: skAlpha, SessionToken: tokAlpha}, Region: "eu-west-3", Service: "bedrock"}
	body := benchBody(4096)
	req, _ := http.NewRequest("POST", "https://bedrock-runtime.eu-west-3.amazonaws.com/model/eu.anthropic.claude-sonnet-4-5-20250929-v1%3A0/invoke-with-response-stream", nil)
	req.Header.Set("Content-Type", "application/json")
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if err := s.Sign(req, body, now); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkBedrockStream: decoding a 200-chunk answer (a text delta each)
// into Anthropic SSE.
func BenchmarkBedrockStream(b *testing.B) {
	var raw bytes.Buffer
	evs := bedrockEvents(bedrockUsage{10, 5, 100, 50})
	raw.Write(chunkEvent(evs[0]))
	raw.Write(chunkEvent(evs[1]))
	for i := 0; i < 200; i++ {
		raw.Write(chunkEvent(evs[2]))
	}
	raw.Write(chunkEvent(evs[3]))
	raw.Write(chunkEvent(evs[4]))
	raw.Write(chunkEvent(evs[5]))
	b.ReportAllocs()
	b.SetBytes(int64(raw.Len()))
	for i := 0; i < b.N; i++ {
		c := newBedrockSSE(bytes.NewReader(raw.Bytes()))
		if n, err := io.Copy(io.Discard, c); err != nil || n == 0 || !c.Complete() {
			b.Fatalf("%d %v %v", n, err, c.Complete())
		}
	}
}
