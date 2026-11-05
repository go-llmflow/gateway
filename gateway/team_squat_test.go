package gateway

import (
	"net/http"
	"testing"
)

// Regression test for the security review of 2026-11-05, finding 10: an
// organisation administrator could create, through the API, a team whose name
// the configuration declares under another organisation, and the next start
// refused ("belongs to another org in the store") for everybody.
func TestTeamNameDeclaredByTheConfigIsReserved(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	e := newEnv(t, cfg)
	// The next configuration declares lab-future under etabA; it is not yet in
	// the store (the gateway creates configured teams at start).
	cfg.Orgs[0].Teams = append(cfg.Orgs[0].Teams, "lab-future")
	ob := e.adminToken("ob", `{"role":"org_admin","org":"etabB"}`)
	oa := e.adminToken("oa", `{"role":"org_admin","org":"etabA"}`)
	resp := e.do("POST", "/admin/teams", ob, `{"name":"lab-future","org":"etabB"}`)
	readAll(t, resp)
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("etabB squatted a team name the config gives etabA: %d", resp.StatusCode)
	}
	// Its own organisation may create it, and others stay free.
	resp = e.do("POST", "/admin/teams", oa, `{"name":"lab-future","org":"etabA"}`)
	readAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("etabA's own team: %d", resp.StatusCode)
	}
	resp = e.do("POST", "/admin/teams", ob, `{"name":"lab-b-new","org":"etabB"}`)
	readAll(t, resp)
	if resp.StatusCode != http.StatusCreated {
		t.Errorf("an unreserved name: %d", resp.StatusCode)
	}
}
