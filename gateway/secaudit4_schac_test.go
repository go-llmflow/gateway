package gateway

// Security audit 4, 2026-11-08 (untracked proof). Bypasses of the v0.38.0 tie
// between the name's scope and the stated institution (person.go
// tiedToInstitution).

import (
	"context"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
)

// (1) Several values: the tie asks only that the name's scope be AMONG the
// stated values (person.go containsInstitution); a map on the same claim
// matches ANY value (jwt.go slices.Contains(ClaimValues)). A univ-d.fr person
// whose token states [univ-d.fr, univ-c.fr] gets univ-c.fr's team.
func TestSecaudit4SchacMultiValued(t *testing.T) {
	a, b := newFakeIdP(t), newFakeIdP(t)
	now := time.Now()
	e := multiJWTEnv(t, a, b, func() time.Time { return now },
		[]config.Accept{{Claim: "schac_home_organization", Values: []string{"univ-c.fr", "univ-d.fr"}}})
	e.cfg.JWTs[1].Maps = []config.JWTMap{{Claim: "schac_home_organization", Value: "univ-c.fr", Team: "etabB"}}
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}
	tok := b.sign(t, rs, b.claims(now, map[string]any{"aud": "other-aud", "sub": "s-dave",
		"schac_home_organization": []string{"univ-d.fr", "univ-c.fr"}, "preferred_username": "dave@univ-d.fr"}))
	if k, err := e.srv.lookupJWT(context.Background(), tok); err == nil {
		t.Errorf("EXPLOIT: dave@univ-d.fr stating [univ-d.fr univ-c.fr] mapped to univ-c.fr's team %q (org %q)", k.Team, k.Org)
	}
}

// (3) Two spellings: the tie reads the claim the accept names
// (schacHomeOrganization); the map reads another one (schac_home_organization).
func TestSecaudit4SchacTwoSpellings(t *testing.T) {
	a, b := newFakeIdP(t), newFakeIdP(t)
	now := time.Now()
	e := multiJWTEnv(t, a, b, func() time.Time { return now },
		[]config.Accept{{Claim: "schacHomeOrganization", Values: []string{"univ-c.fr", "univ-d.fr"}}})
	e.cfg.JWTs[1].Maps = []config.JWTMap{{Claim: "schac_home_organization", Value: "univ-c.fr", Team: "etabB"}}
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}
	tok := b.sign(t, rs, b.claims(now, map[string]any{"aud": "other-aud", "sub": "s-dave",
		"schacHomeOrganization": "univ-d.fr", "schac_home_organization": "univ-c.fr", "preferred_username": "dave@univ-d.fr"}))
	if k, err := e.srv.lookupJWT(context.Background(), tok); err == nil {
		t.Errorf("EXPLOIT: tie on schacHomeOrganization=univ-d.fr, map on schac_home_organization=univ-c.fr: team %q (org %q)", k.Team, k.Org)
	}
}

// (2) No accept: an issuer that lists the institutions in domains (no accept
// block) has no tie at all (tiedToInstitution returns nil without an accept on
// schac), but a map on schac_home_organization still selects the team.
func TestSecaudit4SchacMapWithoutAccept(t *testing.T) {
	a, b := newFakeIdP(t), newFakeIdP(t)
	now := time.Now()
	e := multiJWTEnv(t, a, b, func() time.Time { return now }, nil)
	e.cfg.JWTs[1].Domains = []string{"univ-c.fr", "univ-d.fr"}
	e.cfg.JWTs[1].Maps = []config.JWTMap{{Claim: "schac_home_organization", Value: "univ-c.fr", Team: "etabB"}}
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}
	tok := b.sign(t, rs, b.claims(now, map[string]any{"aud": "other-aud", "sub": "s-dave",
		"schac_home_organization": "univ-c.fr", "preferred_username": "dave@univ-d.fr"}))
	if k, err := e.srv.lookupJWT(context.Background(), tok); err == nil {
		t.Errorf("EXPLOIT: no accept block: dave@univ-d.fr stating univ-c.fr mapped to team %q (org %q), owner %q", k.Team, k.Org, k.Owner)
	}
}
