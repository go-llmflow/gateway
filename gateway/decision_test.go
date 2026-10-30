package gateway

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/energy"
	"github.com/go-llmflow/gateway/lending"
	"github.com/go-llmflow/gateway/store"
)

// fixedDecision is a report with every section filled, from numbers chosen to
// be read by hand.
func fixedDecision() *decisionData {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return &decisionData{
		From: t0, To: t0.AddDate(0, 1, 0), PrevFrom: t0.AddDate(0, -1, 0), Generated: time.Date(2026, 11, 2, 6, 0, 0, 0, time.UTC),
		Partners: []partner{
			{Org: "etabA", CostCenter: "GL-A", Share: 0.75, GPUHours: 120, PrevGPUHours: 100, Requests: 4000, Guaranteed: 2232, UsedInShare: 100, Borrowed: 20,
				BorrowedFromOthers: 18, SelfServed: 2, Lent: 5, Net: -13, Utilisation: 100.0 / 2232, KWh: 40, KWhIdle: 10, FluidsCost: 10.08,
				InShare: lending.Latency{N: 3800, P50: 820, P95: 4100, P99: 9000}, Borrowing: lending.Latency{N: 200, P50: 1500, P95: 8000, P99: 12000}, ActiveUsers: 31},
			{Org: "etabB", Share: 0.25, GPUHours: 30, Requests: 900, Guaranteed: 744, UsedInShare: 30, Lent: 18, Net: 18, Utilisation: 30.0 / 744,
				KWh: 12, KWhIdle: 8, FluidsCost: 3.02, InShare: lending.Latency{N: 900, P50: 700, P95: 3000, P99: 5000},
				WaitInShare: lending.Latency{N: 900, P50: 12, P95: 80, P99: 200}, ActiveUsers: 3}, // a small cell: shown as "fewer than 5"
		},
		Matrix:       []LendingCell{{"etabB", "etabA", 18}},
		Unattributed: 0.5,
		Currency:     "EUR", CurrencyDecimals: 2, MinUsers: 5, HasFluids: true,
		Adoption: &store.Adoption{Users: 38,
			Daily:  []store.AdoptionPoint{{Day: "2026-10-01", ActiveUsers: 12, Requests: 300}},
			Weekly: []store.AdoptionPoint{{Day: "2026-09-28", ActiveUsers: 20, Requests: 1200}, {Day: "2026-10-05", ActiveUsers: 33, Requests: 2000}, {Day: "2026-10-12", ActiveUsers: 3, Requests: 40}},
			Models: []store.AdoptionModel{{Model: "llama", Users: 38, Requests: 4900}}},
		Chargeback: []ChargebackRow{{From: t0, To: t0.AddDate(0, 1, 0), CostCenter: "GL-A", GPUHours: 120, KWh: 40, FluidsCostMinor: 1008, FluidsCost: "10.08", FluidsCurrency: "EUR", ProviderCost: "0.00"},
			{From: t0, To: t0.AddDate(0, 1, 0), CostCenter: CostCenterUnmapped, GPUHours: 30, KWh: 12, FluidsCostMinor: 302, FluidsCost: "3.02", FluidsCurrency: "EUR", ProviderCost: "0.00"}},
		HasRecon: true, ReconNote: "Slurm file: 12 jobs, jobs from 2026-09-30 12:00 to 2026-11-02 08:00 UTC; 11 used (2 only partly inside the period, prorated), 1 outside the period, 0 still running; 14 job steps, 0 without energy and 0 never started left out.",
		EnergyRecon: reconcileEnergy(map[string]float64{"etabA": 3.6e6 * 36, "etabB": 3.6e6 * 20}, // Slurm: 36 and 20 kWh
			map[string]float64{"etabA": 40, "etabB": 12}), // DCGM: 40 (+11.1 %) and 12 (-40 %)
		VarianceLimit: 15,
		Variance: buildVariance(varianceInputs{
			Months:     octoberSpans(),
			Budgets:    []varianceLine{{CostCenter: "GL-A", Amount: 80}, {CostCenter: "GL-A", Unit: "EUR", Amount: 20}},
			Allowances: []varianceLine{{CostCenter: "GL-A", Amount: 14, PeriodDays: 7}},
			Actual:     map[varianceKey]float64{{"2026-10", "GL-A", ""}: 120, {"2026-10", "GL-A", "EUR"}: 18.5, {"2026-10", CostCenterUnmapped, ""}: 30},
			Threshold:  15,
		}),
		Quotas: &QuotaReport{
			MinCell: 5,
			Soft: []SoftRow{{Org: "etabA", Lab: "lab1", Window: "week", Threshold: 80, Events: i64(9), People: i64(7)},
				{Org: "etabB", Window: "session", Threshold: 100, Suppressed: true}},
			Refusals: []RefusalOut{{Org: "etabA", Lab: "lab1", Profile: "std", Kind: "budget", Refusals: i64(12)},
				{Org: "etabB", Kind: "profile_week", Suppressed: true}},
			Profiles: []ProfileRow{{Org: "etabA", Lab: "lab1", Profile: "std", People: i64(7), AssignedDays: f64(31), UsedDays: f64(22), Utilisation: f64(22.0 / 31)},
				{Org: "etabB", Profile: "std", Suppressed: true}},
			Oversub: []OversubRow{{Org: "etabA", Day: "2026-10-01", People: i64(7), AssignedDaysPerWeek: f64(35), CapacityDaysPerWeek: f64(28), Ratio: f64(1.25)},
				{Org: "etabA", Day: "2026-10-02", People: i64(7), AssignedDaysPerWeek: f64(21), CapacityDaysPerWeek: f64(28), Ratio: f64(0.75)},
				{Org: "etabB", Day: "2026-10-01", Suppressed: true}},
			Notes: []string{"Cells over fewer than 5 people are not shown."},
		},
		Daily: []chartPoint{{"2026-10-01", 4}, {"2026-10-02", 6}, {"2026-10-03", 5}},
		Pie:   []chartSlice{{"etabA", 120}, {"etabB", 30}},
		Bars:  []chartBar{{"etabA", 120, 100}, {"etabB", 30, 0}},
	}
}

// The report's HTML is a golden file (go test ./gateway -run DecisionHTML
// -update-golden rewrites it): a change of layout or wording is a reviewed
// diff, not a surprise.
func TestDecisionHTMLGolden(t *testing.T) {
	var b bytes.Buffer
	if err := decisionTmpl.Execute(&b, fixedDecision().view()); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join("testdata", "decision.golden.html")
	if os.Getenv("UPDATE_GOLDEN") != "" {
		if err := os.WriteFile(path, b.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (UPDATE_GOLDEN=1 writes it)", err)
	}
	if !bytes.Equal(b.Bytes(), want) {
		t.Fatalf("the HTML differs from %s (UPDATE_GOLDEN=1 rewrites it); got %d bytes, want %d", path, b.Len(), len(want))
	}
	// Hostile names are text, never markup.
	d := fixedDecision()
	d.Partners[0].Org = `<script>alert(1)</script>`
	d.Partners[0].CostCenter = `"><img src=x>`
	b.Reset()
	if err := decisionTmpl.Execute(&b, d.view()); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), "<script>") || strings.Contains(b.String(), `"><img`) {
		t.Fatal("a name reached the page as markup")
	}
}

func TestDecisionPDF(t *testing.T) {
	var b bytes.Buffer
	if err := writeDecisionPDF(&b, fixedDecision()); err != nil {
		t.Fatal(err)
	}
	out := b.Bytes()
	if !bytes.HasPrefix(out, []byte("%PDF-")) || !bytes.Contains(out[max(0, len(out)-64):], []byte("%%EOF")) {
		t.Fatalf("not a PDF: %q ... %q", out[:min(8, len(out))], out[max(0, len(out)-16):])
	}
	// No action, script, URI link or embedded file (the contents' links are
	// internal destinations).
	pdfSafe(t, out)
	// More partners, more pages: the page-per-partner layout holds (the PDF
	// keeps its page tree in compressed object streams, so the pages are
	// compared by size rather than counted).
	d := fixedDecision()
	d.Partners = d.Partners[:1]
	var one bytes.Buffer
	if err := writeDecisionPDF(&one, d); err != nil {
		t.Fatal(err)
	}
	if one.Len() >= len(out) {
		t.Fatalf("one partner %d bytes, two %d", one.Len(), len(out))
	}
	if len(out) > 5<<20 {
		t.Fatalf("a 2-partner report is %d bytes", len(out))
	}
}

func TestDecisionXLSX(t *testing.T) {
	var b bytes.Buffer
	if err := writeDecisionXLSX(&b, fixedDecision()); err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(b.Bytes()), int64(b.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var wbxml, sheets string
	for _, f := range zr.File {
		rc, _ := f.Open()
		data, _ := io.ReadAll(rc)
		rc.Close()
		if f.Name == "xl/workbook.xml" {
			wbxml = string(data)
		}
		if strings.HasPrefix(f.Name, "xl/worksheets/") || f.Name == "xl/sharedStrings.xml" {
			sheets += string(data)
		}
	}
	for _, name := range []string{"Partners", "Lending matrix", "Chargeback", "Adoption", "Energy reconciliation"} {
		if !strings.Contains(wbxml, name) {
			t.Errorf("no sheet %q in %s", name, wbxml)
		}
	}
	for _, want := range []string{"etabA", "etabB", "GL-A", "Fluids cost (EUR)", "Guaranteed (GPU-hours)"} {
		if !strings.Contains(sheets, want) {
			t.Errorf("no %q in the sheets", want)
		}
	}
}

func TestDecisionPeriod(t *testing.T) {
	d := func(day int) time.Time { return time.Date(2026, 11, day, 12, 0, 0, 0, time.UTC) }
	if _, _, ok := decisionPeriod(d(1), 2); ok {
		t.Fatal("due before its day")
	}
	from, to, ok := decisionPeriod(d(2), 2)
	if !ok || !from.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) || !to.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("%v %v %v", from, to, ok)
	}
	from, _, _ = decisionPeriod(time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC), 2)
	if from.Format("2006-01") != "2026-12" {
		t.Fatalf("january reports december: %v", from)
	}
	if decisionKey(from, "pdf") != "decision/2026-12/decision-2026-12.pdf" {
		t.Fatal(decisionKey(from, "pdf"))
	}
}

// The scheduled delivery: the previous month's report lands in the export
// once, in both formats, and is audited; a second round does nothing more.
func TestDecisionScheduledDelivery(t *testing.T) {
	up := newFakeVLLM(t)
	dir := t.TempDir()
	cfg := testConfig(up.URL())
	cfg.Exports = []config.Export{{Name: "reports", Kind: "file", Directory: dir, QueueSize: 8}}
	relay, pool := newFakeSMTP(t, true, false, false)
	withTrust(t, pool)
	cfg.Alerts.SMTP = &config.SMTP{Server: relay.addr(), From: "gw@example.org", To: []string{"ops@example.org"}, TLS: config.SMTPStartTLS, Timeout: 10 * time.Second}
	cfg.DecisionReport = &config.DecisionReport{Destination: "reports", Formats: []string{"pdf", "xlsx"}, Day: 2, MailTo: []string{"fin@example.org"}}
	cfg.Fluids = &config.Fluids{PUE: 1, PricePerKWh: 1, Currency: "EUR", CurrencyDecimals: 2, IdleEnergy: energy.IdleByShare, WPrefill: 1, WDecode: 1}
	now := time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC)
	e := newEnv(t, cfg, WithClock(func() time.Time { return now }))
	if err := e.st.InsertRecords(context.Background(), []store.Record{
		{Time: time.Date(2026, 10, 10, 9, 0, 0, 0, time.UTC), Org: "etabA", Team: "etabA", User: "alice", KeyID: "k", Model: "llama", Pool: "b300", Cost: 3, Status: 200, LatencyMS: 500},
	}); err != nil {
		t.Fatal(err)
	}
	pdf := filepath.Join(dir, "decision", "2026-10", "decision-2026-10.pdf")
	xl := filepath.Join(dir, "decision", "2026-10", "decision-2026-10.xlsx")
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		_, e1 := os.Stat(pdf)
		_, e2 := os.Stat(xl)
		if e1 == nil && e2 == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, f := range []string{pdf, xl} {
		st, err := os.Stat(f)
		if err != nil || st.Size() < 1000 {
			t.Fatalf("%s: %v", f, err)
		}
	}
	// The mail: the PDF and the XLSX attached, to the configured recipient,
	// over a session that used STARTTLS.
	mailKey := filepath.Join(dir, "decision", "2026-10", "mailed.json")
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, err := os.Stat(mailKey); err == nil {
			break
		}
	}
	if _, err := os.Stat(mailKey); err != nil {
		t.Fatalf("no mail marker: %v", err)
	}
	relay.mu.Lock()
	data := relay.data
	relay.mu.Unlock()
	if !relay.saw("STARTTLS") || !strings.Contains(data, "filename=\"decision-2026-10.pdf\"") || !strings.Contains(data, "filename=\"decision-2026-10.xlsx\"") ||
		!strings.Contains(data, "Content-Transfer-Encoding: base64") || !strings.Contains(data, "To: fin@example.org") {
		t.Fatalf("the mail: starttls=%v\n%.600s", relay.saw("STARTTLS"), data)
	}
	// Idempotent: a new round finds the objects and the marker, builds and
	// sends nothing, and audits what the sink now holds exactly once.
	q := e.srv.exports["reports"]
	e.srv.decisionRound(context.Background(), q)
	e.srv.decisionRound(context.Background(), q)
	body := readAll(t, e.do("GET", "/admin/audit", master, ""))
	if n := strings.Count(body, "deliver to reports"); n != 2 {
		t.Fatalf("want 2 audited deliveries, got %d: %s", n, body)
	}
	if n := strings.Count(body, "mail to 1 recipients"); n != 1 {
		t.Fatalf("want 1 audited mail, got %d", n)
	}
	relay.mu.Lock()
	mails := 0
	for _, c := range relay.commands {
		if strings.HasPrefix(strings.ToUpper(c), "DATA") {
			mails++
		}
	}
	relay.mu.Unlock()
	if mails != 1 {
		t.Fatalf("%d mails sent, want 1", mails)
	}
}

// While an upload is in flight the report is not built again.
func TestDecisionPendingUploadIsNotRebuilt(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Exports = []config.Export{{Name: "reports", Kind: "file", Directory: t.TempDir(), QueueSize: 8}}
	e := newEnv(t, cfg, WithClock(func() time.Time { return time.Date(2026, 11, 5, 8, 0, 0, 0, time.UTC) }))
	// Configured after the start: the test drives the rounds, the server's
	// loop must not run beside it.
	cfg.DecisionReport = &config.DecisionReport{Destination: "reports", Formats: []string{"xlsx"}, Day: 2}
	q := e.srv.exports["reports"]
	key := decisionKey(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), "xlsx")
	e.srv.decision.mu.Lock()
	e.srv.decision.pending = map[string]pendingObject{key: {at: time.Date(2026, 11, 5, 7, 0, 0, 0, time.UTC), size: 10, format: "xlsx"}}
	e.srv.decision.mailed = map[string]bool{}
	e.srv.decision.mu.Unlock()
	before := len(readAll(t, e.do("GET", "/admin/audit", master, "")))
	e.srv.decisionRound(context.Background(), q)
	if after := len(readAll(t, e.do("GET", "/admin/audit", master, ""))); after != before {
		t.Fatal("a queued upload was built and delivered again")
	}
	if _, err := os.Stat(filepath.Join(cfg.Exports[0].Directory, key)); err == nil {
		t.Fatal("the object was written while its upload was pending")
	}
}

// A report too large to typeset is refused, not half-written; heavy reports
// are bounded in number.
func TestDecisionBounds(t *testing.T) {
	d := fixedDecision()
	for i := 0; i <= MaxDecisionPartners; i++ {
		d.Partners = append(d.Partners, partner{Org: "p"})
	}
	if _, err := renderDecision(DecisionPDF, d); err == nil || !strings.Contains(err.Error(), "partners") {
		t.Fatalf("too many partners: %v", err)
	}
	d = fixedDecision()
	d.Chargeback = make([]ChargebackRow, MaxDecisionChargeback+1)
	if _, err := renderDecision(DecisionXLSX, d); err == nil {
		t.Fatal("too many chargeback rows accepted")
	}
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	for i := 0; i < heavyReports; i++ {
		heavySem <- struct{}{}
	}
	resp := e.do("GET", "/admin/decision?from=2026-10-01&to=2026-11-01&format=json", master, "")
	_ = readAll(t, resp)
	for i := 0; i < heavyReports; i++ {
		<-heavySem
	}
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("a busy gateway: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestMailMessageIsMIME(t *testing.T) {
	msg := string(buildReportMail("gw@example.org\r\nBcc: x@evil", []string{"a@b.c"}, "subject\r\nBcc: y@evil", "text", []mailAttachment{{Name: "r.pdf", Type: "application/pdf", Data: make([]byte, 200)}}))
	if strings.Contains(msg, "\r\nBcc:") {
		t.Fatalf("header injection: %s", msg)
	}
	for _, line := range strings.Split(msg, "\r\n") {
		if len(line) > 200 {
			t.Fatalf("a line of %d bytes", len(line))
		}
	}
	if !strings.Contains(msg, "multipart/mixed") || !strings.HasSuffix(msg, "--\r\n") {
		t.Fatalf("not multipart: %s", msg)
	}
}

func TestDecisionEndpoint(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Fluids = &config.Fluids{PUE: 1, PricePerKWh: 1, Currency: "EUR", IdleEnergy: energy.IdleByShare, WPrefill: 1, WDecode: 1}
	e := newEnv(t, cfg)
	now := time.Now().UTC()
	if err := e.st.InsertRecords(context.Background(), []store.Record{
		{Time: now, Org: "etabA", Team: "etabA", User: "alice", KeyID: "k", Model: "llama", Pool: "b300", Cost: 3, Status: 200, LatencyMS: 500},
		{Time: now, Org: "etabB", Team: "etabB", User: "bob", KeyID: "k", Model: "llama", Pool: "b300", Cost: 5, Status: 200, LatencyMS: 500, Borrowing: true},
	}); err != nil {
		t.Fatal(err)
	}
	fin := e.adminToken("fin", `{"role":"finance_viewer"}`)
	oa := e.adminToken("oadminA", `{"role":"org_admin","org":"etabA"}`)
	q := "?from=" + now.AddDate(0, 0, -1).Format("2006-01-02") + "&to=" + now.AddDate(0, 0, 2).Format("2006-01-02")
	// Every partner for finance, its own for an organisation.
	all := readAll(t, e.do("GET", "/admin/decision"+q+"&format=json", fin, ""))
	if !strings.Contains(all, "etabA") || !strings.Contains(all, "etabB") {
		t.Fatalf("finance: %s", all)
	}
	own := readAll(t, e.do("GET", "/admin/decision"+q+"&format=json", oa, ""))
	if !strings.Contains(own, "etabA") || strings.Contains(own, "etabB") {
		t.Fatalf("org admin: %s", own)
	}
	for _, f := range []string{"pdf", "xlsx"} {
		resp := e.do("GET", "/admin/decision"+q+"&format="+f, fin, "")
		data := readAll(t, resp)
		if resp.StatusCode != 200 || len(data) < 1000 || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
			!strings.Contains(resp.Header.Get("Content-Disposition"), "decision-") {
			t.Fatalf("%s: %d %d bytes %v", f, resp.StatusCode, len(data), resp.Header)
		}
	}
	for _, bad := range []string{"?from=2020-01-01&to=2026-01-01&format=pdf", q + "&format=docx", "?from=x&to=y"} {
		if code := e.do("GET", "/admin/decision"+bad, fin, "").StatusCode; code != 400 {
			t.Errorf("%s: %d, want 400", bad, code)
		}
	}
}
