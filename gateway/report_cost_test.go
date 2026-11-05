package gateway

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Regression tests for the security review of 2026-11-05, findings 4 and 6.

// A team member's token may not run a 300-year report: the period is bounded
// like the other reports', whoever asks.
func TestReportPeriodIsBounded(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	m := e.adminToken("m", `{"role":"member","team":"etabB"}`)
	for _, path := range []string{
		"/admin/spend?from=1900-01-01&to=2200-01-01&group_by=org,team,project,user,model,pool,day",
		"/admin/usage.csv?from=1900-01-01&to=2200-01-01",
	} {
		for _, who := range []string{m, master} {
			resp := e.do("GET", path, who, "")
			readAll(t, resp)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("a 300-year report: %s -> %d", path, resp.StatusCode)
			}
		}
	}
	for _, path := range []string{"/admin/spend?from=2026-01-01&to=2026-02-01", "/admin/usage.csv?from=2026-01-01&to=2026-02-01"} {
		resp := e.do("GET", path, m, "")
		readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("a month: %s -> %d", path, resp.StatusCode)
		}
	}
}

// A finance office reads money and energy, not people.
func TestFinanceViewerCannotGroupByPersonOrKey(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	pv := e.adminToken("pv", `{"role":"proxy_viewer"}`)
	for _, gb := range []string{"user", "key", "org,user", "team,key", "day,user,key"} {
		path := "/admin/spend?from=2026-01-01&to=2026-02-01&group_by=" + gb
		resp := e.do("GET", path, fin, "")
		readAll(t, resp)
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("finance_viewer group_by=%s: %d", gb, resp.StatusCode)
		}
		resp = e.do("GET", path, pv, "")
		readAll(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("proxy_viewer group_by=%s: %d", gb, resp.StatusCode)
		}
	}
	resp := e.do("GET", "/admin/spend?from=2026-01-01&to=2026-02-01&group_by=org,team,project,model,pool,day", fin, "")
	readAll(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("finance_viewer group_by money dimensions: %d", resp.StatusCode)
	}
}

// The decision report's per-partner count of active people is a small cell in
// the data too, not only in the pages.
func TestDecisionJSONSuppressesSmallPartnerCells(t *testing.T) {
	d := &decisionData{MinUsers: 5, Partners: []partner{{Org: "small", ActiveUsers: 3}, {Org: "large", ActiveUsers: 31}}}
	b, err := json.Marshal(d.json()["partners"])
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, `"ActiveUsers":"fewer than 5"`) || !strings.Contains(out, `"ActiveUsers":31`) || strings.Contains(out, `"ActiveUsers":3,`) {
		t.Errorf("%s", out)
	}
}
