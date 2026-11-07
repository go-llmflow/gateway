package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

// Third-party audit 2026-11-07, findings 2 and 3: POST /admin/users took any
// valid name from the administrator of ANY organisation.
//
//  2. a bare name, once domains are configured and the accounts migrated, made
//     the next Start refuse to run (every institution's gateway);
//  3. carol@example.org, pre-created by another institution's org admin (a
//     hand-made user, origin ""), was never managed by example.org's SCIM
//     (scim.go) and was refused by its restricted SSO.
//
// The proofs (secaudit3_unscopedboot_test.go, secaudit3_usersquat_test.go)
// skipped when the creation was refused; these assert the refusal itself, and
// that the legitimate creations still work.
func TestAnOrgAdminCannotCreateABareNameOnADomainedGateway(t *testing.T) {
	st := store.NewMemory()
	cfg := scimConfig(t)
	cfg.LegacyNames = false // migrated
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey, SCIMToken: scimToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, srv: srv, st: st, cfg: cfg}
	e.gw = httptest.NewServer(srv.Handler())
	tokA := e.adminToken("admina@example.org", `{"role":"org_admin","org":"etabA"}`)
	for who, tok := range map[string]string{"an org admin": tokA, "the proxy admin": master} {
		resp := e.do("POST", "/admin/users", tok, `{"name":"mallory"}`)
		if out := readAll(t, resp); resp.StatusCode != http.StatusBadRequest || !strings.Contains(out, "local@domain") {
			t.Errorf("%s created a bare name: %d %s", who, resp.StatusCode, out)
		}
	}
	e.gw.Close()
	srv.Close(context.Background())
	// The restart the bare name would have blocked.
	srv2, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey, SCIMToken: scimToken})
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close(context.Background())
	if err := srv2.Start(context.Background()); err != nil {
		t.Errorf("the gateway no longer starts: %v", err)
	}
}

func TestAnOrgAdminCreatesUsersOnlyInItsOwnInstitutionsDomains(t *testing.T) {
	st := store.NewMemory()
	e := newSCIMEnv(t, st) // scim domain example.org, groups mapped to etabA's teams
	tokA := e.adminToken("admina@example.org", `{"role":"org_admin","org":"etabA"}`)
	tokB := e.adminToken("adminb@example.net", `{"role":"org_admin","org":"etabB"}`)

	// etabB's org admin may not squat example.org's person; etabA's may; the
	// proxy admin may create any scoped name.
	for _, tc := range []struct {
		who, tok, name string
		want           int
	}{
		{"etabB's org admin, example.org's person", tokB, "carol@example.org", http.StatusBadRequest},
		{"etabB's org admin, a bare name", tokB, "carol", http.StatusBadRequest},
		{"etabA's org admin, its own domain", tokA, "dora@example.org", http.StatusCreated},
		{"etabA's org admin, the domain in capitals", tokA, "erin@EXAMPLE.ORG", http.StatusCreated},
		{"etabA's org admin, another domain", tokA, "fay@example.net", http.StatusBadRequest},
		{"the proxy admin, any scoped name", master, "gil@example.net", http.StatusCreated},
	} {
		resp := e.do("POST", "/admin/users", tc.tok, `{"name":"`+tc.name+`"}`)
		if out := readAll(t, resp); resp.StatusCode != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.who, resp.StatusCode, out, tc.want)
		}
	}
	// The consequence the squat had: example.org's SCIM manages carol.
	id := e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "carol@example.org", 1))["id"].(string)
	e.scimOK(201, "POST", "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Lab Physics","members":[{"value":"`+id+`"}]}`)
	if !e.hasBinding(store.Binding{User: "carol@example.org", Role: store.RoleMember, Team: "etabA"}) {
		t.Error("SCIM did not give carol her role")
	}
}

// The other half of finding 3, traced only until now: a stored user with no
// role and no link (what a pre-created user is) is refused by a restricted SSO
// provider without adopt, so the squatted person cannot log in either.
func TestARestrictedProviderRefusesAUserWithNoRoleAndNoLink(t *testing.T) {
	m := newMultiEnv(t)
	if err := m.st.CreateUser(context.Background(), "frank@univ-b.fr"); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := m.login("b", m.claims("b", "sb-frank", "frank@univ-b.fr", nil)); code != http.StatusForbidden {
		t.Fatalf("a pre-created user with no role logged in at a restricted provider without adopt: %d %s", code, body)
	}
}
