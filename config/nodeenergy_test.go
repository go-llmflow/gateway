package config

import (
	"strings"
	"testing"
)

func nodeSrc(upstream, fluids string) []byte {
	return []byte(`
master_key_file = "/unused"
cache_salt_key_file = "/unused"
org "a" {
  share = 1
  team "a" {}
}
pool "b300" {
  capacity = 4
}
model "m" {
  pool     = "b300"
  key_file = "/unused"
  upstream "u" {
    base_url = "http://x:8000"
` + upstream + `
  }
}
` + fluids)
}

const bmcUp = `    dcgm_url = "http://x:9400/metrics"
    redfish_url           = "https://bmc-01"
    redfish_user          = "monitor"
    redfish_password_file = "/etc/llmflow/bmc.pw"
    redfish_ca_file       = "/etc/llmflow/bmc-ca.pem"
    node_overhead_factor  = 1.12`

func TestNodeEnergyConfig(t *testing.T) {
	c, err := Parse(nodeSrc(bmcUp, "fluids {\n  node_overhead_factor = 1.2\n  meter \"2026-10\" {\n    kwh = 1234.5\n  }\n  meter \"2026-10\" {\n    kwh = 600\n    pool = \"b300\"\n  }\n}\n"), "n.hcl")
	if err != nil {
		t.Fatal(err)
	}
	u := c.ModelGroups[0].Upstreams[0]
	if u.RedfishURL != "https://bmc-01" || u.RedfishUser != "monitor" || u.RedfishPasswordFile != "/etc/llmflow/bmc.pw" || u.RedfishCAFile != "/etc/llmflow/bmc-ca.pem" || u.NodeOverheadFactor != 1.12 {
		t.Fatalf("%+v", u)
	}
	if c.Fluids.NodeOverheadFactor != 1.2 || len(c.Fluids.Meters) != 2 || c.Fluids.Meters[1].Pool != "b300" || c.Fluids.Meters[0].KWh != 1234.5 {
		t.Fatalf("%+v %+v", c.Fluids.NodeOverheadFactor, c.Fluids.Meters)
	}
	found := false
	for _, r := range c.SecretRefs() {
		found = found || r == "/etc/llmflow/bmc.pw"
	}
	if !found {
		t.Fatal("the BMC password is not a secret reference (check-config would not look at it)")
	}
	for name, tc := range map[string]struct{ up, fl, want string }{
		"http":       {strings.Replace(bmcUp, "https://bmc-01", "http://bmc-01", 1), "fluids {\n}\n", "https"},
		"no dcgm":    {strings.Replace(bmcUp, "    dcgm_url = \"http://x:9400/metrics\"\n", "", 1), "fluids {\n}\n", "dcgm_url"},
		"no user":    {strings.Replace(bmcUp, "    redfish_user          = \"monitor\"\n", "", 1), "fluids {\n}\n", "redfish_user"},
		"creds url":  {strings.Replace(bmcUp, "https://bmc-01", "https://u:p@bmc-01", 1), "fluids {\n}\n", "credentials"},
		"factor":     {strings.Replace(bmcUp, "1.12", "0.9", 1), "fluids {\n}\n", "node_overhead_factor"},
		"fluids":     {"    dcgm_url = \"http://x:9400/metrics\"", "fluids {\n  node_overhead_factor = 9\n}\n", "node_overhead_factor"},
		"orphan":     {"    dcgm_url = \"http://x:9400/metrics\"\n    redfish_user = \"x\"", "fluids {\n}\n", "redfish_url"},
		"meter":      {"    dcgm_url = \"http://x:9400/metrics\"", "fluids {\n  meter \"2026-13\" {\n    kwh = 1\n  }\n}\n", "YYYY-MM"},
		"meter pool": {"    dcgm_url = \"http://x:9400/metrics\"", "fluids {\n  meter \"2026-10\" {\n    kwh = 1\n    pool = \"nope\"\n  }\n}\n", "not declared"},
		"meter dup":  {"    dcgm_url = \"http://x:9400/metrics\"", "fluids {\n  meter \"2026-10\" {\n    kwh = 1\n  }\n  meter \"2026-10\" {\n    kwh = 2\n  }\n}\n", "twice"},
		"meter neg":  {"    dcgm_url = \"http://x:9400/metrics\"", "fluids {\n  meter \"2026-10\" {\n    kwh = -1\n  }\n}\n", "kwh"},
	} {
		if _, err := Parse(nodeSrc(tc.up, tc.fl), "bad.hcl"); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v (want %q)", name, err, tc.want)
		}
	}
}
