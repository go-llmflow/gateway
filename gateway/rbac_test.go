package gateway

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// adminToken creates a user with the given roles through the master key
// and returns one of their admin tokens.
func (e *env) adminToken(user string, roles ...string) string {
	e.t.Helper()
	resp := e.do("POST", "/admin/users", master, fmt.Sprintf(`{"name":%q}`, user))
	if out := readAll(e.t, resp); resp.StatusCode != 201 && resp.StatusCode != 409 {
		e.t.Fatalf("create user %s: %d %s", user, resp.StatusCode, out)
	}
	for _, r := range roles {
		resp := e.do("PUT", "/admin/users/"+user+"/roles", master, r)
		if out := readAll(e.t, resp); resp.StatusCode != 200 {
			e.t.Fatalf("grant %s %s: %d %s", user, r, resp.StatusCode, out)
		}
	}
	resp = e.do("POST", "/admin/users/"+user+"/tokens", master, "")
	out := readAll(e.t, resp)
	var tok struct{ Token string }
	if err := json.Unmarshal([]byte(out), &tok); err != nil || resp.StatusCode != 201 || !strings.HasPrefix(tok.Token, "llma-") {
		e.t.Fatalf("token for %s: %d %s", user, resp.StatusCode, out)
	}
	return tok.Token
}

type call struct {
	who, method, path, body string
	want                    int
}

func (e *env) check(tokens map[string]string, calls []call) {
	e.t.Helper()
	for _, c := range calls {
		tok := tokens[c.who]
		if c.who == "master" {
			tok = master
		}
		resp := e.do(c.method, c.path, tok, c.body)
		out := readAll(e.t, resp)
		if resp.StatusCode != c.want {
			e.t.Errorf("%s %s %s %s: %d, want %d (%s)", c.who, c.method, c.path, c.body, resp.StatusCode, c.want, strings.TrimSpace(out))
		}
	}
}

func TestRBACScopes(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	e.check(nil, []call{
		{"master", "POST", "/admin/orgs", `{"name":"orgC"}`, 201},
		{"master", "POST", "/admin/teams", `{"name":"etabC","org":"orgC"}`, 201},
	})
	tok := map[string]string{
		"alice": e.adminToken("alice", `{"role":"org_admin","org":"etabA"}`),
		"tom":   e.adminToken("tom", `{"role":"team_admin","team":"etabA"}`),
		"mia":   e.adminToken("mia", `{"role":"member","team":"etabA"}`),
		"vic":   e.adminToken("vic", `{"role":"viewer","org":"etabA"}`),
		"eve":   e.adminToken("eve", `{"role":"org_admin","org":"orgC"}`),
		"pat":   e.adminToken("pat", `{"role":"proxy_viewer"}`),
		"nobod": e.adminToken("nobod"),
	}
	e.check(tok, []call{
		// org admin: own organisation only
		{"alice", "POST", "/admin/teams", `{"name":"etabA2","org":"etabA"}`, 201},
		{"alice", "POST", "/admin/teams", `{"name":"x","org":"etabB"}`, 403},
		{"alice", "DELETE", "/admin/teams/etabB", "", 403},
		{"alice", "PUT", "/admin/budgets", `{"kind":"team","name":"etabA","period":"day","max_cost":10}`, 200},
		{"alice", "PUT", "/admin/budgets", `{"kind":"team","name":"etabB","period":"day","max_cost":10}`, 403},
		{"alice", "PUT", "/admin/budgets", `{"kind":"org","name":"etabA","period":"day","max_cost":10}`, 403},
		{"alice", "PUT", "/admin/budgets", `{"kind":"tag","name":"eval","period":"day","max_cost":10}`, 403},
		{"alice", "POST", "/admin/orgs", `{"name":"orgD"}`, 403},
		{"alice", "DELETE", "/admin/orgs/etabA", "", 403},
		{"alice", "POST", "/admin/users", `{"name":"newbie"}`, 201},
		// a binding-less user is a proxy admin's to place (#104)
		{"alice", "PUT", "/admin/users/newbie/roles", `{"role":"team_admin","team":"etabA2"}`, 403},
		{"master", "PUT", "/admin/users/newbie/roles", `{"role":"member","team":"etabA"}`, 200},
		{"alice", "PUT", "/admin/users/newbie/roles", `{"role":"team_admin","team":"etabA2"}`, 200},
		{"alice", "DELETE", "/admin/users/newbie", "", 403},
		// no escalation
		{"alice", "PUT", "/admin/users/alice/roles", `{"role":"proxy_admin"}`, 403},
		{"alice", "PUT", "/admin/users/alice/roles", `{"role":"org_admin","org":"orgC"}`, 403},
		{"alice", "PUT", "/admin/users/alice/roles", `{"role":"team_admin","team":"etabC"}`, 403},
		// team admin: projects, keys, members of the team
		{"tom", "POST", "/admin/teams/etabA/projects", `{"name":"eval"}`, 201},
		{"tom", "POST", "/admin/teams/etabB/projects", `{"name":"eval"}`, 403},
		{"tom", "POST", "/admin/teams", `{"name":"y","org":"etabA"}`, 403},
		{"tom", "PUT", "/admin/budgets", `{"kind":"team","name":"etabA","period":"day","max_cost":10}`, 403},
		{"tom", "PUT", "/admin/budgets", `{"kind":"project","name":"etabA/eval","period":"day","max_cost":3}`, 200},
		{"tom", "POST", "/admin/keys", `{"team":"etabA","name":"service"}`, 201},
		// vic holds only an organisation role: not provisioned into a team (#104)
		{"tom", "PUT", "/admin/users/vic/roles", `{"role":"viewer","team":"etabA"}`, 403},
		{"tom", "PUT", "/admin/users/mia/roles", `{"role":"member","team":"etabA"}`, 409},
		{"tom", "PUT", "/admin/users/mia/roles", `{"role":"team_admin","team":"etabA"}`, 403},
		{"tom", "POST", "/admin/users", `{"name":"z"}`, 403},
		// member: own keys only
		{"mia", "POST", "/admin/keys", `{"team":"etabA","name":"mine"}`, 201},
		{"mia", "POST", "/admin/keys", `{"team":"etabB"}`, 403},
		{"mia", "POST", "/admin/teams/etabA/projects", `{"name":"p2"}`, 403},
		{"mia", "PUT", "/admin/users/mia/roles", `{"role":"team_admin","team":"etabA"}`, 403},
		// viewer: reads, no writes
		{"vic", "GET", "/admin/teams/etabA/projects", "", 200},
		{"vic", "POST", "/admin/keys", `{"team":"etabA"}`, 403},
		{"vic", "PUT", "/admin/budgets", `{"kind":"team","name":"etabA","period":"day","max_cost":1}`, 403},
		// other organisation
		{"eve", "GET", "/admin/teams/etabA/projects", "", 403},
		{"eve", "POST", "/admin/keys", `{"team":"etabA"}`, 403},
		{"eve", "POST", "/admin/teams", `{"name":"etabC2","org":"orgC"}`, 201},
		// proxy viewer: reads everything, writes nothing
		{"pat", "GET", "/admin/teams/etabB/projects", "", 200},
		{"pat", "POST", "/admin/teams/etabB/projects", `{"name":"q"}`, 403},
		{"pat", "POST", "/admin/orgs", `{"name":"orgE"}`, 403},
		// no role at all
		{"nobod", "POST", "/admin/keys", `{"team":"etabA"}`, 403},
		{"nobod", "GET", "/admin/teams/etabA/projects", "", 403},
		// unknown objects
		{"alice", "DELETE", "/admin/teams/nope", "", 404},
	})

	visible := func(who, path, want, notWant string) {
		t.Helper()
		out := readAll(t, e.do("GET", path, tok[who], ""))
		if want != "" && !strings.Contains(out, want) || notWant != "" && strings.Contains(out, notWant) {
			t.Errorf("%s GET %s: %s", who, path, out)
		}
	}
	visible("alice", "/admin/orgs", `"name":"etabA"`, `"name":"orgC"`)
	visible("eve", "/admin/teams", `"name":"etabC"`, `"name":"etabA"`)
	visible("pat", "/admin/orgs", `"name":"orgC"`, "")
	visible("mia", "/admin/keys?team=etabA", `"owner":"mia"`, `"name":"service"`)
	visible("vic", "/admin/keys?team=etabA", `"name":"service"`, "")
	visible("alice", "/admin/budgets", `"name":"etabA"`, "")
	visible("eve", "/admin/budgets", "", `"name":"etabA"`)
	visible("alice", "/admin/fairshare", `"org":"etabA"`, `"org":"etabB"`)
	visible("alice", "/admin/users", `"name":"tom"`, `"name":"eve"`)
	visible("mia", "/admin/whoami", `"name":"mia"`, "")

	// A member revokes their own key, not another member's or the team's.
	var keys []struct{ ID, Owner, Name string }
	if err := json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/keys?team=etabA", tok["tom"], ""))), &keys); err != nil {
		t.Fatal(err)
	}
	var mine, service string
	for _, k := range keys {
		switch k.Name {
		case "mine":
			mine = k.ID
		case "service":
			service = k.ID
		}
	}
	e.check(tok, []call{
		{"mia", "DELETE", "/admin/keys/" + service, "", 403},
		{"mia", "DELETE", "/admin/keys/" + mine, "", 204},
		{"eve", "DELETE", "/admin/keys/" + service, "", 403},
		{"tom", "DELETE", "/admin/keys/" + service, "", 204},
	})
}

func TestAdminTokens(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	mia := e.adminToken("mia", `{"role":"member","team":"etabA"}`)
	tok := map[string]string{"mia": mia, "bad": "llma-not-a-token", "notllma": "garbage"}
	e.check(tok, []call{
		{"mia", "POST", "/admin/users/mia/tokens", "", 201},
		{"mia", "GET", "/admin/users/mia/tokens", "", 200},
		{"mia", "GET", "/admin/users/master/tokens", "", 403},
		{"mia", "GET", "/admin/users/mia/roles", "", 200},
		{"bad", "GET", "/admin/whoami", "", 401},
		{"notllma", "GET", "/admin/whoami", "", 401},
		{"master", "GET", "/admin/whoami", "", 200},
		{"master", "PUT", "/admin/users/mia/roles", `{"role":"member"}`, 400},
		{"master", "PUT", "/admin/users/mia/roles", `{"role":"member","team":"nope"}`, 404},
		{"master", "PUT", "/admin/users/mia/roles", `{"role":"member","team":"etabA"}`, 409},
		{"master", "DELETE", "/admin/users/mia/roles?role=viewer&team=etabA", "", 404},
	})
	// Revoked token, then disabled user.
	var ts []struct{ ID string }
	if err := json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/users/mia/tokens", master, ""))), &ts); err != nil || len(ts) != 2 {
		t.Fatalf("tokens %v %v", ts, err)
	}
	second := e.adminToken("mia")
	e.check(map[string]string{"second": second}, []call{
		{"second", "GET", "/admin/whoami", "", 200},
		{"master", "DELETE", "/admin/users/mia/roles?role=member&team=etabA", "", 204},
		{"master", "DELETE", "/admin/users/mia/tokens/" + ts[0].ID, "", 204},
		{"master", "DELETE", "/admin/users/mia", "", 204},
		{"second", "GET", "/admin/whoami", "", 401},
		{"master", "POST", "/admin/users", `{"name":"a b"}`, 400},
		{"master", "DELETE", "/admin/users/nobody", "", 404},
	})
}
