package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
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

// startJWT builds the verifier. Discovery happens here, at start-up: a
// gateway that cannot reach its issuer cannot authenticate anybody and
// should say so before it listens.
func (s *Server) startJWT(ctx context.Context) error {
	for _, j := range s.cfg.JWTIssuers() {
		v, err := oidc.New(ctx, oidc.Config{
			Issuer: j.Issuer, Audience: j.Audience, JWKSURL: j.JWKSURL, Type: j.Type,
			UsernameClaim: j.UsernameClaim, GroupsClaim: j.GroupsClaim,
			ClockSkew: j.ClockSkew, MinRefresh: j.MinRefresh, Client: s.jwtClient, Now: s.now,
		})
		if err != nil {
			return fmt.Errorf("gateway: jwt issuer %s: %w", j.Issuer, err)
		}
		s.jwt = append(s.jwt, &jwtIssuer{cfg: j, v: v})
	}
	return nil
}

// jwtIssuer is one configured issuer of inference access tokens.
type jwtIssuer struct {
	cfg *config.JWT
	v   *oidc.Verifier
}

var errJWTUnknownIssuer = errors.New("token issued by an issuer no jwt block names")

// jwtFor picks the issuer a token is verified against, by its iss claim: one
// key set, never a try of every one. With a single issuer there is nothing to
// choose and the verifier's own exact iss check refuses a stranger.
func (s *Server) jwtFor(raw string) (*jwtIssuer, error) {
	if len(s.jwt) == 1 {
		return s.jwt[0], nil
	}
	iss, ok := unverifiedIssuer(raw)
	if !ok {
		return nil, errJWTUnknownIssuer
	}
	for _, ji := range s.jwt {
		if ji.cfg.Issuer == iss {
			return ji, nil
		}
	}
	return nil, errJWTUnknownIssuer
}

// unverifiedIssuer reads the iss claim of a compact JWT without verifying
// anything: it only chooses the key set the token is then verified with, and
// that check fails for a token whose signature or iss does not hold.
func unverifiedIssuer(raw string) (string, bool) {
	i := strings.IndexByte(raw, '.')
	if i < 0 || len(raw) > 16<<10 {
		return "", false
	}
	rest := raw[i+1:]
	j := strings.IndexByte(rest, '.')
	if j < 0 {
		return "", false
	}
	payload, err := base64.RawURLEncoding.DecodeString(rest[:j])
	if err != nil {
		return "", false
	}
	var c struct {
		Iss string `json:"iss"`
	}
	if json.Unmarshal(payload, &c) != nil || c.Iss == "" {
		return "", false
	}
	return c.Iss, true
}

// claimValues reads a claim as strings: a string, or a list of strings.
func claimValues(tok *oidc.Token, name string) []string {
	if name == "groups" || name == "" {
		return tok.Groups()
	}
	var one string
	if err := tok.Claim(name, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := tok.Claim(name, &many); err == nil {
		return many
	}
	return nil
}

// jwtIdentity maps a verified token onto a team through the ordered map
// blocks: the first block whose claim holds its value wins.
func (s *Server) jwtIdentity(ctx context.Context, j *config.JWT, tok *oidc.Token) (store.Key, error) {
	ctx, cancel := context.WithTimeout(ctx, storeTimeout)
	defer cancel()
	if ae := checkAccept(j.Accept, tok); ae != nil {
		return store.Key{}, fmt.Errorf("%w: %v", errJWTUnmapped, ae)
	}
	for _, m := range j.Maps {
		if !slices.Contains(claimValues(tok, m.Claim), m.Value) {
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
		return store.Key{
			ID: "jwt-" + base64.RawURLEncoding.EncodeToString(sum[:16]), Org: t.Org, Team: t.Name, Project: m.Project,
			Owner: tok.Username(), Name: tok.Username(), RPM: j.RPM, TPM: j.TPM, MaxInFlight: j.MaxInFlight,
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
	ji, err := s.jwtFor(raw)
	if err != nil {
		return store.Key{}, err
	}
	tok, err := ji.v.Verify(ctx, raw)
	if err != nil {
		return store.Key{}, err
	}
	k, err := s.jwtIdentity(ctx, ji.cfg, tok)
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
