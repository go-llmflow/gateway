package config

import "testing"

func TestParseTracing(t *testing.T) {
	c, err := parse(valid + `
tracing {
  endpoint     = "https://otel.example.org:4318"
  sample_ratio = 0.25
  token_file   = "/etc/llmflow/otel.token"
}
`)
	if err != nil {
		t.Fatal(err)
	}
	tr := c.Tracing
	if tr == nil || tr.Protocol != "http" || tr.SampleRatio != 0.25 || tr.ServiceName != "llmflow-gateway" || tr.TrustClientContext || tr.Insecure {
		t.Fatalf("%+v", tr)
	}
	if c, err := parse(valid + "tracing {\n  endpoint = \"otel:4317\"\n  protocol = \"grpc\"\n  insecure = true\n}\n"); err != nil || c.Tracing.Protocol != "grpc" {
		t.Fatalf("grpc: %v", err)
	}
	for name, block := range map[string]string{
		"plain http":   "tracing {\n  endpoint = \"http://otel:4318\"\n}\n",
		"credentials":  "tracing {\n  endpoint = \"https://u:p@otel:4318\"\n}\n",
		"grpc url":     "tracing {\n  endpoint = \"https://otel:4317\"\n  protocol = \"grpc\"\n}\n",
		"protocol":     "tracing {\n  endpoint = \"https://otel:4318\"\n  protocol = \"udp\"\n}\n",
		"ratio":        "tracing {\n  endpoint = \"https://otel:4318\"\n  sample_ratio = 2\n}\n",
		"service name": "tracing {\n  endpoint = \"https://otel:4318\"\n  service_name = \"a b\"\n}\n",
	} {
		if _, err := parse(valid + block); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
