package config

import (
	"strings"
	"testing"
)

// The peer dispatcher merges the mount's histograms bucket by bucket (the last
// line it reads wins), so a mount whose gateway serves several models reports
// the latency of one of them as the node's. A federation needs ONE model: more
// is refused, naming them.
func TestAFederationRefusesAGatewayThatServesSeveralModels(t *testing.T) {
	const blk = `federation "ilaas" {
  prefix = "/fed/ilaas"
  org = "ilaas"
  metrics_networks = ["10.0.0.0/8"]
}`
	two := fedBase + `model "mistral" {
  pool     = "b300"
  key_file = "/unused"
  upstream "b" { base_url = "http://127.0.0.1:2" }
}
`
	_, err := Parse([]byte(two+blk), "fed.hcl")
	if err == nil || !strings.Contains(err.Error(), "several models") ||
		!strings.Contains(err.Error(), "llama") || !strings.Contains(err.Error(), "mistral") {
		t.Fatalf("two models and a federation: %v", err)
	}
	// Without a federation, any number of models; with one model, a federation.
	if _, err := Parse([]byte(two), "fed.hcl"); err != nil {
		t.Errorf("two models, no federation: %v", err)
	}
	if _, err := fedConfig(blk); err != nil {
		t.Errorf("one model, one federation: %v", err)
	}
}
