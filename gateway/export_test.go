package gateway

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// unzip returns every part of an OOXML package.
func unzip(t testing.TB, b []byte) map[string]string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(b), int64(len(b)))
	if err != nil {
		t.Fatalf("not a zip: %v", err)
	}
	out := map[string]string{}
	for _, f := range zr.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(rc)
		rc.Close()
		out[f.Name] = string(data)
	}
	return out
}

// pdfSafe checks a PDF has no action, script, link or embedded file.
func pdfSafe(t testing.TB, b []byte) {
	t.Helper()
	if msg := pdfUnsafe(b); msg != "" {
		t.Fatal(msg)
	}
}

func pdfUnsafe(b []byte) string {
	if !bytes.HasPrefix(b, []byte("%PDF-")) || !bytes.Contains(b[max(0, len(b)-32):], []byte("%%EOF")) {
		return "not a PDF"
	}
	// The object syntax only: compressed stream bytes can hold anything.
	var syntax []byte
	for rest := b; ; {
		i := bytes.Index(rest, []byte("stream"))
		if i < 0 {
			syntax = append(syntax, rest...)
			break
		}
		syntax = append(syntax, rest[:i]...)
		j := bytes.Index(rest[i:], []byte("endstream"))
		if j < 0 {
			return "unterminated stream"
		}
		rest = rest[i+j+len("endstream"):]
	}
	// A name is a whole token: /JSKQWE+Font (a subset tag) is not /JS.
	for _, bad := range []string{"/JavaScript", "/JS", "/OpenAction", "/AA", "/URI", "/Launch", "/EmbeddedFile", "/SubmitForm"} {
		for rest := syntax; ; {
			i := bytes.Index(rest, []byte(bad))
			if i < 0 {
				break
			}
			rest = rest[i+len(bad):]
			if len(rest) == 0 || bytes.IndexByte([]byte(" \t\r\n/<>[]()%"), rest[0]) >= 0 {
				return "the PDF contains " + bad
			}
		}
	}
	return ""
}

// TestPDFProbe plants the positives the probe must see, and the subset
// tag it must not.
func TestPDFProbe(t *testing.T) {
	for _, bad := range []string{"<< /OpenAction 2 0 R >>", "<< /S /JavaScript /JS (x) >>", "<< /A << /URI (http://x) >> >>", "<</AA<<>>>>"} {
		if pdfUnsafe([]byte("%PDF-1.7\n1 0 obj "+bad+" endobj\n%%EOF\n")) == "" {
			t.Errorf("not seen: %s", bad)
		}
	}
	ok := "%PDF-1.7\n1 0 obj << /BaseFont /JSKQWE+Atkinson >> stream\n/URI /JS\nendstream endobj\n%%EOF\n"
	if msg := pdfUnsafe([]byte(ok)); msg != "" {
		t.Errorf("false positive: %s", msg)
	}
}

// TestPDFSample writes a sample summary to $LLMFLOW_PDF_SAMPLE, for a
// person to look at; it is skipped otherwise.
func TestPDFSample(t *testing.T) {
	path := os.Getenv("LLMFLOW_PDF_SAMPLE")
	if path == "" {
		t.Skip("LLMFLOW_PDF_SAMPLE is not set")
	}
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	b := &reportBundle{From: day, To: day.AddDate(0, 0, 26), PrevFrom: day.AddDate(0, 0, -26), Generated: day.AddDate(0, 0, 26),
		By: map[string][]store.ReportRow{}, Batch: map[string]float64{"etabA": 1200, "etabB": 300}}
	var base []store.ReportRow
	for i, org := range []string{"etabA", "etabB", "etabC"} {
		for j, lab := range []string{"physics", "chemistry", "biology", ""} {
			base = append(base, store.ReportRow{Org: org, Lab: lab, Team: org + "-t" + lab, Project: "p" + lab, User: "user" + lab, Model: "llama", Pool: "b300",
				Requests: int64(10 * (i + j + 1)), Cost: float64(100*(3-i) + 10*j)})
		}
	}
	base = append(base, store.ReportRow{Org: "etabA", Team: "etabA", Model: "claude", Unit: "USD", Requests: 5, Cost: 42.5})
	for _, d := range exportDims {
		b.By[d] = store.Regroup(base, []string{d})
	}
	for i := 0; i < 26; i++ {
		b.Days = append(b.Days, store.ReportRow{Day: day.AddDate(0, 0, i).Format("2006-01-02"), Cost: float64(20 + i%7*5)})
	}
	b.Totals = []unitTotal{{Cost: 1650, Prev: 1400, Requests: 300}, {Unit: "USD", Cost: 42.5, Prev: 50, Requests: 5}}
	var out bytes.Buffer
	if err := writePDF(&out, b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d bytes)", path, out.Len())
}

func TestExports(t *testing.T) {
	e := newSSOEnv(t, "")
	expect(t, "user", e.do("POST", "/admin/users", master, `{"name":"carol"}`).StatusCode, 201)
	expect(t, "grant", e.do("PUT", "/admin/users/carol/roles", master, `{"role":"org_admin","org":"etabB"}`).StatusCode, 200, 201, 204)
	now := e.srv.now().UTC()
	from, to := now.AddDate(0, 0, -10), now.AddDate(0, 0, 1)
	_ = e.st.InsertRecords(t.Context(), []store.Record{
		{Org: "etabA", Lab: "physics", Team: "etabA", Project: "+1", User: "=cmd|' /C calc'!A0", Model: "llama", Pool: "b300", Cost: 2, Time: now},
		{Org: "etabA", Lab: "physics", Team: "etabA", Project: "@x", User: "-2", Model: "llama", Pool: "b300", Cost: 1, Time: from.AddDate(0, 0, -3)},
		{Org: "etabA", Team: "etabA", Model: "claude", Cost: 3, Unit: "USD", Time: now},
		{Org: "etabB", Team: "etabB", Model: "llama", Pool: "b300", Cost: 7, Time: now},
	})
	span := "from=" + from.Format("2006-01-02") + "&to=" + to.Format("2006-01-02")

	// CSV: the new dimensions, formulas neutralised, a safe file name.
	resp := e.do("GET", "/admin/export?"+span+"&format=csv", master, "")
	body := readAll(t, resp)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/csv") ||
		resp.Header.Get("Content-Disposition") != `attachment; filename="llmflow-report-`+from.Format("2006-01-02")+`-to-`+to.Format("2006-01-02")+`.csv"` {
		t.Fatalf("csv: %d %v", resp.StatusCode, resp.Header)
	}
	recs, err := csv.NewReader(strings.NewReader(body)).ReadAll()
	if err != nil || strings.Join(recs[0][:9], ",") != "from,to,org,lab,team,project,user,model,pool" {
		t.Fatalf("csv header: %v %v", recs, err)
	}
	for _, r := range recs[1:] {
		for _, c := range r[2:9] {
			if c != "" && strings.ContainsRune("=+-@", rune(c[0])) {
				t.Fatalf("a cell can be read as a formula: %q", c)
			}
		}
	}
	if !strings.Contains(body, "'=cmd") {
		t.Fatal("the formula was not prefixed")
	}

	// XLSX: a summary and a sheet per grouping, no formula, units in headers.
	resp = e.do("GET", "/admin/export?"+span+"&format=xlsx", master, "")
	raw := []byte(readAll(t, resp))
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Disposition"), ".xlsx") {
		t.Fatalf("xlsx: %d", resp.StatusCode)
	}
	parts := unzip(t, raw)
	for _, want := range []string{"Summary", "By org", "By lab", "By team", "By project", "By user", "By model", "By pool", "By tag", "By day"} {
		if !strings.Contains(parts["xl/workbook.xml"], `name="`+want+`"`) {
			t.Errorf("no sheet %q", want)
		}
	}
	shared := parts["xl/sharedStrings.xml"]
	for _, want := range []string{"Cost (GPU-hours)", "Cost (USD)", "Total (GPU-hours)", "=cmd|"} {
		if !strings.Contains(shared, want) {
			t.Errorf("shared strings miss %q", want)
		}
	}
	for name, p := range parts {
		if strings.HasPrefix(name, "xl/worksheets/") && (strings.Contains(p, "<f>") || strings.Contains(p, "<f ")) {
			t.Fatalf("%s holds a formula", name)
		}
	}

	// PDF: a document without action, script or link.
	resp = e.do("GET", "/admin/export?"+span+"&format=pdf", master, "")
	raw = []byte(readAll(t, resp))
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/pdf" {
		t.Fatalf("pdf: %d", resp.StatusCode)
	}
	pdfSafe(t, raw)

	// Bounds and bad input, all refused.
	for _, bad := range []string{"from=2020-01-01&to=2026-01-01&format=csv", span + "&format=doc", span + "&format=csv&group_by=planet", "format=pdf"} {
		if c := e.do("GET", "/admin/export?"+bad, master, "").StatusCode; c != 400 {
			t.Errorf("%s: %d", bad, c)
		}
	}

	// Every download is audited, refusals included.
	es, _ := e.st.AuditRange(t.Context(), 1, 1000)
	n, refused := 0, 0
	for _, a := range es {
		if a.Kind == "export" {
			n++
			if a.Status == 400 {
				refused++
			}
		}
	}
	if n != 7 || refused != 4 {
		t.Fatalf("export audit: %d entries, %d refused", n, refused)
	}

	// An org_admin exports their organisation only, from the UI too.
	sess, _ := e.login(e.claims("carol"))
	for _, f := range []string{"csv", "xlsx", "pdf"} {
		code, body, csp := e.uiGet("/ui/reports/export?"+span+"&format="+f, sess)
		if code != 200 || csp != uiCSP {
			t.Fatalf("ui %s: %d", f, code)
		}
		if f == "csv" && (strings.Contains(body, "etabA") || !strings.Contains(body, "etabB")) {
			t.Fatalf("org admin export: %s", body)
		}
		if f == "xlsx" && strings.Contains(unzip(t, []byte(body))["xl/sharedStrings.xml"], "etabA") {
			t.Fatal("org admin xlsx shows another org")
		}
	}
	if code, body, _ := e.uiGet("/ui/reports/export?"+span+"&format=doc", sess); code != 200 || !strings.Contains(body, `class="err"`) {
		t.Fatalf("ui bad format: %d", code)
	}
	if _, body, _ := e.uiGet("/ui/reports?"+span, sess); !strings.Contains(body, "/ui/reports/export?") {
		t.Fatal("no export links on the reports page")
	}
}

func TestDownloadNameAndLabels(t *testing.T) {
	d := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)
	if n := downloadName(d, d.AddDate(0, 1, 0), `x"/../\`); n != `llmflow-report-2026-01-02-to-2026-02-02.x__..__` {
		t.Fatalf("%q", n)
	}
	if l := cleanLabel("a\x00b‮c\nd", 10); l != "abcd" {
		t.Fatalf("%q", l)
	}
	if l := cleanLabel(strings.Repeat("é", 50), 10); len([]rune(l)) != 10 {
		t.Fatalf("%q", l)
	}
	if cleanLabel("\x01", 5) != "(none)" {
		t.Fatal("empty label")
	}
}

// FuzzExportWriters writes workbooks and PDFs whose labels are arbitrary:
// no error, no panic, no formula in a sheet, no action in a PDF.
func FuzzExportWriters(f *testing.F) {
	f.Add("=SUM(A1:A9)", "USD", 1.5)
	f.Add("+1\x00‮", "", 1e300)
	f.Add("@x", "EUR", -3.0)
	f.Add(strings.Repeat("é", 400), "usd_cents", 0.0)
	f.Fuzz(func(t *testing.T, label, unit string, cost float64) {
		if _, err := store.NormalizeUnit(unit); err != nil || cost != cost || cost > 1e300 || cost < -1e300 {
			return
		}
		day := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
		row := store.ReportRow{Org: label, Lab: label, Team: label, Project: label, User: label, Model: label, Pool: label, Tag: label,
			Day: "2026-01-01", Unit: unit, Requests: 1, Cost: cost}
		b := &reportBundle{From: day, To: day.AddDate(0, 0, 2), PrevFrom: day.AddDate(0, 0, -2), Generated: day,
			By: map[string][]store.ReportRow{}, Days: []store.ReportRow{row}, Totals: []unitTotal{{Unit: unit, Cost: cost, Requests: 1}},
			Batch: map[string]float64{label: 2}}
		for _, d := range exportDims {
			b.By[d] = []store.ReportRow{row}
		}
		var x bytes.Buffer
		if err := writeXLSX(&x, b); err != nil {
			// The one refusal allowed: a cell longer than a spreadsheet holds.
			if len(label) <= 32767 {
				t.Fatalf("xlsx: %v", err)
			}
			return
		}
		for name, p := range unzip(t, x.Bytes()) {
			if strings.HasPrefix(name, "xl/worksheets/") && strings.Contains(p, "<f>") {
				t.Fatal("formula")
			}
		}
		var p bytes.Buffer
		if err := writePDF(&p, b); err != nil {
			t.Fatalf("pdf: %v", err)
		}
		pdfSafe(t, p.Bytes())
	})
}
