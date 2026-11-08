package gateway

import (
	"context"
	"errors"
	"slices"
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

// A person has ONE institution: the scope of their name, which the issuer is
// allowed to assert (go-authn/oidc ScopedName against the issuer's scopes). It
// is the only value any mapping keys on. The institution CLAIM of a token
// (schac_home_organization, in either spelling, possibly several values) is
// never matched by a map, a role or an accept: it is only cross-checked against
// the name, because a token that states a different institution than its name
// carries is not consistent (audit 2026-11-08: a map matched ANY stated value,
// the tie and the map read two spellings, and an issuer with domains and no
// accept had no tie at all; the 2026-11-07 fix tied the name to the claim
// where the mappings still read the claim).

// institutionOf is the person's institution, lower-cased, or an error: the
// token has no scoped name this issuer may assert, or it states institutions
// among which its name's scope is not.
func institutionOf(tok *oidc.Token, scopes []string, domain string) (string, error) {
	name, err := tok.ScopedUsername(scopes, domain)
	if err != nil {
		return "", err
	}
	_, inst, _ := strings.Cut(name, "@")
	if stated := statedInstitutions(tok); len(stated) > 0 && !slices.ContainsFunc(stated, func(v string) bool { return oidc.EqualASCIIFold(v, inst) }) {
		return "", errInstitutionNotTied
	}
	return inst, nil
}

// statedInstitutions are the values the token gives for the institution claim,
// under either spelling, normalised as the claim folds (ASCII lower, no
// trailing dot).
func statedInstitutions(tok *oidc.Token) []string {
	var out []string
	for _, c := range []string{config.InstitutionClaim, "schacHomeOrganization"} {
		for _, v := range oidc.ClaimValues(tok, c) {
			out = append(out, oidc.Normalize(config.InstitutionClaim, v))
		}
	}
	return out
}

// claimMatches is whether a mapping on claim = value holds for this token. The
// institution claim holds only for the person's own institution; any other
// claim reads the token's values.
func claimMatches(tok *oidc.Token, claim, value, institution string) bool {
	if config.CanonicalClaim(claim) == config.InstitutionClaim {
		return institution != "" && oidc.EqualASCIIFold(oidc.Normalize(config.InstitutionClaim, value), institution)
	}
	return slices.Contains(oidc.ClaimValues(tok, claim), value)
}

// institutionAccepted is whether an accept on the institution claim lists the
// PERSON's institution (their name's scope): the stated claim passing
// oidc.Check is not enough (it may state several values).
func institutionAccepted(accept []config.Accept, institution string) bool {
	for _, a := range accept {
		if config.CanonicalClaim(a.Claim) != config.InstitutionClaim {
			continue
		}
		if !slices.ContainsFunc(a.Values, func(v string) bool {
			return oidc.EqualASCIIFold(oidc.Normalize(config.InstitutionClaim, v), institution)
		}) {
			return false
		}
	}
	return true
}

// usesInstitution says whether any accept or mapping keys on the institution
// claim: then the person must have an institution (a scoped name), accept or
// not.
func usesInstitution(accept []config.Accept, claims ...string) bool {
	for _, a := range accept {
		if config.CanonicalClaim(a.Claim) == config.InstitutionClaim {
			return true
		}
	}
	return slices.ContainsFunc(claims, func(c string) bool { return config.CanonicalClaim(c) == config.InstitutionClaim })
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
