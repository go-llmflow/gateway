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
	k, err := e.srv.lookupJWT(ctx, tok)
	if err != nil {
		t.Fatal(err)
	}
	if k.Owner == "alice@univ-a.fr" {
		t.Errorf("EXPLOIT: a univ-b.fr person of issuer b is attributed to alice@univ-a.fr (linked to issuer a): owner %q", k.Owner)
	}
}
