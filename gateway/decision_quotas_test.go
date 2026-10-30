package gateway

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"
)

// A budget refuses a request with 429: the refusal is counted, flushed, and
// the decision report shows it (planted case), with the budget against actual.
func TestDecisionQuotasAndVarianceEndToEnd(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	key := e.createKey(`{"team":"etabA"}`)
	// 0.2 GPU-hours a month admits one request (see budgets_test.go).
	e.setBudget(`{"kind":"team","name":"etabA","period":"month","max_cost":0.2}`)
	expect(t, "first request", e.chatWith(key, chat), 200)
	e.waitRecords(1)
	for i := 0; i < 5; i++ { // five refusals: a cell of five is shown (default small-cell threshold)
		expect(t, "refused", e.chatWith(key, chat), 429)
	}
	e.srv.flushRefusals(context.Background())

	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	q := "?from=" + from.Format("2006-01-02") + "&to=" + from.AddDate(0, 1, 0).Format("2006-01-02")
	out := readAll(t, e.do("GET", "/admin/decision"+q+"&format=json", fin, ""))
	var got struct {
		Quotas struct {
			Refusals []RefusalOut
			Notes    []string
		}
		Variance  []VarianceRow
		Threshold float64 `json:"variance_threshold_pct"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	var n int64
	for _, r := range got.Quotas.Refusals {
		if r.Org == "etabA" && r.Kind == "budget" && r.Refusals != nil {
			n += *r.Refusals
		}
	}
	if n != 5 {
		t.Errorf("budget refusals shown for etabA: %d, want 5: %+v", n, got.Quotas.Refusals)
	}
	if len(got.Quotas.Notes) == 0 {
		t.Error("the quota section carries no notes")
	}
	if got.Threshold != 15 {
		t.Errorf("threshold %v", got.Threshold)
	}
	// The team budget of 0.2 GPU-hours a month, whole month: budgeted 0.200,
	// actual one request (0.154).
	var row *VarianceRow
	for i, r := range got.Variance {
		if r.HasBudget && r.Unit == "gpu_hours" {
			row = &got.Variance[i]
		}
	}
	if row == nil || row.BudgetedMinor != 200 || row.ActualMinor != 154 || row.BudgetVarMinor != -46 || !row.OverThreshold {
		t.Fatalf("variance row: %+v in %+v", row, got.Variance)
	}
	// Every format carries the sections.
	for _, f := range []string{"pdf", "xlsx"} {
		resp := e.do("GET", "/admin/decision"+q+"&format="+f, fin, "")
		if data := readAll(t, resp); resp.StatusCode != 200 || len(data) < 1000 {
			t.Fatalf("%s: %d, %d bytes", f, resp.StatusCode, len(data))
		}
	}
	// The report names no person.
	for _, who := range []string{"alice", "bob"} {
		if strings.Contains(out, who) {
			t.Errorf("a person's name in the JSON: %s", who)
		}
	}
}

// An organisation's admin sees only its own partner's quota and variance rows.
func TestDecisionQuotasVisibility(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	e.setBudget(`{"kind":"org","name":"etabA","period":"month","max_cost":10}`)
	e.setBudget(`{"kind":"org","name":"etabB","period":"month","max_cost":20}`)
	oa := e.adminToken("oadminA", `{"role":"org_admin","org":"etabA"}`)
	now := time.Now().UTC()
	from := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	q := "?from=" + from.Format("2006-01-02") + "&to=" + from.AddDate(0, 1, 0).Format("2006-01-02") + "&format=json"
	var got struct{ Variance []VarianceRow }
	if err := json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/decision"+q, oa, ""))), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Variance) != 1 || got.Variance[0].BudgetedMinor != 10000 {
		t.Fatalf("org admin A: %+v", got.Variance)
	}
	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	if err := json.Unmarshal([]byte(readAll(t, e.do("GET", "/admin/decision"+q, fin, ""))), &got); err != nil {
		t.Fatal(err)
	}
	var sum int64
	for _, r := range got.Variance {
		sum += r.BudgetedMinor
	}
	if sum != 30000 {
		t.Fatalf("finance sees %d, want 30000: %+v", sum, got.Variance)
	}
}

// zipText is the workbook's workbook.xml (the sheet names).
func zipText(t *testing.T, data []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range zr.File {
		if f.Name == "xl/workbook.xml" {
			rc, err := f.Open()
			if err != nil {
				t.Fatal(err)
			}
			defer rc.Close()
			b, _ := io.ReadAll(rc)
			return b
		}
	}
	t.Fatal("no xl/workbook.xml")
	return nil
}

// The views and the workbook carry the planted figures.
func TestDecisionQuotaViews(t *testing.T) {
	d := fixedDecision()
	var b bytes.Buffer
	if err := decisionTmpl.Execute(&b, d.view()); err != nil {
		t.Fatal(err)
	}
	html := b.String()
	for _, want := range []string{"Budget against actual", "Quotas and profiles", "(&#43;50.0 %)", "suppressed", "beyond 15 %", "Peak ratio"} {
		if !strings.Contains(html, want) {
			t.Errorf("no %q in the report", want)
		}
	}
	b.Reset()
	if err := writeDecisionXLSX(&b, d); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Variance", "Soft quotas", "Refusals", "Profiles", "Oversubscription"} {
		if !bytes.Contains(zipText(t, b.Bytes()), []byte(name)) {
			t.Errorf("no sheet %q", name)
		}
	}
	d.Quotas = nil // a store without the tables: no section, no failure
	b.Reset()
	if err := decisionTmpl.Execute(&b, d.view()); err != nil || strings.Contains(b.String(), "Quotas and profiles") {
		t.Fatalf("nil quotas: %v", err)
	}
	// The bounds.
	d = fixedDecision()
	d.Variance = make([]VarianceRow, MaxVarianceRows+1)
	if err := d.checkBounds(); err == nil {
		t.Error("too many variance rows accepted")
	}
	d = fixedDecision()
	d.Quotas.Oversub = make([]OversubRow, MaxDecisionOversub+1)
	if err := d.checkBounds(); err == nil {
		t.Error("too many oversubscription rows accepted")
	}
}
