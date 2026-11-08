package config

import (
	"fmt"
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
		out = append(out, Accept{Claim: CanonicalClaim(a.Claim), Values: a.Values})
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
		one = oidc.ASCIILower(strings.TrimSuffix(*domain, "."))
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
		n := oidc.ASCIILower(strings.TrimSuffix(d, "."))
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
		if CanonicalClaim(a.Claim) == InstitutionClaim {
			out = append(out, a.Values...)
		}
	}
	return out
}

// DomainsConfigured reports whether any identity source (an sso provider, a jwt
// issuer, the scim client) names the institutions' domains: names are then
// local@domain, and a bare name is no one's.
func (c *Config) DomainsConfigured() bool {
	for _, p := range c.SSO.providers() {
		if len(p.NameScopes()) > 0 {
			return true
		}
	}
	for _, j := range c.JWTIssuers() {
		if len(j.NameScopes()) > 0 {
			return true
		}
	}
	return c.SCIM != nil && len(c.SCIM.NameScopes()) > 0
}

func (s *SSO) providers() []SSOProvider {
	if s == nil {
		return nil
	}
	return s.ProviderList()
}

// OrgNameScopes are the domains org OWNS, the only ones whose people its
// administrators may create (POST /admin/users): a name in another
// institution's domain would squat a person that institution's provisioning or
// login is to own (audit 2026-11-07, then 2026-11-08: a shared SCIM client or
// sso provider gave EVERY org it served every domain it served). Ownership is
// explicit:
//
//   - org { domains = [...] }, in the configuration;
//   - the domains of an sso provider or jwt issuer bound to org ALONE;
//   - those of the scim client when every group it maps lands on org's teams.
//
// A source shared by several organisations owns nothing, nor does the legacy
// unrestricted sso block (it is bound to nobody): their domains are claimed
// with org { domains }, and a domain claimed by two organisations is refused at
// load (see orgDomains). Names compare as the scopes do: ASCII lower case, no
// trailing dot.
func (c *Config) OrgNameScopes(org string) []string {
	var out []string
	add := func(ss []string) {
		for _, s := range ss {
			s = oidc.ASCIILower(strings.TrimSuffix(s, "."))
			if s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	for i := range c.Orgs {
		if c.Orgs[i].Name == org {
			add(c.Orgs[i].Domains)
		}
	}
	for _, p := range c.SSO.providers() {
		if p.Restricted && len(p.Orgs) == 1 && p.Orgs[0] == org {
			add(p.NameScopes())
		}
	}
	for _, j := range c.JWTIssuers() {
		if len(j.Orgs) == 1 && j.Orgs[0] == org {
			add(j.NameScopes())
		}
	}
	if c.SCIM != nil && len(c.SCIM.Groups) > 0 {
		only, any := true, false
		for _, g := range c.SCIM.Groups {
			switch o := c.OrgOfTeam(g.Team); o {
			case "": // a team no organisation declares (created later through the API): nobody's
			case org:
				any = true
			default:
				only = false
			}
		}
		if only && any {
			add(c.SCIM.NameScopes())
		}
	}
	return out
}

// orgDomains refuses a domain that two organisations own, explicitly or through
// a source bound to them alone: one institution's people belong to one
// organisation's administrators.
func (v *validator) orgDomains(raw []orgHCL, c *Config) {
	owner := map[string]string{}
	for i, o := range c.Orgs {
		for _, d := range c.OrgNameScopes(o.Name) {
			if other, dup := owner[d]; dup && other != o.Name {
				v.errf(raw[i].DefRange, "Domain owned twice", "domain %q is owned by organisations %q and %q: an institution's people have one organisation's administrators (give it to one with org { domains = [...] }, and bind the shared sources accordingly)", d, other, o.Name)
				continue
			}
			owner[d] = o.Name
		}
	}
}

// NameScopes of the provider: see scopes.
func (p *SSOProvider) NameScopes() []string { return scopes(p.Domain, p.Domains, p.Accept) }

// NameScopes of the issuer: see scopes.
func (j *JWT) NameScopes() []string { return scopes(j.Domain, j.Domains, j.Accept) }

// NameScopes of the provisioning client: see scopes.
func (s *SCIM) NameScopes() []string { return scopes(s.Domain, s.Domains, nil) }

// LegacyDomain is the one domain that scopes the unscoped names of the
// accounts that predate scoped names, from the sso, jwt and scim blocks ("" when
// none sets one). More than one distinct value is an error: unscoped names belong
// to one institution.
func (c *Config) LegacyDomain() (string, error) {
	var ds []string
	add := func(d string) {
		if d != "" && !slices.Contains(ds, d) {
			ds = append(ds, d)
		}
	}
	if c.SSO != nil {
		add(c.SSO.Domain)
		for _, p := range c.SSO.Providers {
			add(p.Domain)
		}
	}
	if c.JWT != nil {
		add(c.JWT.Domain)
	}
	for _, j := range c.JWTs {
		add(j.Domain)
	}
	if c.SCIM != nil {
		add(c.SCIM.Domain)
	}
	switch len(ds) {
	case 0:
		return "", nil
	case 1:
		return ds[0], nil
	}
	return "", fmt.Errorf("config: several legacy domains %v: the unscoped names of the old accounts belong to one institution", ds)
}

// InstitutionClaim is the ONE spelling of the institution claim (SCHAC
// schacHomeOrganization, AARC-G056 schac_home_organization). The configuration
// is normalised to it where an accept, a jwt map or an sso role names it
// (CanonicalClaim), so that no later check can read one spelling and another
// map on the other (audit 2026-11-08: a tie on schacHomeOrganization and a map
// on schac_home_organization were two different claims).
const InstitutionClaim = "schac_home_organization"

// CanonicalClaim returns the canonical spelling of an alias of the institution
// claim, and every other claim name unchanged.
func CanonicalClaim(c string) string {
	if strings.EqualFold(c, "schacHomeOrganization") || strings.EqualFold(c, InstitutionClaim) {
		return InstitutionClaim
	}
	return c
}
