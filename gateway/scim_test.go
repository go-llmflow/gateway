package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

const scimToken = "scim-token-0123456789abcdef0123456789"

func scimConfig(t *testing.T) *config.Config {
	cfg := testConfig("http://127.0.0.1:1")
	cfg.AuthFailuresPerMinute = 0
	cfg.SCIM = &config.SCIM{BaseURL: "https://llm.example.org/scim/v2", Groups: []config.SCIMGroup{
		{Name: "Lab Physics", Team: "etabA", Role: "member"},
		{Name: "lab leads", Team: "etabA", Role: "team_admin"},
		{Name: "Ghost Lab", Team: "no-such-team", Role: "member"},
	}}
	return cfg
}

func newSCIMEnv(t *testing.T, st *store.Memory) *env {
	t.Helper()
	cfg := scimConfig(t)
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey, SCIMToken: scimToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, srv: srv, st: st, cfg: cfg}
	e.gw = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { e.gw.Close(); srv.Close(context.Background()) })
	return e
}

func (e *env) scim(method, path, body string) (int, map[string]any) {
	e.t.Helper()
	resp := e.do(method, "/scim/v2"+path, scimToken, body, "Content-Type", "application/scim+json")
	b := readAll(e.t, resp)
	var m map[string]any
	if b != "" {
		if err := json.Unmarshal([]byte(b), &m); err != nil {
			e.t.Fatalf("%s %s: %s", method, path, b)
		}
	}
	return resp.StatusCode, m
}

func (e *env) scimOK(want int, method, path, body string) map[string]any {
	e.t.Helper()
	got, m := e.scim(method, path, body)
	if got != want {
		e.t.Fatalf("%s %s: %d %v, want %d", method, path, got, m, want)
	}
	return m
}

func (e *env) hasBinding(b store.Binding) bool {
	e.t.Helper()
	bs, err := e.st.Bindings(context.Background(), b.User)
	if err != nil {
		e.t.Fatal(err)
	}
	return slices.ContainsFunc(bs, func(x store.Binding) bool { return x.Key() == b.Key() })
}

// provenanceOf is the recorded provenance of a stored binding.
func (e *env) provenanceOf(b store.Binding) store.Provenance {
	e.t.Helper()
	bs, err := e.st.Bindings(context.Background(), b.User)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, x := range bs {
		if x.Key() == b.Key() {
			return x.Provenance
		}
	}
	e.t.Fatalf("no binding %+v", b)
	return ""
}

func (e *env) disabled(name string) bool {
	e.t.Helper()
	u, err := e.st.GetUser(context.Background(), name)
	if err != nil {
		e.t.Fatalf("user %s: %v", name, err)
	}
	return u.Disabled
}

const userBody = `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:User"],"userName":"%s","active":true}`

func patchBody(ops string) string {
	return `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],"Operations":` + ops + `}`
}

func TestSCIMProvisioning(t *testing.T) {
	st := store.NewMemory()
	e := newSCIMEnv(t, st)
	ctx := context.Background()

	// No token, a wrong token.
	if resp := e.do("GET", "/scim/v2/Users", "", ""); resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp := e.do("GET", "/scim/v2/Users", master, ""); resp.StatusCode != 401 {
		t.Fatalf("master key as scim token: %d", resp.StatusCode)
	}

	alice := e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "alice@example.org", 1))["id"].(string)
	bob := e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "bob", 1))["id"].(string)
	if e.disabled("alice@example.org") || e.disabled("bob") {
		t.Fatal("provisioned users are not enabled gateway users")
	}
	if got, m := e.scim("POST", "/Users", strings.Replace(userBody, "%s", "carol smith", 1)); got != 400 || m["scimType"] != "invalidValue" {
		t.Fatalf("userName with a space: %d %v", got, m)
	}
	// A manual role, which provisioning must not touch.
	manual := store.Binding{User: "alice@example.org", Role: store.RoleViewer, Team: "etabB"}
	if err := st.Grant(ctx, manual); err != nil {
		t.Fatal(err)
	}

	member := store.Binding{User: "alice@example.org", Role: store.RoleMember, Team: "etabA"}
	g := e.scimOK(201, "POST", "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"lab physics","members":[{"value":"`+alice+`"}]}`)["id"].(string)
	e.scimOK(201, "POST", "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Ghost Lab","members":[{"value":"`+bob+`"}]}`)
	if !e.hasBinding(member) {
		t.Fatal("group member has no role on the mapped team")
	}
	if got := e.provenanceOf(member); got != store.ProvSCIM {
		t.Fatalf("provisioned binding's provenance %q, want scim", got)
	}
	if got, _ := e.scim("POST", "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"x","members":[{"value":"nobody"}]}`); got != 400 {
		t.Fatalf("unknown member: %d", got)
	}
	// Filters reach the store's name index.
	if l := e.scimOK(200, "GET", `/Users?filter=userName%20eq%20%22ALICE@example.org%22`, ""); l["totalResults"].(float64) != 1 {
		t.Fatalf("userName filter %v", l)
	}
	if l := e.scimOK(200, "GET", `/Groups?filter=displayName%20eq%20%22LAB%20PHYSICS%22`, ""); l["totalResults"].(float64) != 1 {
		t.Fatalf("displayName filter %v", l)
	}
	u := e.scimOK(200, "GET", "/Users/"+alice, "")
	if gs, _ := u["groups"].([]any); len(gs) != 1 {
		t.Fatalf("user's groups %v", u)
	}

	// Leaving the group removes the role, not the manual one.
	e.scimOK(200, "PATCH", "/Groups/"+g, patchBody(`[{"op":"Remove","path":"members[value eq \"`+alice+`\"]"}]`))
	if e.hasBinding(member) || !e.hasBinding(manual) {
		t.Fatal("role after leaving the group")
	}
	// Inactive: disabled, and no role even when added back.
	e.scimOK(200, "PATCH", "/Users/"+alice, patchBody(`[{"op":"Replace","path":"active","value":"False"}]`))
	e.scimOK(200, "PATCH", "/Groups/"+g, patchBody(`[{"op":"Add","path":"members","value":[{"value":"`+alice+`"}]}]`))
	if !e.disabled("alice@example.org") || e.hasBinding(member) {
		t.Fatal("inactive user enabled or holding a provisioned role")
	}
	e.scimOK(200, "PATCH", "/Users/"+alice, patchBody(`[{"op":"Replace","path":"active","value":true}]`))
	if e.disabled("alice@example.org") || !e.hasBinding(member) {
		t.Fatal("reactivated user not enabled or without the group's role")
	}
	// Renamed: the old name is retired, the new one gets the role.
	e.scimOK(200, "PATCH", "/Users/"+alice, patchBody(`[{"op":"replace","path":"userName","value":"alice.l@example.org"}]`))
	if !e.disabled("alice@example.org") || e.hasBinding(member) {
		t.Fatal("old name still active after a rename")
	}
	renamed := store.Binding{User: "alice.l@example.org", Role: store.RoleMember, Team: "etabA"}
	if e.disabled("alice.l@example.org") || !e.hasBinding(renamed) {
		t.Fatal("new name has no role after a rename")
	}
	// A second mapped group, then the user deleted.
	e.scimOK(201, "POST", "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Lab Leads","members":[{"value":"`+alice+`"}]}`)
	admin := store.Binding{User: "alice.l@example.org", Role: store.RoleTeamAdmin, Team: "etabA"}
	if !e.hasBinding(admin) {
		t.Fatal("team_admin group gives no role")
	}
	e.scimOK(204, "DELETE", "/Users/"+alice, "")
	if !e.disabled("alice.l@example.org") || e.hasBinding(renamed) || e.hasBinding(admin) {
		t.Fatal("deleted user still enabled or holding roles")
	}
	if grp := e.scimOK(200, "GET", "/Groups/"+g, ""); grp["members"] != nil {
		t.Fatalf("deleted user still a member: %v", grp)
	}
	// Deleting a group removes its roles.
	carol := e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "carol", 1))["id"].(string)
	e.scimOK(200, "PUT", "/Groups/"+g, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Lab Physics","members":[{"value":"`+carol+`"}]}`)
	cm := store.Binding{User: "carol", Role: store.RoleMember, Team: "etabA"}
	if !e.hasBinding(cm) {
		t.Fatal("PUT group gives no role")
	}
	e.scimOK(204, "DELETE", "/Groups/"+g, "")
	if e.hasBinding(cm) {
		t.Fatal("role after the group was deleted")
	}

	// A restart repairs a role lost half-way.
	e.scimOK(201, "POST", "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Lab Physics","members":[{"value":"`+carol+`"}]}`)
	if err := st.Ungrant(ctx, cm); err != nil {
		t.Fatal(err)
	}
	newSCIMEnv(t, st)
	if !e.hasBinding(cm) {
		t.Fatal("start-up reconciliation did not restore the role")
	}
}

// FuzzSCIM sends arbitrary bodies to the provisioning endpoints with the
// gateway's backend behind go-authn/scim: never a 5xx, and the
// reconciliation never grants a role outside the mapped (role, team) pairs.
func FuzzSCIM(f *testing.F) {
	cfg := scimConfig(nil)
	st := store.NewMemory()
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey, SCIMToken: scimToken})
	if err != nil {
		f.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { srv.Close(context.Background()) })
	h := srv.Handler()
	send := func(method, path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/scim/v2"+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+scimToken)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	var u, g struct{ ID string }
	_ = json.Unmarshal(send("POST", "/Users", strings.Replace(userBody, "%s", "seed", 1)).Body.Bytes(), &u)
	_ = json.Unmarshal(send("POST", "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Lab Physics","members":[{"value":"`+u.ID+`"}]}`).Body.Bytes(), &g)
	f.Add("PATCH", "/Groups/"+g.ID, patchBody(`[{"op":"replace","path":"displayName","value":"lab leads"}]`))
	f.Add("PATCH", "/Users/"+u.ID, patchBody(`[{"op":"replace","path":"userName","value":"x@y"}]`))
	f.Add("POST", "/Users", strings.Replace(userBody, "%s", "n+1@e", 1))
	f.Add("PUT", "/Groups/"+g.ID, `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Ghost Lab","members":[{"value":"`+u.ID+`"}]}`)
	f.Add("DELETE", "/Users/"+u.ID, "")
	f.Fuzz(func(t *testing.T, method, path, body string) {
		if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, " #?%") || len(path) > 512 ||
			strings.ContainsFunc(path, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
			return // not a request target httptest can build
		}
		if method == "" || strings.ContainsFunc(method, func(r rune) bool { return r < 'A' || r > 'Z' }) {
			return
		}
		if w := send(method, path, body); w.Code >= 500 {
			t.Fatalf("%s %s %q: %d %s", method, path, body, w.Code, w.Body)
		}
		bs, _ := st.Bindings(context.Background(), "")
		for _, b := range bs {
			if b.Team != "etabA" && b.Team != "no-such-team" || b.Role != store.RoleMember && b.Role != store.RoleTeamAdmin {
				t.Fatalf("binding outside the mapping: %+v", b)
			}
		}
	})
}

func TestSCIMConfig(t *testing.T) {
	cfg := scimConfig(t)
	if _, err := New(cfg, store.NewMemory(), Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey, SCIMToken: "short"}); err == nil {
		t.Fatal("short scim token accepted")
	}
	// Without a scim block there is no endpoint.
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	if resp := e.do("GET", "/scim/v2/Users", scimToken, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("scim without configuration: %d", resp.StatusCode)
	}
}
