package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-llmflow/gateway/lending"
	"github.com/go-llmflow/gateway/store"
	"github.com/go-ooxml/ooxml/xlsx"
	"github.com/go-pdfkit/html2pdf"
)

// The monthly decision report (#97): what the partners of a shared GPU
// platform need to settle their agreement. A one-page summary per partner
// (guaranteed share against use, borrowed and lent GPU-hours, fluids, latency
// in share against borrowing, adoption, cost centre), then the details (the
// lending matrix, fluids, adoption, chargeback rows) and charts drawn with
// go-plot. PDF is typeset by go-pdfkit/html2pdf (v0.3.0): A4 pages, page
// numbers in the margin, a table of contents whose page numbers come from
// CSS target-counter. XLSX carries the same figures as typed cells, one
// column per unit.
//
// Who reads it: the proxy roles and finance_viewer read every partner (and
// the matrix); org_admin and org-level viewers read their own partner only.
// Every figure comes from a report the principal could call on its own, so
// the decision report adds no way to read what those hide.

// MaxDecisionSpan bounds a decision report: a month is the use, a year the
// limit.
const MaxDecisionSpan = store.MaxReportSpan

// Formats of a decision report.
const (
	DecisionPDF  = "pdf"
	DecisionXLSX = "xlsx"
)

// partner is one partner's page.
type partner struct {
	Org        string
	CostCenter string
	Share      float64
	// GPU-hours of the period and of the one before (inference spend, the
	// Reports page's figure), and requests.
	GPUHours, PrevGPUHours float64
	Requests               int64
	// From lending: guaranteed capacity, use in share, borrowing, lending.
	Guaranteed, UsedInShare, Borrowed, BorrowedFromOthers, SelfServed, Lent, Net, Utilisation float64
	InShare, Borrowing                                                                        lending.Latency
	// Fluids: IT energy and its cost.
	KWh, KWhIdle, FluidsCost float64
	// ActiveUsers of the period.
	ActiveUsers int64
}

// decisionData is a report's data.
type decisionData struct {
	From, To, PrevFrom, Generated time.Time
	Partners                      []partner
	Matrix                        []LendingCell // proxy roles and finance_viewer only
	Unattributed                  float64
	Currency                      string // of the fluids; "" without a fluids block
	HasFluids                     bool
	Adoption                      *store.Adoption
	Chargeback                    []ChargebackRow
	Daily                         []chartPoint
	Pie                           []chartSlice
	Bars                          []chartBar
}

// buildDecision gathers the figures from the reports p may read.
func (s *Server) buildDecision(r *http.Request, p *principal, from, to time.Time) (*decisionData, error) {
	d := &decisionData{From: from, To: to, PrevFrom: from.Add(-to.Sub(from)), Generated: s.now().UTC()}
	visible := func(org string) bool { return p.reportView() || p.orgView(org) }
	byOrg := map[string]*partner{}
	get := func(org string) *partner {
		if x := byOrg[org]; x != nil {
			return x
		}
		x := &partner{Org: org, CostCenter: s.cfg.CostCenterOf(org, "", "", "")}
		byOrg[org] = x
		return x
	}
	for _, o := range s.cfg.Orgs {
		if visible(o.Name) {
			get(o.Name).Share = o.Share
		}
	}
	// Inference spend, now and before it.
	spend, err := s.visibleReport(r, p, from, to, []string{"org"})
	if err != nil {
		return nil, err
	}
	for _, x := range spend {
		if x.Unit == "" && visible(x.Org) {
			get(x.Org).GPUHours += x.Cost
			get(x.Org).Requests += x.Requests
		}
	}
	prev, err := s.visibleReport(r, p, d.PrevFrom, from, []string{"org"})
	if err != nil {
		return nil, err
	}
	for _, x := range prev {
		if x.Unit == "" && visible(x.Org) {
			get(x.Org).PrevGPUHours += x.Cost
		}
	}
	days, err := s.visibleReport(r, p, from, to, []string{"day"})
	if err != nil {
		return nil, err
	}
	for _, x := range days {
		if x.Unit == "" {
			d.Daily = append(d.Daily, chartPoint{X: x.Day, Y: x.Cost})
		}
	}
	// Lending.
	if ls, ok := s.st.(store.LendingStore); ok {
		res, err := s.lending(r, from, to, ls)
		if err != nil {
			return nil, err
		}
		for _, o := range res.Orgs {
			if !visible(o.Org) {
				continue
			}
			x := get(o.Org)
			x.Guaranteed, x.UsedInShare, x.Borrowed, x.Lent, x.Net = o.GuaranteedHours, o.UsedInShare, o.Borrowed, o.Lent, o.Net
			x.BorrowedFromOthers, x.SelfServed, x.InShare, x.Borrowing = o.BorrowedFromOthers, o.SelfServed, o.InShare, o.Borrow
			if o.GuaranteedHours > 0 {
				x.Utilisation = o.UsedInShare / o.GuaranteedHours
			}
		}
		if p.reportView() {
			d.Unattributed = res.Unattributed
			for l, m := range res.Matrix {
				for b, v := range m {
					d.Matrix = append(d.Matrix, LendingCell{l, b, v})
				}
			}
			sort.Slice(d.Matrix, func(i, j int) bool {
				if d.Matrix[i].Lender != d.Matrix[j].Lender {
					return d.Matrix[i].Lender < d.Matrix[j].Lender
				}
				return d.Matrix[i].Borrower < d.Matrix[j].Borrower
			})
		}
	}
	// Fluids.
	if f := s.cfg.Fluids; f != nil {
		d.HasFluids, d.Currency = true, f.Currency
		rows, err := s.fluidsData(r, p, from, to, []string{"org"})
		if err != nil {
			return nil, err
		}
		for _, x := range rows {
			if x.Org == "" || x.KWh == 0 && x.Cost == 0 {
				continue
			}
			y := get(x.Org)
			y.KWh, y.KWhIdle, y.FluidsCost = x.KWh, x.KWhIdle, x.Cost
		}
		if cb, err := s.chargeback(r, p, from, to); err == nil {
			d.Chargeback = cb
		} else {
			return nil, err
		}
	} else if cb, err := s.chargeback(r, p, from, to); err == nil {
		d.Chargeback = cb
	} else {
		return nil, err
	}
	// Adoption: overall for the readers of the whole platform, and per partner.
	if as, ok := s.st.(store.AdoptionStore); ok {
		var orgs []string
		if !p.reportView() {
			orgs = []string{}
			for o := range byOrg {
				orgs = append(orgs, o)
			}
		}
		if d.Adoption, err = as.Adoption(r.Context(), from, to, orgs); err != nil {
			return nil, err
		}
		for o, x := range byOrg {
			a, err := as.Adoption(r.Context(), from, to, []string{o})
			if err != nil {
				return nil, err
			}
			x.ActiveUsers = a.Users
		}
	}
	for _, x := range byOrg {
		d.Partners = append(d.Partners, *x)
	}
	sort.Slice(d.Partners, func(i, j int) bool { return d.Partners[i].Org < d.Partners[j].Org })
	for _, x := range d.Partners {
		d.Pie = append(d.Pie, chartSlice{Label: x.Org, Value: x.GPUHours})
		d.Bars = append(d.Bars, chartBar{Label: x.Org, Current: x.GPUHours, Previous: x.PrevGPUHours})
	}
	sort.SliceStable(d.Bars, func(i, j int) bool { return d.Bars[i].Current > d.Bars[j].Current })
	return d, nil
}

// ---- XLSX ----

func writeDecisionXLSX(w io.Writer, d *decisionData) error {
	wb := xlsx.New()
	num := func(v float64) xlsx.Cell { return xlsx.Number(v).Format(xlsx.Decimal(3)) }
	sheet := func(name string, widths []float64, head ...string) (*xlsx.Sheet, error) {
		sh, err := wb.AddSheet(name)
		if err != nil {
			return nil, err
		}
		for i, wd := range widths {
			_ = sh.SetColumnWidth(i, wd)
		}
		return sh, sh.SetHeader(head...)
	}
	sh, err := sheet("Partners", []float64{24, 16}, "Partner", "Cost centre", "Share", "Guaranteed (GPU-hours)", "Used in share (GPU-hours)",
		"Borrowed (GPU-hours)", "Borrowed from others (GPU-hours)", "Lent (GPU-hours)", "Net lent (GPU-hours)", "Guarantee used", "Inference (GPU-hours)",
		"Previous period (GPU-hours)", "Requests", "Energy (kWh)", "of which idle (kWh)", "Fluids cost ("+currencyOr(d.Currency)+")",
		"Latency p50 in share (ms)", "Latency p95 in share (ms)", "Latency p50 borrowing (ms)", "Latency p95 borrowing (ms)", "Active people")
	if err != nil {
		return err
	}
	for _, x := range d.Partners {
		if err := sh.AppendRow(xlsx.String(x.Org), xlsx.String(x.CostCenter), xlsx.Number(x.Share).Format(xlsx.Percent(1)), num(x.Guaranteed),
			num(x.UsedInShare), num(x.Borrowed), num(x.BorrowedFromOthers), num(x.Lent), num(x.Net), xlsx.Number(x.Utilisation).Format(xlsx.Percent(1)),
			num(x.GPUHours), num(x.PrevGPUHours), xlsx.Number(float64(x.Requests)), num(x.KWh), num(x.KWhIdle), num(x.FluidsCost),
			num(x.InShare.P50), num(x.InShare.P95), num(x.Borrowing.P50), num(x.Borrowing.P95), xlsx.Number(float64(x.ActiveUsers))); err != nil {
			return err
		}
	}
	mx, err := sheet("Lending matrix", []float64{24, 24}, "Lender", "Borrower", "GPU-hours")
	if err != nil {
		return err
	}
	for _, c := range d.Matrix {
		if err := mx.AppendRow(xlsx.String(c.Lender), xlsx.String(c.Borrower), num(c.GPUHours)); err != nil {
			return err
		}
	}
	cb, err := sheet("Chargeback", []float64{14, 14, 18}, "Period from", "Period to", "Cost centre", "GPU-hours", "Energy (kWh)",
		"Fluids cost", "Fluids currency", "Provider cost", "Provider currency")
	if err != nil {
		return err
	}
	for _, c := range d.Chargeback {
		if err := cb.AppendRow(xlsx.String(c.From.UTC().Format("2006-01-02")), xlsx.String(c.To.UTC().Format("2006-01-02")), xlsx.String(c.CostCenter),
			num(c.GPUHours), num(c.KWh), num(c.FluidsCost), xlsx.String(c.FluidsCurrency), num(c.ProviderCost), xlsx.String(c.ProviderCurrency)); err != nil {
			return err
		}
	}
	ad, err := sheet("Adoption", []float64{16, 14}, "Series", "Period or model", "Active people", "Requests")
	if err != nil {
		return err
	}
	if d.Adoption != nil {
		emit := func(series string, ps []store.AdoptionPoint) error {
			for _, x := range ps {
				if err := ad.AppendRow(xlsx.String(series), xlsx.String(x.Day), xlsx.Number(float64(x.ActiveUsers)), xlsx.Number(float64(x.Requests))); err != nil {
					return err
				}
			}
			return nil
		}
		if err := emit("day", d.Adoption.Daily); err != nil {
			return err
		}
		if err := emit("week", d.Adoption.Weekly); err != nil {
			return err
		}
		for _, m := range d.Adoption.Models {
			if err := ad.AppendRow(xlsx.String("model"), xlsx.String(m.Model), xlsx.Number(float64(m.Users)), xlsx.Number(float64(m.Requests))); err != nil {
				return err
			}
		}
	}
	data, err := wb.Write()
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

func currencyOr(c string) string {
	if c == "" {
		return "no price set"
	}
	return c
}

// ---- PDF ----

// decisionView is the template's data: everything already formatted.
type decisionView struct {
	Title, Period, Generated string
	Partners                 []partnerView
	Matrix                   []matrixView
	Unattributed             string
	HasFluids                bool
	Adoption                 *store.Adoption
	AdoptionUsers            int64
	Chargeback               []chargebackView
	Pie, Bars, Series        template.HTML
}

type partnerView struct {
	partner
	Anchor                                                                 string
	ShareS, GuaranteedS, UsedS, BorrowedS, FromOthersS, SelfS, LentS, NetS string
	UtilS, GPUS, PrevS, ChangeS, KWhS, KWhIdleS, CostS                     string
	LatIn, LatBorrow                                                       string
}

type matrixView struct{ Lender, Borrower, GPUHours string }

type chargebackView struct{ Period, CostCenter, GPUHours, KWh, Fluids, Provider string }

func pctS(v float64) string { return strconv.FormatFloat(100*v, 'f', 1, 64) + " %" }

func latS(l lending.Latency) string {
	if l.N == 0 {
		return "no request"
	}
	return fmt.Sprintf("p50 %s ms, p95 %s ms (%d requests)", fmtNum(l.P50), fmtNum(l.P95), l.N)
}

func (d *decisionData) view() decisionView {
	v := decisionView{
		Title:     "Decision report",
		Period:    d.From.UTC().Format("2006-01-02") + " to " + d.To.UTC().Add(-time.Nanosecond).Format("2006-01-02") + " (UTC)",
		Generated: d.Generated.Format("2006-01-02 15:04 UTC"), HasFluids: d.HasFluids, Adoption: d.Adoption,
	}
	if d.Adoption != nil {
		v.AdoptionUsers = d.Adoption.Users
	}
	for _, x := range d.Partners {
		change := "no previous period"
		if x.PrevGPUHours > 0 {
			change = strconv.FormatFloat(100*(x.GPUHours-x.PrevGPUHours)/x.PrevGPUHours, 'f', 1, 64) + " % against the previous period"
		}
		cost := fmtNum(x.FluidsCost)
		if d.Currency != "" {
			cost += " " + d.Currency
		}
		v.Partners = append(v.Partners, partnerView{
			partner: x, Anchor: "p-" + x.Org, ShareS: pctS(x.Share), GuaranteedS: fmtNum(x.Guaranteed), UsedS: fmtNum(x.UsedInShare),
			BorrowedS: fmtNum(x.Borrowed), FromOthersS: fmtNum(x.BorrowedFromOthers), SelfS: fmtNum(x.SelfServed), LentS: fmtNum(x.Lent),
			NetS: fmtNum(x.Net), UtilS: pctS(x.Utilisation), GPUS: fmtNum(x.GPUHours), PrevS: fmtNum(x.PrevGPUHours), ChangeS: change,
			KWhS: fmtNum(x.KWh), KWhIdleS: fmtNum(x.KWhIdle), CostS: cost, LatIn: latS(x.InShare), LatBorrow: latS(x.Borrowing),
		})
	}
	for _, c := range d.Matrix {
		v.Matrix = append(v.Matrix, matrixView{c.Lender, c.Borrower, fmtNum(c.GPUHours)})
	}
	if d.Unattributed > 1e-9 {
		v.Unattributed = fmtNum(d.Unattributed)
	}
	for _, c := range d.Chargeback {
		prov := ""
		if c.ProviderCurrency != "" {
			prov = fmtNum(c.ProviderCost) + " " + c.ProviderCurrency
		}
		fl := ""
		if c.FluidsCurrency != "" || c.FluidsCost != 0 {
			fl = fmtNum(c.FluidsCost) + " " + c.FluidsCurrency
		}
		v.Chargeback = append(v.Chargeback, chargebackView{c.From.UTC().Format("2006-01"), c.CostCenter, fmtNum(c.GPUHours), fmtNum(c.KWh), fl, prov})
	}
	pc := plotCharts{}
	v.Pie = pc.Pie("GPU-hours by partner", "GPU-hours", d.Pie)
	v.Bars = pc.Bars("GPU-hours by partner, against the previous period", "GPU-hours", d.Bars)
	v.Series = pc.Series("GPU-hours per day", "GPU-hours", d.Daily)
	return v
}

// decisionTmpl is the report's HTML. The CSS is the subset go-pdfkit/html2pdf
// reads (README, opened 2026-10-29): @page with margin boxes
// (counter(page), counter(pages)), target-counter for the contents, break-*
// for the partner pages. Everything from the data goes through html/template's
// contextual escaping; the charts are the gateway's own escaped SVG.
var decisionTmpl = template.Must(template.New("decision").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>{{.Title}}</title>
<style>
@page { size: A4; margin: 20mm 16mm 18mm 16mm;
  @top-left { content: "Decision report"; font-size: 8pt; color: #555555 }
  @bottom-center { content: counter(page) " / " counter(pages); font-size: 9pt } }
body { font-family: sans-serif; font-size: 10pt; color: #1d2330 }
h1 { font-size: 22pt; margin: 0 0 4pt 0 }
h2 { font-size: 14pt; margin: 14pt 0 6pt 0; break-after: avoid }
h3 { font-size: 11pt; margin: 10pt 0 4pt 0; break-after: avoid }
table { border-collapse: collapse; width: 100% }
th, td { border-bottom: 0.5pt solid #bbbbbb; padding: 3pt 4pt; text-align: left; font-size: 9.5pt }
th { background: #eef1f6 }
td.n, th.n { text-align: right }
.partner { break-after: page }
.note { font-size: 8.5pt; color: #555555 }
ol.toc { padding-left: 14pt }
a.toc { color: #1d2330; text-decoration: none }
a.toc::after { content: " … " target-counter(attr(href url), page) }
</style></head><body>
<h1>{{.Title}}</h1>
<p>Period {{.Period}}. Generated {{.Generated}}.</p>
<h2>Contents</h2>
<ol class="toc">
{{range .Partners}}<li><a class="toc" href="#{{.Anchor}}">Partner {{.Org}}</a></li>
{{end}}<li><a class="toc" href="#d-lending">Lending matrix and balance</a></li>
{{if .HasFluids}}<li><a class="toc" href="#d-fluids">Fluids</a></li>
{{end}}<li><a class="toc" href="#d-adoption">Adoption</a></li>
<li><a class="toc" href="#d-chargeback">Chargeback</a></li>
<li><a class="toc" href="#d-charts">Charts</a></li>
</ol>
<p class="note">Units are never added: GPU-hours, kWh and each currency have their own columns. GPU-hours are the gateway's cost unit; the guaranteed capacity is the reserved slots of the partner's share of each pool times the hours of the period. Latency is end-to-end: the gateway does not record queue time.</p>
<div style="break-after: page"></div>
{{range .Partners}}<section class="partner">
<h2 id="{{.Anchor}}">Partner {{.Org}}</h2>
<p class="note">Share {{.ShareS}}{{if .CostCenter}}. Cost centre {{.CostCenter}}{{end}}.</p>
<table>
<tr><th>Guarantee</th><th class="n">GPU-hours</th></tr>
<tr><td>Guaranteed capacity (share × C × hours)</td><td class="n">{{.GuaranteedS}}</td></tr>
<tr><td>Used within the share</td><td class="n">{{.UsedS}} ({{.UtilS}} of the guarantee)</td></tr>
<tr><td>Borrowed above the share</td><td class="n">{{.BorrowedS}}</td></tr>
<tr><td>… of which from other partners' idle capacity</td><td class="n">{{.FromOthersS}}</td></tr>
<tr><td>… of which on its own idle reservation</td><td class="n">{{.SelfS}}</td></tr>
<tr><td>Lent: its idle reservation used by others</td><td class="n">{{.LentS}}</td></tr>
<tr><td>Net balance (lent − borrowed from others)</td><td class="n">{{.NetS}}</td></tr>
<tr><td>Inference GPU-hours</td><td class="n">{{.GPUS}} ({{.ChangeS}})</td></tr>
<tr><td>Requests</td><td class="n">{{.Requests}}</td></tr>
</table>
{{if $.HasFluids}}<h3>Fluids, on measured energy</h3>
<table>
<tr><th>Item</th><th class="n">Value</th></tr>
<tr><td>Energy (IT)</td><td class="n">{{.KWhS}} kWh</td></tr>
<tr><td>… of which idle, allocated to the partner</td><td class="n">{{.KWhIdleS}} kWh</td></tr>
<tr><td>Fluids cost (× PUE)</td><td class="n">{{.CostS}}</td></tr>
</table>{{end}}
<h3>The guarantee held?</h3>
<table>
<tr><th>Requests</th><th>End-to-end latency</th></tr>
<tr><td>Within the share</td><td>{{.LatIn}}</td></tr>
<tr><td>Borrowing</td><td>{{.LatBorrow}}</td></tr>
</table>
<h3>Adoption</h3>
<p>{{.ActiveUsers}} active people over the period.</p>
</section>
{{end}}
<h2 id="d-lending">Lending matrix and balance</h2>
{{if .Matrix}}<table><tr><th>Lender</th><th>Borrower</th><th class="n">GPU-hours</th></tr>
{{range .Matrix}}<tr><td>{{.Lender}}</td><td>{{.Borrower}}</td><td class="n">{{.GPUHours}}</td></tr>
{{end}}</table>{{else}}<p>No lending between partners in this period, or the matrix is not part of this reader's view.</p>{{end}}
{{if .Unattributed}}<p class="note">{{.Unattributed}} GPU-hours were borrowed while no partner had idle reservation: they are not charged to any lender.</p>{{end}}
{{if .HasFluids}}<h2 id="d-fluids">Fluids</h2>
<table><tr><th>Partner</th><th class="n">Energy kWh</th><th class="n">of which idle</th><th class="n">Cost</th></tr>
{{range .Partners}}<tr><td>{{.Org}}</td><td class="n">{{.KWhS}}</td><td class="n">{{.KWhIdleS}}</td><td class="n">{{.CostS}}</td></tr>
{{end}}</table>{{end}}
<h2 id="d-adoption">Adoption</h2>
{{if .Adoption}}<p>{{.AdoptionUsers}} active people over the period.</p>
<table><tr><th>Week of</th><th class="n">Active people</th><th class="n">Requests</th></tr>
{{range .Adoption.Weekly}}<tr><td>{{.Day}}</td><td class="n">{{.ActiveUsers}}</td><td class="n">{{.Requests}}</td></tr>
{{end}}</table>
<h3>Models</h3>
<table><tr><th>Model</th><th class="n">Active people</th><th class="n">Requests</th></tr>
{{range .Adoption.Models}}<tr><td>{{.Model}}</td><td class="n">{{.Users}}</td><td class="n">{{.Requests}}</td></tr>
{{end}}</table>{{else}}<p>No adoption figures.</p>{{end}}
<h2 id="d-chargeback">Chargeback</h2>
{{if .Chargeback}}<table><tr><th>Month</th><th>Cost centre</th><th class="n">GPU-hours</th><th class="n">kWh</th><th class="n">Fluids</th><th class="n">Provider spend</th></tr>
{{range .Chargeback}}<tr><td>{{.Period}}</td><td>{{.CostCenter}}</td><td class="n">{{.GPUHours}}</td><td class="n">{{.KWh}}</td><td class="n">{{.Fluids}}</td><td class="n">{{.Provider}}</td></tr>
{{end}}</table>{{else}}<p>No chargeback rows.</p>{{end}}
<h2 id="d-charts">Charts</h2>
<div>{{.Pie}}</div>
<div>{{.Bars}}</div>
<div>{{.Series}}</div>
</body></html>
`))

// writeDecisionPDF typesets the report.
func writeDecisionPDF(w io.Writer, d *decisionData) error {
	var src bytes.Buffer
	if err := decisionTmpl.Execute(&src, d.view()); err != nil {
		return err
	}
	doc, err := html2pdf.Export(src.String(), html2pdf.Options{Title: "Decision report " + d.From.UTC().Format("2006-01-02")})
	if err != nil {
		return err
	}
	return doc.Write(w)
}

// ---- the endpoint ----

// decisionReport answers GET /admin/decision?from=&to=&format=pdf|xlsx|json.
func (s *Server) decisionReport(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.anyOrgView() {
		forbidden(w)
		return
	}
	q := r.URL.Query()
	from, err1 := ParseDay(q.Get("from"))
	to, err2 := ParseDay(q.Get("to"))
	if err1 != nil || err2 != nil || !from.Before(to) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "from and to are required (RFC 3339 or YYYY-MM-DD), from < to")
		return
	}
	if to.Sub(from) > MaxDecisionSpan {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "a decision report covers at most 366 days")
		return
	}
	format := q.Get("format")
	if format == "" {
		format = DecisionPDF
	}
	if format != DecisionPDF && format != DecisionXLSX && format != FormatJSON {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "format: want pdf, xlsx or json")
		return
	}
	d, err := s.buildDecision(r, p, from, to)
	if errors.Is(err, store.ErrTooMany) || errors.Is(err, lending.ErrTooMany) {
		apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "too many requests in the period; shorten it")
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	var out bytes.Buffer
	ctype := ""
	switch format {
	case FormatJSON:
		ctype = "application/json"
		err = json.NewEncoder(&out).Encode(d.json())
	case DecisionXLSX:
		ctype = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		err = writeDecisionXLSX(&out, d)
	default:
		ctype = "application/pdf"
		err = writeDecisionPDF(&out, d)
	}
	if err != nil {
		s.log.Error("decision report", "format", format, "err", err)
		apiError(w, http.StatusInternalServerError, "server_error", "the report could not be written")
		return
	}
	s.auditDecision(r.Context(), p.name, "GET /admin/decision", format, from, to, out.Len(), http.StatusOK)
	h := w.Header()
	h.Set("Content-Type", ctype)
	if format != FormatJSON {
		h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", decisionName(from, to, format)))
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(out.Bytes())
}

func decisionName(from, to time.Time, format string) string {
	return "decision-" + from.UTC().Format("2006-01-02") + "_" + to.UTC().Format("2006-01-02") + "." + format
}

// json is the report as data.
func (d *decisionData) json() map[string]any {
	return map[string]any{
		"from": d.From.UTC(), "to": d.To.UTC(), "generated": d.Generated, "partners": d.Partners, "matrix": d.Matrix,
		"unattributed_gpu_hours": d.Unattributed, "chargeback": d.Chargeback, "currency": d.Currency,
	}
}

// auditDecision records the generation or the delivery of a report: who, which
// period, how big. Never the content.
func (s *Server) auditDecision(ctx context.Context, actor, action, format string, from, to time.Time, size, status int) {
	after, _ := json.Marshal(map[string]any{"format": format, "from": from.UTC().Format("2006-01-02"), "to": to.UTC().Format("2006-01-02"), "bytes": size})
	s.appendAudit(ctx, store.AuditEntry{Kind: "export", Actor: actor, Roles: "[]", Action: action, Target: "decision-report",
		Before: "null", After: string(after), Status: status})
}
