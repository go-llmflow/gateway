package gateway

import (
	"context"
	"errors"
	"strings"

	"github.com/go-authn/oidc"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// Scoped names. An account is relative to an institution's domain: there is no
// global "alice". A person is named local@domain, the domain checked against
// what the issuer that vouched for them may assert (its domain and domains, or
// the institutions of the federation it serves: go-authn/oidc ScopedName, as a
// Shibboleth service checks a scope). An unscoped name is scoped to the
// issuer's domain when it has one (an institution whose accounts predate
// scoped names), and is no name at all otherwise.

var errPersonStore = errors.New("user store unreachable")

// scopedPerson returns the person's name, or an oidc error (ErrNoScope,
// ErrScopeNotAllowed, ErrBadName) when the token has no name this issuer may
// assert. When the issuer has a domain and the person's scope is that domain, a
// stored user with the unscoped name (created before scoped names) is the same
// person and keeps their name until renamed; a user already stored under the
// scoped name wins.
func (s *Server) scopedPerson(ctx context.Context, tok *oidc.Token, scopes []string, domain string) (string, error) {
	name, err := tok.ScopedUsername(scopes, domain)
	if err != nil {
		return "", err
	}
	if !config.ValidUserName(name) {
		return "", oidc.ErrBadName
	}
	return s.legacyAlias(ctx, name, domain)
}

var errInstitutionNotTied = errors.New("the name's scope is not the institution the token states")

// tiedToInstitution holds a person to the institution their token states. An
// issuer that serves a federation filters on the institution claim (accept on
// schac_home_organization) and takes the name scopes from the same values, but
// those were two independent checks: a token of univ-d.fr's IdP stating
// schac_home_organization = univ-c.fr passed the filter, kept its own scope, and
// the role maps on that claim gave it univ-c.fr's team (audit 2026-11-07). With
// such an accept, the scope of the person's name (as ScopedName returns it,
// before any legacy alias) must EQUAL a value of the claim, and that value must
// be one the accept block accepts; otherwise the person is nobody and is given
// no team and no role. Without an institution accept there is nothing to tie.
func tiedToInstitution(tok *oidc.Token, accept []config.Accept, scopes []string, domain string) error {
	var claims []config.Accept
	for _, a := range accept {
		if strings.EqualFold(a.Claim, "schac_home_organization") || strings.EqualFold(a.Claim, "schacHomeOrganization") {
			claims = append(claims, a)
		}
	}
	if len(claims) == 0 {
		return nil
	}
	name, err := tok.ScopedUsername(scopes, domain)
	if err != nil {
		return err
	}
	_, scope, _ := strings.Cut(name, "@")
	for _, a := range claims {
		stated := oidc.ClaimValues(tok, a.Claim)
		if !containsInstitution(stated, scope) || !containsInstitution(a.Values, scope) {
			return errInstitutionNotTied
		}
	}
	return nil
}

func containsInstitution(values []string, scope string) bool {
	for _, v := range values {
		if oidc.EqualASCIIFold(oidc.Normalize("schac_home_organization", v), scope) {
			return true
		}
	}
	return false
}

// legacyAlias is name, or, when the issuer has a domain and name is local@domain,
// the stored user with the unscoped name: the same person, created before scoped
// names, who keeps their name until renamed. A user stored under the scoped
// name wins.
func (s *Server) legacyAlias(ctx context.Context, name, domain string) (string, error) {
	if !s.cfg.LegacyNames {
		return name, nil // migrated: the user was renamed (migrate-users)
	}
	if domain == "" || !strings.HasSuffix(name, "@"+domain) {
		return name, nil
	}
	legacy := strings.TrimSuffix(name, "@"+domain)
	if !config.ValidUserName(legacy) {
		return name, nil
	}
	switch _, err := s.st.GetUser(ctx, name); {
	case err == nil:
		return name, nil
	case !errors.Is(err, store.ErrNotFound):
		return "", errPersonStore
	}
	switch _, err := s.st.GetUser(ctx, legacy); {
	case err == nil:
		return legacy, nil
	case !errors.Is(err, store.ErrNotFound):
		return "", errPersonStore
	}
	return name, nil
}
