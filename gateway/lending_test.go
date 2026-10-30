package gateway

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// etabA (share 0.75 of C=4: 3 slots) works one request for an hour; etabB
// (1 slot) borrows two slots during that hour on top of its own: etabA has
// two idle slots and lends them.
func TestLendingReport(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	t0 := time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC)
	hour := float64(time.Hour.Milliseconds())
	rec := func(org string, borrow bool) store.Record {
		return store.Record{Time: t0.Add(time.Hour), Org: org, Team: org, KeyID: "k", Model: "llama", Pool: "b300", Status: 200,
			Cost: 1, LatencyMS: hour, Borrowing: borrow, PromptTokens: 1}
	}
	if err := e.st.InsertRecords(context.Background(), []store.Record{rec("etabA", false), rec("etabB", false), rec("etabB", true), rec("etabB", true)}); err != nil {
		t.Fatal(err)
	}
	oa := e.adminToken("oadminA", `{"role":"org_admin","org":"etabA"}`)
	tm := e.adminToken("tmember", `{"role":"member","team":"etabA"}`)
	get := func(tok, q string) (int, struct {
		Orgs   []LendingRow  `json:"orgs"`
		Matrix []LendingCell `json:"matrix"`
		Unattr float64       `json:"unattributed_gpu_hours"`
	}) {
		resp := e.do("GET", "/admin/lending?from=2026-10-01&to=2026-11-01"+q, tok, "")
		body := readAll(t, resp)
		var a struct {
			Orgs   []LendingRow  `json:"orgs"`
			Matrix []LendingCell `json:"matrix"`
			Unattr float64       `json:"unattributed_gpu_hours"`
		}
		if resp.StatusCode == 200 {
			if err := json.Unmarshal([]byte(body), &a); err != nil {
				t.Fatalf("%v: %s", err, body)
			}
		}
		return resp.StatusCode, a
	}
	code, a := get(master, "")
	if code != 200 || len(a.Orgs) != 2 || len(a.Matrix) != 1 {
		t.Fatalf("%d %+v", code, a)
	}
	// 31 days × 24 h × 3 reserved slots.
	if A := a.Orgs[0]; A.Org != "etabA" || !near(A.GuaranteedHours, 31*24*3) || !near(A.Lent, 2) || !near(A.Net, 2) || !near(A.UsedInShare, 1) {
		t.Fatalf("etabA %+v", A)
	}
	if B := a.Orgs[1]; !near(B.Borrowed, 2) || !near(B.BorrowedFromOthers, 2) || !near(B.Net, -2) || B.Borrowing.N != 2 {
		t.Fatalf("etabB %+v", B)
	}
	if c := a.Matrix[0]; c.Lender != "etabA" || c.Borrower != "etabB" || !near(c.GPUHours, 2) {
		t.Fatalf("matrix %+v", a.Matrix)
	}
	// An organisation reads its own row and no matrix.
	code, a = get(oa, "")
	if code != 200 || len(a.Orgs) != 1 || a.Orgs[0].Org != "etabA" || len(a.Matrix) != 0 || a.Unattr != 0 {
		t.Fatalf("org admin: %d %+v", code, a)
	}
	if code, _ = get(tm, ""); code != 403 {
		t.Fatalf("member: %d", code)
	}
	if code, _ = get(master, "&format=pdf"); code != 400 {
		t.Errorf("format: %d, want 400", code)
	}
	for _, q := range []string{"from=2026-11-01&to=2026-10-01", "from=2020-01-01&to=2026-10-01"} {
		if resp := e.do("GET", "/admin/lending?"+q, master, ""); resp.StatusCode != 400 {
			t.Errorf("%s: %d, want 400", q, resp.StatusCode)
		}
	}
	resp := e.do("GET", "/admin/lending?from=2026-10-01&to=2026-11-01&format=csv", master, "")
	if csv := readAll(t, resp); len(csv) == 0 || csv[:10] != "from,to,or" {
		t.Fatalf("csv %q", csv)
	}
}
