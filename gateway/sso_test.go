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

const gwOrigin = "https://gw.example"

type ssoEnv struct {
	*env
	idp   *fakeIdP
	mu    sync.Mutex
	clock time.Time
}

func newSSOEnv(t *testing.T, clientSecret string) *ssoEnv {
	t.Helper()
	idp := newFakeIdP(t)
	idp.secret = clientSecret
	cfg := testConfig("http://127.0.0.1:1")
	cfg.AuthFailuresPerMinute = 0
	cfg.SSO = &config.SSO{
		Issuer: idp.srv.URL, ClientID: "llmflow-admin", RedirectURL: gwOrigin + "/admin/callback",
		Scopes: []string{"openid", "profile"}, SessionTTL: time.Hour,
		Roles: []config.SSORole{
			{Claim: "groups", Value: "llm-admins", Role: "proxy_admin"},
			{Claim: "groups", Value: "lab-a-leads", Role: "team_admin", Team: "etabA"},
		},
	}
	e := &ssoEnv{idp: idp, clock: time.Now()}
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey, SSOClientSecret: clientSecret},
		WithClock(func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.clock }))
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { gw.Close(); srv.Close(context.Background()) })
	e.env = &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	return e
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// req sends a request with cookies and headers, without following
// redirects. The gateway's cookies are Secure; the test speaks plain http
// to it and sends them by hand, as a browser would to the https origin.
func (e *ssoEnv) req(method, path string, cookies map[string]string, body string, hdr ...string) *http.Response {
	e.t.Helper()
	r, err := http.NewRequest(method, e.gw.URL+path, strings.NewReader(body))
	if err != nil {
		e.t.Fatal(err)
	}
	for k, v := range cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := noRedirect.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	return resp
}

func cookieOf(resp *http.Response, name string) (string, bool) {
	for _, c := range resp.Cookies() {
		if c.Name == name {
			return c.Value, true
		}
	}
	return "", false
}

func (e *ssoEnv) claims(sub string, groups ...string) map[string]any {
	now := time.Now()
	return map[string]any{"iss": e.idp.srv.URL, "aud": "llmflow-admin", "sub": sub, "preferred_username": sub,
		"groups": groups, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
}

// start begins a login and returns the authorization URL and login cookie.
func (e *ssoEnv) start() (authURL, loginCk string) {
	e.t.Helper()
	resp := e.req("GET", "/admin/login", nil, "")
	readAll(e.t, resp)
	if resp.StatusCode != http.StatusFound {
		e.t.Fatalf("login: %d", resp.StatusCode)
	}
	ck, ok := cookieOf(resp, loginCookie)
	if !ok {
		e.t.Fatal("login: no login cookie")
	}
	for _, c := range resp.Cookies() {
		if c.Name == loginCookie && (!c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.Path != "/") {
			e.t.Fatalf("login cookie attributes: %+v", c)
		}
	}
	return resp.Header.Get("Location"), ck
}

func (e *ssoEnv) callback(state, code, loginCk string) *http.Response {
	e.t.Helper()
	cks := map[string]string{}
	if loginCk != "" {
		cks[loginCookie] = loginCk
	}
	return e.req("GET", "/admin/callback?"+url.Values{"state": {state}, "code": {code}}.Encode(), cks, "")
}

// login runs a whole login and returns the session cookie and CSRF token.
func (e *ssoEnv) login(claims map[string]any) (string, string) {
	e.t.Helper()
	authURL, ck := e.start()
	state, code := e.idp.issue(e.t, authURL, claims)
	resp := e.callback(state, code, ck)
	body := readAll(e.t, resp)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/admin/session" {
		e.t.Fatalf("callback: %d %s", resp.StatusCode, body)
	}
	sess, ok := cookieOf(resp, sessionCookie)
	if !ok {
		e.t.Fatal("callback: no session cookie")
	}
	resp = e.req("GET", "/admin/session", map[string]string{sessionCookie: sess}, "")
	var info struct {
		Name string
		CSRF string `json:"csrf_token"`
	}
	if err := json.Unmarshal([]byte(readAll(e.t, resp)), &info); err != nil || resp.StatusCode != 200 || info.CSRF == "" {
		e.t.Fatalf("session: %d %v", resp.StatusCode, err)
	}
	return sess, info.CSRF
}

func TestSSOLoginSessionAndCSRF(t *testing.T) {
	for _, secret := range []string{"", "client-secret"} {
		e := newSSOEnv(t, secret)
		sess, csrf := e.login(e.claims("alice", "llm-admins"))
		cks := map[string]string{sessionCookie: sess}

		expect(t, "read with the session", e.req("GET", "/admin/orgs", cks, "").StatusCode, 200)
		// CSRF: a state-changing request needs the token...
		expect(t, "write without CSRF token", e.req("POST", "/admin/orgs", cks, `{"name":"o1"}`).StatusCode, 403)
		expect(t, "write with a wrong CSRF token", e.req("POST", "/admin/orgs", cks, `{"name":"o1"}`, csrfHeader, csrf+"x").StatusCode, 403)
		// ...and must not come from another origin, token or not.
		expect(t, "cross-origin write", e.req("POST", "/admin/orgs", cks, `{"name":"o1"}`, csrfHeader, csrf, "Origin", "https://evil.example").StatusCode, 403)
		expect(t, "cross-site fetch", e.req("POST", "/admin/orgs", cks, `{"name":"o1"}`, csrfHeader, csrf, "Sec-Fetch-Site", "cross-site").StatusCode, 403)
		expect(t, "write with CSRF token", e.req("POST", "/admin/orgs", cks, `{"name":"o1"}`, csrfHeader, csrf, "Origin", gwOrigin).StatusCode, 201)
		// A bearer request is not cookie-authenticated: no CSRF token needed.
		expect(t, "bearer write", e.do("POST", "/admin/orgs", master, `{"name":"o2"}`).StatusCode, 201)

		// Logout ends the session here.
		expect(t, "logout without CSRF", e.req("POST", "/admin/logout", cks, "").StatusCode, 403)
		resp := e.req("POST", "/admin/logout", cks, "", csrfHeader, csrf)
		if b := readAll(t, resp); resp.StatusCode != 200 || !strings.Contains(b, e.idp.srv.URL+"/logout") {
			t.Fatalf("logout: %d %s", resp.StatusCode, b)
		}
		expect(t, "after logout", e.req("GET", "/admin/orgs", cks, "").StatusCode, 401)
	}
}

func TestSSOCallbackRefusals(t *testing.T) {
	e := newSSOEnv(t, "")

	// Replayed state: the second use of a state fails, even with a fresh code.
	authURL, ck := e.start()
	state, code := e.idp.issue(t, authURL, e.claims("alice", "llm-admins"))
	expect(t, "first callback", e.callback(state, code, ck).StatusCode, 303)
	_, code2 := e.idp.issue(t, authURL, e.claims("alice", "llm-admins"))
	expect(t, "replayed state", e.callback(state, code2, ck).StatusCode, 400)

	// Login CSRF: the callback must come from the browser that started it.
	authURL, ck = e.start()
	state, code = e.idp.issue(t, authURL, e.claims("alice", "llm-admins"))
	expect(t, "no login cookie", e.callback(state, code, "").StatusCode, 400)
	authURL, ck = e.start()
	state, code = e.idp.issue(t, authURL, e.claims("alice", "llm-admins"))
	expect(t, "other browser's login cookie", e.callback(state, code, ck+"x").StatusCode, 400)

	// An ID token for another login (wrong nonce).
	authURL, ck = e.start()
	c := e.claims("alice", "llm-admins")
	c["nonce"] = "another-login"
	state, code = e.idp.issue(t, authURL, c)
	expect(t, "wrong nonce", e.callback(state, code, ck).StatusCode, 400)

	// An ID token for another client.
	authURL, ck = e.start()
	c = e.claims("alice", "llm-admins")
	c["aud"] = "other-client"
	state, code = e.idp.issue(t, authURL, c)
	expect(t, "wrong audience", e.callback(state, code, ck).StatusCode, 400)

	// A code the provider does not know (or a PKCE mismatch).
	authURL, ck = e.start()
	state, _ = e.idp.issue(t, authURL, e.claims("alice", "llm-admins"))
	expect(t, "unknown code", e.callback(state, "forged", ck).StatusCode, 400)

	// The provider's error.
	expect(t, "provider error", e.req("GET", "/admin/callback?error=access_denied&state=x", nil, "").StatusCode, 400)

	// Expired login.
	authURL, ck = e.start()
	state, code = e.idp.issue(t, authURL, e.claims("alice", "llm-admins"))
	e.mu.Lock()
	e.clock = e.clock.Add(loginTTL + time.Second)
	e.mu.Unlock()
	expect(t, "expired login", e.callback(state, code, ck).StatusCode, 400)

	// Nobody with neither a stored user nor a matching role block.
	authURL, ck = e.start()
	state, code = e.idp.issue(t, authURL, e.claims("mallory", "students"))
	expect(t, "no role", e.callback(state, code, ck).StatusCode, 403)
}

// One network cannot fill the table of logins in progress.
func TestSSOLoginFloodIsBoundedPerNetwork(t *testing.T) {
	e := newSSOEnv(t, "")
	for i := 0; i < maxLoginsPerNet; i++ {
		e.start()
	}
	resp := e.req("GET", "/admin/login", nil, "")
	readAll(t, resp)
	expect(t, "login beyond the per-network bound", resp.StatusCode, 503)
	// Once they expire, logins are possible again.
	e.mu.Lock()
	e.clock = e.clock.Add(loginTTL + time.Second)
	e.mu.Unlock()
	e.start()
}

func TestSSORolesAndRevocation(t *testing.T) {
	e := newSSOEnv(t, "")
	// tom is team_admin of etabA through his group, not more.
	sess, csrf := e.login(e.claims("tom", "lab-a-leads"))
	cks := map[string]string{sessionCookie: sess}
	expect(t, "team admin creates a project", e.req("POST", "/admin/teams/etabA/projects", cks, `{"name":"p"}`, csrfHeader, csrf).StatusCode, 201)
	expect(t, "team admin cannot create orgs", e.req("POST", "/admin/orgs", cks, `{"name":"o"}`, csrfHeader, csrf).StatusCode, 403)

	// A stored user's roles come from the store, read on each request, and
	// disabling the user ends the session at once.
	expect(t, "create user", e.do("POST", "/admin/users", master, `{"name":"bob"}`).StatusCode, 201)
	expect(t, "grant", e.do("PUT", "/admin/users/bob/roles", master, `{"role":"viewer","team":"etabB"}`).StatusCode, 200, 201, 204)
	sess, _ = e.login(e.claims("bob"))
	cks = map[string]string{sessionCookie: sess}
	resp := e.req("GET", "/admin/session", cks, "")
	if b := readAll(t, resp); !strings.Contains(b, `"etabB"`) {
		t.Fatalf("stored roles missing from the session: %s", b)
	}
	expect(t, "disable", e.do("DELETE", "/admin/users/bob", master, "").StatusCode, 200, 204)
	expect(t, "disabled user's session", e.req("GET", "/admin/teams", cks, "").StatusCode, 401)

	// Sessions expire.
	sess, _ = e.login(e.claims("alice", "llm-admins"))
	cks = map[string]string{sessionCookie: sess}
	e.mu.Lock()
	e.clock = e.clock.Add(time.Hour + time.Second)
	e.mu.Unlock()
	expect(t, "expired session", e.req("GET", "/admin/orgs", cks, "").StatusCode, 401)
	expect(t, "forged session", e.req("GET", "/admin/orgs", map[string]string{sessionCookie: "forged"}, "").StatusCode, 401)
}

// FuzzSSOCallback sends arbitrary callback queries and cookies: nothing
// panics and no session is ever created.
func FuzzSSOCallback(f *testing.F) {
	idp := newFakeIdP(f)
	cfg := testConfig("http://127.0.0.1:1")
	cfg.AuthFailuresPerMinute = 0
	cfg.SSO = &config.SSO{Issuer: idp.srv.URL, ClientID: "llmflow-admin", RedirectURL: gwOrigin + "/admin/callback",
		Scopes: []string{"openid"}, SessionTTL: time.Hour, Roles: []config.SSORole{{Claim: "groups", Value: "g", Role: "proxy_admin"}}}
	srv, err := New(cfg, store.NewMemory(), Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		f.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { srv.Close(context.Background()) })
	h := srv.Handler()
	// A real pending login, so the fuzzer can reach past the state lookup.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/admin/login", nil))
	loc, _ := url.Parse(rec.Header().Get("Location"))
	state := loc.Query().Get("state")
	f.Add("state="+state+"&code=abc", "")
	f.Add("error=access_denied", "")
	f.Add("state=&code=", "x")
	f.Add("state="+strings.Repeat("a", 300)+"&code=b", "y")
	f.Fuzz(func(t *testing.T, query, cookie string) {
		r := httptest.NewRequest("GET", "/admin/callback", nil)
		r.URL.RawQuery = query
		if cookie != "" {
			r.Header.Set("Cookie", loginCookie+"="+cookie)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code < 400 {
			t.Fatalf("callback %q accepted: %d", query, w.Code)
		}
		if _, ok := cookieOf(w.Result(), sessionCookie); ok {
			t.Fatalf("callback %q set a session", query)
		}
	})
}
