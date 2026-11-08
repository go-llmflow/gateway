package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/go-authn/oidc"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// JWT access tokens on the inference API, as an alternative to virtual
// keys. The token is verified by github.com/go-authn/oidc (discovery, JWKS,
// algorithm allowlist without none or HMAC, exact iss, mandatory aud and
// exp, bounded clock skew, typ check, bounded refetch on an unknown kid);
// this file only maps the verified identity onto a team.

var errJWTUnmapped = errors.New("token verified, but no jwt map block matches it")

// startJWT builds the verifiers, one per issuer, into a go-authn/oidc Set that
// routes a token by its iss. Discovery happens here, at start-up: a gateway
// that cannot reach its issuer cannot authenticate anybody and should say so
// before it listens.
func (s *Server) startJWT(ctx context.Context) error {
	var vs []*oidc.Verifier
	s.jwtCfg = map[string]*config.JWT{}
	for _, j := range s.cfg.JWTIssuers() {
		v, err := oidc.New(ctx, oidc.Config{
			Issuer: j.Issuer, Audience: j.Audience, JWKSURL: j.JWKSURL, Type: j.Type,
			UsernameClaim: j.UsernameClaim, GroupsClaim: j.GroupsClaim,
			ClockSkew: j.ClockSkew, MinRefresh: j.MinRefresh, Client: s.jwtClient, Now: s.now,
		})
		if err != nil {
			return fmt.Errorf("gateway: jwt issuer %s: %w", j.Issuer, err)
		}
		vs = append(vs, v)
		s.jwtCfg[v.Issuer()] = j
	}
	if len(vs) == 0 {
		return nil
	}
	set, err := oidc.NewSet(vs...)
	if err != nil {
		return fmt.Errorf("gateway: jwt: %w", err)
	}
	s.jwtSet = set
	if len(vs) == 1 {
		s.jwtOne = s.jwtCfg[vs[0].Issuer()]
	}
	return nil
}

// jwtIdentity maps a verified token onto a team through the ordered map
// blocks: the first block whose claim holds its value wins.
func (s *Server) jwtIdentity(ctx context.Context, j *config.JWT, tok *oidc.Token) (store.Key, error) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	// The person's ONE institution (their name's scope), when anything keys on
	// the institution claim, accept or not; mappings never read the raw claim.
	if ref := oidc.Check(j.Accept, tok); ref != nil {
		return store.Key{}, fmt.Errorf("%w: %v", errJWTUnmapped, ref)
	}
	inst := ""
	claims := make([]string, 0, len(j.Maps))
	for _, m := range j.Maps {
		claims = append(claims, m.Claim)
	}
	if usesInstitution(j.Accept, claims...) {
		var err error
		if inst, err = institutionOf(tok, j.NameScopes(), j.Domain); err != nil {
			return store.Key{}, fmt.Errorf("%w: %v", errJWTUnmapped, err)
		}
		if !institutionAccepted(j.Accept, inst) {
			return store.Key{}, fmt.Errorf("%w: the person's institution %q is not accepted", errJWTUnmapped, inst)
		}
	}
	for _, m := range j.Maps {
		if !claimMatches(tok, m.Claim, m.Value, inst) {
			continue
		}
		t, err := s.st.GetTeam(ctx, m.Team)
		if errors.Is(err, store.ErrNotFound) {
			return store.Key{}, fmt.Errorf("%w: team %s does not exist", errJWTUnmapped, m.Team)
		}
		if err != nil {
			return store.Key{}, fmt.Errorf("%w: %v", errAuthUnavailable, err)
		}
		if t.Disabled {
			return store.Key{}, fmt.Errorf("%w: team %s is disabled", errJWTUnmapped, t.Name)
		}
		if m.Project != "" {
			ps, err := s.st.ListProjects(ctx, t.Name)
			if err != nil {
				return store.Key{}, fmt.Errorf("%w: %v", errAuthUnavailable, err)
			}
			if !slices.ContainsFunc(ps, func(p store.Project) bool { return p.Name == m.Project && !p.Disabled }) {
				return store.Key{}, fmt.Errorf("%w: project %s/%s does not exist or is disabled", errJWTUnmapped, t.Name, m.Project)
			}
		}
		// A stable key ID per (issuer, subject): budgets, limits, allowlists
		// and reports see one key per person.
		sum := sha256.Sum256([]byte(tok.Issuer() + "\x00" + tok.Subject()))
		id := "jwt-" + base64.RawURLEncoding.EncodeToString(sum[:16])
		// The person is (issuer, subject), never a name alone (OpenID Connect
		// Core §2: sub is unique within its issuer; a username claim is not,
		// and an issuer's users may choose it). The synthetic key id stands
		// for them, unless the scoped name they assert is no stored user's, or is
		// a stored user linked to exactly this issuer and subject (jwtPerson).
		owner, err := s.jwtPerson(ctx, j, tok, id)
		if err != nil {
			return store.Key{}, fmt.Errorf("%w: %v", errAuthUnavailable, err)
		}
		return store.Key{
			ID: id, Org: t.Org, Team: t.Name, Project: m.Project,
			Owner: owner, Name: tok.Username(), RPM: j.RPM, TPM: j.TPM, MaxInFlight: j.MaxInFlight,
		}, nil
	}
	return store.Key{}, errJWTUnmapped
}

// lookupJWT verifies a token (or finds it in the cache) and returns its
// identity. The error says why, for the server's log only.
func (s *Server) lookupJWT(ctx context.Context, raw string) (store.Key, error) {
	h := sha256.Sum256([]byte(raw))
	if k, found, cached := s.jwtCache.get(h); cached && found {
		return k, nil
	}
	// Routed by iss to one issuer's verifier (an unknown iss is refused with
	// no network access), then verified there.
	tok, err := s.jwtSet.Verify(ctx, raw)
	if err != nil {
		return store.Key{}, err
	}
	// With one issuer there is nothing to look up (reading the issuer from the
	// token would cost an allocation on every verification).
	jc := s.jwtOne
	if jc == nil {
		jc = s.jwtCfg[tok.Issuer()]
	}
	if jc == nil {
		return store.Key{}, errJWTUnmapped
	}
	k, err := s.jwtIdentity(ctx, jc, tok)
	if err != nil {
		return store.Key{}, err
	}
	until := s.now().Add(s.cfg.KeyCacheTTL)
	if exp, ok := tok.Expiry(); ok && exp.Before(until) {
		until = exp
	}
	s.jwtCache.putValidUntil(h, k, until)
	return k, nil
}

// looksLikeJWT: three base64url segments; virtual keys start with llmf-.
func looksLikeJWT(s string) bool {
	if len(s) < 16 || len(s) > 16<<10 {
		return false
	}
	dots := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '.' {
			dots++
		}
	}
	return dots == 2
}

// defaultJWTClient fetches discovery and the key set: no environment
// proxy, a bounded time. go-authn/oidc itself refuses cleartext issuers
// and key sets (except loopback) and checks every redirect.
func defaultJWTClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil}}
}

// jwtPerson is the person a verified token is attributed to: its scoped name
// (local@domain, the scope checked against what this issuer may assert, see
// scopedPerson), or, when it has none this issuer may assert, the synthetic key
// id (iss, sub). A bare username claim is never a person: two issuers, or two
// institutions behind one federation proxy, each have an "alice".
func (s *Server) jwtPerson(ctx context.Context, j *config.JWT, tok *oidc.Token, id string) (string, error) {
	name, err := s.scopedPerson(ctx, tok, j.NameScopes(), j.Domain)
	switch {
	case err == nil:
		// An identity is (provider, subject), as at the SSO login (sso.go
		// bindIdentity): a stored user linked to another provider or subject is
		// somebody else, whatever name this token asserts. The scoped name is the
		// person only when no link exists for it or the link is this issuer's and
		// this subject's. A token never creates a link: that stays the SSO login's.
		switch cur, lerr := s.st.UserIdP(ctx, name); {
		case errors.Is(lerr, store.ErrNotFound):
			return name, nil
		case lerr != nil:
			return "", errPersonStore
		case cur == (store.IdPLink{Provider: j.Name, Subject: tok.Subject()}):
			return name, nil
		}
		return id, nil
	case errors.Is(err, errPersonStore):
		return "", err
	}
	return id, nil
}
