package gateway

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// finance_viewer (#97): reports of every organisation, read-only, and
// nothing else; only a proxy admin grants it.
func TestFinanceViewer(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	now := time.Now().UTC()
	if err := e.st.InsertRecords(context.Background(), []store.Record{
		{Time: now, Org: "etabA", Team: "etabA", KeyID: "k", Model: "llama", Pool: "b300", Cost: 3, Status: 200},
		{Time: now, Org: "etabB", Team: "etabB", KeyID: "k", Model: "llama", Pool: "b300", Cost: 5, Status: 200},
	}); err != nil {
		t.Fatal(err)
	}
	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	oa := e.adminToken("oadminA", `{"role":"org_admin","org":"etabA"}`)
	span := "?from=" + now.AddDate(0, 0, -1).Format("2006-01-02") + "&to=" + now.AddDate(0, 0, 2).Format("2006-01-02")

	// Both organisations' spend; an organisation admin reads its own only.
	body := readAll(t, e.do("GET", "/admin/spend"+span+"&group_by=org", fin, ""))
	if !strings.Contains(body, "etabA") || !strings.Contains(body, "etabB") {
		t.Fatalf("finance_viewer spend: %s", body)
	}
	if body := readAll(t, e.do("GET", "/admin/spend"+span+"&group_by=org", oa, "")); strings.Contains(body, "etabB") {
		t.Fatalf("org admin reads another organisation: %s", body)
	}
	// Read-only, and no listing of people or objects.
	e.check(map[string]string{"fin": fin, "oa": oa}, []call{
		{"fin", "POST", "/admin/orgs", `{"name":"x"}`, 403},
		{"fin", "POST", "/admin/keys", `{"team":"etabA","name":"k"}`, 403},
		{"fin", "PUT", "/admin/budgets", `{"kind":"team","name":"etabA","period":"day","max_cost":1}`, 403},
		{"fin", "DELETE", "/admin/orgs/etabA", "", 403},
		{"fin", "PUT", "/admin/users/fin/roles", `{"role":"proxy_admin"}`, 403},
		{"fin", "GET", "/admin/audit", "", 403},
		// Only a proxy admin grants it: an organisation's admin may not, not
		// even to a person of its own organisation.
		{"oa", "PUT", "/admin/users/fin/roles", `{"role":"finance_viewer"}`, 403},
		{"oa", "DELETE", "/admin/users/fin/roles?role=finance_viewer", "", 403},
		{"master", "POST", "/admin/users", `{"name":"x2"}`, 201},
		{"oa", "PUT", "/admin/users/x2/roles", `{"role":"finance_viewer"}`, 403},
		{"master", "PUT", "/admin/users/x2/roles", `{"role":"finance_viewer"}`, 200},
		{"master", "PUT", "/admin/users/x2/roles", `{"role":"finance_viewer","org":"etabA"}`, 400},
	})
	for _, path := range []string{"/admin/keys", "/admin/teams", "/admin/orgs", "/admin/users"} {
		if body := readAll(t, e.do("GET", path, fin, "")); strings.Contains(body, "etabA") || strings.Contains(body, "oadminA") { // a listing of users names the caller, no one else
			t.Errorf("finance_viewer lists %s: %s", path, body)
		}
	}
}
