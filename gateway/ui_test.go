package gateway

import (
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

// uiPost posts a form as a browser on the gateway's origin would.
func (e *ssoEnv) uiPost(path, sess string, form url.Values, hdr ...string) (int, string) {
	e.t.Helper()
	h := append([]string{"Content-Type", "application/x-www-form-urlencoded", "Origin", gwOrigin}, hdr...)
	resp := e.req("POST", path, map[string]string{sessionCookie: sess}, form.Encode(), h...)
	return resp.StatusCode, readAll(e.t, resp)
}

func (e *ssoEnv) uiGet(path, sess string) (int, string, string) {
	e.t.Helper()
	resp := e.req("GET", path, map[string]string{sessionCookie: sess}, "")
	return resp.StatusCode, readAll(e.t, resp), resp.Header.Get("Content-Security-Policy")
}

var secretRE2 = regexp.MustCompile(`llmf-[A-Za-z0-9_-]+`)

func TestUIRequiresSSOSession(t *testing.T) {
	e := newSSOEnv(t, "")
	resp := e.req("GET", "/ui/", nil, "")
	readAll(t, resp)
	if resp.StatusCode != 303 || resp.Header.Get("Location") != "/admin/login?next=ui" {
		t.Fatalf("%d %s", resp.StatusCode, resp.Header.Get("Location"))
	}
	// A bearer token does not open the UI: it is a browser surface.
	resp = e.req("GET", "/ui/keys", nil, "", "Authorization", "Bearer "+master)
	readAll(t, resp)
	if resp.StatusCode != 303 {
		t.Fatalf("bearer: %d", resp.StatusCode)
	}
	// Login with next=ui lands on the UI.
	resp = e.req("GET", "/admin/login?next=ui", nil, "")
	readAll(t, resp)
	ck, _ := cookieOf(resp, loginCookie)
	state, code := e.idp.issue(t, resp.Header.Get("Location"), e.claims("alice", "llm-admins"))
	cb := e.callback(state, code, ck)
	readAll(t, cb)
	if cb.StatusCode != 303 || cb.Header.Get("Location") != "/ui/" {
		t.Fatalf("callback: %d %s", cb.StatusCode, cb.Header.Get("Location"))
	}
}

func TestUIKeysAndCSRF(t *testing.T) {
	e := newSSOEnv(t, "")
	sess, csrf := e.login(e.claims("alice", "llm-admins"))
	code, body, csp := e.uiGet("/ui/", sess)
	if code != 200 || !strings.Contains(csp, "default-src 'none'") || strings.Contains(csp, "script-src") || strings.Contains(body, "<script") {
		t.Fatalf("%d %q", code, csp)
	}
	form := url.Values{"csrf": {csrf}, "team": {"etabA"}, "name": {"<script>alert(1)</script>"}, "rpm": {"60"}}
	// No token, a wrong token, another origin: refused.
	if c, _ := e.uiPost("/ui/keys", sess, url.Values{"team": {"etabA"}}); c != 403 {
		t.Fatalf("no csrf: %d", c)
	}
	if c, _ := e.uiPost("/ui/keys", sess, url.Values{"csrf": {csrf + "x"}, "team": {"etabA"}}); c != 403 {
		t.Fatalf("wrong csrf: %d", c)
	}
	resp := e.req("POST", "/ui/keys", map[string]string{sessionCookie: sess}, form.Encode(),
		"Content-Type", "application/x-www-form-urlencoded", "Origin", "https://evil.example")
	if readAll(t, resp); resp.StatusCode != 403 {
		t.Fatalf("cross-origin: %d", resp.StatusCode)
	}
	// Created: the secret shown once, on a page that is not cached.
	resp = e.req("POST", "/ui/keys", map[string]string{sessionCookie: sess}, form.Encode(),
		"Content-Type", "application/x-www-form-urlencoded", "Origin", gwOrigin)
	body = readAll(t, resp)
	secret := secretRE2.FindString(body)
	if resp.StatusCode != 200 || secret == "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	// It works on the inference API, and the list never shows it again.
	if c := e.chatWith(secret, chat); c == 401 {
		t.Fatal("the secret shown does not authenticate")
	}
	code, body, _ = e.uiGet("/ui/keys", sess)
	if code != 200 || strings.Contains(body, secret) || !strings.Contains(body, "&lt;script&gt;") || strings.Contains(body, "<script>alert") {
		t.Fatalf("keys page: %d", code)
	}
	// Rotate (a new secret, once) and revoke.
	ks, _ := e.st.ListKeys(t.Context(), "etabA")
	id := ks[len(ks)-1].ID
	c, body := e.uiPost("/ui/keys/"+id+"/rotate", sess, url.Values{"csrf": {csrf}, "grace_period": {"0s"}})
	if c != 200 || secretRE2.FindString(body) == "" || secretRE2.FindString(body) == secret {
		t.Fatalf("rotate: %d %s", c, body)
	}
	if c, body = e.uiPost("/ui/keys/"+id+"/revoke", sess, url.Values{"csrf": {csrf}}); c != 200 || !strings.Contains(body, "revoked") {
		t.Fatalf("revoke: %d %s", c, body)
	}
	// Every UI action went through the admin API's audit.
	es, _ := e.st.AuditRange(t.Context(), 1, 100)
	actions := map[string]bool{}
	for _, a := range es {
		if a.Actor == "alice" {
			actions[a.Action] = true
		}
	}
	for _, want := range []string{"POST /admin/keys", "POST /admin/keys/{id}/rotate", "DELETE /admin/keys/{id}"} {
		if !actions[want] {
			t.Errorf("not audited: %s (have %v)", want, actions)
		}
	}
}

func TestUIScopedToOrg(t *testing.T) {
	e := newSSOEnv(t, "")
	// carol administers etabB only.
	expect(t, "user", e.do("POST", "/admin/users", master, `{"name":"carol"}`).StatusCode, 201)
	expect(t, "grant", e.do("PUT", "/admin/users/carol/roles", master, `{"role":"org_admin","org":"etabB"}`).StatusCode, 200, 201, 204)
	idA, _ := e.newKey("etabA")
	idB, _ := e.newKey("etabB")
	e.setBudget(`{"kind":"team","name":"etabA","period":"day","max_cost":5}`)
	e.setBudget(`{"kind":"team","name":"etabB","period":"day","max_cost":7}`)
	sess, csrf := e.login(e.claims("carol"))

	_, keys, _ := e.uiGet("/ui/keys", sess)
	if strings.Contains(keys, idA) || !strings.Contains(keys, idB) {
		t.Fatalf("keys page shows another org's key or misses its own")
	}
	_, budgets, _ := e.uiGet("/ui/budgets", sess)
	if strings.Contains(budgets, ">etabA<") || !strings.Contains(budgets, ">etabB<") {
		t.Fatalf("budgets: %s", budgets)
	}
	_, fs, _ := e.uiGet("/ui/fairshare", sess)
	if strings.Contains(fs, ">etabA<") || !strings.Contains(fs, ">etabB<") {
		t.Fatalf("fair share: %s", fs)
	}
	// Writing outside the org is refused by the API, and shown as such.
	c, body := e.uiPost("/ui/keys", sess, url.Values{"csrf": {csrf}, "team": {"etabA"}, "name": {"x"}})
	if c != 200 || secretRE2.MatchString(body) || !strings.Contains(body, "class=\"err\"") {
		t.Fatalf("cross-org create: %d", c)
	}
	// The audit log is for proxy roles.
	_, audit, _ := e.uiGet("/ui/audit", sess)
	if !strings.Contains(audit, "class=\"err\"") || strings.Contains(audit, "POST /admin/keys") {
		t.Fatalf("audit visible to an org admin")
	}
}

func TestUIBudgetsSpendAudit(t *testing.T) {
	e := newSSOEnv(t, "")
	sess, csrf := e.login(e.claims("alice", "llm-admins"))
	c, body := e.uiPost("/ui/budgets", sess, url.Values{"csrf": {csrf}, "kind": {"team"}, "name": {"etabA"}, "period": {"month"}, "max_cost": {"12.5"}})
	if c != 200 || !strings.Contains(body, "budget saved") || !strings.Contains(body, ">12.5<") {
		t.Fatalf("%d %s", c, body)
	}
	if c, body = e.uiPost("/ui/budgets", sess, url.Values{"csrf": {csrf}, "kind": {"team"}, "name": {"etabA"}, "period": {"month"}, "max_cost": {"lots"}}); !strings.Contains(body, "must be numbers") {
		t.Fatalf("bad number: %d", c)
	}
	if c, body = e.uiPost("/ui/budgets/delete", sess, url.Values{"csrf": {csrf}, "kind": {"team"}, "name": {"etabA"}}); !strings.Contains(body, "budget deleted") {
		t.Fatalf("delete: %d %s", c, body)
	}
	if b, _ := e.st.Budgets(t.Context()); len(b) != 0 {
		t.Fatalf("budget still there: %+v", b)
	}
	_ = e.st.InsertRecords(t.Context(), []store.Record{{Org: "etabA", Team: "etabA", Model: "llama", Pool: "b300", Cost: 0.25, PromptTokens: 10, Time: e.srv.now()}})
	code, spend, _ := e.uiGet("/ui/spend?group_by=org,team", sess)
	if code != 200 || !strings.Contains(spend, ">0.25") || !strings.Contains(strings.ToLower(spend), "gpu") {
		t.Fatalf("spend: %d %s", code, spend)
	}
	code, audit, _ := e.uiGet("/ui/audit", sess)
	if code != 200 || !strings.Contains(audit, "PUT /admin/budgets") {
		t.Fatalf("audit: %d", code)
	}
	c, body = e.uiPost("/ui/audit/verify", sess, url.Values{"csrf": {csrf}})
	if c != 200 || !strings.Contains(body, "Chain verified") {
		t.Fatalf("verify: %d %s", c, body)
	}
	// Signing out ends the session.
	if c, _ = e.uiPost("/ui/logout", sess, url.Values{"csrf": {csrf}}); c != 200 {
		t.Fatalf("logout: %d", c)
	}
	if resp := e.req("GET", "/ui/keys", map[string]string{sessionCookie: sess}, ""); readAll(t, resp) == "" || resp.StatusCode != 401 {
		t.Fatalf("after logout: %d", resp.StatusCode)
	}
}

// FuzzUIForms posts arbitrary forms to every form handler with a valid
// session and token: no 5xx, no panic, every answer under the CSP, and
// nothing the forms said comes back unescaped.
func FuzzUIForms(f *testing.F) {
	f.Add(0, "team=etabA&name=x&rpm=1")
	f.Add(1, "kind=team&name=etabA&period=day&max_cost=1e308&soft_limit=-1")
	f.Add(2, "kind=key&name=%3Cb%3E&model=llama")
	f.Add(3, "grace_period=999999h")
	f.Add(4, "")
	f.Add(5, "from=-1")
	// One gateway for every iteration, owned by f (so its cleanup runs
	// once, at the end of the fuzz run).
	e := newSSOEnv(f, "")
	sess, csrf := e.login(e.claims("alice", "llm-admins"))
	f.Fuzz(func(t *testing.T, which int, form string) {
		e.t = t // iterations run one after the other in a fuzz process
		vals, err := url.ParseQuery(form)
		if err != nil {
			return
		}
		vals.Set("csrf", csrf)
		paths := []string{"/ui/keys", "/ui/budgets", "/ui/budgets/delete", "/ui/keys/" + url.PathEscape(vals.Get("name")) + "/rotate",
			"/ui/keys/x/revoke", "/ui/audit/verify"}
		p := paths[(which%len(paths)+len(paths))%len(paths)]
		if strings.ContainsAny(p, "?#") {
			return
		}
		resp := e.req("POST", p, map[string]string{sessionCookie: sess}, vals.Encode(),
			"Content-Type", "application/x-www-form-urlencoded", "Origin", gwOrigin)
		body := readAll(t, resp)
		if resp.StatusCode >= 500 {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, body)
		}
		if resp.Header.Get("Content-Security-Policy") != uiCSP {
			t.Fatalf("%s: no CSP", p)
		}
		if strings.Contains(body, "<script") {
			t.Fatalf("%s: a script tag in the page", p)
		}
	})
}
