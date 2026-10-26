package gateway

import (
	"bytes"
	"io"
	"os"
	"sort"

	"github.com/go-llmflow/gateway/slurm"
)

// Slurm reconciliation (#68): batch GPU-hours per organisation from an
// `sreport cluster AccountUtilizationByUser -T gres/gpu` output, shown next
// to inference GPU-hours. The input is the configured slurm.sreport_file,
// or a file uploaded with an export (POST /admin/export, or the Reports
// page); it is parsed as untrusted input (package slurm: bounded, fuzzed).
//
// Only account totals count (rows without a login), mapped to an
// organisation by slurm.accounts or by name. A principal sees the batch
// usage of the organisations they may view (org_viewer, org_admin, proxy
// roles); a team-level role sees none.

type batchInput struct {
	ByOrg  map[string]float64
	Period string // the period sreport printed, "" if it printed none
	Source string // "file" or "upload"
}

// batch reads the Slurm input: upload when not nil, else the configured
// file; nil, nil without either.
func (s *Server) batch(p *principal, upload []byte) (*batchInput, error) {
	var (
		rd     io.Reader
		source string
	)
	switch {
	case upload != nil:
		rd, source = bytes.NewReader(upload), "upload"
	case s.cfg.Slurm.SreportFile != "":
		f, err := os.Open(s.cfg.Slurm.SreportFile)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		rd, source = f, "file"
	default:
		return nil, nil
	}
	rep, err := slurm.Parse(rd)
	if err != nil {
		return nil, err
	}
	b := &batchInput{ByOrg: map[string]float64{}, Period: rep.Period, Source: source}
	for acct, h := range rep.ByAccount() {
		org := s.cfg.Slurm.OrgOfAccount(acct)
		if p.orgView(org) {
			b.ByOrg[org] += h
		}
	}
	return b, nil
}

// reconRow is one organisation of the reconciliation table.
type reconRow struct {
	Org              string
	Inference, Batch float64
}

// reconcile pairs inference GPU-hours per org with batch GPU-hours; the
// orgs of either side, sorted.
func reconcile(inference map[string]float64, b *batchInput) []reconRow {
	orgs := map[string]bool{}
	for o := range inference {
		orgs[o] = true
	}
	if b != nil {
		for o := range b.ByOrg {
			orgs[o] = true
		}
	}
	out := make([]reconRow, 0, len(orgs))
	for o := range orgs {
		r := reconRow{Org: o, Inference: inference[o]}
		if b != nil {
			r.Batch = b.ByOrg[o]
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Org < out[j].Org })
	return out
}
