package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// Several identity providers: OpenID Connect Core §2 (sub is unique within
// its issuer), RFC 9207 §2.4 and RFC 9700 §4.4 (mix-up), the institution
// filter on a federation proxy.

type multiEnv struct {
	*env
	idps map[string]*fakeIdP
}

// newMultiEnv has provider "a" (organisation etabA), "b" (etabB) and "fed",
// a federation proxy for both that accepts two institutions only.
func newMultiEnv(t testing.TB) *multiEnv {
	t.Helper()
	m := &multiEnv{idps: map[string]*fakeIdP{"a": newFakeIdP(t), "b": newFakeIdP(t), "fed": newFakeIdP(t)}}
	cfg := testConfig("http://127.0.0.1:1")
	cfg.AuthFailuresPerMinute = 0
	cfg.SSO = &config.SSO{RedirectURL: gwOrigin + "/admin/callback", SessionTTL: time.Hour}
	mk := func(name string, orgs []string, accept []config.Accept, roles []config.SSORole) {
		cfg.SSO.Providers = append(cfg.SSO.Providers, config.SSOProvider{
			Name: name, Issuer: m.idps[name].srv.URL, ClientID: "client-" + name, RedirectURL: gwOrigin + "/admin/callback/" + name,
			Scopes: []string{"openid"}, Restricted: true, Orgs: orgs, Accept: accept, Roles: roles})
	}
	mk("a", []string{"etabA"}, nil, []config.SSORole{
		{Claim: "groups", Value: "admins", Role: "org_admin", Org: "etabA"},
		{Claim: "groups", Value: "evil", Role: "org_admin", Org: "etabB"},
		{Claim: "groups", Value: "evil-team", Role: "team_admin", Team: "etabB"},
		{Claim: "groups", Value: "root", Role: "proxy_admin"},
	})
	mk("b", []string{"etabB"}, nil, []config.SSORole{
		{Claim: "groups", Value: "admins", Role: "org_admin", Org: "etabB"},
		{Claim: "groups", Value: "users", Role: "member", Team: "etabB"},
	})
	mk("fed", []string{"etabA", "etabB"}, []config.Accept{{Claim: "schac_home_organization", Values: []string{"univ-a.fr", "univ-b.fr"}}},
		[]config.SSORole{{Claim: "groups", Value: "users", Role: "member", Team: "etabA"}})
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { gw.Close(); srv.Close(context.Background()) })
	m.env = &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	return m
}

func (m *multiEnv) req(method, path string, cookies map[string]string) *http.Response {
	m.t.Helper()
	r, err := http.NewRequest(method, m.gw.URL+path, nil)
	if err != nil {
		m.t.Fatal(err)
	}
	for k, v := range cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	resp, err := noRedirect.Do(r)
	if err != nil {
		m.t.Fatal(err)
	}
	return resp
}

func (m *multiEnv) claims(p, sub, user string, extra map[string]any) map[string]any {
	now := time.Now()
	c := map[string]any{"iss": m.idps[p].srv.URL, "aud": "client-" + p, "sub": sub, "preferred_username": user,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
	for k, v := range extra {
		c[k] = v
	}
	return c
}

// begin starts a login at provider p: the authorization URL and login cookie.
func (m *multiEnv) begin(p string) (string, string) {
	m.t.Helper()
	resp := m.req("GET", "/admin/login/"+p, nil)
	readAll(m.t, resp)
	if resp.StatusCode != http.StatusFound {
		m.t.Fatalf("login %s: %d", p, resp.StatusCode)
	}
	ck, _ := cookieOf(resp, loginCookie)
	return resp.Header.Get("Location"), ck
}

// finish delivers the answer of provider issuer to callback cb, with iss
// appended when non-empty.
func (m *multiEnv) finish(cb, state, code, ck, iss string) (int, string, string) {
	m.t.Helper()
	q := url.Values{"state": {state}, "code": {code}}
	if iss != "" {
		q.Set("iss", iss)
	}
	resp := m.req("GET", "/admin/callback/"+cb+"?"+q.Encode(), map[string]string{loginCookie: ck})
	body := readAll(m.t, resp)
	sess, _ := cookieOf(resp, sessionCookie)
	return resp.StatusCode, body, sess
}

// login runs a login at p and returns the status, the body and the session.
func (m *multiEnv) login(p string, claims map[string]any) (int, string, string) {
	m.t.Helper()
	authURL, ck := m.begin(p)
	state, code := m.idps[p].issue(m.t, authURL, claims)
	return m.finish(p, state, code, ck, "")
}

func (m *multiEnv) roles(sess string) []store.Binding {
	m.t.Helper()
	resp := m.req("GET", "/admin/session", map[string]string{sessionCookie: sess})
	var info struct{ Roles []store.Binding }
	if err := json.Unmarshal([]byte(readAll(m.t, resp)), &info); err != nil || resp.StatusCode != 200 {
		m.t.Fatalf("session: %d %v", resp.StatusCode, err)
	}
	return info.Roles
}

func TestMultiIdPLoginPage(t *testing.T) {
	m := newMultiEnv(t)
	resp := m.req("GET", "/admin/login?next=ui", nil)
	body := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") {
		t.Fatalf("login page: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	for _, p := range []string{"a", "b", "fed"} {
		if !strings.Contains(body, `href="/admin/login/`+p+`?next=ui"`) {
			t.Errorf("provider %s not listed: %s", p, body)
		}
	}
	if strings.Contains(strings.ToLower(body), "<script") {
		t.Error("the login page carries a script")
	}
	// The bare callback of the single-issuer form does not exist here.
	if r := m.req("GET", "/admin/callback?state=x&code=y", nil); r.StatusCode != 404 {
		t.Errorf("/admin/callback with provider blocks: %d", r.StatusCode)
	}
	if r := m.req("GET", "/admin/login/nope", nil); r.StatusCode != 404 {
		t.Errorf("unknown provider: %d", r.StatusCode)
	}
}

func TestMultiIdPLogin(t *testing.T) {
	m := newMultiEnv(t)
	code, body, sess := m.login("a", m.claims("a", "sa-1", "carol", map[string]any{"groups": []string{"admins"}}))
	if code != http.StatusSeeOther || sess == "" {
		t.Fatalf("login a: %d %s", code, body)
	}
	rs := m.roles(sess)
	if len(rs) != 1 || rs[0].Role != store.RoleOrgAdmin || rs[0].Org != "etabA" {
		t.Fatalf("roles: %+v", rs)
	}
	code, body, sess = m.login("b", m.claims("b", "sb-1", "dave", map[string]any{"groups": []string{"admins"}}))
	if code != http.StatusSeeOther {
		t.Fatalf("login b: %d %s", code, body)
	}
	if rs := m.roles(sess); len(rs) != 1 || rs[0].Org != "etabB" {
		t.Fatalf("roles b: %+v", rs)
	}
}

// RFC 9700 §4.4: the answer of provider A delivered to B's callback is
// refused, and so is a response whose iss is not the provider's (RFC 9207).
func TestMultiIdPMixUp(t *testing.T) {
	m := newMultiEnv(t)
	adm := map[string]any{"groups": []string{"admins"}}

	authURL, ck := m.begin("a")
	state, code := m.idps["a"].issue(t, authURL, m.claims("a", "sa-1", "carol", adm))
	if c, _, sess := m.finish("b", state, code, ck, ""); c != 400 || sess != "" {
		t.Fatalf("response for A at B's callback: %d, session %q", c, sess)
	}
	// The state was used up by the refusal: the right callback now refuses too.
	if c, _, _ := m.finish("a", state, code, ck, ""); c != 400 {
		t.Fatalf("state reused: %d", c)
	}

	// A mix-up where the attacker sends B's iss with A's answer.
	authURL, ck = m.begin("a")
	state, code = m.idps["a"].issue(t, authURL, m.claims("a", "sa-1", "carol", adm))
	if c, _, sess := m.finish("a", state, code, ck, m.idps["b"].srv.URL); c != 400 || sess != "" {
		t.Fatalf("iss of another provider: %d", c)
	}
	// The right iss passes.
	authURL, ck = m.begin("a")
	state, code = m.idps["a"].issue(t, authURL, m.claims("a", "sa-1", "carol", adm))
	if c, body, _ := m.finish("a", state, code, ck, m.idps["a"].srv.URL); c != http.StatusSeeOther {
		t.Fatalf("right iss: %d %s", c, body)
	}

	// An ID token whose iss is another provider's, from the right endpoint:
	// refused by the verifier (iss compared with THIS provider's).
	bad := m.claims("a", "sa-1", "carol", adm)
	bad["iss"] = m.idps["b"].srv.URL
	if c, _, _ := m.login("a", bad); c != 400 {
		t.Fatalf("ID token iss mismatch: %d", c)
	}
	// An ID token signed by B's key, claiming A's iss.
	authURL, ck = m.begin("a")
	state, code = m.idps["b"].issue(t, authURL, m.claims("a", "sa-1", "carol", adm)) // b's token endpoint holds the grant
	if c, _, _ := m.finish("a", state, code, ck, ""); c != 400 {
		t.Fatalf("code from another provider: %d", c)
	}
}

func TestMultiIdPOrgEscalation(t *testing.T) {
	m := newMultiEnv(t)
	// Only roles outside provider a's organisations: nothing granted, refused.
	code, _, sess := m.login("a", m.claims("a", "sa-9", "mallory", map[string]any{"groups": []string{"evil", "evil-team", "root"}}))
	if code != http.StatusForbidden || sess != "" {
		t.Fatalf("escalation only: %d", code)
	}
	// Mixed: the own role is granted, the others dropped.
	code, _, sess = m.login("a", m.claims("a", "sa-9", "mallory", map[string]any{"groups": []string{"admins", "evil", "evil-team", "root"}}))
	if code != http.StatusSeeOther {
		t.Fatalf("mixed: %d", code)
	}
	if rs := m.roles(sess); len(rs) != 1 || rs[0].Org != "etabA" || rs[0].Role != store.RoleOrgAdmin {
		t.Fatalf("roles: %+v", rs)
	}
	// A provider that allows proxy roles does grant them.
	m.srv.sso.providers["a"].cfg.AllowProxyRoles = true
	_, _, sess = m.login("a", m.claims("a", "sa-9", "mallory", map[string]any{"groups": []string{"root"}}))
	if rs := m.roles(sess); len(rs) != 1 || rs[0].Role != store.RoleProxyAdmin {
		t.Fatalf("allowed proxy role: %+v", rs)
	}
}

// An identity is (provider, subject): a name bound to provider A cannot be
// logged into through B, nor through A with another subject.
func TestMultiIdPTakeover(t *testing.T) {
	m := newMultiEnv(t)
	ctx := context.Background()
	if err := m.st.CreateUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if err := m.st.Grant(ctx, store.Binding{User: "alice", Role: store.RoleProxyAdmin, Provenance: store.ProvProxyAdmin}); err != nil {
		t.Fatal(err)
	}
	// B asserts alice before alice ever logged in: B is not the first
	// provider and alice holds a proxy-wide role: refused, no link made.
	code, _, _ := m.login("b", m.claims("b", "sb-evil", "alice", map[string]any{"groups": []string{"admins"}}))
	if code != http.StatusForbidden {
		t.Fatalf("takeover before the first login: %d", code)
	}
	// The real alice, at the first provider, which may vouch for proxy roles:
	// linked, proxy admin.
	m.srv.sso.providers["a"].cfg.AllowProxyRoles = true
	code, _, sess := m.login("a", m.claims("a", "sa-alice", "alice", nil))
	if code != http.StatusSeeOther {
		t.Fatalf("alice at a: %d", code)
	}
	if rs := m.roles(sess); len(rs) != 1 || rs[0].Role != store.RoleProxyAdmin {
		t.Fatalf("alice's roles: %+v", rs)
	}
	// Now B (with its own role mapping) and A with another subject.
	if code, _, _ = m.login("b", m.claims("b", "sb-evil", "alice", map[string]any{"groups": []string{"admins"}})); code != http.StatusForbidden {
		t.Fatalf("takeover through b: %d", code)
	}
	if code, _, _ = m.login("a", m.claims("a", "other-sub", "alice", map[string]any{"groups": []string{"admins"}})); code != http.StatusForbidden {
		t.Fatalf("takeover with another subject: %d", code)
	}
	// The federation proxy, an accepted institution, same name: refused.
	if code, _, _ = m.login("fed", m.claims("fed", "f-1", "alice", map[string]any{"groups": []string{"users"}, "schac_home_organization": "univ-a.fr"})); code != http.StatusForbidden {
		t.Fatalf("takeover through the federation: %d", code)
	}
	if l, err := m.st.UserIdP(ctx, "alice"); err != nil || l != (store.IdPLink{Provider: "a", Subject: "sa-alice"}) {
		t.Fatalf("link: %+v %v", l, err)
	}
}

// A user provisioned for one institution (stored roles inside the provider's
// organisations, no link yet) logs in through that institution's provider.
func TestMultiIdPProvisionedUser(t *testing.T) {
	m := newMultiEnv(t)
	ctx := context.Background()
	if err := m.st.CreateUser(ctx, "erin"); err != nil {
		t.Fatal(err)
	}
	if err := m.st.Grant(ctx, store.Binding{User: "erin", Role: store.RoleMember, Team: "etabB", Provenance: store.ProvSCIM}); err != nil {
		t.Fatal(err)
	}
	// b is not the first provider, but erin's only role is inside etabB.
	code, _, sess := m.login("b", m.claims("b", "sb-erin", "erin", nil))
	if code != http.StatusSeeOther {
		t.Fatalf("erin at b: %d", code)
	}
	if rs := m.roles(sess); len(rs) != 1 || rs[0].Team != "etabB" {
		t.Fatalf("roles: %+v", rs)
	}
	if code, _, _ := m.login("a", m.claims("a", "sa-erin", "erin", map[string]any{"groups": []string{"admins"}})); code != http.StatusForbidden {
		t.Fatalf("erin at a after being linked to b: %d", code)
	}
}

func TestMultiIdPAccept(t *testing.T) {
	m := newMultiEnv(t)
	users := map[string]any{"groups": []string{"users"}}
	with := func(extra map[string]any) map[string]any {
		c := map[string]any{"groups": []string{"users"}}
		for k, v := range extra {
			c[k] = v
		}
		return c
	}
	_ = users
	for _, tc := range []struct {
		name   string
		claims map[string]any
		want   int
		msg    string
	}{
		{"absent claim", with(nil), 403, "did not state your institution"},
		{"wrong value", with(map[string]any{"schac_home_organization": "univ-z.fr"}), 403, `institution \"univ-z.fr\" is not accepted`},
		{"right value", with(map[string]any{"schac_home_organization": "univ-a.fr"}), 303, ""},
		{"domain compared without case", with(map[string]any{"schac_home_organization": "UNIV-B.FR"}), 303, ""},
		{"multi-valued, one accepted", with(map[string]any{"schac_home_organization": []string{"univ-z.fr", "univ-b.fr"}}), 303, ""},
		{"multi-valued, none accepted", with(map[string]any{"schac_home_organization": []string{"univ-y.fr", "univ-z.fr"}}), 403, `institution \"univ-y.fr\"`},
		{"suffix is not a match", with(map[string]any{"schac_home_organization": "evil-univ-a.fr"}), 403, "not accepted"},
		{"hostile value is shown safely", with(map[string]any{"schac_home_organization": "x\r\n<script>"}), 403, "not accepted"},
	} {
		code, body, _ := m.login("fed", m.claims("fed", "f-"+tc.name, "user-"+strings.ReplaceAll(tc.name, " ", "-"), tc.claims))
		if code != tc.want || !strings.Contains(body, tc.msg) {
			t.Errorf("%s: %d %s", tc.name, code, body)
		}
		if strings.ContainsAny(body, "\r<") && !strings.Contains(body, `<`) {
			t.Errorf("%s: unsafe echo: %q", tc.name, body)
		}
		if tc.want == 403 && (strings.Contains(body, "schac_home_organization") || strings.Contains(body, "claim")) {
			t.Errorf("%s: internals in the message: %s", tc.name, body)
		}
	}
}

// ---- inference JWTs ----

func multiJWTEnv(t *testing.T, a, b *fakeIdP, clock func() time.Time, accept []config.Accept) *env {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.AuthFailuresPerMinute = 0
	cfg.JWTs = []*config.JWT{
		{Name: "a", Issuer: a.srv.URL, Audience: "llmflow", ClockSkew: time.Minute, MinRefresh: time.Hour, Orgs: []string{"etabA"},
			Maps: []config.JWTMap{{Claim: "groups", Value: "lab", Team: "etabA"}}},
		{Name: "b", Issuer: b.srv.URL, Audience: "other-aud", ClockSkew: time.Minute, MinRefresh: time.Hour, Orgs: []string{"etabB"},
			Accept: accept, Maps: []config.JWTMap{{Claim: "groups", Value: "lab", Team: "etabB"}}},
	}
	return newEnv(t, cfg, WithClock(clock))
}

func TestMultiIdPJWT(t *testing.T) {
	a, b, c := newFakeIdP(t), newFakeIdP(t), newFakeIdP(t)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := multiJWTEnv(t, a, b, clock, []config.Accept{{Claim: "schac_home_organization", Values: []string{"univ-b.fr"}}})
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}
	lab := map[string]any{"groups": []string{"lab"}}
	withB := func(extra map[string]any) map[string]any {
		x := map[string]any{"aud": "other-aud", "groups": []string{"lab"}}
		for k, v := range extra {
			x[k] = v
		}
		return x
	}

	expect(t, "issuer a", e.chatWith(a.sign(t, rs, a.claims(now, lab)), chat), 200)
	expect(t, "issuer b, accepted", e.chatWith(b.sign(t, rs, b.claims(now, withB(map[string]any{"schac_home_organization": "UNIV-B.fr"}))), chat), 200)
	expect(t, "issuer b, claim absent", e.chatWith(b.sign(t, rs, b.claims(now, withB(nil))), chat), 401)
	expect(t, "issuer b, wrong institution", e.chatWith(b.sign(t, rs, b.claims(now, withB(map[string]any{"schac_home_organization": "univ-z.fr"}))), chat), 401)
	expect(t, "unknown issuer", e.chatWith(c.sign(t, rs, c.claims(now, lab)), chat), 401)
	// Signed by b's key but claiming a's iss: verified against a's key set only.
	expect(t, "b's key, a's iss", e.chatWith(b.sign(t, rs, a.claims(now, lab)), chat), 401)
	// a's audience is not b's.
	expect(t, "wrong audience for the issuer", e.chatWith(a.sign(t, rs, a.claims(now, map[string]any{"aud": "other-aud"})), chat), 401)
	if got := a.fetches.Load() + b.fetches.Load(); got < 2 {
		t.Fatalf("key sets fetched %d times", got)
	}
	if n := c.fetches.Load(); n != 0 {
		t.Fatalf("the unknown issuer's key set was fetched %d times", n)
	}
}

func FuzzUnverifiedIssuer(f *testing.F) {
	f.Add("aaa." + b64u([]byte(`{"iss":"https://x"}`)) + ".sig")
	f.Add("..")
	f.Add("a.b.c")
	f.Fuzz(func(t *testing.T, raw string) {
		iss, ok := unverifiedIssuer(raw)
		if ok && iss == "" {
			t.Fatal("ok with an empty issuer")
		}
	})
}

// Review of #136, F3: a grant held at provider B but bound to a login started
// at A must be refused at B's callback. (Without the provider check on the
// pending login the answer is a 303.)
func TestMultiIdPFlowBoundToProvider(t *testing.T) {
	m := newMultiEnv(t)
	authURL, ck := m.begin("a")
	state, code := m.idps["b"].issue(t, authURL, m.claims("b", "sb-1", "carol", map[string]any{"groups": []string{"admins"}}))
	if c, body, sess := m.finish("b", state, code, ck, ""); c != 400 || sess != "" {
		t.Fatalf("login started at a finished at b: %d %s", c, body)
	}
}

// Review of #136, F1: a provider block adopts an unlinked user only when it
// may vouch for every role the user holds.
func TestMultiIdPAdoptionNeedsOrgs(t *testing.T) {
	m := newMultiEnv(t)
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	adm := map[string]any{"groups": []string{"admins"}}
	// An etabB user (SCIM) cannot be taken over through a, the first provider.
	must(m.st.CreateUser(ctx, "erin"))
	must(m.st.Grant(ctx, store.Binding{User: "erin", Role: store.RoleMember, Team: "etabB", Provenance: store.ProvSCIM}))
	if code, _, _ := m.login("a", m.claims("a", "attacker", "erin", adm)); code != http.StatusForbidden {
		t.Fatalf("a adopted an etabB user: %d", code)
	}
	// Nor an unlinked proxy admin, as long as a does not allow proxy roles.
	must(m.st.CreateUser(ctx, "root"))
	must(m.st.Grant(ctx, store.Binding{User: "root", Role: store.RoleProxyAdmin, Provenance: store.ProvProxyAdmin}))
	if code, _, _ := m.login("a", m.claims("a", "attacker", "root", adm)); code != http.StatusForbidden {
		t.Fatalf("a adopted a proxy admin: %d", code)
	}
	// A user with no role is adopted only with adopt = true.
	must(m.st.CreateUser(ctx, "nobody"))
	if code, _, _ := m.login("a", m.claims("a", "s-n", "nobody", adm)); code != http.StatusForbidden {
		t.Fatalf("a adopted a user with no role: %d", code)
	}
	m.srv.sso.providers["a"].cfg.Adopt = true
	if code, _, _ := m.login("a", m.claims("a", "s-n", "nobody", adm)); code != http.StatusSeeOther {
		t.Fatalf("adopt = true: %d", code)
	}
	// The real owner of erin's account is still free to log in at b.
	if code, _, _ := m.login("b", m.claims("b", "sb-erin", "erin", nil)); code != http.StatusSeeOther {
		t.Fatalf("erin at b: %d", code)
	}
}

// Review of #136, F2: a session opened before the user was stored does not
// become theirs, and cannot mint an admin token carrying their roles.
func TestMultiIdPSessionBeforeUser(t *testing.T) {
	m := newMultiEnv(t)
	ctx := context.Background()
	code, _, sess := m.login("b", m.claims("b", "sb-mallory", "frank", map[string]any{"groups": []string{"users"}}))
	if code != http.StatusSeeOther {
		t.Fatalf("login: %d", code)
	}
	resp := m.req("GET", "/admin/session", map[string]string{sessionCookie: sess})
	var info struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal([]byte(readAll(t, resp)), &info)
	if err := m.st.CreateUser(ctx, "frank"); err != nil {
		t.Fatal(err)
	}
	if err := m.st.Grant(ctx, store.Binding{User: "frank", Role: store.RoleOrgAdmin, Org: "etabA", Provenance: store.ProvProxyAdmin}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.st.LinkUserIdP(ctx, "frank", store.IdPLink{Provider: "a", Subject: "sa-frank"}); err != nil {
		t.Fatal(err)
	}
	r, _ := http.NewRequest("POST", m.gw.URL+"/admin/users/frank/tokens", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
	r.Header.Set(csrfHeader, info.CSRF)
	r.Header.Set("Origin", gwOrigin)
	resp, err := noRedirect.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	if body := readAll(t, resp); resp.StatusCode == http.StatusCreated || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("token minted or wrong refusal: %d %s", resp.StatusCode, body)
	}
	// The session is gone.
	if r := m.req("GET", "/admin/session", map[string]string{sessionCookie: sess}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session still valid: %d", r.StatusCode)
	}
}
