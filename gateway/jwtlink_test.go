package gateway

// jwtPerson and the stored (provider, subject) link: the scoped name is the
// person only when no link exists for it or the link is this issuer's and this
// subject's (the SSO login makes the same check, sso.go bindIdentity). And a
// name with two scopes is no name.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func TestJWTPersonKeepsItsNameOnlyWithItsOwnLink(t *testing.T) {
	a, b := newFakeIdP(t), newFakeIdP(t)
	now := time.Now()
	e := multiJWTEnv(t, a, b, func() time.Time { return now }, nil)
	e.cfg.JWTs[0].Domain = "univ-a.fr"
	ctx := context.Background()
	for _, n := range []string{"alice@univ-a.fr", "carol@univ-a.fr"} {
		if err := e.st.CreateUser(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.st.LinkUserIdP(ctx, "alice@univ-a.fr", store.IdPLink{Provider: "a", Subject: "a-alice"}); err != nil {
		t.Fatal(err)
	}
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}
	owner := func(sub, user string) string {
		t.Helper()
		k, err := e.srv.lookupJWT(ctx, a.sign(t, rs, a.claims(now, map[string]any{"sub": sub, "preferred_username": user, "groups": []string{"lab"}})))
		if err != nil {
			t.Fatal(err)
		}
		return k.Owner
	}
	if got := owner("a-alice", "alice"); got != "alice@univ-a.fr" {
		t.Errorf("the linked person: %q", got)
	}
	if got := owner("a-other", "alice"); !strings.HasPrefix(got, "jwt-") {
		t.Errorf("another subject of the same issuer asserting alice's name: %q, want the synthetic person", got)
	}
	if got := owner("a-carol", "carol"); got != "carol@univ-a.fr" {
		t.Errorf("a stored user with no link keeps the scoped name: %q", got)
	}
	if got := owner("a-new", "newbie"); got != "newbie@univ-a.fr" {
		t.Errorf("a name no stored user holds: %q", got)
	}
	if _, err := e.st.UserIdP(ctx, "carol@univ-a.fr"); err == nil {
		t.Error("a token created a link")
	}
	// Two scopes are no name.
	for _, bad := range []string{"a@b@univ-a.fr", "alice@univ-a.fr@univ-a.fr"} {
		if got := owner("a-x", bad); !strings.HasPrefix(got, "jwt-") {
			t.Errorf("%q: %q, want the synthetic person", bad, got)
		}
	}
}

func TestValidUserNameHasOneScopeAtMost(t *testing.T) {
	for name, want := range map[string]bool{"alice": true, "alice@univ-a.fr": true, "a+b@x.org": true,
		"a@b@c": false, "@x": false, "x@": false, "@": false, "a@@b": false, "": false} {
		if got := config.ValidUserName(name); got != want {
			t.Errorf("ValidUserName(%q) = %v, want %v", name, got, want)
		}
	}
}
