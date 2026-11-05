package gateway

// Regression test for the security review of 2026-11-05 (v0.27.0), finding 1: the person a JWT is
// attributed to is its username claim alone, not (issuer, subject). A token
// of issuer B (organisation etabB) that says preferred_username "alice"
// is charged to, limited by, and shown the usage of the person "alice" of
// issuer A (organisation etabA).

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func TestJWTPersonIsIssuerAndSubjectNotName(t *testing.T) {
	a, b := newFakeIdP(t), newFakeIdP(t)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := multiJWTEnv(t, a, b, clock, nil)
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}

	// alice of etabA, at issuer a.
	alice := a.sign(t, rs, a.claims(now, map[string]any{"sub": "a-alice", "preferred_username": "alice", "groups": []string{"lab"}}))
	// mallory of etabB, at issuer b, whose IdP (or whose own profile) says
	// preferred_username "alice".
	mallory := b.sign(t, rs, b.claims(now, map[string]any{"sub": "b-mallory", "preferred_username": "alice",
		"aud": "other-aud", "groups": []string{"lab"}}))

	// etabA's person budget for alice (set by the proxy admin here; a team
	// admin of etabA may set it too).
	e.check(nil, []call{{"master", "PUT", "/admin/budgets", `{"kind":"user","name":"alice","period":"day","max_cost":0.4}`, 200}})

	body := `{"model":"llama","max_tokens":20,"messages":[]}`
	for i := 0; i < 2; i++ {
		resp := e.do("POST", "/v1/chat/completions", mallory, body)
		readAll(t, resp)
		if resp.StatusCode != 200 {
			t.Fatalf("mallory request %d: %d", i, resp.StatusCode)
		}
	}
	// etabB's mallory has spent etabA's alice's budget: alice is refused.
	resp := e.do("POST", "/v1/chat/completions", alice, body)
	out := readAll(t, resp)
	if resp.StatusCode == 429 && strings.Contains(out, "user alice") {
		t.Errorf("EXPLOIT: a token of issuer b (etabB) exhausted the user budget of alice of issuer a (etabA): %d %s", resp.StatusCode, out)
	}

	// And reads alice's usage windows through /v1/me/usage.
	resp = e.do("GET", "/v1/me/usage", mallory, "")
	me := readAll(t, resp)
	if strings.Contains(me, `"user":"alice"`) && strings.Contains(me, `"budgets":[{`) {
		t.Errorf("EXPLOIT: issuer b's token reads alice's person budgets: %s", me)
	}

	e.srv.Close(context.Background())
	users := map[string][]string{}
	for _, r := range e.st.Records() {
		users[r.User] = append(users[r.User], r.Org)
	}
	if orgs := users["alice"]; len(orgs) > 0 && strings.Contains(strings.Join(orgs, ","), "etabB") {
		t.Errorf("EXPLOIT: records of etabB attributed to person %q: %v", "alice", orgs)
	}
}

// A stored user linked to exactly this issuer and subject keeps their name;
// the same name from another subject, or from an issuer with no sso provider,
// is a synthetic person.
func TestJWTPersonKeepsTheNameOfALinkedUser(t *testing.T) {
	a, b := newFakeIdP(t), newFakeIdP(t)
	now := time.Now()
	e := multiJWTEnv(t, a, b, func() time.Time { return now }, nil)
	e.cfg.SSO = &config.SSO{Providers: []config.SSOProvider{{Name: "pa", Issuer: a.srv.URL, Restricted: true, Orgs: []string{"etabA"}}}}
	ctx := context.Background()
	if err := e.st.CreateUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.LinkUserIdP(ctx, "alice", store.IdPLink{Provider: "pa", Subject: "a-alice"}); err != nil {
		t.Fatal(err)
	}
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}
	lab := []string{"lab"}
	owner := func(raw string) string {
		k, err := e.srv.lookupJWT(ctx, raw)
		if err != nil {
			t.Fatal(err)
		}
		return k.Owner
	}
	if got := owner(a.sign(t, rs, a.claims(now, map[string]any{"sub": "a-alice", "preferred_username": "alice", "groups": lab}))); got != "alice" {
		t.Errorf("the linked user: %q", got)
	}
	if got := owner(a.sign(t, rs, a.claims(now, map[string]any{"sub": "a-other", "preferred_username": "alice", "groups": lab}))); !strings.HasPrefix(got, "jwt-") {
		t.Errorf("another subject of the same issuer: %q", got)
	}
	if got := owner(b.sign(t, rs, b.claims(now, map[string]any{"sub": "b-1", "preferred_username": "alice", "aud": "other-aud", "groups": lab}))); !strings.HasPrefix(got, "jwt-") {
		t.Errorf("another issuer: %q", got)
	}
}
