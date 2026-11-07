package gateway

// Security audit 2026-11-05 (untracked proof). A federation jwt issuer whose
// accept block lists two institutions: a person of univ-b.fr (the token's own
// schac_home_organization) whose preferred_username says alice@univ-a.fr is
// attributed to alice@univ-a.fr, a stored user linked to ANOTHER issuer.

import (
	"context"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func TestSecauditFederationScopeNotTiedToInstitution(t *testing.T) {
	a, b := newFakeIdP(t), newFakeIdP(t)
	now := time.Now()
	e := multiJWTEnv(t, a, b, func() time.Time { return now },
		[]config.Accept{{Claim: "schac_home_organization", Values: []string{"univ-a.fr", "univ-b.fr"}}})
	e.cfg.JWTs[0].Domain = "univ-a.fr"
	ctx := context.Background()
	if err := e.st.CreateUser(ctx, "alice@univ-a.fr"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.LinkUserIdP(ctx, "alice@univ-a.fr", store.IdPLink{Provider: "a", Subject: "a-alice"}); err != nil {
		t.Fatal(err)
	}
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}
	tok := b.sign(t, rs, b.claims(now, map[string]any{"aud": "other-aud", "sub": "b-mallory", "groups": []string{"lab"},
		"schac_home_organization": "univ-b.fr", "preferred_username": "alice@univ-a.fr"}))
	// Since v0.38.0 (audit 2026-11-07) the name's scope must BE the institution
	// the token states, so this token is refused outright: no team, no role, and
	// no owner (before it was admitted with the synthetic id).
	k, err := e.srv.lookupJWT(ctx, tok)
	if err == nil {
		t.Errorf("EXPLOIT: a univ-b.fr person of issuer b whose name says univ-a.fr was admitted: team %q, owner %q", k.Team, k.Owner)
	}
}
