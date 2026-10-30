package gateway

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// Report formats.
const (
	FormatJSON = "json"
	FormatCSV  = "csv"
	// FormatParsable2 is '|'-delimited without a trailing '|', like Slurm
	// sreport --parsable2, for side-by-side reconciliation.
	FormatParsable2 = "parsable2"
)

// ReportTimeout bounds the time one report may take in the store.
const ReportTimeout = 60 * time.Second

// ParseGroupBy splits a comma-separated dimension list.
func ParseGroupBy(v string) ([]string, error) {
	var dims []string
	for _, d := range strings.Split(v, ",") {
		if d = strings.TrimSpace(d); d != "" {
			dims = append(dims, d)
		}
	}
	return store.ValidateDims(dims)
}

// spendReport answers GET /admin/spend?from=&to=&group_by=&format=.
func (s *Server) spendReport(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	from, err1 := ParseDay(q.Get("from"))
	to, err2 := ParseDay(q.Get("to"))
	if err1 != nil || err2 != nil || !from.Before(to) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "from and to are required (RFC 3339 or YYYY-MM-DD), from < to")
		return
	}
	groupBy := q.Get("group_by")
	if groupBy == "" {
		groupBy = "org,team"
	}
	dims, err := ParseGroupBy(groupBy)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	format := q.Get("format")
	if format == "" {
		format = FormatJSON
	}
	if format != FormatJSON && format != FormatCSV && format != FormatParsable2 {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "format: want json, csv or parsable2")
		return
	}
	rows, err := s.visibleReport(r, p, from, to, dims)
	if err != nil {
		storeError(w, err)
		return
	}
	_ = writeReport(w, format, from, to, dims, rows)
}

// usageCSV keeps the v0.1 export: per team and pool, as CSV.
func (s *Server) usageCSV(w http.ResponseWriter, r *http.Request, p *principal) {
	q := r.URL.Query()
	q.Set("group_by", "team,pool")
	q.Set("format", FormatCSV)
	r.URL.RawQuery = q.Encode()
	s.spendReport(w, r, p)
}

// visibleReport returns the report restricted to what p may see. For a
// principal without a proxy role, the report is computed per team and key
// first, filtered (teams the principal may view; for a member, only their
// own keys), then regrouped onto the dimensions asked for.
//
// A report reads every record of its period: it runs under ReportTimeout
// whatever the period (the API keeps accepting any period; the UI and the
// exports also cap it at store.MaxReportSpan).
func (s *Server) visibleReport(r *http.Request, p *principal, from, to time.Time, dims []string) ([]store.ReportRow, error) {
	ctx, cancel := context.WithTimeout(r.Context(), ReportTimeout)
	defer cancel()
	if p.reportView() { // the proxy roles and finance_viewer read every organisation
		return s.st.Report(ctx, from, to, dims)
	}
	inner, _ := store.ValidateDims(append(append([]string{}, dims...), "team", "key"))
	rows, err := s.st.Report(ctx, from, to, inner)
	if err != nil {
		return nil, err
	}
	teams, err := s.teamsByName(r)
	if err != nil {
		return nil, err
	}
	keys, err := s.st.ListKeys(r.Context(), "")
	if err != nil {
		return nil, err
	}
	byID := make(map[string]store.Key, len(keys))
	for _, k := range keys {
		byID[k.ID] = k
	}
	kept := rows[:0]
	for _, row := range rows {
		t, ok := teams[row.Team]
		if ok && keyVisible(p, t, byID[row.Key]) {
			kept = append(kept, row)
		}
	}
	return store.Regroup(kept, dims), nil
}

// csvSafe neutralises cells a spreadsheet would read as a formula (OWASP
// CSV injection): model names come from the configuration and may start
// with any character.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

func reportHeader(dims []string) []string {
	h := append([]string{"from", "to"}, dims...)
	// cost and unit are appended (v0.12.0) so that the earlier columns keep
	// their positions; gpu_hours is 0 on a row of another unit.
	return append(h, "requests", "prompt_tokens", "completion_tokens", "cached_tokens", "gpu_hours", "estimated_requests", "cost", "unit")
}

func reportRecord(from, to time.Time, dims []string, r store.ReportRow) []string {
	rec := []string{from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339)}
	for _, d := range dims {
		rec = append(rec, csvSafe(r.Dim(d)))
	}
	return append(rec, strconv.FormatInt(r.Requests, 10), strconv.FormatInt(r.PromptTokens, 10),
		strconv.FormatInt(r.CompletionTokens, 10), strconv.FormatInt(r.CachedTokens, 10),
		strconv.FormatFloat(r.GPUHours, 'f', 6, 64), strconv.FormatInt(r.EstimatedRequests, 10),
		strconv.FormatFloat(r.Cost, 'f', 6, 64), store.UnitName(r.Unit))
}

// reportUnit is the report's unit when every row has the same one
// ("gpu_hours" for an empty report), "per_row" otherwise.
func reportUnit(rows []store.ReportRow) string {
	u := ""
	for i, r := range rows {
		if i > 0 && r.Unit != u {
			return "per_row"
		}
		u = r.Unit
	}
	return store.UnitName(u)
}

// writeReport writes a spend report in one of the formats.
func writeReport(w io.Writer, format string, from, to time.Time, dims []string, rows []store.ReportRow) error {
	if rw, ok := w.(http.ResponseWriter); ok {
		switch format {
		case FormatJSON:
			rw.Header().Set("Content-Type", "application/json")
		default:
			rw.Header().Set("Content-Type", "text/csv; charset=utf-8")
		}
	}
	switch format {
	case FormatJSON:
		if rows == nil {
			rows = []store.ReportRow{}
		}
		return json.NewEncoder(w).Encode(map[string]any{
			"from": from.UTC(), "to": to.UTC(), "group_by": dims, "unit": reportUnit(rows), "rows": rows,
		})
	case FormatCSV, FormatParsable2:
		cw := csv.NewWriter(w)
		if format == FormatParsable2 {
			cw.Comma = '|'
		}
		_ = cw.Write(reportHeader(dims))
		for _, r := range rows {
			_ = cw.Write(reportRecord(from, to, dims, r))
		}
		cw.Flush()
		return cw.Error()
	}
	return fmt.Errorf("unknown format %q", format)
}

// WriteReport is writeReport for the CLI.
func WriteReport(w io.Writer, format string, from, to time.Time, dims []string, rows []store.ReportRow) error {
	if format != FormatJSON && format != FormatCSV && format != FormatParsable2 {
		return fmt.Errorf("format %q: want json, csv or parsable2", format)
	}
	return writeReport(w, format, from, to, dims, rows)
}
