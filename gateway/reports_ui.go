package gateway

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/slurm"
	"github.com/go-llmflow/gateway/store"
)

// The Reports page (#68): a period, a grouping over every report
// dimension, filters that drill down org → lab → team → project → user,
// the previous period of the same length for comparison, and server-side
// SVG charts. It reads GET /admin/spend in-process like every UI page, so
// what an org_admin sees is what the API shows them: their organisation
// only. Every amount is shown in its unit, one section per unit: GPU-hours
// and a currency are never added together.

// drillNext is the grouping a drill-down link opens.
var drillNext = map[string]string{"org": "lab", "lab": "team", "team": "project", "project": "user", "user": "model"}

// maxReportLines bounds the table of one unit section.
const maxReportLines = 500

// maxFilterValue bounds a filter value taken from the query.
const maxFilterValue = 256

type reportFilter struct {
	Dim, Value string
	Raw        string // the value as filtered ("" for none)
	Remove     string // the page's URL without this filter
}

type reportLine struct {
	Label, Link               string
	Requests                  int64
	Cost, Prev, Delta, Shared string
}

type reportSection struct {
	Unit               string // as shown: "GPU-hours" or a currency code
	Total, Prev, Delta string
	Requests           int64
	Pie, Bars, Series  template.HTML
	Lines              []reportLine
	Truncated          int
}

type reportsData struct {
	From, To, PrevFrom, PrevTo string
	Group                      string
	Dims                       []string
	Presets                    []reportFilter // Dim = label, Remove = link
	Filters                    []reportFilter
	Exports                    []reportFilter // Dim = label, Remove = link
	Sections                   []reportSection
	Upload                     string // the reconciliation form's action
	Recon                      []reconLine
	ReconNote                  string
}

// uiExport downloads an export of GET /admin/export (audited there).
func (s *Server) uiExport(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	q := r.URL.Query()
	v := url.Values{"from": {q.Get("from")}, "to": {q.Get("to")}, "format": {q.Get("format")}}
	rec := s.callAdminRecorded(r, p, "GET /admin/export", "/admin/export", v, nil, nil)
	if rec.status != http.StatusOK {
		s.render(w, "reports", view{Title: "Reports", User: p.name, CSRF: csrf, Error: apiMessage(rec.body.Bytes())})
		return
	}
	for _, k := range []string{"Content-Type", "Content-Disposition"} {
		w.Header().Set(k, rec.Header().Get(k))
	}
	_, _ = w.Write(rec.body.Bytes())
}

func displayUnit(u string) string {
	if u == "" {
		return "GPU-hours"
	}
	return u
}

func delta(cur, prev float64) string {
	switch {
	case prev == 0 && cur == 0:
		return "—"
	case prev == 0:
		return "new"
	}
	return fmt.Sprintf("%+.1f %%", 100*(cur-prev)/prev)
}

// fetchReport reads GET /admin/spend as JSON for p.
func (s *Server) fetchReport(r *http.Request, p *principal, from, to time.Time, dims []string) ([]store.ReportRow, string) {
	g := ""
	for i, d := range dims {
		if i > 0 {
			g += ","
		}
		g += d
	}
	q := url.Values{"from": {from.Format(time.RFC3339)}, "to": {to.Format(time.RFC3339)}, "group_by": {g}, "format": {FormatJSON}}
	st, raw := s.callAdmin(r, p, "GET /admin/spend", "/admin/spend", q, nil, nil)
	if st != http.StatusOK {
		return nil, apiMessage(raw)
	}
	var out struct {
		Rows []struct {
			store.ReportRow
			U string `json:"unit"`
		} `json:"rows"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "the report could not be read"
	}
	rows := make([]store.ReportRow, 0, len(out.Rows))
	for _, x := range out.Rows {
		row := x.ReportRow
		u, err := store.NormalizeUnit(x.U)
		if err != nil {
			continue
		}
		row.Unit = u
		rows = append(rows, row)
	}
	return rows, ""
}

func keep(rows []store.ReportRow, filters map[string]string) []store.ReportRow {
	out := rows[:0:0]
	for _, r := range rows {
		ok := true
		for d, v := range filters {
			if r.Dim(d) != v {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, r)
		}
	}
	return out
}

func (s *Server) uiReports(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	s.reportsPage(w, r, p, csrf, nil)
}

// uiReconcile takes an uploaded sreport output: the Reports page with the
// reconciliation table (format html), or an XLSX or PDF export with it.
func (s *Server) uiReconcile(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	var upload []byte
	if fhs := r.MultipartForm.File["sreport"]; len(fhs) == 1 {
		f, err := fhs[0].Open()
		if err == nil {
			upload, err = io.ReadAll(io.LimitReader(f, slurm.MaxBytes+1))
			f.Close()
		}
		if err != nil || len(upload) > slurm.MaxBytes {
			s.render(w, "reports", view{Title: "Reports", User: p.name, CSRF: csrf, Error: "the Slurm report could not be read (at most 1 MiB)"})
			return
		}
	}
	if upload == nil {
		s.render(w, "reports", view{Title: "Reports", User: p.name, CSRF: csrf, Error: "choose an sreport output to upload"})
		return
	}
	switch f := r.PostForm.Get("format"); f {
	case FormatXLSX, FormatPDF:
		q := r.URL.Query()
		v := url.Values{"from": {q.Get("from")}, "to": {q.Get("to")}, "format": {f}}
		rec := s.callAdminRecorded(r, p, "POST /admin/export", "/admin/export", v, upload, nil)
		if rec.status != http.StatusOK {
			s.render(w, "reports", view{Title: "Reports", User: p.name, CSRF: csrf, Error: apiMessage(rec.body.Bytes())})
			return
		}
		for _, k := range []string{"Content-Type", "Content-Disposition"} {
			w.Header().Set(k, rec.Header().Get(k))
		}
		_, _ = w.Write(rec.body.Bytes())
	default:
		s.reportsPage(w, r, p, csrf, upload)
	}
}

// reconLine is a row of the page's reconciliation table.
type reconLine struct {
	Org, Inference, Batch, Share string
}

func (s *Server) reportsPage(w http.ResponseWriter, r *http.Request, p *principal, csrf string, upload []byte) {
	q := r.URL.Query()
	now := s.now().UTC()
	today := now.Truncate(24 * time.Hour)
	from, to := today.AddDate(0, 0, -30), today.AddDate(0, 0, 1)
	var errMsg string
	if v := q.Get("from"); v != "" {
		t, err := ParseDay(v)
		if err != nil {
			errMsg = "from: want YYYY-MM-DD"
		} else {
			from = t
		}
	}
	if v := q.Get("to"); v != "" {
		t, err := ParseDay(v)
		if err != nil {
			errMsg = "to: want YYYY-MM-DD"
		} else {
			to = t
		}
	}
	group := q.Get("group")
	if group == "" {
		group = "org"
	}
	if g, err := store.ValidateDims([]string{group}); err != nil || len(g) != 1 {
		errMsg, group = "unknown grouping", "org"
	}
	filters := map[string]string{}
	for _, d := range store.ReportDims {
		if q.Has(d) {
			if v := q.Get(d); len(v) <= maxFilterValue {
				filters[d] = v
			}
		}
	}
	data := reportsData{Group: group, Dims: store.ReportDims}
	link := func(from, to time.Time, group string, filters map[string]string) string {
		v := url.Values{"from": {from.Format("2006-01-02")}, "to": {to.Format("2006-01-02")}, "group": {group}}
		for d, x := range filters {
			v.Set(d, x)
		}
		return "/ui/reports?" + v.Encode()
	}
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	for _, pr := range []struct {
		label    string
		from, to time.Time
	}{
		{"last 7 days", today.AddDate(0, 0, -6), today.AddDate(0, 0, 1)},
		{"last 30 days", today.AddDate(0, 0, -29), today.AddDate(0, 0, 1)},
		{"this month", monthStart, monthStart.AddDate(0, 1, 0)},
		{"last month", monthStart.AddDate(0, -1, 0), monthStart},
		{"this year", time.Date(now.Year(), 1, 1, 0, 0, 0, 0, time.UTC), time.Date(now.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)},
	} {
		data.Presets = append(data.Presets, reportFilter{Dim: pr.label, Remove: link(pr.from, pr.to, group, filters)})
	}
	fdims := make([]string, 0, len(filters))
	for d := range filters {
		fdims = append(fdims, d)
	}
	sort.Strings(fdims)
	for _, d := range fdims {
		rest := map[string]string{}
		for k, v := range filters {
			if k != d {
				rest[k] = v
			}
		}
		data.Filters = append(data.Filters, reportFilter{Dim: d, Value: labelOr(filters[d]), Raw: filters[d], Remove: link(from, to, group, rest)})
	}
	data.From, data.To = from.Format("2006-01-02"), to.Format("2006-01-02")
	data.Upload = uiUploadPath + "?" + url.Values{"from": {data.From}, "to": {data.To}, "group": {group}}.Encode()
	for _, f := range []string{FormatCSV, FormatXLSX, FormatPDF} {
		data.Exports = append(data.Exports, reportFilter{Dim: strings.ToUpper(f),
			Remove: "/ui/reports/export?" + url.Values{"from": {data.From}, "to": {data.To}, "format": {f}}.Encode()})
	}
	if errMsg == "" && !from.Before(to) {
		errMsg = "from must be before to"
	}
	if errMsg == "" && to.Sub(from) > store.MaxReportSpan {
		errMsg = "a report covers at most 366 days"
	}
	if errMsg != "" {
		s.render(w, "reports", view{Title: "Reports", User: p.name, CSRF: csrf, Error: errMsg, Data: data})
		return
	}
	span := to.Sub(from)
	prevFrom := from.Add(-span)
	data.PrevFrom, data.PrevTo = prevFrom.Format("2006-01-02"), from.Format("2006-01-02")

	dims, _ := store.ValidateDims(append([]string{group}, fdims...))
	cur, e1 := s.fetchReport(r, p, from, to, dims)
	prev, e2 := s.fetchReport(r, p, prevFrom, from, dims)
	sdims, _ := store.ValidateDims(append([]string{"day"}, fdims...))
	days, e3 := s.fetchReport(r, p, from, to, sdims)
	for _, e := range []string{e1, e2, e3} {
		if e != "" {
			s.render(w, "reports", view{Title: "Reports", User: p.name, CSRF: csrf, Error: e, Data: data})
			return
		}
	}
	cur = store.Regroup(keep(cur, filters), []string{group})
	prev = store.Regroup(keep(prev, filters), []string{group})
	days = store.Regroup(keep(days, filters), []string{"day"})

	units := map[string]bool{}
	for _, x := range cur {
		units[x.Unit] = true
	}
	for _, x := range prev {
		units[x.Unit] = true
	}
	order := make([]string, 0, len(units))
	for u := range units {
		order = append(order, u)
	}
	sort.Strings(order) // "" (GPU-hours) first
	var charts chartRenderer = plotCharts{}
	next := drillNext[group]
	for _, u := range order {
		sec := reportSection{Unit: displayUnit(u)}
		prevBy := map[string]float64{}
		var total, ptotal float64
		for _, x := range prev {
			if x.Unit == u {
				prevBy[x.Dim(group)] += x.Cost
				ptotal += x.Cost
			}
		}
		var slices []chartSlice
		var bars []chartBar
		var mine []store.ReportRow
		for _, x := range cur {
			if x.Unit == u {
				mine = append(mine, x)
				total += x.Cost
				sec.Requests += x.Requests
			}
		}
		sort.SliceStable(mine, func(i, j int) bool { return mine[i].Cost > mine[j].Cost })
		for _, x := range mine {
			label := x.Dim(group)
			slices = append(slices, chartSlice{Label: label, Value: x.Cost})
			bars = append(bars, chartBar{Label: label, Current: x.Cost, Previous: prevBy[label]})
			if len(sec.Lines) >= maxReportLines {
				sec.Truncated++
				continue
			}
			line := reportLine{Label: labelOr(label), Requests: x.Requests, Cost: fmtNum(x.Cost),
				Prev: fmtNum(prevBy[label]), Delta: delta(x.Cost, prevBy[label])}
			if total > 0 {
				line.Shared = fmt.Sprintf("%.1f %%", 100*x.Cost/total)
			}
			if next != "" {
				f := map[string]string{group: label}
				for k, v := range filters {
					f[k] = v
				}
				line.Link = link(from, to, next, f)
			}
			sec.Lines = append(sec.Lines, line)
		}
		// Groups that spent only in the previous period.
		seen := make(map[string]bool, len(mine))
		for _, x := range mine {
			seen[x.Dim(group)] = true
		}
		gone := make([]string, 0)
		for label := range prevBy {
			if !seen[label] {
				gone = append(gone, label)
			}
		}
		sort.Strings(gone)
		for _, label := range gone {
			v := prevBy[label]
			if len(sec.Lines) < maxReportLines {
				sec.Lines = append(sec.Lines, reportLine{Label: labelOr(label), Cost: "0", Prev: fmtNum(v), Delta: delta(0, v)})
			}
		}
		sec.Total, sec.Prev, sec.Delta = fmtNum(total), fmtNum(ptotal), delta(total, ptotal)
		var points []chartPoint
		byDay := map[string]float64{}
		for _, x := range days {
			if x.Unit == u {
				byDay[x.Day] += x.Cost
			}
		}
		for d := from; d.Before(to) && len(points) <= 400; d = d.AddDate(0, 0, 1) {
			k := d.Format("2006-01-02")
			points = append(points, chartPoint{X: k, Y: byDay[k]})
		}
		title := fmt.Sprintf("%s by %s, %s to %s", sec.Unit, group, data.From, data.To)
		sec.Pie = charts.Pie("Share of "+title, sec.Unit, slices)
		sec.Bars = charts.Bars(title+" against "+data.PrevFrom+" to "+data.PrevTo, sec.Unit, bars)
		sec.Series = charts.Series(sec.Unit+" per day, "+data.From+" to "+data.To, sec.Unit, points)
		data.Sections = append(data.Sections, sec)
	}
	// Reconciliation with Slurm batch usage, when an input is at hand.
	b, err := s.batch(p, upload)
	if err != nil {
		msg := "the configured Slurm report could not be read"
		if upload != nil {
			msg = clip(err.Error(), 200)
		} else {
			s.log.Error("reports: slurm.sreport_file", "err", err)
		}
		s.render(w, "reports", view{Title: "Reports", User: p.name, CSRF: csrf, Error: msg, Data: data})
		return
	}
	if b != nil {
		orgRows, e := s.fetchReport(r, p, from, to, []string{"org"})
		if e != "" {
			s.render(w, "reports", view{Title: "Reports", User: p.name, CSRF: csrf, Error: e, Data: data})
			return
		}
		inf := map[string]float64{}
		for _, x := range orgRows {
			if x.Unit == "" {
				inf[x.Org] += x.Cost
			}
		}
		for _, x := range reconcile(inf, b) {
			l := reconLine{Org: labelOr(x.Org), Inference: fmtNum(x.Inference), Batch: fmtNum(x.Batch)}
			if t := x.Inference + x.Batch; t > 0 {
				l.Share = fmt.Sprintf("%.1f %%", 100*x.Inference/t)
			}
			data.Recon = append(data.Recon, l)
		}
		data.ReconNote = "Slurm report (" + b.Source + ")"
		if b.Period != "" {
			data.ReconNote += ", period " + b.Period
		} else {
			data.ReconNote += ", no period printed: check it covers this one"
		}
	}
	s.render(w, "reports", view{Title: "Reports", User: p.name, CSRF: csrf, Data: data})
}
