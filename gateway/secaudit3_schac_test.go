package gateway

// Security audit 3, 2026-11-07 (untracked proof). The institution filter of a
// federation issuer (accept on schac_home_organization) and the scope check of
// the name are two independent checks: nothing ties the institution a token
// states to the scope of the name it carries. Behind go-authn/bridge the name
// (eppn, subject-id) is held to the asserting IdP's shibmd scopes
// (go-authn/saml sp.go:529, assertion.go:42-47) but schacHomeOrganization is
// not (saml assertion.go:42-47 omits it; bridge internal/app/claims.go:136-137
// copies it as asserted). So an IdP of a NON-accepted institution passes the
// filter by stating an accepted one, and a map/role on that claim hands it the
// accepted institution's team.

import (
	"context"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
)

func TestSecaudit3SchacNotTiedToNameScope(t *testing.T) {
	a, b := newFakeIdP(t), newFakeIdP(t)
	now := time.Now()
	e := multiJWTEnv(t, a, b, func() time.Time { return now },
		[]config.Accept{{Claim: "schac_home_organization", Values: []string{"univ-c.fr", "univ-d.fr"}}})
	// The README's pattern (role on schac_home_organization), as a jwt map.
	e.cfg.JWTs[1].Maps = []config.JWTMap{{Claim: "schac_home_organization", Value: "univ-c.fr", Team: "etabB"}}
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}
	for _, tc := range []struct{ who, name string }{
		{"an IdP of a non-accepted institution (univ-e.fr)", "eve@univ-e.fr"},
		{"an IdP of the other accepted institution (univ-d.fr)", "dave@univ-d.fr"},
	} {
		tok := b.sign(t, rs, b.claims(now, map[string]any{"aud": "other-aud", "sub": "s-" + tc.name,
			"schac_home_organization": "univ-c.fr", "preferred_username": tc.name}))
		k, err := e.srv.lookupJWT(context.Background(), tok)
		if err == nil {
			t.Errorf("EXPLOIT: %s, name %q stating schac univ-c.fr, admitted on team %q (org %q), owner %q",
				tc.who, tc.name, k.Team, k.Org, k.Owner)
		}
	}
}
