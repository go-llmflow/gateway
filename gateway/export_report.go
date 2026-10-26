package gateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Report exports (#68): GET /admin/export?from=&to=&format=csv|xlsx|pdf.
// Every download is audited (kind "export"), whether it succeeds or not.
// A period is at most store.MaxReportSpan, a report runs under
// ReportTimeout, and an export holds at most maxExportRows rows.

const (
	FormatXLSX = "xlsx"
	FormatPDF  = "pdf"
)

// maxExportRows bounds the finest grouping an export reads.
const maxExportRows = 100_000

// exportDims are the groupings an XLSX export gives a sheet each, and the
// default grouping of a CSV export (tag excepted: a record counts under
// each of its tags).
var exportDims = []string{"org", "lab", "team", "project", "user", "model", "pool", "tag"}

var errTooManyRows = errors.New("the export has too many rows: narrow the period")

// unitTotal is one unit's total over the period and the previous one.
type unitTotal struct {
	Unit      string // "" is GPU-hours
	Cost      float64
	Prev      float64
	Requests  int64
	Estimated int64
}

// reportBundle is everything an export shows, already restricted to what
// the principal may see.
type reportBundle struct {
	From, To, PrevFrom time.Time
	Generated          time.Time
	By                 map[string][]store.ReportRow // per export dimension
	Days               []store.ReportRow
	Totals             []unitTotal
	Batch              map[string]float64 // Slurm batch GPU-hours per org; nil without input
}

func (s *Server) buildBundle(r *http.Request, p *principal, from, to time.Time) (*reportBundle, error) {
	b := &reportBundle{From: from, To: to, PrevFrom: from.Add(-to.Sub(from)), Generated: s.now().UTC(), By: map[string][]store.ReportRow{}}
	base, err := s.visibleReport(r, p, from, to, []string{"org", "lab", "team", "project", "user", "model", "pool"})
	if err != nil {
		return nil, err
	}
	if len(base) > maxExportRows {
		return nil, errTooManyRows
	}
	for _, d := range exportDims {
		if d != "tag" {
			b.By[d] = store.Regroup(base, []string{d})
		}
	}
	if b.By["tag"], err = s.visibleReport(r, p, from, to, []string{"tag"}); err != nil {
		return nil, err
	}
	if b.Days, err = s.visibleReport(r, p, from, to, []string{"day"}); err != nil {
		return nil, err
	}
	prev, err := s.visibleReport(r, p, b.PrevFrom, from, nil)
	if err != nil {
		return nil, err
	}
	tot := map[string]*unitTotal{}
	get := func(u string) *unitTotal {
		if tot[u] == nil {
			tot[u] = &unitTotal{Unit: u}
		}
		return tot[u]
	}
	for _, x := range store.Regroup(base, nil) {
		t := get(x.Unit)
		t.Cost, t.Requests, t.Estimated = x.Cost, x.Requests, x.EstimatedRequests
	}
	for _, x := range prev {
		get(x.Unit).Prev += x.Cost
	}
	for _, t := range tot {
		b.Totals = append(b.Totals, *t)
	}
	sort.Slice(b.Totals, func(i, j int) bool { return b.Totals[i].Unit < b.Totals[j].Unit })
	return b, nil
}

// units lists the units present in rows, GPU-hours first.
func units(rows []store.ReportRow) []string {
	seen := map[string]bool{}
	var out []string
	for _, r := range rows {
		if !seen[r.Unit] {
			seen[r.Unit] = true
			out = append(out, r.Unit)
		}
	}
	sort.Strings(out)
	return out
}

// downloadName is a file name made of safe characters only.
func downloadName(from, to time.Time, ext string) string {
	n := "llmflow-report-" + from.UTC().Format("2006-01-02") + "-to-" + to.UTC().Format("2006-01-02") + "." + ext
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '.' || r == '_' {
			return r
		}
		return '_'
	}, n)
}

// exportReport answers GET /admin/export.
func (s *Server) exportReport(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	format := q.Get("format")
	status, size, rows := http.StatusOK, 0, 0
	defer func() {
		after, _ := json.Marshal(map[string]any{"format": format, "bytes": size, "rows": rows})
		addr, _ := clientAddrOf(r)
		ip := ""
		if addr.IsValid() {
			ip = addr.String()
		}
		s.appendAudit(r.Context(), store.AuditEntry{Kind: "export", Actor: p.name, Roles: rolesJSON(p),
			Action: "GET /admin/export", Target: auditTarget(r), Before: "null", After: string(after),
			Status: status, SourceIP: ip, RequestID: r.Header.Get("X-Request-Id")})
	}()
	fail := func(code int, msg string) {
		status = code
		apiError(w, code, "invalid_request_error", msg)
	}
	from, err1 := ParseDay(q.Get("from"))
	to, err2 := ParseDay(q.Get("to"))
	if err1 != nil || err2 != nil || !from.Before(to) {
		fail(http.StatusBadRequest, "from and to are required (RFC 3339 or YYYY-MM-DD), from < to")
		return
	}
	if to.Sub(from) > store.MaxReportSpan {
		fail(http.StatusBadRequest, "an export covers at most 366 days")
		return
	}
	var out bytes.Buffer
	ctype := ""
	switch format {
	case FormatCSV:
		g := q.Get("group_by")
		if g == "" {
			g = "org,lab,team,project,user,model,pool"
		}
		dims, err := ParseGroupBy(g)
		if err != nil {
			fail(http.StatusBadRequest, err.Error())
			return
		}
		data, err := s.visibleReport(r, p, from, to, dims)
		if err != nil {
			status = http.StatusInternalServerError
			storeError(w, err)
			return
		}
		if len(data) > maxExportRows {
			fail(http.StatusRequestEntityTooLarge, errTooManyRows.Error())
			return
		}
		rows = len(data)
		_ = writeReport(&out, FormatCSV, from, to, dims, data)
		ctype = "text/csv; charset=utf-8"
	case FormatXLSX, FormatPDF:
		b, err := s.buildBundle(r, p, from, to)
		if errors.Is(err, errTooManyRows) {
			fail(http.StatusRequestEntityTooLarge, err.Error())
			return
		}
		if err != nil {
			status = http.StatusInternalServerError
			storeError(w, err)
			return
		}
		b.Batch = s.batchByOrg(r, p, from, to)
		for _, d := range exportDims {
			rows += len(b.By[d])
		}
		if format == FormatXLSX {
			err = writeXLSX(&out, b)
			ctype = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
		} else {
			err = writePDF(&out, b)
			ctype = "application/pdf"
		}
		if err != nil {
			status = http.StatusInternalServerError
			s.log.Error("export", "format", format, "err", err)
			apiError(w, status, "server_error", "the export could not be written")
			return
		}
	default:
		fail(http.StatusBadRequest, "format: want csv, xlsx or pdf")
		return
	}
	size = out.Len()
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", downloadName(from, to, format)))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(out.Bytes())
}

// batchByOrg is the Slurm batch usage per org, when an input is set (#68
// part 4); nil otherwise.
func (s *Server) batchByOrg(_ *http.Request, _ *principal, _, _ time.Time) map[string]float64 {
	return nil
}
