package config

import (
	"strings"
	"testing"
)

const fedBase = `
master_key_file = "/unused"
cache_salt_key_file = "/unused"
org "owner" {
  share = 1
  team "owner" {}
}
org "ilaas" {
  share = 0
  team "ilaas" {}
}
pool "b300" {
  capacity = 8
  borrow   = "spare"
}
model "llama" {
  pool     = "b300"
  key_file = "/unused"
  upstream "a" { base_url = "http://127.0.0.1:1" }
}
`

func fedConfig(block string) (*Config, error) {
	return Parse([]byte(fedBase+block), "fed.hcl")
}

func TestFederationBlock(t *testing.T) {
	c, err := fedConfig(`federation "ilaas" {
  prefix = "/fed/ilaas"
  org = "ilaas"
  metrics_networks = ["10.0.0.0/8", "2001:db8::/32"]
}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Federations) != 1 {
		t.Fatalf("federations: %+v", c.Federations)
	}
	f := c.Federations[0]
	if f.Name != "ilaas" || f.Prefix != "/fed/ilaas" || f.Org != "ilaas" || len(f.MetricsNetworks) != 2 {
		t.Fatalf("%+v", f)
	}
	if c2, err := fedConfig(""); err != nil || len(c2.Federations) != 0 {
		t.Fatalf("no block: %v %+v", err, c2)
	}
}

// Each refusal names its cause; every case is the only fault of its block.
func TestFederationRefusals(t *testing.T) {
	blk := func(prefix, org, nets string) string {
		return `federation "ilaas" {
  prefix = "` + prefix + `"
  org = "` + org + `"
  metrics_networks = ` + nets + `
}`
	}
	ok := `["10.0.0.0/8"]`
	cases := []struct{ name, block, want string }{
		{"no slash", blk("fed", "ilaas", ok), "segments"},
		{"root", blk("/", "ilaas", ok), "segments"},
		{"trailing slash", blk("/fed/", "ilaas", ok), "segments"},
		{"dotdot", blk("/fed/../admin", "ilaas", ok), "segments"},
		{"dot first", blk("/.hidden", "ilaas", ok), "segments"},
		{"empty segment", blk("/fed//x", "ilaas", ok), "segments"},
		{"four segments", blk("/a/b/c/d", "ilaas", ok), "segments"},
		{"space", blk("/fed x", "ilaas", ok), "segments"},
		{"reserved admin", blk("/admin", "ilaas", ok), "served by the gateway itself"},
		{"reserved v1", blk("/v1/x", "ilaas", ok), "served by the gateway itself"},
		{"reserved metrics", blk("/metrics", "ilaas", ok), "served by the gateway itself"},
		{"unknown org", blk("/fed/ilaas", "nobody", ok), "nobody"},
		{"org with a share", blk("/fed/ilaas", "owner", ok), "holds a share"},
		{"no networks", blk("/fed/ilaas", "ilaas", `[]`), "metrics_networks"},
		{"any address", blk("/fed/ilaas", "ilaas", `["0.0.0.0/0"]`), "every address"},
		{"bad network", blk("/fed/ilaas", "ilaas", `["10.0.0.0/33"]`), "CIDR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fedConfig(tc.block)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error with %q, got %v", tc.want, err)
			}
		})
	}
	t.Run("overlap", func(t *testing.T) {
		_, err := fedConfig(blk("/fed/ilaas", "ilaas", ok) + "\n" + strings.Replace(blk("/fed/ilaas/sub", "ilaas", ok), `"ilaas" {`, `"two" {`, 1))
		if err == nil || !strings.Contains(err.Error(), "overlaps") {
			t.Fatalf("overlap: %v", err)
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		_, err := fedConfig(blk("/fed/a", "ilaas", ok) + "\n" + blk("/fed/b", "ilaas", ok))
		if err == nil || !strings.Contains(err.Error(), "declared once") {
			t.Fatalf("duplicate: %v", err)
		}
	})
}

// FuzzFederationPrefix: a prefix the validator accepts is a clean path of
// plain segments (what the router relies on), whatever the input.
func FuzzFederationPrefix(f *testing.F) {
	for _, s := range []string{"/fed/ilaas", "/", "", "/..", "/a/../b", "/a//b", "/%2e%2e", "/admin", "/ü", "/a b", "/a\x00", "/a/b/c/d"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		if FederationPrefixProblem(p) != "" {
			return
		}
		if !strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "//") ||
			strings.ContainsAny(p, "%?# \x00\\") || len(p) > 4*33 {
			t.Fatalf("accepted %q", p)
		}
		for _, seg := range strings.Split(p[1:], "/") {
			if seg == "." || seg == ".." || seg == "" {
				t.Fatalf("accepted %q with segment %q", p, seg)
			}
		}
	})
}
