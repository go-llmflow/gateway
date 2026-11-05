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
