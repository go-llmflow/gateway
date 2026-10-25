package config

import (
	"net/url"
	"regexp"

	"github.com/hashicorp/hcl/v2"
)

// The tracing block. References opened (2026-10-25): OTLP specification
// (default ports 4317 gRPC and 4318 HTTP, /v1/traces); W3C Trace Context
// (traceparent; an entry point into a secure network should restart the
// trace rather than continue an untrusted caller's).

type tracingHCL struct {
	Endpoint           string    `hcl:"endpoint"`
	EndpointRange      hcl.Range `hcl:"endpoint,attr_value_range"`
	Protocol           *string   `hcl:"protocol,optional"`
	ProtocolRange      hcl.Range `hcl:"protocol,attr_value_range"`
	Insecure           *bool     `hcl:"insecure,optional"`
	SampleRatio        *float64  `hcl:"sample_ratio,optional"`
	SampleRange        hcl.Range `hcl:"sample_ratio,attr_value_range"`
	ServiceName        *string   `hcl:"service_name,optional"`
	ServiceRange       hcl.Range `hcl:"service_name,attr_value_range"`
	TokenFile          *string   `hcl:"token_file,optional"`
	TrustClientContext *bool     `hcl:"trust_client_context,optional"`
}

var hostPort = regexp.MustCompile(`^[A-Za-z0-9.-]{1,253}:[0-9]{1,5}$`)

func (v *validator) tracing(t *tracingHCL) *Tracing {
	if t == nil {
		return nil
	}
	c := &Tracing{Endpoint: t.Endpoint, Protocol: strOr(t.Protocol, "http"), SampleRatio: 1,
		ServiceName: strOr(t.ServiceName, "llmflow-gateway"), TokenFile: strOr(t.TokenFile, "")}
	if t.Insecure != nil {
		c.Insecure = *t.Insecure
	}
	if t.TrustClientContext != nil {
		c.TrustClientContext = *t.TrustClientContext
	}
	if t.SampleRatio != nil {
		c.SampleRatio = *t.SampleRatio
		if !(c.SampleRatio >= 0 && c.SampleRatio <= 1) {
			v.errf(t.SampleRange, "Invalid sample_ratio", "sample_ratio %v: want 0..1", c.SampleRatio)
		}
	}
	if !ValidName(c.ServiceName) {
		v.errf(t.ServiceRange, "Invalid service_name", "service_name %q: want [A-Za-z0-9_.-]{1,64}", c.ServiceName)
	}
	switch c.Protocol {
	case "http":
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" || u.User != nil || !(u.Scheme == "https" || u.Scheme == "http" && c.Insecure) {
			v.errf(t.EndpointRange, "Invalid endpoint", "endpoint %q: want https://host:port (http:// only with insecure = true)", c.Endpoint)
		}
	case "grpc":
		if !hostPort.MatchString(c.Endpoint) {
			v.errf(t.EndpointRange, "Invalid endpoint", "endpoint %q: want host:port for grpc", c.Endpoint)
		}
	default:
		v.errf(t.ProtocolRange, "Invalid protocol", "protocol %q: want http or grpc", c.Protocol)
	}
	return c
}
