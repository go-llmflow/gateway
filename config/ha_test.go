package config

import (
	"strings"
	"testing"
)

const pgStore = `
store {
  kind      = "postgres"
  dsn_file  = "/run/dsn"
  spool_dir = "/var/spool/llmflow"
}
`

const haBlock = `
ha {
  listen        = "10.0.0.5:9444"
  peer_networks = ["10.0.0.0/24"]
  secret_file   = "/run/ha"
}
`

func TestParseHA(t *testing.T) {
	c, err := parse(valid + pgStore + haBlock)
	if err != nil {
		t.Fatal(err)
	}
	h := c.HA
	if h == nil || h.Advertise != "10.0.0.5:9444" || h.LeaseTTL != DefaultLeaseTTL || h.Heartbeat != DefaultHeartbeat ||
		h.ForwardTimeout != DefaultForwardTimeout || h.UsagePersist != DefaultUsagePersist || h.OnOwnerUnreachable != "lowest_priority" ||
		len(h.PeerNetworks) != 1 {
		t.Fatalf("%+v", h)
	}
	found := false
	for _, r := range c.SecretRefs() {
		found = found || r == "/run/ha"
	}
	if !found {
		t.Fatal("the ha secret is not among the secret references check-config reads")
	}
	// No ha block: nothing changes.
	if c, err := parse(valid); err != nil || c.HA != nil {
		t.Fatalf("%v %+v", err, c.HA)
	}
}

func TestParseHAErrors(t *testing.T) {
	with := func(attrs string) string {
		return valid + pgStore + "ha {\n  secret_file = \"/run/ha\"\n" + attrs + "\n}\n"
	}
	ok := "  listen = \"10.0.0.5:9444\"\n  peer_networks = [\"10.0.0.0/24\"]\n"
	for _, c := range []struct{ name, src, want string }{
		{"memory store", valid + haBlock, "HA without PostgreSQL"},
		{"advertise outside", with(ok + "  advertise = \"192.168.1.1:9444\""), "Advertise address outside peer_networks"},
		{"advertise a name", with(ok + "  advertise = \"gw1:9444\""), "Invalid advertise address"},
		{"unspecified", with("  listen = \"0.0.0.0:9444\"\n  peer_networks = [\"10.0.0.0/24\"]"), "Invalid advertise address"},
		{"no peers", with("  listen = \"10.0.0.5:9444\"\n  peer_networks = []"), "Missing peer_networks"},
		{"bad cidr", with("  listen = \"10.0.0.5:9444\"\n  peer_networks = [\"10.0.0.0/33\"]"), "Invalid prefix"},
		{"heartbeat", with(ok + "  lease_ttl = \"3s\"\n  heartbeat = \"2s\""), "Invalid heartbeat"},
		{"forward", with(ok + "  forward_timeout = \"2s\""), "Invalid forward_timeout"},
		{"ttl", with(ok + "  lease_ttl = \"500ms\"\n  heartbeat = \"100ms\"\n  forward_timeout = \"50ms\""), "Invalid lease_ttl"},
		{"persist", with(ok + "  usage_persist = \"100ms\""), "Invalid usage_persist"},
		{"unreachable", with(ok + "  on_owner_unreachable = \"guess\""), "Invalid on_owner_unreachable"},
		{"replica id", with(ok + "  replica_id = \"a b\""), "Invalid replica_id"},
		{"tls incomplete", with(ok + "  tls_cert_file = \"/run/c\"\n  tls_key_file = \"/run/k\""), "Incomplete ha TLS"},
		{"tls name", with(ok + "  tls_server_name = \"bad name\""), "Invalid tls_server_name"},
		{"same listener", valid + pgStore + "listen = \"10.0.0.5:9444\"\n" + haBlock, "Internal listener on the client one"},
	} {
		_, err := parse(c.src)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	// Mutual TLS: the three files are secret references check-config reads.
	c, err := parse(with(ok + "  tls_cert_file = \"/run/c\"\n  tls_key_file = \"/run/k\"\n  tls_ca_file = \"/run/ca\""))
	if err != nil || c.HA.TLSServerName != DefaultHATLSServerName {
		t.Fatalf("%v %+v", err, c)
	}
	refs := map[string]bool{}
	for _, r := range c.SecretRefs() {
		refs[r] = true
	}
	if !refs["/run/c"] || !refs["/run/k"] || !refs["/run/ca"] {
		t.Fatalf("tls files not among the secret references: %v", refs)
	}
	// The short durations of a test cluster are accepted.
	if _, err := parse(with(ok + "  lease_ttl = \"1500ms\"\n  heartbeat = \"200ms\"\n  forward_timeout = \"200ms\"\n  usage_persist = \"300ms\"")); err != nil {
		t.Fatal(err)
	}
}
