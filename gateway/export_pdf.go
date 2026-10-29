package gateway

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/go-llmflow/gateway/store"
	"github.com/go-opentype/fonts"
	"github.com/go-pdfkit/pdfkit"
)

// The PDF "management summary" (#68): period, totals per unit, totals per
// level (org, lab, team, project) with pie charts, the daily trend, and a
// reconciliation table of inference against Slurm batch GPU-hours.
//
// Written with go-pdfkit/pdfkit, which draws vector paths and embeds a
// subset of one font (go-opentype/fonts' Atkinson Hyperlegible, embedded in
// the binary: no file is read). Every text is drawn as glyphs of that font
// (Identity-H): no string reaches the PDF syntax, so a label cannot inject
// an operator, and the document has no action, link or script. Labels have
// their control characters removed and are clipped.
//
// go-pdfkit/html2pdf was the other candidate: it lays HTML out with
// go-webengine, i.e. an HTML and CSS engine in front of the same writer,
// for a document whose layout is a few tables and charts. More code, a
// parser of its own on the path, and no released version yet.

var (
	pdfFontOnce sync.Once
	pdfFont     *pdfkit.Font
	pdfFontErr  error
)

func reportFont() (*pdfkit.Font, error) {
	pdfFontOnce.Do(func() { pdfFont, pdfFontErr = pdfkit.LoadFont(fonts.MostLegible()) })
	return pdfFont, pdfFontErr
}

const (
	pdfMargin = 48.0
	pdfTopN   = 10
)

type pdfDoc struct {
	doc  *pdfkit.Document
	p    *pdfkit.Page
	f    *pdfkit.Font
	y    float64
	page int
}

func hexColour(h string) pdfkit.RGB {
	v, _ := strconv.ParseUint(strings.TrimPrefix(h, "#"), 16, 32)
	return pdfkit.RGB8(uint8(v>>16), uint8(v>>8), uint8(v))
}

// cleanLabel removes control and format characters and clips a label.
func cleanLabel(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, s)
	if s == "" {
		return "(none)"
	}
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

func (d *pdfDoc) newPage() {
	d.p = d.doc.AddPage(pdfkit.A4)
	d.y = d.p.Height() - pdfMargin
	d.page++
	d.p.SetFillColor(hexColour("#666666"))
	d.p.SetFont(d.f, 8)
	_ = d.p.Text(pdfMargin, pdfMargin/2, fmt.Sprintf("llmflow — page %d", d.page))
}

func (d *pdfDoc) need(h float64) {
	if d.p == nil || d.y-h < pdfMargin {
		d.newPage()
	}
}

func (d *pdfDoc) text(x, size float64, colour, s string) {
	d.p.SetFillColor(hexColour(colour))
	d.p.SetFont(d.f, size)
	_ = d.p.Text(x, d.y, s)
}

func (d *pdfDoc) heading(s string, size float64) {
	d.need(size + 40)
	d.y -= size + 8
	d.text(pdfMargin, size, "#1d2330", s)
	d.y -= 6
}

func (d *pdfDoc) line(s string) {
	d.need(14)
	d.y -= 13
	d.text(pdfMargin, 10, "#1d2330", s)
}

// table draws a table; columns after the first are right-aligned.
func (d *pdfDoc) table(cols []string, widths []float64, rows [][]string) {
	draw := func(cells []string, head bool) {
		d.need(14)
		d.y -= 14
		if head {
			d.p.SetFillColor(hexColour("#eef1f6"))
			total := 0.0
			for _, w := range widths {
				total += w
			}
			d.p.Rectangle(pdfkit.Rect{X: pdfMargin, Y: d.y - 4, Width: total, Height: 14})
			d.p.Fill()
		}
		x := pdfMargin
		for i, c := range cells {
			d.p.SetFont(d.f, 9)
			tx := x + 3
			if i > 0 {
				tx = x + widths[i] - 3 - d.p.TextWidth(c)
			}
			d.text(tx, 9, "#1d2330", c)
			x += widths[i]
		}
	}
	draw(cols, true)
	for _, r := range rows {
		draw(r, false)
	}
	d.y -= 6
}

// pie draws the share of each slice, with its legend on the right.
func (d *pdfDoc) pie(unit string, in []chartSlice) {
	slices := topSlices(in)
	total := 0.0
	for _, s := range slices {
		total += s.Value
	}
	if total <= 0 {
		return
	}
	const r = 60.0
	d.need(2*r + 20)
	cx, cy := pdfMargin+r+4, d.y-r-8
	angle := math.Pi / 2
	for i, s := range slices {
		colour := palette[i%len(palette)]
		if s.Label == "other" {
			colour = otherColour
		}
		frac := s.Value / total
		d.p.SetFillColor(hexColour(colour))
		if frac >= 0.9999 {
			d.circle(cx, cy, r)
		} else {
			// Counter-clockwise (PDF y is up), in arcs of at most 90°.
			end := angle - 2*math.Pi*frac
			d.p.MoveTo(cx, cy)
			d.p.LineTo(cx+r*math.Cos(angle), cy+r*math.Sin(angle))
			for a := angle; a > end; {
				b := math.Max(a-math.Pi/2, end)
				d.arc(cx, cy, r, a, b)
				a = b
			}
			d.p.ClosePath()
			angle = end
		}
		d.p.Fill()
		ly := d.y - 14 - float64(i)*15
		d.p.SetFillColor(hexColour(colour))
		d.p.Rectangle(pdfkit.Rect{X: cx + r + 24, Y: ly - 1, Width: 9, Height: 9})
		d.p.Fill()
		saved := d.y
		d.y = ly
		d.text(cx+r+38, 9, "#1d2330", fmt.Sprintf("%s — %s %s (%.1f %%)", cleanLabel(s.Label, 40), fmtNum(s.Value), unit, 100*frac))
		d.y = saved
	}
	d.y = cy - r - 12
}

// arc appends a cubic Bézier approximation of the arc from a to b (radians,
// |a-b| ≤ π/2) to the current path.
func (d *pdfDoc) arc(cx, cy, r, a, b float64) {
	k := 4.0 / 3.0 * math.Tan((b-a)/4)
	x1, y1 := cx+r*math.Cos(a), cy+r*math.Sin(a)
	x4, y4 := cx+r*math.Cos(b), cy+r*math.Sin(b)
	d.p.CurveTo(x1-k*r*math.Sin(a), y1+k*r*math.Cos(a), x4+k*r*math.Sin(b), y4-k*r*math.Cos(b), x4, y4)
}

func (d *pdfDoc) circle(cx, cy, r float64) {
	d.p.MoveTo(cx+r, cy)
	for i := 0; i < 4; i++ {
		a := float64(i) * math.Pi / 2
		d.arc(cx, cy, r, a, a+math.Pi/2)
	}
	d.p.ClosePath()
}

// series draws a daily line with its axes.
func (d *pdfDoc) series(points []chartPoint) {
	if len(points) == 0 {
		return
	}
	const h, w = 110.0, 460.0
	d.need(h + 30)
	x0, y0 := pdfMargin+40, d.y-h-6
	max := 0.0
	for _, p := range points {
		max = math.Max(max, p.Y)
	}
	if max <= 0 {
		max = 1
	}
	d.p.SetStrokeColor(hexColour("#888888"))
	d.p.SetLineWidth(0.5)
	d.p.MoveTo(x0, y0+h)
	d.p.LineTo(x0, y0)
	d.p.LineTo(x0+w, y0)
	d.p.Stroke()
	step := 0.0
	if len(points) > 1 {
		step = w / float64(len(points)-1)
	}
	d.p.SetStrokeColor(hexColour(palette[0]))
	d.p.SetLineWidth(1.5)
	for i, p := range points {
		x, y := x0+step*float64(i), y0+h*p.Y/max
		if i == 0 {
			d.p.MoveTo(x, y)
		} else {
			d.p.LineTo(x, y)
		}
	}
	if len(points) == 1 {
		d.p.LineTo(x0+1, y0+h*points[0].Y/max)
	}
	d.p.Stroke()
	saved := d.y
	d.y = y0 + h - 6
	d.text(pdfMargin, 8, "#1d2330", fmtNum(max))
	d.y = y0 - 12
	d.text(x0, 8, "#1d2330", points[0].X)
	d.p.SetFont(d.f, 8)
	last := points[len(points)-1].X
	d.text(x0+w-d.p.TextWidth(last), 8, "#1d2330", last)
	d.y = saved - h - 26
}

// writePDF writes the management summary.
func writePDF(w io.Writer, b *reportBundle) error {
	f, err := reportFont()
	if err != nil {
		return err
	}
	d := &pdfDoc{f: f, doc: pdfkit.New(pdfkit.Options{Title: "llmflow management summary", Producer: "llmflow-gateway",
		Lang: "en", Compress: true})}
	day := func(t interface{ Format(string) string }) string { return t.Format("2006-01-02") }
	d.heading("Inference usage — management summary", 18)
	d.line(fmt.Sprintf("Period: %s to %s (UTC, end excluded). Previous period: %s to %s.", day(b.From), day(b.To), day(b.PrevFrom), day(b.From)))
	d.line("Generated " + b.Generated.Format("2006-01-02 15:04") + " UTC. GPU-hours and currencies are reported apart, never added together.")

	d.heading("Totals", 13)
	var rows [][]string
	for _, t := range b.Totals {
		rows = append(rows, []string{displayUnit(t.Unit), fmtNum(t.Cost), fmtNum(t.Prev), delta(t.Cost, t.Prev), strconv.FormatInt(t.Requests, 10)})
	}
	if len(rows) == 0 {
		d.line("No usage in this period.")
	} else {
		d.table([]string{"Unit", "This period", "Previous", "Change", "Requests"}, []float64{130, 100, 100, 80, 89}, rows)
	}

	for _, level := range []string{"org", "lab", "team", "project", "profile"} {
		data := b.By[level]
		if len(data) == 0 {
			continue
		}
		d.heading("By "+level, 13)
		for _, u := range units(data) {
			var mine []store.ReportRow
			total := 0.0
			for _, r := range data {
				if r.Unit == u {
					mine = append(mine, r)
					total += r.Cost
				}
			}
			sort.SliceStable(mine, func(i, j int) bool { return mine[i].Cost > mine[j].Cost })
			unit := displayUnit(u)
			var rows [][]string
			var slices []chartSlice
			for i, r := range mine {
				slices = append(slices, chartSlice{Label: r.Dim(level), Value: r.Cost})
				if i < pdfTopN {
					share := ""
					if total > 0 {
						share = fmt.Sprintf("%.1f %%", 100*r.Cost/total)
					}
					rows = append(rows, []string{cleanLabel(r.Dim(level), 48), fmtNum(r.Cost), share, strconv.FormatInt(r.Requests, 10)})
				}
			}
			if len(mine) > pdfTopN {
				rows = append(rows, []string{fmt.Sprintf("… %d more", len(mine)-pdfTopN), "", "", ""})
			}
			d.table([]string{level, unit, "Share", "Requests"}, []float64{250, 100, 80, 69}, rows)
			if level != "project" && len(slices) > 1 {
				d.pie(unit, slices)
			}
		}
	}

	if len(b.Days) > 0 {
		d.heading("Trend per day", 13)
		for _, u := range units(b.Days) {
			byDay := map[string]float64{}
			for _, r := range b.Days {
				if r.Unit == u {
					byDay[r.Day] += r.Cost
				}
			}
			var points []chartPoint
			for t := b.From; t.Before(b.To) && len(points) <= 400; t = t.AddDate(0, 0, 1) {
				k := t.Format("2006-01-02")
				points = append(points, chartPoint{X: k, Y: byDay[k]})
			}
			d.line(displayUnit(u) + " per day")
			d.series(points)
		}
	}

	d.heading("Reconciliation: inference and Slurm batch GPU-hours", 13)
	rows = nil
	for _, r := range reconcile(b.inferenceByOrg(), b.Batch) {
		batch, share := "not provided", ""
		if b.Batch != nil {
			batch = fmtNum(r.Batch)
			if t := r.Inference + r.Batch; t > 0 {
				share = fmt.Sprintf("%.1f %%", 100*r.Inference/t)
			}
		}
		rows = append(rows, []string{cleanLabel(r.Org, 40), fmtNum(r.Inference), batch, share})
	}
	switch {
	case b.Batch == nil:
		d.line("No Slurm accounting input was given: only inference is shown.")
	case b.Batch.Period != "":
		d.line("Slurm report period: " + cleanLabel(b.Batch.Period, 60) + " (sreport AccountUtilizationByUser, gres/gpu).")
	default:
		d.line("The Slurm report printed no period: check it covers the period above.")
	}
	d.table([]string{"org", "Inference GPU-hours", "Batch GPU-hours", "Inference share"}, []float64{170, 110, 110, 109}, rows)
	return d.doc.Write(w)
}
