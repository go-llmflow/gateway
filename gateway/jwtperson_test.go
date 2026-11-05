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

// A person is local@domain, the domain being one the issuer may assert: the
// name of a legacy account is scoped by the issuer's domain, another scope is
// refused (the person is then the synthetic key id), an unscoped name with no
// domain to scope it is no name.
func TestJWTPersonIsScopedAndTheScopeChecked(t *testing.T) {
	a, b := newFakeIdP(t), newFakeIdP(t)
	now := time.Now()
	e := multiJWTEnv(t, a, b, func() time.Time { return now }, nil)
	e.cfg.JWTs[0].Domain = "univ-a.fr"            // the institution whose accounts predate scoped names
	e.cfg.JWTs[1].Domains = []string{"univ-b.fr"} // accounts of univ-b.fr, always scoped
	ctx := context.Background()
	if err := e.st.CreateUser(ctx, "legacy"); err != nil {
		t.Fatal(err)
	}
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}
	lab := []string{"lab"}
	owner := func(p *fakeIdP, extra map[string]any) string {
		extra["groups"] = lab
		k, err := e.srv.lookupJWT(ctx, p.sign(t, rs, p.claims(now, extra)))
		if err != nil {
			t.Fatal(err)
		}
		return k.Owner
	}
	bAud := func(m map[string]any) map[string]any { m["aud"] = "other-aud"; return m }
	for _, tc := range []struct {
		name, got, want string // want "" is the synthetic jwt-... person
	}{
		{"unscoped, scoped by the issuer's domain", owner(a, map[string]any{"sub": "1", "preferred_username": "alice"}), "alice@univ-a.fr"},
		{"unscoped legacy user of the issuer's domain", owner(a, map[string]any{"sub": "2", "preferred_username": "legacy"}), "legacy"},
		{"scoped, allowed", owner(a, map[string]any{"sub": "3", "preferred_username": "Alice@UNIV-A.fr"}), "alice@univ-a.fr"},
		{"another institution's scope", owner(a, map[string]any{"sub": "4", "preferred_username": "alice@univ-b.fr"}), ""},
		{"b: unscoped, no domain", owner(b, bAud(map[string]any{"sub": "5", "preferred_username": "alice"})), ""},
		{"b: its own scope", owner(b, bAud(map[string]any{"sub": "6", "preferred_username": "alice@univ-b.fr"})), "alice@univ-b.fr"},
		{"b: the other institution's scope", owner(b, bAud(map[string]any{"sub": "7", "preferred_username": "alice@univ-a.fr"})), ""},
		{"b: the legacy name of the other institution", owner(b, bAud(map[string]any{"sub": "8", "preferred_username": "legacy"})), ""},
	} {
		if tc.want == "" && !strings.HasPrefix(tc.got, "jwt-") || tc.want != "" && tc.got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}
