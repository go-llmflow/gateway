package config

import (
	"slices"
	"strings"
	"time"

	"github.com/go-authn/oidc"
	"github.com/hashicorp/hcl/v2"
)

// Several identity providers (v0.27.0).
//
// OpenID Connect Core 1.0 §2: sub is "locally unique and never reassigned
// within the Issuer", so an identity is (issuer, sub), never a name alone.
// RFC 9700 §4.4.2: a client of several authorization servers keeps the issuer
// each request went to and aborts on a mismatch.

// SSOProvider is one OpenID provider the admin login accepts.
type SSOProvider struct {
	// Name is the provider's label in the configuration ("" for the single
	// sso block of earlier versions). It names the callback
	// /admin/callback/<name> and is the provider half of an identity.
	Name string
	// Issuer, ClientID, ClientSecretFile, RedirectURL, Scopes, MinRefresh as
	// in SSO.
	Issuer, ClientID, ClientSecretFile, RedirectURL string
	Scopes                                          []string
	MinRefresh                                      time.Duration
	// Restricted is false only for the single sso block of earlier versions:
	// it may grant any role. A provider block is restricted to Orgs.
	Restricted bool
	// Orgs are the organisations this provider may vouch for: a role it would
	// grant on any other organisation is refused.
	Orgs []string
	// AllowProxyRoles lets this provider grant proxy-wide roles
	// (proxy_admin, proxy_viewer, finance_viewer).
	AllowProxyRoles bool
	// Adopt lets this provider claim a stored user that has no role at all
	// and no link yet. Without it such a user is refused: nothing says which
	// institution they belong to.
	Adopt bool
	// Accept are conditions on the ID token's claims, all of which must hold.
	Accept []Accept
	// Roles map claims to role bindings.
	Roles []SSORole
	// Domain and Domains are the institutions' domains this provider may
	// assert names for (a name is local@domain). Domain also scopes the
	// unscoped names of institutions whose accounts predate scoped names:
	// "alice" is "alice@Domain", and a stored user named "alice" is that person.
	Domain  string
	Domains []string
}

// ProviderList returns the providers of an sso block: its provider blocks, or
// the single issuer of earlier versions as one unrestricted provider.
func (s *SSO) ProviderList() []SSOProvider {
	if len(s.Providers) > 0 {
		return s.Providers
	}
	return []SSOProvider{{Issuer: s.Issuer, ClientID: s.ClientID, ClientSecretFile: s.ClientSecretFile,
		RedirectURL: s.RedirectURL, Scopes: s.Scopes, MinRefresh: s.MinRefresh, Roles: s.Roles, Domain: s.Domain, Domains: s.Domains}}
}

// JWTIssuers returns every issuer of inference access tokens: the jwt blocks,
// or the single one of earlier versions.
func (c *Config) JWTIssuers() []*JWT {
	if len(c.JWTs) > 0 {
		return c.JWTs
	}
	if c.JWT != nil {
		return []*JWT{c.JWT}
	}
	return nil
}

// OrgOfTeam returns the organisation declaring team, or "".
func (c *Config) OrgOfTeam(team string) string {
	for i := range c.Orgs {
		for _, t := range c.Orgs[i].Teams {
			if t == team {
				return c.Orgs[i].Name
			}
		}
	}
	return ""
}

// MayGrant reports whether a provider may grant r: a restricted provider only
// on its own organisations (a team counts for the organisation declaring it),
// and proxy-wide roles only with AllowProxyRoles.
func (p *SSOProvider) MayGrant(c *Config, r SSORole) bool {
	if !p.Restricted {
		return true
	}
	switch {
	case r.Org != "":
		return slices.Contains(p.Orgs, r.Org)
	case r.Team != "":
		o := c.OrgOfTeam(r.Team)
		return o != "" && slices.Contains(p.Orgs, o)
	}
	return p.AllowProxyRoles
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// accepts validates accept blocks: a claim, and at least one value, none
// empty (an empty value would match a claim that is present but blank).
func (v *validator) accepts(in []acceptHCL, where string) []Accept {
	var out []Accept
	for _, a := range in {
		if a.Claim == "" {
			v.errf(a.ClaimRange, "Empty accept claim", "%s: accept needs the name of a claim", where)
		}
		if len(a.Values) == 0 {
			v.errf(a.ValuesRange, "Empty accept list", "%s: accept { claim = %q } with no values would refuse everybody", where, a.Claim)
		}
		for _, x := range a.Values {
			if strings.TrimSpace(x) == "" {
				v.errf(a.ValuesRange, "Empty accept value", "%s: accept %q has an empty value", where, a.Claim)
			}
		}
		out = append(out, Accept{Claim: a.Claim, Values: a.Values})
	}
	return out
}

// Accept is "claim = ..., values = [...]" (go-authn/oidc): the login (or token)
// is refused unless the claim holds one of Values; several must all hold. The
// case rules (SCHAC, eduPerson) and the matching are the library's.
type Accept = oidc.Accept

// domains validates the domain and domains attributes of an issuer: DNS names
// (RFC 1035 letters, digits, dots and hyphens), lower-cased. The one domain
// that scopes legacy unscoped names (domain) is declared by one issuer only: two
// issuers doing so would make "alice" two people. domains lists the institutions a
// federation issuer may assert names for; domain is included.
func (v *validator) domains(domain *string, domains []string, r, rs hcl.Range, where, issuer string, owner map[string]string) (string, []string) {
	ok := func(d string) bool {
		if d == "" || len(d) > 253 {
			return false
		}
		for _, c := range d {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '-') {
				return false
			}
		}
		return !strings.HasPrefix(d, ".") && !strings.Contains(d, "..")
	}
	one := ""
	var out []string
	if domain != nil {
		one = strings.ToLower(strings.TrimSuffix(*domain, "."))
		if !ok(one) {
			v.errf(r, "Invalid domain", "%s: domain %q: want a DNS name such as polytechnique.edu", where, *domain)
			one = ""
		} else {
			// The unscoped names of the old single-issuer setups belong to ONE
			// institution: a second issuer with a domain would claim the same
			// "alice". Other issuers list their institutions in domains.
			iss := strings.TrimRight(issuer, "/")
			if other, dup := owner[""]; dup && other != iss {
				v.errf(r, "Second legacy domain", "%s: domain %q would scope unscoped names for a second issuer (%q already does): unscoped names belong to one institution; list this one's domains in domains = [...] instead", where, one, other)
			}
			owner[""] = iss
			out = append(out, one)
		}
	}
	for _, d := range domains {
		n := strings.ToLower(strings.TrimSuffix(d, "."))
		if !ok(n) {
			v.errf(rs, "Invalid domain", "%s: domains entry %q: want a DNS name", where, d)
			continue
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return one, out
}

// scopes are the domains the issuer may assert names for: domain, domains, and
// the institutions of a federation it serves (the values of its accept blocks
// on schac_home_organization).
func scopes(domain string, domains []string, accept []Accept) []string {
	var out []string
	out = append(out, domains...)
	if domain != "" && !slices.Contains(out, domain) {
		out = append(out, domain)
	}
	for _, a := range accept {
		if strings.EqualFold(a.Claim, "schac_home_organization") || strings.EqualFold(a.Claim, "schacHomeOrganization") {
			out = append(out, a.Values...)
		}
	}
	return out
}

// NameScopes of the provider: see scopes.
func (p *SSOProvider) NameScopes() []string { return scopes(p.Domain, p.Domains, p.Accept) }

// NameScopes of the issuer: see scopes.
func (j *JWT) NameScopes() []string { return scopes(j.Domain, j.Domains, j.Accept) }

// NameScopes of the provisioning client: see scopes.
func (s *SCIM) NameScopes() []string { return scopes(s.Domain, s.Domains, nil) }
