package gateway

// Security audit 2026-11-05 (untracked proof). finance_viewer may not group
// /admin/spend by user or key (report.go:68), but /admin/export (CSV default
// group_by includes user; XLSX/PDF bundle has a "user" sheet) has no such check
// (export_report.go:175-184, 73).

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

func TestSecauditExportShowsPeopleToFinance(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	now := time.Now().UTC()
	if err := e.st.InsertRecords(context.Background(), []store.Record{{ID: "r1", Time: now.Add(-time.Hour),
		Org: "etabA", Team: "etabA", User: "alice-secret-person", KeyID: "k-secret", Model: "llama", Pool: "b300", Cost: 2, Status: 200}}); err != nil {
		t.Fatal(err)
	}
	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	span := "from=" + now.AddDate(0, 0, -2).Format("2006-01-02") + "&to=" + now.AddDate(0, 0, 1).Format("2006-01-02")
	if resp := e.do("GET", "/admin/spend?group_by=user&"+span, fin, ""); resp.StatusCode != 403 {
		t.Fatalf("control: /admin/spend group_by=user for finance_viewer: %d", resp.StatusCode)
	}
	for _, q := range []string{"format=csv", "format=csv&group_by=key"} {
		resp := e.do("GET", "/admin/export?"+span+"&"+q, fin, "")
		body := readAll(t, resp)
		if resp.StatusCode == 200 && (strings.Contains(body, "alice-secret-person") || strings.Contains(body, "k-secret")) {
			t.Errorf("EXPLOIT: finance_viewer /admin/export?%s shows people or keys: %.300s", q, body)
		}
	}
}
