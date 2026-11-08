package config

import (
	"slices"
	"strings"
	"testing"
)

// Audit 2026-11-08: an org owns a domain only explicitly (org { domains }), or
// through a source bound to it alone; a shared source owns nothing; a domain
// owned by two organisations is refused at load.
func TestOrgDomainOwnership(t *testing.T) {
	const base = `
master_key_file = "/unused"
cache_salt_key_file = "/unused"
%s
org "A" {
  share = 0.5
  %s
  team "ta" {}
}
org "B" {
  share = 0.5
  %s
  team "tb" {}
}
pool "p" { capacity = 4 }
model "m" {
  pool     = "p"
  key_file = "/unused"
  upstream "u" { base_url = "http://127.0.0.1:1" }
}
`
	scim := func(groups string) string {
		return `scim {
  token_file = "/t"
  base_url   = "https://gw.example/scim/v2"
  domains    = ["univ-a.fr", "univ-b.fr"]
` + groups + `}`
	}
	both := `  group "ga" { team = "ta" }
  group "gb" { team = "tb" }
`
	onlyA := `  group "ga" { team = "ta" }
`
	parseF := func(scimBlock, a, b string) (*Config, error) {
		return Parse([]byte(strings.Replace(strings.Replace(strings.Replace(base, "%s", scimBlock, 1), "%s", a, 1), "%s", b, 1)), "o.hcl")
	}
	t.Run("a shared scim client owns nothing", func(t *testing.T) {
		c, err := parseF(scim(both), "", "")
		if err != nil {
			t.Fatal(err)
		}
		if got := append(c.OrgNameScopes("A"), c.OrgNameScopes("B")...); len(got) != 0 {
			t.Errorf("a shared client gave %v", got)
		}
	})
	t.Run("explicit domains are owned", func(t *testing.T) {
		c, err := parseF(scim(both), `domains = ["Univ-A.fr"]`, `domains = ["univ-b.fr"]`)
		if err != nil {
			t.Fatal(err)
		}
		if got := c.OrgNameScopes("A"); !slices.Equal(got, []string{"univ-a.fr"}) {
			t.Errorf("A owns %v", got)
		}
		if got := c.OrgNameScopes("B"); !slices.Equal(got, []string{"univ-b.fr"}) {
			t.Errorf("B owns %v", got)
		}
	})
	t.Run("a scim client that serves one org is that org's", func(t *testing.T) {
		c, err := parseF(scim(onlyA), "", "")
		if err != nil {
			t.Fatal(err)
		}
		if got := c.OrgNameScopes("A"); !slices.Equal(got, []string{"univ-a.fr", "univ-b.fr"}) || len(c.OrgNameScopes("B")) != 0 {
			t.Errorf("A owns %v, B owns %v", got, c.OrgNameScopes("B"))
		}
	})
	t.Run("a domain owned twice is refused", func(t *testing.T) {
		_, err := parseF(scim(both), `domains = ["univ-a.fr"]`, `domains = ["UNIV-A.fr"]`)
		if err == nil || !strings.Contains(err.Error(), "owned by organisations") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a domain that is not a DNS name is refused", func(t *testing.T) {
		if _, err := parseF("", `domains = ["a@b"]`, ""); err == nil || !strings.Contains(err.Error(), "not a DNS name") {
			t.Fatalf("err = %v", err)
		}
	})
}
