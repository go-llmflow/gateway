package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"math"
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
// go-plot. PDF is typeset by go-pdfkit/html2pdf (v0.4.0): A4 pages, page
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
	// Admission wait (the gateway's fair-share queue), in share and borrowing:
	// the evidence that the guarantee held.
	WaitInShare, WaitBorrowing lending.Latency
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
	CurrencyDecimals              int    // its minor unit
	MinUsers                      int    // the small-cell threshold of the adoption figures
	HasFluids                     bool
	Adoption                      *store.Adoption
	Chargeback                    []ChargebackRow
	// EnergyRecon compares Slurm's energy (sacct) with the DCGM energy
	// attributed, per partner; ReconNote says why it is empty when the input
	// is set and unreadable.
	EnergyRecon []EnergyReconRow
	ReconNote   string
	HasRecon    bool
	// Quotas: soft-quota events, hard refusals, workload days and
	// oversubscription (#97); Variance: budget against actual per cost centre.
	Quotas        *QuotaReport
	Variance      []VarianceRow
	VarianceLimit float64
	Daily         []chartPoint
	Pie           []chartSlice
	Bars          []chartBar
}

// buildDecision gathers the figures from the reports p may read.
func (s *Server) buildDecision(r *http.Request, p *principal, from, to time.Time) (*decisionData, error) {
	d := &decisionData{From: from, To: to, PrevFrom: from.Add(-to.Sub(from)), Generated: s.now().UTC(), MinUsers: s.adoptionMin(), CurrencyDecimals: 2}
	if s.cfg.Fluids != nil {
		d.CurrencyDecimals = s.cfg.Fluids.CurrencyDecimals
	}
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
			x.WaitInShare, x.WaitBorrowing = o.WaitInShare, o.WaitBorrow
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
	// Slurm's energy against the DCGM energy attributed (#97).
	if s.cfg.Slurm.SacctFile != "" {
		d.HasRecon = true
		slurmJ, rep, cov, err := s.sacctEnergy(from, to)
		dcgm := map[string]float64{}
		for o, x := range byOrg {
			dcgm[o] = x.KWh
		}
		switch {
		case err != nil:
			s.log.Error("decision report: slurm.sacct_file unreadable", "err", err)
			d.ReconNote = "The Slurm energy input (slurm.sacct_file) could not be read: " + clip(err.Error(), 160)
			d.EnergyRecon = reconcileEnergy(nil, dcgm)
		default:
			note := coverageText(cov, rep)
			if !p.reportView() { // the job counts of other partners are not this reader's
				note = "Slurm file: jobs from " + cov.Earliest.Format("2006-01-02 15:04") + " to " + cov.Latest.Format("2006-01-02 15:04") + " UTC."
			}
			if !cov.Covers {
				// A file for another period: a difference against it would be
				// a confident wrong number.
				d.ReconNote = "The Slurm file does not cover the report period: no difference is shown. " + note
				d.EnergyRecon = reconcileEnergy(nil, dcgm)
				break
			}
			d.ReconNote = note
			visibleJ := map[string]float64{}
			for o, j := range slurmJ {
				if visible(o) {
					visibleJ[o] = j
				}
			}
			d.EnergyRecon = reconcileEnergy(visibleJ, dcgm)
		}
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
		if d.Adoption, err = s.adoptionOf(r.Context(), as, from, to, orgs); err != nil {
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
	if d.Quotas, err = s.quotaSection(r.Context(), p, from, to); err != nil {
		return nil, err
	}
	d.VarianceLimit = s.varianceThreshold()
	if d.Variance, err = s.varianceReport(r.Context(), p, d.Chargeback, from, to); err != nil {
		return nil, err
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
		"Admission wait p50 in share (ms)", "Admission wait p95 in share (ms)", "Admission wait p50 borrowing (ms)", "Admission wait p95 borrowing (ms)",
		"Latency p50 in share (ms)", "Latency p95 in share (ms)", "Latency p50 borrowing (ms)", "Latency p95 borrowing (ms)", "Active people")
	if err != nil {
		return err
	}
	for _, x := range d.Partners {
		if err := sh.AppendRow(xlsx.String(x.Org), xlsx.String(x.CostCenter), xlsx.Number(x.Share).Format(xlsx.Percent(1)), num(x.Guaranteed),
			num(x.UsedInShare), num(x.Borrowed), num(x.BorrowedFromOthers), num(x.Lent), num(x.Net), xlsx.Number(x.Utilisation).Format(xlsx.Percent(1)),
			num(x.GPUHours), num(x.PrevGPUHours), xlsx.Number(float64(x.Requests)), num(x.KWh), num(x.KWhIdle), num(x.FluidsCost),
			num(x.WaitInShare.P50), num(x.WaitInShare.P95), num(x.WaitBorrowing.P50), num(x.WaitBorrowing.P95),
			num(x.InShare.P50), num(x.InShare.P95), num(x.Borrowing.P50), num(x.Borrowing.P95), usersCell(x.ActiveUsers, d.MinUsers)); err != nil {
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
			num(c.GPUHours), num(c.KWh), money(c.FluidsCostMinor, d.CurrencyDecimals), xlsx.String(c.FluidsCurrency),
			money(c.ProviderCostMinor, ProviderDecimals), xlsx.String(c.ProviderCurrency)); err != nil {
			return err
		}
	}
	if d.HasRecon {
		er, err := sheet("Energy reconciliation", []float64{24, 16, 16, 18, 14}, "Partner", "Slurm energy (kWh, sacct)", "DCGM energy attributed (kWh)",
			"Difference DCGM - Slurm (kWh)", "Difference (% of Slurm)")
		if err != nil {
			return err
		}
		for _, note := range []string{reconAssumption, d.ReconNote} {
			if note == "" {
				continue
			}
			if err := er.AppendRow(xlsx.String(note)); err != nil {
				return err
			}
		}
		for _, r := range d.EnergyRecon {
			slurm, diff, pct := xlsx.String("no Slurm data"), xlsx.String("-"), xlsx.String("n/a")
			if r.HasSlurm {
				slurm = num(r.SlurmKWh)
			}
			if r.Compared {
				diff = num(r.DiffKWh)
			}
			if r.DiffPct != nil {
				pct = xlsx.Number(*r.DiffPct / 100).Format(xlsx.Percent(1))
			}
			if err := er.AppendRow(xlsx.String(r.Org), slurm, num(r.DCGMKWh), diff, pct); err != nil {
				return err
			}
		}
	}
	ad, err := sheet("Adoption", []float64{16, 14}, "Series", "Period or model", "Active people", "Requests")
	if err != nil {
		return err
	}
	if d.Adoption != nil {
		av := suppressAdoption(d.Adoption, d.MinUsers)
		cnt := func(p *int64) xlsx.Cell {
			if p == nil {
				return xlsx.String("suppressed")
			}
			return xlsx.Number(float64(*p))
		}
		emit := func(series string, ps []adoptionPoint) error {
			for _, x := range ps {
				if err := ad.AppendRow(xlsx.String(series), xlsx.String(x.Period), cnt(x.ActiveUsers), cnt(x.Requests)); err != nil {
					return err
				}
			}
			return nil
		}
		if err := emit("day", av.Daily); err != nil {
			return err
		}
		if err := emit("week", av.Weekly); err != nil {
			return err
		}
		for _, m := range av.Models {
			if err := ad.AppendRow(xlsx.String("model"), xlsx.String(m.Model), cnt(m.ActiveUsers), cnt(m.Requests)); err != nil {
				return err
			}
		}
	}
	if err := writeQuotaSheets(d, sheet); err != nil {
		return err
	}
	data, err := wb.Write()
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// usersCell is a count of distinct people, or the words for a small cell.
func usersCell(n int64, k int) xlsx.Cell {
	if hidden(n, k) {
		return xlsx.String("fewer than " + strconv.Itoa(k))
	}
	return xlsx.Number(float64(n))
}

// usersLabel is usersCell as text.
func usersLabel(n int64, k int) string {
	if hidden(n, k) {
		return "fewer than " + strconv.Itoa(k)
	}
	return strconv.FormatInt(n, 10)
}

// money is an amount in minor units as a typed cell with the currency's decimals.
func money(minor int64, decimals int) xlsx.Cell {
	return xlsx.Number(float64(minor) / math.Pow10(decimals)).Format(xlsx.Decimal(decimals))
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
	AdoptionUsers            string
	Weekly                   []adoptionRowView
	Models                   []adoptionRowView
	HasAdoption              bool
	MinUsers                 int
	Chargeback               []chargebackView
	HasRecon                 bool
	ReconNote                string
	ReconAssumption          string
	Recon                    []reconView
	Pie, Bars, Series        template.HTML
	Quota                    quotaView
	HasVariance              bool
	Variance                 []varianceView
	VarianceNote             string
}

type partnerView struct {
	partner
	Anchor                                                                 string
	ShareS, GuaranteedS, UsedS, BorrowedS, FromOthersS, SelfS, LentS, NetS string
	UtilS, GPUS, PrevS, ChangeS, KWhS, KWhIdleS, CostS                     string
	LatIn, LatBorrow, WaitIn, WaitBorrow, UsersS                           string
}

// adoptionRowView is a week or a model, counts as text ("suppressed" for a
// small cell).
type adoptionRowView struct{ Name, Users, Requests string }

// reconView is one partner of the energy reconciliation, as text.
type reconView struct{ Org, Slurm, DCGM, Diff, Pct string }

func (d *decisionData) reconViews() []reconView {
	var out []reconView
	for _, r := range d.EnergyRecon {
		v := reconView{Org: r.Org, Slurm: "no Slurm data", DCGM: fmtNum(r.DCGMKWh), Diff: "-", Pct: "n/a"}
		if r.HasSlurm {
			v.Slurm = fmtNum(r.SlurmKWh)
		}
		if r.Compared {
			v.Diff = signed(r.DiffKWh)
		}
		if r.DiffPct != nil {
			v.Pct = signed(*r.DiffPct) + " %"
		}
		out = append(out, v)
	}
	return out
}

// signed is fmtNum with an explicit + for a positive number.
func signed(v float64) string {
	if v > 0 {
		return "+" + fmtNum(v)
	}
	return fmtNum(v)
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
		Generated: d.Generated.Format("2006-01-02 15:04 UTC"), HasFluids: d.HasFluids, MinUsers: d.MinUsers,
	}
	if d.Adoption != nil {
		v.HasAdoption = true
		av := suppressAdoption(d.Adoption, d.MinUsers)
		cnt := func(p *int64) string {
			if p == nil {
				return "suppressed"
			}
			return strconv.FormatInt(*p, 10)
		}
		v.AdoptionUsers = usersLabel(d.Adoption.Users, d.MinUsers)
		for _, x := range av.Weekly {
			v.Weekly = append(v.Weekly, adoptionRowView{x.Period, cnt(x.ActiveUsers), cnt(x.Requests)})
		}
		for _, m := range av.Models {
			v.Models = append(v.Models, adoptionRowView{m.Model, cnt(m.ActiveUsers), cnt(m.Requests)})
		}
	}
	for _, x := range d.Partners {
		change := "no previous period"
		if x.PrevGPUHours > 0 {
			change = strconv.FormatFloat(100*(x.GPUHours-x.PrevGPUHours)/x.PrevGPUHours, 'f', 1, 64) + " % against the previous period"
		}
		cost := formatMinor(minorUnits(x.FluidsCost, d.CurrencyDecimals), d.CurrencyDecimals)
		if d.Currency != "" {
			cost += " " + d.Currency
		}
		v.Partners = append(v.Partners, partnerView{
			partner: x, Anchor: "p-" + x.Org, ShareS: pctS(x.Share), GuaranteedS: fmtNum(x.Guaranteed), UsedS: fmtNum(x.UsedInShare),
			BorrowedS: fmtNum(x.Borrowed), FromOthersS: fmtNum(x.BorrowedFromOthers), SelfS: fmtNum(x.SelfServed), LentS: fmtNum(x.Lent),
			NetS: fmtNum(x.Net), UtilS: pctS(x.Utilisation), GPUS: fmtNum(x.GPUHours), PrevS: fmtNum(x.PrevGPUHours), ChangeS: change,
			KWhS: fmtNum(x.KWh), KWhIdleS: fmtNum(x.KWhIdle), CostS: cost, LatIn: latS(x.InShare), LatBorrow: latS(x.Borrowing),
			WaitIn: latS(x.WaitInShare), WaitBorrow: latS(x.WaitBorrowing), UsersS: usersLabel(x.ActiveUsers, d.MinUsers),
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
			prov = c.ProviderCost + " " + c.ProviderCurrency
		}
		fl := ""
		if c.FluidsCurrency != "" || c.FluidsCostMinor != 0 {
			fl = c.FluidsCost + " " + c.FluidsCurrency
		}
		v.Chargeback = append(v.Chargeback, chargebackView{c.From.UTC().Format("2006-01"), c.CostCenter, fmtNum(c.GPUHours), fmtNum(c.KWh), fl, prov})
	}
	v.HasRecon, v.ReconNote, v.Recon, v.ReconAssumption = d.HasRecon, d.ReconNote, d.reconViews(), reconAssumption
	v.Quota = d.Quotas.view()
	v.HasVariance, v.Variance, v.VarianceNote = true, d.varianceViews(), varianceNote(d.VarianceLimit)
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
{{if .HasRecon}}<li><a class="toc" href="#d-recon">Energy reconciliation</a></li>
{{end}}<li><a class="toc" href="#d-chargeback">Chargeback</a></li>
<li><a class="toc" href="#d-variance">Budget against actual</a></li>
{{if .Quota.Has}}<li><a class="toc" href="#d-quotas">Quotas and profiles</a></li>
{{end}}
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
<tr><th>Requests</th><th>Admission wait (gateway queue)</th><th>End-to-end latency</th></tr>
<tr><td>Within the share</td><td>{{.WaitIn}}</td><td>{{.LatIn}}</td></tr>
<tr><td>Borrowing</td><td>{{.WaitBorrow}}</td><td>{{.LatBorrow}}</td></tr>
</table>
<h3>Adoption</h3>
<p>{{.UsersS}} active people over the period.</p>
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
{{if .HasAdoption}}<p>{{.AdoptionUsers}} active people over the period. Figures over fewer than {{.MinUsers}} people are not shown.</p>
<table><tr><th>Week of</th><th class="n">Active people</th><th class="n">Requests</th></tr>
{{range .Weekly}}<tr><td>{{.Name}}</td><td class="n">{{.Users}}</td><td class="n">{{.Requests}}</td></tr>
{{end}}</table>
<h3>Models</h3>
<table><tr><th>Model</th><th class="n">Active people</th><th class="n">Requests</th></tr>
{{range .Models}}<tr><td>{{.Name}}</td><td class="n">{{.Users}}</td><td class="n">{{.Requests}}</td></tr>
{{end}}</table>{{else}}<p>No adoption figures.</p>{{end}}
{{if .HasRecon}}<h2 id="d-recon">Energy reconciliation: Slurm against DCGM</h2>
<p class="note">Slurm's energy for the partner's accounts (sacct ConsumedEnergyRaw, which reflects a job's real energy only for an exclusive allocation) against the IT energy the gateway attributed from the DCGM counters. The difference is DCGM minus Slurm, in kWh and as a percentage of Slurm's figure.</p>
<p class="note">{{.ReconAssumption}}</p>
{{if .ReconNote}}<p>{{.ReconNote}}</p>{{end}}
<table><tr><th>Partner</th><th class="n">Slurm kWh</th><th class="n">DCGM kWh</th><th class="n">Difference kWh</th><th class="n">Difference</th></tr>
{{range .Recon}}<tr><td>{{.Org}}</td><td class="n">{{.Slurm}}</td><td class="n">{{.DCGM}}</td><td class="n">{{.Diff}}</td><td class="n">{{.Pct}}</td></tr>
{{end}}</table>{{end}}
<h2 id="d-chargeback">Chargeback</h2>
{{if .Chargeback}}<table><tr><th>Month</th><th>Cost centre</th><th class="n">GPU-hours</th><th class="n">kWh</th><th class="n">Fluids</th><th class="n">Provider spend</th></tr>
{{range .Chargeback}}<tr><td>{{.Period}}</td><td>{{.CostCenter}}</td><td class="n">{{.GPUHours}}</td><td class="n">{{.KWh}}</td><td class="n">{{.Fluids}}</td><td class="n">{{.Provider}}</td></tr>
{{end}}</table>{{else}}<p>No chargeback rows.</p>{{end}}
<h2 id="d-variance">Budget against actual</h2>
<p class="note">{{.VarianceNote}}</p>
{{if .Variance}}<table><tr><th>Month</th><th>Cost centre</th><th>Unit</th><th class="n">Budgeted</th><th class="n">Estimated</th><th class="n">Actual</th><th class="n">Against budget</th><th class="n">Against estimate</th><th>Flag</th></tr>
{{range .Variance}}<tr><td>{{.Month}}</td><td>{{.CostCenter}}</td><td>{{.Unit}}</td><td class="n">{{.Budgeted}}</td><td class="n">{{.Estimated}}</td><td class="n">{{.Actual}}</td><td class="n">{{.BudgetVar}}</td><td class="n">{{.EstimateVar}}</td><td>{{.Flag}}</td></tr>
{{end}}</table>{{else}}<p>No budget, profile allowance or spend in this period.</p>{{end}}
{{if .Quota.Has}}<h2 id="d-quotas">Quotas and profiles</h2>
<p class="note">{{range .Quota.Notes}}{{.}} {{end}}</p>
<h3>Soft-quota notifications</h3>
{{if .Quota.Soft}}<table><tr><th>Partner</th><th>Lab</th><th>Window</th><th class="n">Threshold</th><th class="n">Events</th><th class="n">People</th></tr>
{{range .Quota.Soft}}<tr><td>{{.A}}</td><td>{{.B}}</td><td>{{.C}}</td><td class="n">{{.D}}</td><td class="n">{{.E}}</td><td class="n">{{.F}}</td></tr>
{{end}}</table>{{else}}<p>None.</p>{{end}}
<h3>Hard refusals (429)</h3>
{{if .Quota.Refusals}}<table><tr><th>Partner</th><th>Lab</th><th>Profile</th><th>Kind</th><th class="n">Refusals</th></tr>
{{range .Quota.Refusals}}<tr><td>{{.A}}</td><td>{{.B}}</td><td>{{.C}}</td><td>{{.D}}</td><td class="n">{{.E}}</td></tr>
{{end}}</table>{{else}}<p>None.</p>{{end}}
<h3>Workload days: assigned and used</h3>
{{if .Quota.Profiles}}<table><tr><th>Partner</th><th>Lab</th><th>Profile</th><th class="n">People</th><th class="n">Assigned days</th><th class="n">Used days</th><th class="n">Used / assigned</th></tr>
{{range .Quota.Profiles}}<tr><td>{{.A}}</td><td>{{.B}}</td><td>{{.C}}</td><td class="n">{{.D}}</td><td class="n">{{.E}}</td><td class="n">{{.F}}</td><td class="n">{{.G}}</td></tr>
{{end}}</table>{{else}}<p>None.</p>{{end}}
<h3>Oversubscription over time</h3>
{{if .Quota.Oversub}}<table><tr><th>Partner</th><th class="n">Days sampled</th><th class="n">Days above 1.0</th><th class="n">Peak ratio</th><th class="n">Average ratio</th></tr>
{{range .Quota.Oversub}}<tr><td>{{.A}}</td><td class="n">{{.B}}</td><td class="n">{{.C}}</td><td class="n">{{.D}}</td><td class="n">{{.E}}</td></tr>
{{end}}</table>{{else}}<p>None.</p>{{end}}{{end}}
<h2 id="d-charts">Charts</h2>
<div>{{.Pie}}</div>
<div>{{.Bars}}</div>
<div>{{.Series}}</div>
</body></html>
`))

// decisionPDFOptions are the options the report is typeset with. Fetch is
// html2pdf.Offline: the report has no sub-resource but data: URLs, so the
// renderer makes no network request at all. The names in the report are
// escaped (decisionTmpl); this is the second barrier, so that a name which
// somehow became markup could still not make the gateway fetch a URL from
// inside the site's network.
func decisionPDFOptions(d *decisionData) html2pdf.Options {
	return html2pdf.Options{
		Title: "Decision report " + d.From.UTC().Format("2006-01-02"),
		Fetch: html2pdf.Offline,
	}
}

// writeDecisionPDF typesets the report.
func writeDecisionPDF(w io.Writer, d *decisionData) error {
	var src bytes.Buffer
	if err := decisionTmpl.Execute(&src, d.view()); err != nil {
		return err
	}
	doc, err := html2pdf.Export(src.String(), decisionPDFOptions(d))
	if err != nil {
		return err
	}
	return doc.Write(w)
}

// ---- bounds ----

// Media types of the two documents.
const (
	decisionPDFType  = "application/pdf"
	decisionXLSXType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
)

// Bounds on a decision report (#111). The layout engine's time and memory
// grow with what it is given, and one partner's page is a page: the number of
// partners, of table rows and of the bytes written are bounded, and a report
// beyond them is refused with a clear error, never half-written.
const (
	MaxDecisionPartners   = 100
	MaxDecisionMatrix     = 2000
	MaxDecisionChargeback = 3000
	MaxDecisionAdoption   = 400  // weekly and model rows together
	MaxDecisionQuotas     = 3000 // soft, refusal and profile rows together
	MaxDecisionOversub    = 1500 // sampled days of all partners
	// MaxDecisionBytes bounds a document: a PDF of the largest accepted report
	// is a few MiB.
	MaxDecisionBytes = 16 << 20
	// decisionRenderTimeout bounds the typesetting.
	decisionRenderTimeout = 2 * time.Minute
)

// checkBounds refuses a report too large to typeset.
func (d *decisionData) checkBounds() error {
	adopt := 0
	if d.Adoption != nil {
		adopt = len(d.Adoption.Weekly) + len(d.Adoption.Models)
	}
	switch {
	case len(d.Partners) > MaxDecisionPartners:
		return fmt.Errorf("the report has %d partners: at most %d", len(d.Partners), MaxDecisionPartners)
	case len(d.Matrix) > MaxDecisionMatrix:
		return fmt.Errorf("the lending matrix has %d cells: at most %d", len(d.Matrix), MaxDecisionMatrix)
	case len(d.Chargeback) > MaxDecisionChargeback:
		return fmt.Errorf("the chargeback has %d rows: at most %d; shorten the period", len(d.Chargeback), MaxDecisionChargeback)
	case adopt > MaxDecisionAdoption:
		return fmt.Errorf("the adoption tables have %d rows: at most %d", adopt, MaxDecisionAdoption)
	case len(d.Variance) > MaxVarianceRows:
		return fmt.Errorf("the variance table has %d rows: at most %d; shorten the period", len(d.Variance), MaxVarianceRows)
	}
	if q := d.Quotas; q != nil {
		if n := len(q.Soft) + len(q.Refusals) + len(q.Profiles); n > MaxDecisionQuotas {
			return fmt.Errorf("the quota tables have %d rows: at most %d; shorten the period", n, MaxDecisionQuotas)
		}
		if len(q.Oversub) > MaxDecisionOversub {
			return fmt.Errorf("the oversubscription table has %d rows: at most %d", len(q.Oversub), MaxDecisionOversub)
		}
	}
	return nil
}

// renderDecision writes a report in a format, bounded in time and size.
func renderDecision(format string, d *decisionData) ([]byte, error) {
	if err := d.checkBounds(); err != nil {
		return nil, err
	}
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		var out bytes.Buffer
		var err error
		if format == DecisionXLSX {
			err = writeDecisionXLSX(&out, d)
		} else {
			err = writeDecisionPDF(&out, d)
		}
		done <- result{out.Bytes(), err}
	}()
	select {
	case r := <-done:
		if r.err == nil && len(r.b) > MaxDecisionBytes {
			return nil, fmt.Errorf("the %s is %d bytes: at most %d", format, len(r.b), MaxDecisionBytes)
		}
		return r.b, r.err
	case <-time.After(decisionRenderTimeout):
		// The layout engine cannot be interrupted: the goroutine ends with it,
		// and the bounds above are what keep that short.
		return nil, fmt.Errorf("typesetting the %s took longer than %s", format, decisionRenderTimeout)
	}
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
	release, ok := heavy(w)
	if !ok {
		return
	}
	defer release()
	// The whole build is bounded, whatever the period.
	ctx, cancel := context.WithTimeout(r.Context(), 2*ReportTimeout)
	defer cancel()
	r = r.WithContext(ctx)
	d, err := s.buildDecision(r, p, from, to)
	if errors.Is(err, store.ErrTooMany) || errors.Is(err, lending.ErrTooMany) {
		apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "too many requests in the period; shorten it")
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "the report took too long; shorten the period")
		return
	}
	if err != nil {
		storeError(w, err)
		return
	}
	if err := d.checkBounds(); err != nil {
		apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", err.Error())
		return
	}
	var out bytes.Buffer
	ctype := ""
	switch format {
	case FormatJSON:
		ctype = "application/json"
		err = json.NewEncoder(&out).Encode(d.json())
	default:
		var body []byte
		body, err = renderDecision(format, d)
		out.Write(body)
		ctype = decisionPDFType
		if format == DecisionXLSX {
			ctype = decisionXLSXType
		}
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
		"energy_reconciliation": d.EnergyRecon, "energy_reconciliation_note": d.ReconNote, "energy_reconciliation_assumption": reconAssumption,
		"quotas": d.Quotas, "variance": d.Variance, "variance_threshold_pct": d.VarianceLimit,
	}
}

// auditDecision records the generation or the delivery of a report: who, which
// period, how big. Never the content.
func (s *Server) auditDecision(ctx context.Context, actor, action, format string, from, to time.Time, size, status int) {
	after, _ := json.Marshal(map[string]any{"format": format, "from": from.UTC().Format("2006-01-02"), "to": to.UTC().Format("2006-01-02"), "bytes": size})
	s.appendAudit(ctx, store.AuditEntry{Kind: "export", Actor: actor, Roles: "[]", Action: action, Target: "decision-report",
		Before: "null", After: string(after), Status: status})
}
