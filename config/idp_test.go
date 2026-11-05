package config

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestFederationExample(t *testing.T) {
	c, err := Load(filepath.Join("..", "examples", "federation.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	ps := c.SSO.ProviderList()
	if len(ps) != 3 || ps[0].Name != "univ-a" || ps[2].Name != "partners" || !ps[2].Restricted {
		t.Fatalf("providers: %+v", ps)
	}
	if ps[0].RedirectURL != "https://llm.example.org/admin/callback/univ-a" {
		t.Fatalf("redirect: %s", ps[0].RedirectURL)
	}
	if len(ps[2].Accept) != 1 || ps[2].Accept[0].Claim != "schac_home_organization" {
		t.Fatalf("accept: %+v", ps[2].Accept)
	}
	if js := c.JWTIssuers(); len(js) != 2 || js[1].Name != "partners" || len(js[1].Accept) != 1 {
		t.Fatalf("jwts: %+v", js)
	}
	for _, r := range []string{"vault:secret/llmflow#sso-univ-a", "vault:secret/llmflow#sso-partners"} {
		if !slices.Contains(c.SecretRefs(), r) {
			t.Errorf("secret %s is not listed", r)
		}
	}
}

func TestMultiIdPConfigErrors(t *testing.T) {
	prov := func(name, issuer, body string) string {
		return `provider "` + name + `" {
  issuer    = "` + issuer + `"
  client_id = "c"
  ` + body + `
}
`
	}
	sso := func(parts ...string) string {
		return valid + "sso {\n  redirect_url = \"https://llm.example.org/admin/callback\"\n" + strings.Join(parts, "") + "}\n"
	}
	jwtN := func(name, issuer, extra string) string {
		return "jwt \"" + name + "\" {\n  issuer = \"" + issuer + "\"\n  audience = \"a\"\n  " + extra + "\n  map {\n    value = \"x\"\n    team = \"etabA\"\n  }\n}\n"
	}
	cases := []struct{ name, src, want string }{
		{"duplicate issuer", sso(prov("a", "https://i.example", `orgs = ["etabA"]`), prov("b", "https://i.example/", `orgs = ["etabB"]`)), "Duplicate issuer"},
		{"duplicate name", sso(prov("a", "https://i.example", `orgs = ["etabA"]`), prov("a", "https://j.example", `orgs = ["etabA"]`)), "Duplicate provider"},
		{"unknown org", sso(prov("a", "https://i.example", `orgs = ["nowhere"]`)), "Unknown organisation"},
		{"empty orgs", sso(prov("a", "https://i.example", `orgs = []`)), "Empty orgs"},
		{"missing orgs", sso(prov("a", "https://i.example", ``)), "orgs"},
		{"empty accept list", sso(prov("a", "https://i.example", "orgs = [\"etabA\"]\n  accept {\n    claim = \"c\"\n    values = []\n  }")), "Empty accept list"},
		{"empty accept value", sso(prov("a", "https://i.example", "orgs = [\"etabA\"]\n  accept {\n    claim = \"c\"\n    values = [\"\"]\n  }")), "Empty accept value"},
		{"role on another organisation", sso(prov("a", "https://i.example", "orgs = [\"etabA\"]\n  role {\n    value = \"x\"\n    role = \"org_admin\"\n    org = \"etabB\"\n  }")), "Role outside the provider's organisations"},
		{"role on a team of another organisation", sso(prov("a", "https://i.example", "orgs = [\"etabA\"]\n  role {\n    value = \"x\"\n    role = \"member\"\n    team = \"etabB\"\n  }")), "Role outside the provider's organisations"},
		{"proxy-wide role", sso(prov("a", "https://i.example", "orgs = [\"etabA\"]\n  role {\n    value = \"x\"\n    role = \"proxy_admin\"\n  }")), "Proxy-wide role not allowed"},
		{"single issuer next to providers", sso("issuer = \"https://z.example\"\n", prov("a", "https://i.example", `orgs = ["etabA"]`)), "Provider blocks and a single issuer"},
		{"jwt duplicate issuer", valid + jwtN("a", "https://i.example", `orgs = ["etabA"]`) + jwtN("b", "https://i.example", `orgs = ["etabA"]`), "Duplicate issuer"},
		{"jwt map to another organisation", valid + jwtN("a", "https://i.example", `orgs = ["etabB"]`), "Map outside the issuer's organisations"},
		{"jwt without orgs", valid + jwtN("a", "https://i.example", ``), "Missing orgs"},
		{"jwt empty accept", valid + jwtN("a", "https://i.example", "orgs = [\"etabA\"]\n  accept {\n    claim = \"c\"\n    values = []\n  }"), "Empty accept list"},
		{"unnamed jwt among several", valid + jwtBlock("https://u.example", "a") + jwtN("b", "https://i.example", `orgs = ["etabA"]`), "Unnamed jwt block among several"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(tc.src)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "test.hcl:") {
				t.Fatalf("error %q: want %q with file:line", err, tc.want)
			}
		})
	}
	// The single-issuer forms stay valid.
	c, err := parse(valid + jwtBlock("https://u.example", "a") + ssoBlock("https://i.example", "https://llm.example.org/admin/callback", ""))
	if err != nil {
		t.Fatal(err)
	}
	if c.JWT == nil || len(c.JWTs) != 0 || c.JWT.Name != "" || len(c.SSO.ProviderList()) != 1 || c.SSO.ProviderList()[0].Restricted {
		t.Fatalf("legacy forms: %+v %+v", c.JWT, c.SSO)
	}
}

func TestDomainsConfig(t *testing.T) {
	prov := func(name, issuer, body string) string {
		return `provider "` + name + `" {
  issuer    = "` + issuer + `"
  client_id = "c"
  orgs      = ["etabA"]
  ` + body + `
}
`
	}
	sso := func(parts ...string) string {
		return valid + "sso {\n  redirect_url = \"https://llm.example.org/admin/callback\"\n" + strings.Join(parts, "") + "}\n"
	}
	// Valid: one legacy domain, others by domains; the federation's accepted
	// institutions are scopes too.
	c, err := parse(sso(
		prov("a", "https://a.example", `domain = "Univ-A.fr."`),
		prov("b", "https://b.example", `domains = ["univ-b.fr", "univ-c.fr"]`),
		prov("fed", "https://f.example", "accept {\n    claim = \"schac_home_organization\"\n    values = [\"Univ-D.fr\"]\n  }")))
	if err != nil {
		t.Fatal(err)
	}
	ps := c.SSO.ProviderList()
	if ps[0].Domain != "univ-a.fr" || !slices.Equal(ps[0].NameScopes(), []string{"univ-a.fr"}) {
		t.Errorf("a: %q %v", ps[0].Domain, ps[0].NameScopes())
	}
	if ps[1].Domain != "" || !slices.Equal(ps[1].NameScopes(), []string{"univ-b.fr", "univ-c.fr"}) {
		t.Errorf("b: %q %v", ps[1].Domain, ps[1].NameScopes())
	}
	if !slices.Equal(ps[2].NameScopes(), []string{"Univ-D.fr"}) { // compared without case by ScopedName
		t.Errorf("fed: %v", ps[2].NameScopes())
	}
	for _, tc := range []struct{ name, src, want string }{
		{"two legacy domains", sso(prov("a", "https://a.example", `domain = "univ-a.fr"`), prov("b", "https://b.example", `domain = "univ-b.fr"`)), "Second legacy domain"},
		{"invalid domain", sso(prov("a", "https://a.example", `domain = "univ a.fr"`)), "Invalid domain"},
		{"invalid in domains", sso(prov("a", "https://a.example", `domains = ["x@y"]`)), "Invalid domain"},
		{"empty domain", sso(prov("a", "https://a.example", `domain = ""`)), "Invalid domain"},
	} {
		if _, err := parse(tc.src); err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "test.hcl:") {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// The same issuer in sso and jwt may carry the same domain.
	if _, err := parse(valid + jwtBlock("https://u.example", "a")[:len(jwtBlock("https://u.example", "a"))-2] + "  domain = \"univ-a.fr\"\n}\n" + ssoBlock("https://u.example", "https://llm.example.org/admin/callback", "domain = \"univ-a.fr\"")); err != nil {
		t.Errorf("one issuer, one legacy domain: %v", err)
	}
}
