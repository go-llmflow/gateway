package gateway

import (
	"io"

	"github.com/go-llmflow/gateway/store"
	"github.com/go-ooxml/ooxml/xlsx"
)

// writeXLSX writes a workbook: a Summary sheet, then one sheet per
// grouping. Numbers are typed (a spreadsheet can sum them), each cost
// column names its unit in its header, and a unit has its own column:
// GPU-hours and a currency are never in the same column. go-ooxml's writer
// has no formula API and stores every string as a string, with quotePrefix
// on one that starts with = + - @, so nothing from the data can become a
// formula.
func writeXLSX(w io.Writer, b *reportBundle) error {
	wb := xlsx.New()
	sum, err := wb.AddSheet("Summary")
	if err != nil {
		return err
	}
	_ = sum.SetColumnWidth(0, 30)
	_ = sum.SetColumnWidth(1, 22)
	if err := sum.SetHeader("Item", "Value", "Previous period", "Change"); err != nil {
		return err
	}
	day := xlsx.Date
	rows := [][]xlsx.Cell{
		{xlsx.String("Period start (UTC)"), day(b.From)},
		{xlsx.String("Period end (UTC, excluded)"), day(b.To)},
		{xlsx.String("Previous period start (UTC)"), day(b.PrevFrom)},
		{xlsx.String("Generated (UTC)"), xlsx.Date(b.Generated).Format(xlsx.DateTimeFormat)},
	}
	for _, t := range b.Totals {
		u := displayUnit(t.Unit)
		change := xlsx.Cell{}
		if t.Prev != 0 {
			change = xlsx.Number((t.Cost - t.Prev) / t.Prev).Format(xlsx.Percent(1))
		}
		rows = append(rows,
			[]xlsx.Cell{xlsx.String("Total (" + u + ")"), xlsx.Number(t.Cost).Format(xlsx.Decimal(3)), xlsx.Number(t.Prev).Format(xlsx.Decimal(3)), change},
			[]xlsx.Cell{xlsx.String("Requests (" + u + ")"), xlsx.Number(float64(t.Requests))},
			[]xlsx.Cell{xlsx.String("Estimated requests (" + u + ")"), xlsx.Number(float64(t.Estimated))})
	}
	if b.Batch != nil && b.Batch.Period != "" {
		rows = append(rows, []xlsx.Cell{xlsx.String("Slurm report period"), xlsx.String(b.Batch.Period)})
	}
	for _, r := range rows {
		if err := sum.AppendRow(r...); err != nil {
			return err
		}
	}
	// Reconciliation: inference against batch GPU-hours per org.
	rec, err := wb.AddSheet("Reconciliation")
	if err != nil {
		return err
	}
	if err := rec.SetHeader("org", "Inference (GPU-hours)", "Slurm batch (GPU-hours)", "Inference share"); err != nil {
		return err
	}
	_ = rec.SetColumnWidth(0, 28)
	for _, r := range reconcile(b.inferenceByOrg(), b.Batch) {
		cells := []xlsx.Cell{xlsx.String(r.Org), xlsx.Number(r.Inference).Format(xlsx.Decimal(3))}
		if b.Batch != nil {
			cells = append(cells, xlsx.Number(r.Batch).Format(xlsx.Decimal(3)))
			if t := r.Inference + r.Batch; t > 0 {
				cells = append(cells, xlsx.Number(r.Inference/t).Format(xlsx.Percent(1)))
			}
		}
		if err := rec.AppendRow(cells...); err != nil {
			return err
		}
	}
	for _, d := range exportDims {
		if err := xlsxSheet(wb, d, b.By[d]); err != nil {
			return err
		}
	}
	if err := xlsxSheet(wb, "day", b.Days); err != nil {
		return err
	}
	data, err := wb.Write()
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// xlsxSheet writes one grouping: one row per value, one cost column per unit.
func xlsxSheet(wb *xlsx.Workbook, dim string, rows []store.ReportRow) error {
	sh, err := wb.AddSheet("By " + dim)
	if err != nil {
		return err
	}
	us := units(rows)
	head := []string{dim, "Requests", "Prompt tokens", "Completion tokens", "Cached tokens"}
	for _, u := range us {
		head = append(head, "Cost ("+displayUnit(u)+")")
	}
	if err := sh.SetHeader(head...); err != nil {
		return err
	}
	_ = sh.SetColumnWidth(0, 28)
	col := map[string]int{}
	for i, u := range us {
		col[u] = i
	}
	// One line per value: the units of a value side by side.
	type line struct {
		row   store.ReportRow
		costs []float64
		has   []bool
	}
	byVal := map[string]*line{}
	var order []string
	for _, r := range rows {
		v := r.Dim(dim)
		l := byVal[v]
		if l == nil {
			l = &line{row: store.ReportRow{}, costs: make([]float64, len(us)), has: make([]bool, len(us))}
			byVal[v] = l
			order = append(order, v)
		}
		l.row.Requests += r.Requests
		l.row.PromptTokens += r.PromptTokens
		l.row.CompletionTokens += r.CompletionTokens
		l.row.CachedTokens += r.CachedTokens
		l.costs[col[r.Unit]] += r.Cost
		l.has[col[r.Unit]] = true
	}
	for _, v := range order {
		l := byVal[v]
		label := v
		if label == "" {
			label = "(none)"
		}
		cells := []xlsx.Cell{xlsx.String(label), xlsx.Number(float64(l.row.Requests)), xlsx.Number(float64(l.row.PromptTokens)),
			xlsx.Number(float64(l.row.CompletionTokens)), xlsx.Number(float64(l.row.CachedTokens))}
		for i := range us {
			if l.has[i] {
				cells = append(cells, xlsx.Number(l.costs[i]).Format(xlsx.Decimal(3)))
			} else {
				cells = append(cells, xlsx.Cell{})
			}
		}
		if err := sh.AppendRow(cells...); err != nil {
			return err
		}
	}
	return nil
}
