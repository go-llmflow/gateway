package gateway

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/go-llmflow/gateway/lending"
	"github.com/go-llmflow/gateway/store"
)

// GET /admin/lending?from=&to=&format=json|csv (#97): per organisation and
// period, the guaranteed capacity, the GPU-hours used in share, borrowed and
// lent, the lending matrix and the net balance, and the latency of in-share
// against borrowing requests (package lending).
//
// Proxy roles read everything. org_admin and org-level viewer read their own
// organisation's row, whose lent and borrowed totals say what the agreement
// owes them, but not the matrix: who lent to whom is another partner's
// business, and only the proxy roles (and, later, finance_viewer) read across
// organisations. Team-level roles have no view.

// LendingRow is one organisation of the lending report.
type LendingRow struct {
	Org                string  `json:"org"`
	GuaranteedHours    float64 `json:"guaranteed_gpu_hours"`
	UsedInShare        float64 `json:"used_in_share_gpu_hours"`
	Borrowed           float64 `json:"borrowed_gpu_hours"`
	BorrowedFromOthers float64 `json:"borrowed_from_others_gpu_hours"`
	SelfServed         float64 `json:"borrowed_self_served_gpu_hours"`
	Lent               float64 `json:"lent_gpu_hours"`
	Net                float64 `json:"net_lent_gpu_hours"`
	// Utilisation is the share of the guarantee used in share.
	Utilisation float64 `json:"guarantee_utilisation"`
	// Wait is the admission wait of the requests, in the gateway's fair-share
	// queue: the evidence that the guarantee held. Latency is arrival to end.
	WaitInShare   lending.Latency `json:"wait_in_share_ms"`
	WaitBorrowing lending.Latency `json:"wait_borrowing_ms"`
	InShare       lending.Latency `json:"latency_in_share_ms"`
	Borrowing     lending.Latency `json:"latency_borrowing_ms"`
}

// LendingCell is one cell of the matrix.
type LendingCell struct {
	Lender   string  `json:"lender"`
	Borrower string  `json:"borrower"`
	GPUHours float64 `json:"gpu_hours"`
}

// lendingPools describes the pools as they were over [from, to): the shares
// of the organisations and the history of each pool's C. A pool fed by Slurm
// jobs is configured with capacity 0 and a C that follows its instances;
// reading the configuration alone would give it no guarantee at all (#108).
func (s *Server) lendingPools(ctx context.Context, from, to time.Time) ([]lending.Pool, error) {
	shares := s.orgShares()
	hist := map[string][]lending.CapPoint{}
	if cs, ok := s.st.(store.CapacityStore); ok {
		pts, err := cs.CapacityBetween(ctx, from, to)
		if err != nil {
			return nil, err
		}
		for _, p := range pts {
			hist[p.Pool] = append(hist[p.Pool], lending.CapPoint{At: p.At, C: p.C})
		}
	}
	var ps []lending.Pool
	for _, p := range s.cfg.Pools {
		ps = append(ps, lending.Pool{Name: p.Name, Capacity: p.Capacity, Timeline: hist[p.Name], Shares: shares})
	}
	return ps, nil
}

func lendingRows(res *lending.Result, visible func(org string) bool) []LendingRow {
	var rows []LendingRow
	for _, o := range res.Orgs {
		if !visible(o.Org) {
			continue
		}
		u := 0.0
		if o.GuaranteedHours > 0 {
			u = o.UsedInShare / o.GuaranteedHours
		}
		rows = append(rows, LendingRow{o.Org, o.GuaranteedHours, o.UsedInShare, o.Borrowed, o.BorrowedFromOthers, o.SelfServed, o.Lent, o.Net, u, o.WaitInShare, o.WaitBorrow, o.InShare, o.Borrow})
	}
	return rows
}

func (s *Server) lendingReport(w http.ResponseWriter, r *http.Request, p *principal) {
	if !p.anyOrgView() {
		forbidden(w)
		return
	}
	ls, ok := s.st.(store.LendingStore)
	if !ok {
		apiError(w, http.StatusNotImplemented, "not_implemented", "this store cannot report lending")
		return
	}
	q := r.URL.Query()
	from, err1 := ParseDay(q.Get("from"))
	to, err2 := ParseDay(q.Get("to"))
	if err1 != nil || err2 != nil || !from.Before(to) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "from and to are required (RFC 3339 or YYYY-MM-DD), from < to")
		return
	}
	if to.Sub(from) > store.MaxReportSpan {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "the period is longer than 366 days")
		return
	}
	format := q.Get("format")
	if format == "" {
		format = FormatJSON
	}
	if format != FormatJSON && format != FormatCSV {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "format: want json or csv")
		return
	}
	release, ok := heavy(w)
	if !ok {
		return
	}
	defer release()
	res, err := s.lending(r, from, to, ls)
	if errors.Is(err, store.ErrTooMany) || errors.Is(err, lending.ErrTooMany) {
		apiError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "too many requests in the period (more than "+strconv.Itoa(lending.MaxRequests)+"); shorten it")
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
	visible := func(org string) bool { return p.reportView() || p.orgView(org) }
	rows := lendingRows(res, visible)
	var cells []LendingCell
	if p.reportView() {
		for l, m := range res.Matrix {
			for b, v := range m {
				cells = append(cells, LendingCell{l, b, v})
			}
		}
		sort.Slice(cells, func(i, j int) bool {
			if cells[i].Lender != cells[j].Lender {
				return cells[i].Lender < cells[j].Lender
			}
			return cells[i].Borrower < cells[j].Borrower
		})
	}
	if format == FormatCSV {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		cw := csv.NewWriter(w)
		f := func(v float64) string { return strconv.FormatFloat(v, 'f', 6, 64) }
		_ = cw.Write([]string{"from", "to", "org", "guaranteed_gpu_hours", "used_in_share_gpu_hours", "borrowed_gpu_hours", "borrowed_from_others_gpu_hours",
			"borrowed_self_served_gpu_hours", "lent_gpu_hours", "net_lent_gpu_hours", "guarantee_utilisation",
			"p50_wait_in_share_ms", "p95_wait_in_share_ms", "p50_wait_borrowing_ms", "p95_wait_borrowing_ms",
			"p50_latency_in_share_ms", "p95_latency_in_share_ms", "p50_latency_borrowing_ms", "p95_latency_borrowing_ms"})
		for _, x := range rows {
			_ = cw.Write([]string{from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), csvSafe(x.Org), f(x.GuaranteedHours), f(x.UsedInShare), f(x.Borrowed),
				f(x.BorrowedFromOthers), f(x.SelfServed), f(x.Lent), f(x.Net), f(x.Utilisation),
				f(x.WaitInShare.P50), f(x.WaitInShare.P95), f(x.WaitBorrowing.P50), f(x.WaitBorrowing.P95),
				f(x.InShare.P50), f(x.InShare.P95), f(x.Borrowing.P50), f(x.Borrowing.P95)})
		}
		cw.Flush()
		return
	}
	if rows == nil {
		rows = []LendingRow{}
	}
	if cells == nil {
		cells = []LendingCell{}
	}
	out := map[string]any{"from": from.UTC(), "to": to.UTC(), "orgs": rows, "matrix": cells,
		"wait_note": "wait is the admission wait in the gateway's fair-share queue (arrival to grant); latency is arrival to end. The time a granted request spends in vLLM's own queue is not recorded."}
	if p.reportView() {
		out["unattributed_gpu_hours"] = res.Unattributed
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) lending(r *http.Request, from, to time.Time, ls store.LendingStore) (*lending.Result, error) {
	ctx, cancel := context.WithTimeout(r.Context(), ReportTimeout)
	defer cancel()
	pools, err := s.lendingPools(ctx, from, to)
	if err != nil {
		return nil, err
	}
	rows, err := ls.LendingRequests(ctx, from, to, lending.MaxRequests)
	if err != nil {
		return nil, err
	}
	reqs := make([]lending.Req, len(rows))
	for i, x := range rows {
		reqs[i] = lending.Req{Org: x.Org, Pool: x.Pool, End: x.End, LatencyMS: x.LatencyMS, WaitMS: x.WaitMS, Borrowing: x.Borrowing, GPUHours: x.Cost}
	}
	return lending.Compute(from, to, pools, reqs)
}
