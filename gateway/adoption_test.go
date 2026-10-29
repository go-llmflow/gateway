package gateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Adoption is counts: no person's name, no key, in any format or for any
// reader; an organisation reads its own, finance_viewer all.
func TestAdoptionReport(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	at := time.Date(2026, 10, 14, 10, 0, 0, 0, time.UTC)
	rec := func(org, user string) store.Record {
		return store.Record{Time: at, Org: org, Team: org, User: user, KeyID: "secret-key-id", Model: "llama", Pool: "b300", Status: 200}
	}
	if err := e.st.InsertRecords(context.Background(), []store.Record{rec("etabA", "alice"), rec("etabA", "alice"), rec("etabA", "bob"), rec("etabB", "carol")}); err != nil {
		t.Fatal(err)
	}
	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	oa := e.adminToken("oadminA", `{"role":"org_admin","org":"etabA"}`)
	tm := e.adminToken("tmember", `{"role":"member","team":"etabA"}`)
	q := "?from=2026-10-01&to=2026-11-01"
	type ans struct {
		Active int64 `json:"active_users"`
		Daily  []store.AdoptionPoint
		Models []store.AdoptionModel
	}
	read := func(tok, extra string) (int, string, ans) {
		resp := e.do("GET", "/admin/adoption"+q+extra, tok, "")
		body := readAll(t, resp)
		var a ans
		if resp.StatusCode == 200 && extra == "" {
			if err := json.Unmarshal([]byte(body), &a); err != nil {
				t.Fatal(err)
			}
		}
		return resp.StatusCode, body, a
	}
	code, body, a := read(fin, "")
	if code != 200 || a.Active != 3 || len(a.Daily) != 1 || a.Daily[0].Requests != 4 || a.Models[0].Users != 3 {
		t.Fatalf("finance: %d %s", code, body)
	}
	code, body, a = read(oa, "")
	if code != 200 || a.Active != 2 || a.Daily[0].Requests != 3 {
		t.Fatalf("org admin sees %s", body)
	}
	if code, _, _ = read(tm, ""); code != 403 {
		t.Fatalf("member: %d", code)
	}
	for _, extra := range []string{"", "&format=csv"} {
		_, body, _ = read(master, extra)
		for _, secret := range []string{"alice", "bob", "carol", "secret-key-id"} {
			if strings.Contains(body, secret) {
				t.Errorf("adoption%s names %q: %s", extra, secret, body)
			}
		}
	}
}
