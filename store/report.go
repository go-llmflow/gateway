package store

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Report dimensions. A record with several tags counts once under each of
// its tags when "tag" is a dimension (and once under "" when it has none),
// so totals over tags can exceed the grand total.
var reportDims = map[string]string{
	"org": "org", "team": "team", "project": "project", "key": "key_id",
	"model": "model", "pool": "pool", "tag": "tag",
}

// ReportDims lists the accepted dimensions, in canonical order.
var ReportDims = []string{"org", "team", "project", "key", "model", "pool", "tag"}

// ValidateDims checks and orders report dimensions.
func ValidateDims(dims []string) ([]string, error) {
	seen := map[string]bool{}
	for _, d := range dims {
		if _, ok := reportDims[d]; !ok {
			return nil, fmt.Errorf("unknown dimension %q: want %s", d, strings.Join(ReportDims, ", "))
		}
		seen[d] = true
	}
	out := []string{}
	for _, d := range ReportDims {
		if seen[d] {
			out = append(out, d)
		}
	}
	return out, nil
}

// ReportRow is one group of a spend report. Only the dimensions asked for
// are filled.
type ReportRow struct {
	Org               string  `json:"org,omitempty"`
	Team              string  `json:"team,omitempty"`
	Project           string  `json:"project,omitempty"`
	Key               string  `json:"key,omitempty"`
	Model             string  `json:"model,omitempty"`
	Pool              string  `json:"pool,omitempty"`
	Tag               string  `json:"tag,omitempty"`
	Requests          int64   `json:"requests"`
	PromptTokens      int64   `json:"prompt_tokens"`
	CompletionTokens  int64   `json:"completion_tokens"`
	CachedTokens      int64   `json:"cached_tokens"`
	GPUHours          float64 `json:"gpu_hours"`
	EstimatedRequests int64   `json:"estimated_requests"`
}

// Dim returns the value of one dimension.
func (r ReportRow) Dim(d string) string {
	switch d {
	case "org":
		return r.Org
	case "team":
		return r.Team
	case "project":
		return r.Project
	case "key":
		return r.Key
	case "model":
		return r.Model
	case "pool":
		return r.Pool
	case "tag":
		return r.Tag
	}
	return ""
}

func (r *ReportRow) setDim(d, v string) {
	switch d {
	case "org":
		r.Org = v
	case "team":
		r.Team = v
	case "project":
		r.Project = v
	case "key":
		r.Key = v
	case "model":
		r.Model = v
	case "pool":
		r.Pool = v
	case "tag":
		r.Tag = v
	}
}

func (r *ReportRow) add(o ReportRow) {
	r.Requests += o.Requests
	r.PromptTokens += o.PromptTokens
	r.CompletionTokens += o.CompletionTokens
	r.CachedTokens += o.CachedTokens
	r.GPUHours += o.GPUHours
	r.EstimatedRequests += o.EstimatedRequests
}

// Regroup aggregates rows onto fewer dimensions, sorted.
func Regroup(rows []ReportRow, dims []string) []ReportRow {
	agg := map[string]*ReportRow{}
	var order []string
	for _, r := range rows {
		parts := make([]string, len(dims))
		for i, d := range dims {
			parts[i] = r.Dim(d)
		}
		k := strings.Join(parts, "\x00")
		g := agg[k]
		if g == nil {
			g = &ReportRow{}
			for i, d := range dims {
				g.setDim(d, parts[i])
			}
			agg[k] = g
			order = append(order, k)
		}
		g.add(r)
	}
	sort.Strings(order)
	out := make([]ReportRow, 0, len(order))
	for _, k := range order {
		out = append(out, *agg[k])
	}
	return out
}

func (m *Memory) Report(_ context.Context, from, to time.Time, dims []string) ([]ReportRow, error) {
	dims, err := ValidateDims(dims)
	if err != nil {
		return nil, err
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	var rows []ReportRow
	for _, r := range m.records {
		if r.Time.Before(from) || !r.Time.Before(to) {
			continue
		}
		base := ReportRow{Org: r.Org, Team: r.Team, Project: r.Project, Key: r.KeyID, Model: r.Model, Pool: r.Pool,
			Requests: 1, PromptTokens: r.PromptTokens, CompletionTokens: r.CompletionTokens, CachedTokens: r.CachedTokens,
			GPUHours: r.Cost}
		if r.Estimated {
			base.EstimatedRequests = 1
		}
		tags := dedupSorted(r.Tags)
		if len(tags) == 0 || !contains(dims, "tag") {
			rows = append(rows, base)
			continue
		}
		for _, t := range tags {
			row := base
			row.Tag = t
			rows = append(rows, row)
		}
	}
	return Regroup(rows, dims), nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func (p *Postgres) Report(ctx context.Context, from, to time.Time, dims []string) ([]ReportRow, error) {
	dims, err := ValidateDims(dims)
	if err != nil {
		return nil, err
	}
	// Column names come from the fixed reportDims table only, never from
	// the caller's strings.
	cols := make([]string, len(dims))
	for i, d := range dims {
		cols[i] = reportDims[d]
	}
	src := `llmflow_usage`
	if contains(dims, "tag") {
		src = `(SELECT u.*, t.tag FROM llmflow_usage u,
			LATERAL unnest(CASE WHEN cardinality(u.tags) = 0 THEN ARRAY[''] ELSE u.tags END) AS t(tag)) AS u`
	}
	sel := strings.Join(cols, ", ")
	if sel != "" {
		sel += ", "
	}
	q := `SELECT ` + sel + `count(*), coalesce(sum(prompt_tokens), 0), coalesce(sum(completion_tokens), 0),
		coalesce(sum(cached_tokens), 0), coalesce(sum(cost), 0), count(*) FILTER (WHERE estimated)
		FROM ` + src + ` WHERE ts >= $1 AND ts < $2`
	if len(cols) > 0 {
		q += ` GROUP BY ` + strings.Join(cols, ", ")
	}
	rows, err := p.pool.Query(ctx, q, from, to)
	if err != nil {
		return nil, err
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (ReportRow, error) {
		var row ReportRow
		vals := make([]string, len(dims))
		dest := make([]any, 0, len(dims)+6)
		for i := range vals {
			dest = append(dest, &vals[i])
		}
		dest = append(dest, &row.Requests, &row.PromptTokens, &row.CompletionTokens, &row.CachedTokens, &row.GPUHours, &row.EstimatedRequests)
		if err := r.Scan(dest...); err != nil {
			return row, err
		}
		for i, d := range dims {
			row.setDim(d, vals[i])
		}
		return row, nil
	})
	if err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, r := range out {
		if r.Requests > 0 { // an aggregate without GROUP BY returns one row even when empty
			kept = append(kept, r)
		}
	}
	return Regroup(kept, dims), nil
}
