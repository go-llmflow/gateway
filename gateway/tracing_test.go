package gateway

import (
	"context"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/go-llmflow/gateway/config"
	collectortrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"
)

// fakeCollector is an OTLP/HTTP collector: it keeps the spans it receives.
type fakeCollector struct {
	srv   *httptest.Server
	mu    sync.Mutex
	spans []*tracepb.Span
	auth  string
}

func newFakeCollector(t *testing.T) *fakeCollector {
	c := &fakeCollector{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/traces" {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var req collectortrace.ExportTraceServiceRequest
		if err := proto.Unmarshal(raw, &req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		c.mu.Lock()
		c.auth = r.Header.Get("Authorization")
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				c.spans = append(c.spans, ss.Spans...)
			}
		}
		c.mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		out, _ := proto.Marshal(&collectortrace.ExportTraceServiceResponse{})
		_, _ = w.Write(out)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *fakeCollector) byName(name string) []*tracepb.Span {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*tracepb.Span
	for _, s := range c.spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

func attr(s *tracepb.Span, key string) string {
	for _, a := range s.Attributes {
		if a.Key == key {
			v := a.Value
			if sv := v.GetStringValue(); sv != "" {
				return sv
			}
			return strings.TrimSpace(v.String())
		}
	}
	return ""
}

func tracedEnv(t *testing.T, trust bool) (*env, *fakeVLLM, *fakeCollector) {
	up, col := newFakeVLLM(t), newFakeCollector(t)
	cfg := testConfig(up.URL())
	cfg.Tracing = &config.Tracing{Endpoint: col.srv.URL, Protocol: "http", Insecure: true, SampleRatio: 1,
		ServiceName: "llmflow-gateway", TrustClientContext: trust}
	e := newEnv(t, cfg)
	e.srv.secrets.TracingToken = ""
	return e, up, col
}

func TestTracesSpansAndPropagation(t *testing.T) {
	e, up, col := tracedEnv(t, false)
	_, key := e.newKey("etabA")
	clientTrace := "0af7651916cd43dd8448eb211c80319c"
	body := `{"model":"llama","messages":[{"role":"user","content":"TOPSECRETPROMPT"}]}`
	resp := e.do("POST", "/v1/chat/completions", key, body, "traceparent", "00-"+clientTrace+"-b7ad6b7169203331-01")
	readAll(t, resp)
	e.gw.Close()
	e.srv.Close(context.Background()) // flushes the batch

	roots := col.byName("llmflow /v1/chat/completions")
	if len(roots) != 1 {
		t.Fatalf("%d request spans", len(roots))
	}
	root := roots[0]
	if attr(root, "llmflow.team") != "etabA" || attr(root, "llmflow.model") != "llama" || attr(root, "llmflow.pool") != "b300" {
		t.Fatalf("attributes %v", root.Attributes)
	}
	// Not the client's trace: an entry point restarts it.
	if hex.EncodeToString(root.TraceId) == clientTrace {
		t.Fatal("the client's trace was continued")
	}
	for _, name := range []string{"auth", "upstream"} {
		spans := col.byName(name)
		if len(spans) != 1 || hex.EncodeToString(spans[0].TraceId) != hex.EncodeToString(root.TraceId) {
			t.Fatalf("span %s: %v", name, spans)
		}
	}
	// vLLM got the gateway's trace context, under the upstream span.
	tp := up.last().Header.Get("traceparent")
	upstream := col.byName("upstream")[0]
	if !strings.Contains(tp, hex.EncodeToString(root.TraceId)) || !strings.Contains(tp, hex.EncodeToString(upstream.SpanId)) {
		t.Fatalf("traceparent %q", tp)
	}
	// No content anywhere in the spans.
	col.mu.Lock()
	defer col.mu.Unlock()
	for _, s := range col.spans {
		if strings.Contains(s.String(), "TOPSECRETPROMPT") {
			t.Fatalf("prompt in span %s", s.Name)
		}
	}
}

func TestTracesContinueTrustedContext(t *testing.T) {
	e, _, col := tracedEnv(t, true)
	_, key := e.newKey("etabA")
	clientTrace := "0af7651916cd43dd8448eb211c80319c"
	readAll(t, e.do("POST", "/v1/chat/completions", key, chat, "traceparent", "00-"+clientTrace+"-b7ad6b7169203331-01"))
	e.gw.Close()
	e.srv.Close(context.Background())
	roots := col.byName("llmflow /v1/chat/completions")
	if len(roots) != 1 || hex.EncodeToString(roots[0].TraceId) != clientTrace {
		t.Fatalf("%v", roots)
	}
}
