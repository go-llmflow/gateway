package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-pdfkit/html2pdf"
)

// The decision PDF is rendered by an HTML engine that can fetch sub-resources
// (html2pdf applies external stylesheets). A name an organisation admin sets
// (partner, cost centre, lab, profile) must never make the gateway fetch a
// URL: that would be a server-side request from inside the site's network.
// The names are escaped (TestDecisionHTMLGolden); this checks the effect: a
// listener on loopback receives nothing while a report full of hostile names
// is rendered.
func TestDecisionPDFFetchesNothing(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "text/css")
		_, _ = w.Write([]byte(`body{color:#000}`))
	}))
	defer srv.Close()
	u := srv.URL

	// The control: the same engine, with the options the gateway passes,
	// fetches a stylesheet that is real markup. Without it, "no request"
	// could only mean the engine never fetches, and the test would prove
	// nothing about the names.
	raw := `<html><head><link rel="stylesheet" href="` + u + `/control.css"></head><body><p>x</p></body></html>`
	if _, err := html2pdf.Export(raw, html2pdf.Options{Title: "control"}); err != nil {
		t.Fatal(err)
	}
	control := hits.Load()

	hostile := []string{
		`<link rel="stylesheet" href="` + u + `/a.css">`,
		`<img src="` + u + `/b.png">`,
		`<style>@import url(` + u + `/c.css);</style>`,
		`"><iframe src="` + u + `/d"></iframe>`,
		`x" style="background:url(` + u + `/e.png)`,
	}
	d := fixedDecision()
	d.Partners[0].Org = hostile[0]
	d.Partners[0].CostCenter = hostile[1]
	d.Partners[1].Org = hostile[2]
	d.Chargeback[0].CostCenter = hostile[3]
	d.Quotas.Soft[0].Lab = hostile[4]
	d.Quotas.Profiles[0].Profile = hostile[0]
	d.Pie[0].Label = hostile[1]
	d.Bars[0].Label = hostile[2]
	hits.Store(0)
	var b bytes.Buffer
	if err := writeDecisionPDF(&b, d); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("rendering the report made %d request(s) to a URL taken from a name", n)
	}
	if control == 0 {
		// The engine fetched nothing even for real markup. Then the zero
		// above is not evidence; say so rather than pass silently.
		t.Fatalf("control: html2pdf fetched no stylesheet with the gateway's options, so this test cannot see a fetch; adapt the control")
	}
}
