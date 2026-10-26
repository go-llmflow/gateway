package gateway

import (
	"bytes"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

const sreportP = `Cluster/Account/User Utilization 2026-10-01T00:00:00 - 2026-10-31T23:59:59 (2678400 secs)
Usage reported in TRES Minutes
Cluster|Account|Login|Proper Name|TRES Name|Used|Energy
jz|root|||gres/gpu|99000|0
jz|phys-a|||gres/gpu|60000|0
jz|phys-a|alice|Alice|gres/gpu|60000|0
jz|etabB|||gres/gpu|30000|0
jz|etabB|bob|Bob|gres/gpu|30000|0
`

// uiUpload posts the reconciliation form with a file.
func (e *ssoEnv) uiUpload(path, sess, csrf, format string, file []byte) (int, string, string) {
	e.t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("csrf", csrf)
	_ = mw.WriteField("format", format)
	fw, _ := mw.CreateFormFile("sreport", "sreport.txt")
	_, _ = fw.Write(file)
	_ = mw.Close()
	resp := e.req("POST", path, map[string]string{sessionCookie: sess}, body.String(),
		"Content-Type", mw.FormDataContentType(), "Origin", gwOrigin)
	return resp.StatusCode, readAll(e.t, resp), resp.Header.Get("Content-Type")
}

func TestSlurmReconciliation(t *testing.T) {
	e := newSSOEnv(t, "")
	expect(t, "user", e.do("POST", "/admin/users", master, `{"name":"carol"}`).StatusCode, 201)
	expect(t, "grant", e.do("PUT", "/admin/users/carol/roles", master, `{"role":"org_admin","org":"etabB"}`).StatusCode, 200, 201, 204)
	e.srv.cfg.Slurm.Accounts = map[string]string{"phys-a": "etabA"}
	now := e.srv.now().UTC()
	from, to := now.AddDate(0, 0, -10), now.AddDate(0, 0, 1)
	_ = e.st.InsertRecords(t.Context(), []store.Record{
		{Org: "etabA", Team: "etabA", Model: "llama", Pool: "b300", Cost: 250, Time: now},
		{Org: "etabB", Team: "etabB", Model: "llama", Pool: "b300", Cost: 500, Time: now},
	})
	span := "from=" + from.Format("2006-01-02") + "&to=" + to.Format("2006-01-02")

	// The API, with the report as the body: 1000 + 500 batch GPU-hours.
	resp := e.do("POST", "/admin/export?"+span+"&format=xlsx", master, sreportP, "Content-Type", "text/plain")
	raw := []byte(readAll(t, resp))
	if resp.StatusCode != 200 {
		t.Fatalf("upload: %d %s", resp.StatusCode, raw)
	}
	parts := unzip(t, raw)
	sheet := ""
	for name, p := range parts {
		if strings.HasPrefix(name, "xl/worksheets/") && strings.Contains(p, "<v>1000</v>") && strings.Contains(p, "<v>500</v>") {
			sheet = name
		}
	}
	if sheet == "" || !strings.Contains(parts["xl/sharedStrings.xml"], "Slurm batch (GPU-hours)") ||
		!strings.Contains(parts["xl/sharedStrings.xml"], "2026-10-01T00:00:00 - 2026-10-31T23:59:59") {
		t.Fatal("no reconciliation sheet with the batch hours")
	}
	resp = e.do("POST", "/admin/export?"+span+"&format=pdf", master, sreportP, "Content-Type", "text/plain")
	if pdf := []byte(readAll(t, resp)); resp.StatusCode != 200 {
		t.Fatalf("pdf: %d", resp.StatusCode)
	} else {
		pdfSafe(t, pdf)
	}
	// Refused: not text, a percent report, too large; each audited.
	for _, c := range []struct{ ctype, body string }{
		{"application/json", sreportP},
		{"text/plain", "Usage reported in Percentage of Total\nAccount|Used\na|1\n"},
		{"text/plain", strings.Repeat("x", 1<<20+1)},
	} {
		if r := e.do("POST", "/admin/export?"+span+"&format=xlsx", master, c.body, "Content-Type", c.ctype); r.StatusCode != 400 {
			t.Errorf("%s: %d", c.ctype, r.StatusCode)
		}
	}

	// The configured file; then the file gone: a generic 500, logged.
	path := filepath.Join(t.TempDir(), "sreport.txt")
	if err := os.WriteFile(path, []byte(sreportP), 0o600); err != nil {
		t.Fatal(err)
	}
	e.srv.cfg.Slurm.SreportFile = path
	sess, _ := e.login(e.claims("alice", "llm-admins"))
	_, body, _ := e.uiGet("/ui/reports?"+span, sess)
	if !strings.Contains(body, "Reconciliation: inference and Slurm batch") || !strings.Contains(body, "Slurm report (file), period 2026-10-01") ||
		!strings.Contains(body, ">1000<") || !strings.Contains(body, "20.0 %") {
		t.Fatalf("reconciliation on the page: %s", body)
	}
	_ = os.Remove(path)
	if c, body, _ := e.uiGet("/ui/reports?"+span, sess); c != 200 || !strings.Contains(body, "the configured Slurm report could not be read") || strings.Contains(body, path) {
		t.Fatalf("missing file: %d", c)
	}
	if r := e.do("GET", "/admin/export?"+span+"&format=pdf", master, ""); r.StatusCode != 500 || strings.Contains(readAll(t, r), path) {
		t.Fatalf("missing file export: %d", r.StatusCode)
	}
	e.srv.cfg.Slurm.SreportFile = ""

	// An org_admin uploads through the UI and sees their org's batch only.
	sess, csrf := e.login(e.claims("carol"))
	code, body, _ := e.uiUpload(uiUploadPath+"?"+span, sess, csrf, "html", []byte(sreportP))
	if code != 200 || !strings.Contains(body, ">etabB<") || strings.Contains(body, "etabA") || strings.Contains(body, ">1000<") ||
		!strings.Contains(body, "Slurm report (upload)") {
		t.Fatalf("org admin upload: %d %s", code, body)
	}
	code, body, ctype := e.uiUpload(uiUploadPath+"?"+span, sess, csrf, "pdf", []byte(sreportP))
	if code != 200 || ctype != "application/pdf" {
		t.Fatalf("ui pdf: %d %s", code, ctype)
	}
	pdfSafe(t, []byte(body))
	if code, body, _ = e.uiUpload(uiUploadPath+"?"+span, sess, csrf, "html", []byte("nothing here\n")); !strings.Contains(body, `class="err"`) {
		t.Fatalf("bad upload: %d", code)
	}
	// No token: refused before anything is read.
	if code, _, _ = e.uiUpload(uiUploadPath+"?"+span, sess, "", "html", []byte(sreportP)); code != 403 {
		t.Fatalf("no csrf: %d", code)
	}

	es, _ := e.st.AuditRange(t.Context(), 1, 1000)
	slurmAudited := 0
	for _, a := range es {
		if a.Kind == "export" && strings.Contains(a.After, `"slurm":"upload"`) {
			slurmAudited++
		}
	}
	if slurmAudited != 3 {
		t.Fatalf("audited uploads: %d", slurmAudited)
	}
}
