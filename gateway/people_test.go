package gateway

// A finance office reads money and energy, not people (rbac.go reportView).
// This test asks every GET admin route as a finance_viewer, with the query a
// reader would use to reach a person or a key, and fails if any answer carries
// the name of a person or the id of a key. The routes are the ones the server
// registered (adminFuncs), so a report route added later that forgets the guard
// (refusePeople, visibleReport) fails here without anyone listing it.

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/store"
)

func TestFinanceViewerSeesNoPersonOnAnyAdminRoute(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC).UnixNano())
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Fluids = &config.Fluids{PUE: 1, PricePerKWh: 1, Currency: "EUR", CurrencyDecimals: 2, IdleEnergy: energy.IdleByShare, WPrefill: 1, WDecode: 1}
	e := newEnv(t, cfg, WithClock(func() time.Time { return time.Unix(0, clock.Load()).UTC() }))
	ctx := context.Background()
	at := time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC)
	saveEnergy(t, e, at, kWhMJ, 0, "etabA")
	if err := e.st.InsertRecords(ctx, []store.Record{{ID: "r1", Time: at, Org: "etabA", Team: "etabA", User: "alice-secret-person", KeyID: "k-secret",
		Model: "llama", Pool: "b300", Cost: 2, Status: 200}}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SaveEnergy(ctx, store.EnergyInterval{Upstream: "n", Start: at.AddDate(0, 0, 2), End: at.AddDate(0, 0, 2).Add(time.Minute), TotalMJ: 100},
		[]energy.Attributed{{Consumer: energy.Consumer{Org: "etabA", Team: "etabA", User: "bob-secret-person"}, MJ: 100}}); err != nil {
		t.Fatal(err)
	}
	if resp := e.do("POST", "/admin/billing/close?month=2026-10", master, ""); resp.StatusCode != 200 {
		t.Fatalf("close: %d %s", resp.StatusCode, readAll(t, resp))
	}
	secrets := []string{"alice-secret-person", "bob-secret-person", "k-secret"}
	span := "from=2026-10-01&to=2026-11-01&month=2026-10"

	// Control: the proxy roles do see them, so the test is not vacuous.
	seen := readAll(t, e.do("GET", "/admin/export?format=csv&"+span, master, ""))
	if !strings.Contains(seen, "alice-secret-person") {
		t.Fatalf("control: the master's export has no person: %.200s", seen)
	}
	if !strings.Contains(readAll(t, e.do("GET", "/admin/billing/closed?month=2026-10", master, "")), "alice-secret-person") {
		t.Fatal("control: the master's closed snapshot has no person")
	}

	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	routes := 0
	for pattern := range e.srv.adminFuncs {
		method, path, _ := strings.Cut(pattern, " ")
		if method != "GET" {
			continue
		}
		path = strings.NewReplacer("{name}", "etabA", "{team}", "etabA", "{id}", "etabA").Replace(path)
		routes++
		for _, q := range []string{"group_by=user&" + span, "group_by=key&" + span, "group_by=org,user,key&format=csv&" + span, "format=csv&" + span, "format=xlsx&" + span, span} {
			resp := e.do("GET", path+"?"+q, fin, "")
			body := readAll(t, resp)
			for _, s := range secrets {
				if strings.Contains(body, s) {
					t.Errorf("finance_viewer: GET %s?%s (%d) carries %q: %.200s", path, q, resp.StatusCode, s, body)
				}
			}
		}
	}
	if routes < 20 {
		t.Fatalf("only %d GET admin routes were asked: the registry is not what this test thinks", routes)
	}
	// And the closed snapshot is still a snapshot, minus the people.
	snap := readAll(t, e.do("GET", "/admin/billing/closed?month=2026-10", fin, ""))
	if !strings.Contains(snap, `"usage"`) || strings.Contains(snap, "gateway_version\":\"(devel)") {
		t.Errorf("the redacted snapshot: %.300s", snap)
	}
}
