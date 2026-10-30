package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

// Security audit 2026-10-26: every admin route of the router, called by
// every kind of principal, on objects of organisation etabA. A route added
// to the router without a row here fails the test, so its authorisation is
// decided on purpose, not by omission.
//
// Principals: the master key; proxy_admin; proxy_viewer; org_admin of etabA;
// org_admin of another organisation (orgC, the IDOR attacker); team_admin,
// member, team viewer and org viewer of etabA; a user with no role; an
// inference key and a JWT-shaped token presented as admin bearers.

// matrixFixture is what every case starts from.
type matrixFixture struct {
	tok        map[string]string
	serviceKey string // etabA key without owner
	memberKey  string // etabA key owned by "member"
	targetTok  string // an admin token of user "target"
}

var matrixPrincipals = []string{"master", "padmin", "pview", "oadmin", "oadminC", "tadmin", "member", "tviewer", "oviewer", "nobody", "fviewer", "infkey", "jwtish"}

func matrixSetup(e *env) matrixFixture {
	t := e.t
	t.Helper()
	e.check(nil, []call{
		{"master", "POST", "/admin/orgs", `{"name":"orgC"}`, 201},
		{"master", "POST", "/admin/teams", `{"name":"etabC","org":"orgC"}`, 201},
		{"master", "POST", "/admin/teams/etabA/projects", `{"name":"p1"}`, 201},
		{"master", "PUT", "/admin/budgets", `{"kind":"team","name":"etabA","period":"day","max_cost":10}`, 200},
		{"master", "PUT", "/admin/allowlists", `{"kind":"team","name":"etabA","cidrs":["0.0.0.0/0","::/0"]}`, 200},
	})
	f := matrixFixture{tok: map[string]string{
		"padmin":  e.adminToken("padmin", `{"role":"proxy_admin"}`),
		"pview":   e.adminToken("pview", `{"role":"proxy_viewer"}`),
		"oadmin":  e.adminToken("oadmin", `{"role":"org_admin","org":"etabA"}`),
		"oadminC": e.adminToken("oadminC", `{"role":"org_admin","org":"orgC"}`),
		"tadmin":  e.adminToken("tadmin", `{"role":"team_admin","team":"etabA"}`),
		"member":  e.adminToken("member", `{"role":"member","team":"etabA"}`),
		"tviewer": e.adminToken("tviewer", `{"role":"viewer","team":"etabA"}`),
		"oviewer": e.adminToken("oviewer", `{"role":"viewer","org":"etabA"}`),
		"nobody":  e.adminToken("nobody"),
		"fviewer": e.adminToken("fviewer", `{"role":"finance_viewer"}`),
		"jwtish":  "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.c2ln",
	}}
	f.targetTok = e.adminToken("target", `{"role":"viewer","team":"etabA"}`)
	f.serviceKey, f.tok["infkey"] = e.newKey("etabA")
	resp := e.do("POST", "/admin/keys", f.tok["member"], `{"team":"etabA","name":"mine"}`)
	var mk struct{ ID string }
	if err := json.Unmarshal([]byte(readAll(t, resp)), &mk); err != nil || resp.StatusCode != 201 {
		t.Fatalf("member key: %d %v", resp.StatusCode, err)
	}
	f.memberKey = mk.ID
	// Spend of etabA, for the report routes' leak check.
	if err := e.st.InsertRecords(context.Background(), []store.Record{{Time: time.Now().UTC(), Org: "etabA", Team: "etabA",
		KeyID: f.serviceKey, Model: "llama", Pool: "b300", PromptTokens: 10, Cost: 1, Status: 200}}); err != nil {
		t.Fatal(err)
	}
	// Measured energy of etabA, for the fluids route's leak check.
	if err := e.st.SaveEnergy(context.Background(), store.EnergyInterval{Upstream: "n", Start: time.Now().UTC().Add(-time.Hour), End: time.Now().UTC().Add(-59 * time.Minute), TotalMJ: 100},
		[]energy.Attributed{{Consumer: energy.Consumer{Org: "etabA", Team: "etabA", User: "member"}, MJ: 100}}); err != nil {
		t.Fatal(err)
	}
	// A grant of a replica that is gone, on etabA's team budget.
	now := time.Now()
	if _, err := e.st.GrowGrants(context.Background(), store.Holder{Replica: "ghost", Incarnation: "g1"}, now, time.Minute,
		[]store.GrantRequest{{Key: store.BudgetKey{Kind: store.ScopeTeam, Name: "etabA"}, Start: store.Day.Start(now), Need: 1, Want: 1}}); err != nil {
		t.Fatal(err)
	}
	return f
}

type matrixRow struct {
	pattern string // the router's pattern
	method  string
	path    func(f matrixFixture) string
	body    string
	// allowed get a 2xx; the others 403. Empty: a listing everyone may
	// call, filtered (see leak).
	allowed []string
	// leak, for a listing: the principals that must not see etabA's
	// objects in the answer.
	leak []string
}

func p(s string) func(matrixFixture) string { return func(matrixFixture) string { return s } }

var (
	writeAll   = []string{"master", "padmin"}
	orgAdmins  = []string{"master", "padmin", "oadmin"}
	teamAdmins = []string{"master", "padmin", "oadmin", "tadmin"}
	teamViews  = []string{"master", "padmin", "pview", "oadmin", "tadmin", "member", "tviewer", "oviewer"}
	proxyViews = []string{"master", "padmin", "pview"}
	// outsiders must not see etabA's objects in a listing. finance_viewer
	// reads reports of every organisation and nothing else: a listing of
	// teams, keys or users is not a report.
	outsiders = []string{"oadminC", "nobody", "fviewer"}
	// reportOutsiders are the outsiders of a report: finance_viewer is
	// meant to read etabA's spend there.
	reportOutsiders = []string{"oadminC", "nobody"}
)

func matrixRows() []matrixRow {
	day := time.Now().UTC()
	span := "?from=" + day.AddDate(0, 0, -1).Format("2006-01-02") + "&to=" + day.AddDate(0, 0, 2).Format("2006-01-02")
	return []matrixRow{
		{"GET /admin/whoami", "GET", p("/admin/whoami"), "", nil, nil},
		{"GET /admin/allowlists", "GET", p("/admin/allowlists"), "", nil, outsiders},
		{"PUT /admin/allowlists", "PUT", p("/admin/allowlists"), `{"kind":"team","name":"etabA","cidrs":["10.0.0.0/8"]}`, teamAdmins, nil},
		{"GET /admin/orgs", "GET", p("/admin/orgs"), "", nil, outsiders},
		{"POST /admin/orgs", "POST", p("/admin/orgs"), `{"name":"orgZ"}`, writeAll, nil},
		{"DELETE /admin/orgs/{name}", "DELETE", p("/admin/orgs/etabA"), "", writeAll, nil},
		{"GET /admin/teams", "GET", p("/admin/teams"), "", nil, outsiders},
		{"POST /admin/teams", "POST", p("/admin/teams"), `{"name":"etabA9","org":"etabA"}`, orgAdmins, nil},
		{"DELETE /admin/teams/{name}", "DELETE", p("/admin/teams/etabA"), "", orgAdmins, nil},
		{"GET /admin/teams/{team}/models", "GET", p("/admin/teams/etabA/models"), "", teamViews, nil},
		{"PUT /admin/teams/{team}/models", "PUT", p("/admin/teams/etabA/models"), `{"models":["llama"]}`, teamAdmins, nil},
		{"GET /admin/teams/{team}/tools", "GET", p("/admin/teams/etabA/tools"), "", teamViews, nil},
		{"PUT /admin/teams/{team}/tools", "PUT", p("/admin/teams/etabA/tools"), `{"tools":[]}`, orgAdmins, nil},
		{"GET /admin/teams/{team}/projects", "GET", p("/admin/teams/etabA/projects"), "", teamViews, nil},
		{"POST /admin/teams/{team}/projects", "POST", p("/admin/teams/etabA/projects"), `{"name":"p9"}`, teamAdmins, nil},
		{"DELETE /admin/teams/{team}/projects/{name}", "DELETE", p("/admin/teams/etabA/projects/p1"), "", teamAdmins, nil},
		{"GET /admin/keys", "GET", p("/admin/keys"), "", nil, outsiders},
		{"POST /admin/keys", "POST", p("/admin/keys"), `{"team":"etabA","name":"new"}`, append(teamAdmins, "member"), nil},
		{"DELETE /admin/keys/{id}", "DELETE", func(f matrixFixture) string { return "/admin/keys/" + f.serviceKey }, "", teamAdmins, nil},
		{"POST /admin/keys/{id}/rotate", "POST", func(f matrixFixture) string { return "/admin/keys/" + f.serviceKey + "/rotate" }, "", teamAdmins, nil},
		{"PUT /admin/keys/{id}/rotation", "PUT", func(f matrixFixture) string { return "/admin/keys/" + f.serviceKey + "/rotation" }, `{}`, teamAdmins, nil},
		{"GET /admin/users", "GET", p("/admin/users"), "", nil, nil},
		{"POST /admin/users", "POST", p("/admin/users"), `{"name":"newbie"}`, append(orgAdmins, "oadminC"), nil},
		{"DELETE /admin/users/{name}", "DELETE", p("/admin/users/target"), "", writeAll, nil},
		{"GET /admin/users/{name}/tokens", "GET", p("/admin/users/target/tokens"), "", writeAll, nil},
		{"POST /admin/users/{name}/tokens", "POST", p("/admin/users/target/tokens"), "", writeAll, nil},
		{"DELETE /admin/users/{name}/tokens/{id}", "DELETE", nil, "", writeAll, nil},
		{"GET /admin/users/{name}/roles", "GET", p("/admin/users/target/roles"), "", nil, outsiders},
		{"PUT /admin/users/{name}/roles", "PUT", p("/admin/users/target/roles"), `{"role":"member","team":"etabA"}`, teamAdmins, nil},
		{"DELETE /admin/users/{name}/roles", "DELETE", p("/admin/users/target/roles?role=viewer&team=etabA"), "", teamAdmins, nil},
		{"GET /admin/budgets", "GET", p("/admin/budgets"), "", nil, outsiders},
		{"PUT /admin/budgets", "PUT", p("/admin/budgets"), `{"kind":"team","name":"etabA","period":"day","max_cost":20}`, orgAdmins, nil},
		{"POST /admin/budgets/increase", "POST", p("/admin/budgets/increase"),
			`{"kind":"team","name":"etabA","amount":1,"expires_at":"` + day.Add(time.Hour).Format(time.RFC3339) + `"}`, orgAdmins, nil},
		{"DELETE /admin/budgets", "DELETE", p("/admin/budgets?kind=team&name=etabA"), "", orgAdmins, nil},
		// Hard budgets (#87): releasing a gone replica's grants, the proxy's
		// and the budget's own organisation admins only.
		{"POST /admin/budgets/release-replica", "POST", p("/admin/budgets/release-replica"), `{"replica":"ghost"}`, orgAdmins, nil},
		{"GET /admin/spend", "GET", p("/admin/spend" + span), "", nil, reportOutsiders},
		// Fluids (#97): the proxy roles and the organisation-level roles; a
		// team-level role has no energy view, and another organisation's admin
		// reads nothing of etabA's.
		{"GET /admin/fluids", "GET", p("/admin/fluids" + span), "", []string{"master", "padmin", "pview", "oadmin", "oadminC", "oviewer", "fviewer"}, []string{"oadminC"}},
		// Lending (#97): the same audience as fluids; only the proxy roles
		// read the matrix across organisations.
		{"GET /admin/lending", "GET", p("/admin/lending" + span), "", []string{"master", "padmin", "pview", "oadmin", "oadminC", "oviewer", "fviewer"}, []string{"oadminC"}},
		{"GET /admin/chargeback", "GET", p("/admin/chargeback" + span), "", []string{"master", "padmin", "pview", "oadmin", "oadminC", "oviewer", "fviewer"}, []string{"oadminC"}},
		{"GET /admin/adoption", "GET", p("/admin/adoption" + span), "", []string{"master", "padmin", "pview", "oadmin", "oadminC", "oviewer", "fviewer"}, []string{"oadminC"}},
		{"GET /admin/decision", "GET", p("/admin/decision" + span + "&format=json"), "", []string{"master", "padmin", "pview", "oadmin", "oadminC", "oviewer", "fviewer"}, []string{"oadminC"}},
		{"GET /admin/usage.csv", "GET", p("/admin/usage.csv" + span), "", nil, reportOutsiders},
		{"GET /admin/export", "GET", p("/admin/export" + span + "&format=csv"), "", nil, reportOutsiders},
		{"POST /admin/export", "POST", p("/admin/export" + span + "&format=csv"), "", nil, reportOutsiders},
		{"GET /admin/fairshare", "GET", p("/admin/fairshare"), "", nil, outsiders},
		{"GET /admin/audit", "GET", p("/admin/audit"), "", proxyViews, nil},
		{"POST /admin/audit/verify", "POST", p("/admin/audit/verify"), "", proxyViews, nil},
		// Workload profiles (#90). The people and capacity listings are
		// filtered to the caller's scope; calibration reads everyone's usage
		// (proxy roles); an assignment is decided by the person's bindings
		// (the person here is etabA's alone: its admins and the proxy's).
		{"GET /admin/profiles", "GET", p("/admin/profiles"), "", nil, nil},
		{"GET /admin/profiles/people", "GET", p("/admin/profiles/people"), "", nil, outsiders},
		{"GET /admin/profiles/capacity", "GET", p("/admin/profiles/capacity"), "", nil, outsiders},
		{"GET /admin/profiles/calibrate", "GET", p("/admin/profiles/calibrate"), "", proxyViews, nil},
		{"POST /admin/profiles/calibrate", "POST", p("/admin/profiles/calibrate"), `{"unit":"gpu_hours","amount":2}`, writeAll, nil},
		{"GET /admin/users/{name}/profile", "GET", p("/admin/users/target/profile"), "",
			[]string{"master", "padmin", "pview", "oadmin", "tadmin", "tviewer", "oviewer"}, nil},
		{"PUT /admin/users/{name}/profile", "PUT", p("/admin/users/target/profile"), `{"profile":"p"}`, teamAdmins, nil},
		{"DELETE /admin/users/{name}/profile", "DELETE", p("/admin/users/target/profile"), "", teamAdmins, nil},
		// Soft quotas (#96): the listings are open to every administrator and
		// filtered (an organisation's thresholds only to those who view it; the
		// people above a threshold only among those the caller may read); the
		// gateway default and the profiles' thresholds are the proxy admins',
		// an organisation's its own admins' (never another organisation's:
		// oadminC gets 403 on etabA).
		{"GET /admin/soft-quotas", "GET", p("/admin/soft-quotas"), "", nil, outsiders},
		{"GET /admin/soft-quotas/standing", "GET", p("/admin/soft-quotas/standing"), "", nil, outsiders},
		{"PUT /admin/soft-quotas/gateway", "PUT", p("/admin/soft-quotas/gateway"), `{"warn_at":[70,90]}`, writeAll, nil},
		{"DELETE /admin/soft-quotas/gateway", "DELETE", p("/admin/soft-quotas/gateway"), "", writeAll, nil},
		{"PUT /admin/soft-quotas/orgs/{name}", "PUT", p("/admin/soft-quotas/orgs/etabA"), `{"warn_at":[80,95]}`, orgAdmins, nil},
		{"DELETE /admin/soft-quotas/orgs/{name}", "DELETE", p("/admin/soft-quotas/orgs/etabA"), "", orgAdmins, nil},
		{"PUT /admin/soft-quotas/profiles/{name}", "PUT", p("/admin/soft-quotas/profiles/p"), `{"warn_at":[50]}`, writeAll, nil},
		{"DELETE /admin/soft-quotas/profiles/{name}", "DELETE", p("/admin/soft-quotas/profiles/p"), "", writeAll, nil},
	}
}

func TestAuthzMatrix(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := profileConfig(up.URL()) // the profile routes answer 404 without profile blocks
	cfg.Fluids = &config.Fluids{PUE: 1, IdleEnergy: "by_share", WPrefill: 1, WDecode: 1}
	rows := matrixRows()

	// Every route of the router has a row, and every row a route.
	probe := newEnv(t, cfg)
	var registered, listed []string
	for pat := range probe.srv.adminFuncs {
		registered = append(registered, pat)
	}
	for _, r := range rows {
		listed = append(listed, r.pattern)
	}
	sort.Strings(registered)
	sort.Strings(listed)
	if strings.Join(registered, "\n") != strings.Join(listed, "\n") {
		t.Fatalf("the matrix and the router differ:\nrouter:\n%s\n\nmatrix:\n%s", strings.Join(registered, "\n"), strings.Join(listed, "\n"))
	}

	for _, row := range rows {
		t.Run(row.pattern, func(t *testing.T) {
			for _, who := range matrixPrincipals {
				// A fresh gateway per call: a write must not change what the
				// next principal is judged on.
				e := newEnv(t, cfg)
				f := matrixSetup(e)
				path := ""
				if row.path != nil {
					path = row.path(f)
				} else {
					// The target's first admin token.
					var ts []struct{ ID string }
					_ = json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/users/target/tokens", master, ""))), &ts)
					if len(ts) == 0 {
						t.Fatal("no token for target")
					}
					path = "/admin/users/target/tokens/" + ts[0].ID
				}
				tok := f.tok[who]
				if who == "master" {
					tok = master
				}
				resp := e.do(row.method, path, tok, row.body)
				out := readAll(t, resp)
				want := "2xx"
				switch {
				case who == "infkey" || who == "jwtish":
					want = "401"
				case row.allowed == nil:
				case !contains(row.allowed, who):
					want = "403"
				}
				got := resp.StatusCode
				ok := want == "2xx" && got >= 200 && got < 300 || want == fmt.Sprint(got)
				if !ok {
					t.Errorf("%-8s %s %s: %d, want %s (%s)", who, row.method, path, got, want, strings.TrimSpace(out))
					continue
				}
				if contains(row.leak, who) && got == http.StatusOK && leaksEtabA(out, f) {
					t.Errorf("%-8s %s %s leaks etabA: %s", who, row.method, path, out)
				}
			}
		})
	}
}

// Security audit 2026-10-26, finding A3: POST /admin/users is open to every
// organisation admin, and creating a user that exists but is disabled
// re-enabled it. Disabling a user is a proxy-admin decision, and it keeps
// the user's role bindings, so any organisation's admin could bring back a
// disabled proxy_admin account with every role it had.
func TestOrgAdminCannotReenableADisabledUser(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	e.adminToken("boss", `{"role":"proxy_admin"}`)
	alice := e.adminToken("alice", `{"role":"org_admin","org":"etabA"}`)
	e.check(map[string]string{"alice": alice}, []call{
		{"master", "DELETE", "/admin/users/boss", "", 204},
		{"alice", "POST", "/admin/users", `{"name":"boss"}`, 403},
	})
	if u, err := e.st.GetUser(context.Background(), "boss"); err != nil || !u.Disabled {
		t.Fatalf("boss: %+v %v, want still disabled", u, err)
	}
	// A proxy administrator may re-enable (the documented way back).
	e.check(nil, []call{{"master", "POST", "/admin/users", `{"name":"boss"}`, 201}})
	if u, _ := e.st.GetUser(context.Background(), "boss"); u.Disabled {
		t.Fatal("proxy admin could not re-enable")
	}
}

// leaksEtabA reports whether an answer names an object of etabA.
func leaksEtabA(out string, f matrixFixture) bool {
	return strings.Contains(out, "etabA") || strings.Contains(out, f.serviceKey) || strings.Contains(out, f.memberKey) ||
		strings.Contains(out, `"tadmin"`) || strings.Contains(out, `"member"`)
}
