package gateway

// Security audit 4, 2026-11-08 (untracked proof). Bypass of the v0.38.0
// userNameProblem fix: OrgNameScopes (config/idp.go) gives an org EVERY domain
// of the shared SCIM client as soon as one SCIM group lands on one of its teams.
// With one SCIM client provisioning two institutions (domains univ-a.fr and
// univ-b.fr, groups for both orgs), etabB's admin may still pre-create a
// univ-a.fr person, and SCIM then never manages her (scim.go origin rule).

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func TestSecaudit4SquatThroughSharedSCIMDomains(t *testing.T) {
	st := store.NewMemory()
	cfg := scimConfig(t)
	cfg.SCIM.Domain = ""
	cfg.SCIM.Domains = []string{"univ-a.fr", "univ-b.fr"}
	cfg.SCIM.Groups = []config.SCIMGroup{
		{Name: "Lab Physics", Team: "etabA", Role: "member"},
		{Name: "Lab Chemistry", Team: "etabB", Role: "member"},
	}
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey, SCIMToken: scimToken})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, srv: srv, st: st, cfg: cfg}
	e.gw = httptest.NewServer(srv.Handler())
	t.Cleanup(func() { e.gw.Close(); srv.Close(context.Background()) })
	tokB := e.adminToken("adminb@univ-b.fr", `{"role":"org_admin","org":"etabB"}`)
	resp := e.do("POST", "/admin/users", tokB, `{"name":"carol@univ-a.fr"}`)
	out := readAll(t, resp)
	if resp.StatusCode != 201 {
		t.Skipf("refused: %d %s", resp.StatusCode, out)
	}
	id := e.scimOK(201, "POST", "/Users", strings.Replace(userBody, "%s", "carol@univ-a.fr", 1))["id"].(string)
	e.scimOK(201, "POST", "/Groups", `{"schemas":["urn:ietf:params:scim:schemas:core:2.0:Group"],"displayName":"Lab Physics","members":[{"value":"`+id+`"}]}`)
	if !e.hasBinding(store.Binding{User: "carol@univ-a.fr", Role: store.RoleMember, Team: "etabA"}) {
		t.Errorf("EXPLOIT: etabB's admin pre-created carol@univ-a.fr; univ-a's SCIM group no longer gives her member@etabA")
	}
}
