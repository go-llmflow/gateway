package gateway

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-llmflow/gateway/config"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// OpenTelemetry traces, with the official Go SDK, exported over OTLP/HTTP or
// OTLP/gRPC. A request is one server span ("llmflow <route>") with child
// spans for authentication, the guardrails of each stage (and one per
// classifier call), and the upstream call; the W3C trace context of the
// upstream span is sent to vLLM (traceparent/tracestate, which
// vllm/tracing reads), so vLLM's own spans join the trace.
//
// No content is ever put in a span: the attributes are the team, org, key
// id, models, pool, priority, status and token counts — what the spend
// record holds — and errors are the gateway's own messages, which never
// quote a prompt or an answer.

// tracing is the tracer and its shutdown; a noop tracer when no tracing
// block is configured (a few nanoseconds per span).
type tracing struct {
	tracer     trace.Tracer
	propagator propagation.TextMapPropagator
	trust      bool
	shutdown   func(context.Context) error
}

func newTracing(c *config.Tracing, token string) (*tracing, error) {
	t := &tracing{tracer: noop.NewTracerProvider().Tracer("llmflow-gateway"), propagator: propagation.TraceContext{},
		shutdown: func(context.Context) error { return nil }}
	if c == nil {
		return t, nil
	}
	headers := map[string]string{}
	if token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	var client otlptrace.Client
	switch c.Protocol {
	case "grpc":
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(c.Endpoint), otlptracegrpc.WithHeaders(headers)}
		if c.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		client = otlptracegrpc.NewClient(opts...)
	default:
		u, err := url.Parse(c.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("gateway: tracing endpoint: %w", err)
		}
		path := strings.TrimRight(u.Path, "/") + "/v1/traces"
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(u.Host), otlptracehttp.WithURLPath(path), otlptracehttp.WithHeaders(headers)}
		if u.Scheme == "http" {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		client = otlptracehttp.NewClient(opts...)
	}
	exp, err := otlptrace.New(context.Background(), client)
	if err != nil {
		return nil, fmt.Errorf("gateway: tracing exporter: %w", err)
	}
	res := resource.NewSchemaless(attribute.String("service.name", c.ServiceName))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(c.SampleRatio))),
	)
	t.tracer, t.trust, t.shutdown = tp.Tracer("llmflow-gateway"), c.TrustClientContext, tp.Shutdown
	return t, nil
}

// startRequest starts the server span of a request: a client's trace
// context is continued only when the configuration trusts it; otherwise
// the trace starts here (W3C Trace Context: an entry point restarts the
// trace of untrusted callers, which also keeps a caller from forcing
// sampling).
func (t *tracing) startRequest(r *http.Request, route string) (context.Context, trace.Span) {
	ctx := r.Context()
	if t.trust {
		ctx = t.propagator.Extract(ctx, propagation.HeaderCarrier(r.Header))
	} else {
		ctx = trace.ContextWithSpanContext(ctx, trace.SpanContext{})
	}
	return t.tracer.Start(ctx, "llmflow "+route, trace.WithSpanKind(trace.SpanKindServer),
		trace.WithAttributes(attribute.String("http.route", route)))
}

// inject writes the trace context of ctx into an upstream request.
func (t *tracing) inject(ctx context.Context, h http.Header) {
	t.propagator.Inject(ctx, propagation.HeaderCarrier(h))
}
