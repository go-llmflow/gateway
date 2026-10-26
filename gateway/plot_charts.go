package gateway

import (
	"bytes"
	"fmt"
	"html/template"
	"math"
	"regexp"
	"strings"

	"github.com/go-plot/plot"
)

// plotCharts draws the Reports page's charts with go-plot/plot (v0.3.0).
//
// No data-derived string is ever typeset as TeX: every figure is made with
// ParseMath(false), and every call that draws a string from the data
// (tick labels, titles, axis labels, legend) also passes PlainText(). The
// pie receives no name at all: its legend, which carries the names, is
// the gateway's own escaped SVG text (selectable, and read by a screen
// reader, where go-plot draws text as outlines).
//
// go-plot writes presentation attributes only (no style attribute, no
// script, no link), so the UI's CSP holds. Its ids are kept when the
// document refers to them (clip paths, whose ids derive from the
// rectangle) and removed otherwise.
type plotCharts struct{}

const chartDPI = 96

var (
	xmlProlog = regexp.MustCompile(`^<\?xml[^>]*\?>\s*`)
	svgIDAttr = regexp.MustCompile(` id="[^"]*"`)
	// The root element's width and height (the first ones in the document).
	svgRootSize = regexp.MustCompile(`^<svg([^>]*?) width="[0-9.]+pt" height="[0-9.]+pt"`)
)

// figureHook, when set (tests), sees every figure made, e.g. to spy on its
// text renderer.
var figureHook func(*plot.Figure)

func newFigure(wPx, hPx int) *plot.Figure {
	f := plot.NewFigure(plot.FigSize(float64(wPx)/chartDPI, float64(hPx)/chartDPI), plot.DPI(chartDPI), plot.ParseMath(false))
	if figureHook != nil {
		figureHook(f)
	}
	return f
}

// embed turns a figure into an inline SVG of wPx × hPx at (x, 0) inside
// a box of boxW × hPx titled title, with extra (escaped) markup after it;
// ok is false when go-plot could not write it.
func embed(f *plot.Figure, wPx, hPx, boxW int, title, unit, extra string) (template.HTML, bool) {
	var b bytes.Buffer
	if err := f.SaveSVG(&b); err != nil {
		return "", false
	}
	inner := xmlProlog.ReplaceAllString(b.String(), "")
	if !strings.Contains(inner, "url(#") && !strings.Contains(inner, `href="#`) {
		inner = svgIDAttr.ReplaceAllString(inner, "")
	}
	if !svgRootSize.MatchString(inner) {
		return "", false
	}
	inner = svgRootSize.ReplaceAllString(inner, fmt.Sprintf(`<svg$1 x="0" y="0" width="%d" height="%d"`, wPx, hPx))
	return template.HTML(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img"><title>%s (%s)</title>%s%s</svg>`,
		boxW, hPx, boxW, hPx, esc(title), esc(unit), inner, extra)), true // every string of ours is escaped; go-plot's text is outlines
}

func (plotCharts) Pie(title, unit string, in []chartSlice) template.HTML {
	slices := topSlices(in)
	total := 0.0
	vals := make([]float64, len(slices))
	cols := make([]string, len(slices))
	for i, s := range slices {
		vals[i], total = s.Value, total+s.Value
		cols[i] = palette[i%len(palette)]
		if s.Label == "other" {
			cols[i] = otherColour
		}
	}
	if total <= 0 {
		return noData(title, unit)
	}
	f := newFigure(240, 240)
	ax := f.AddAxes(0, 0, 1, 1)
	if err := ax.Pie(vals, plot.Colors(cols...), plot.StartAngle(90), plot.CounterClock(false), plot.PlainText(),
		plot.AutoPctFunc(func(pct float64) string {
			if pct < 5 {
				return ""
			}
			return fmt.Sprintf("%.0f%%", pct)
		}), plot.TextColor("#ffffff"), plot.EdgeColor("#ffffff"), plot.LineWidth(1)); err != nil {
		return noData(title, unit)
	}
	var legend strings.Builder
	for i, s := range slices {
		y := 24 + i*24
		fmt.Fprintf(&legend, `<rect x="250" y="%d" width="12" height="12" fill="%s"/><text x="268" y="%d" font-size="12" fill="#1d2330">%s — %s %s (%.1f %%)</text>`,
			y-10, cols[i], y, esc(labelOr(s.Label)), fmtNum(s.Value), esc(unit), 100*s.Value/total)
	}
	out, ok := embed(f, 240, 240, 620, title, unit, legend.String())
	if !ok {
		return noData(title, unit)
	}
	return out
}

// Bars draws this period against the previous one, one pair of horizontal
// bars per group (the 20 largest), the largest on top.
func (plotCharts) Bars(title, unit string, bars []chartBar) template.HTML {
	var kept []chartBar
	for _, b := range bars {
		if finite(b.Current) && finite(b.Previous) && b.Current >= 0 && b.Previous >= 0 {
			kept = append(kept, b)
		}
	}
	if len(kept) > 20 {
		kept = kept[:20]
	}
	if len(kept) == 0 {
		return noData(title, unit)
	}
	n := len(kept)
	h := 80 + 26*n
	f := newFigure(620, h)
	// 45 px below for the axis, 30 px above for the legend (ours).
	ax := f.AddAxes(0.3, 45/float64(h), 0.65, 1-75/float64(h))
	ys, cur, prev, labels := make([]float64, n), make([]float64, n), make([]float64, n), make([]string, n)
	for i, b := range kept {
		ys[i] = float64(n - 1 - i)
		cur[i], prev[i] = b.Current, b.Previous
		labels[i] = clip(labelOr(b.Label), 28)
	}
	up, down := make([]float64, n), make([]float64, n)
	for i := range ys {
		up[i], down[i] = ys[i]+0.2, ys[i]-0.2
	}
	ax.Barh(up, cur, plot.Width(0.4), plot.Color(palette[0]))
	ax.Barh(down, prev, plot.Width(0.4), plot.Color(otherColour))
	ax.SetYTickLabels(ys, labels, plot.PlainText())
	ax.SetXLabel(unit, plot.PlainText())
	legend := fmt.Sprintf(`<rect x="186" y="8" width="10" height="10" fill="%s"/><text x="200" y="17" font-size="12" fill="#1d2330">this period</text>`+
		`<rect x="290" y="8" width="10" height="10" fill="%s"/><text x="304" y="17" font-size="12" fill="#1d2330">previous period</text>`, palette[0], otherColour)
	out, ok := embed(f, 620, h, 620, title, unit, legend)
	if !ok {
		return noData(title, unit)
	}
	return out
}

// Series draws the amount per day.
func (plotCharts) Series(title, unit string, points []chartPoint) template.HTML {
	if len(points) == 0 {
		return noData(title, unit)
	}
	xs, ys := make([]float64, len(points)), make([]float64, len(points))
	for i, p := range points {
		xs[i], ys[i] = float64(i), p.Y
		if !finite(p.Y) {
			ys[i] = 0
		}
	}
	f := newFigure(620, 240)
	ax := f.AddAxes(0.1, 0.18, 0.86, 0.75)
	ax.Plot(xs, ys, plot.Color(palette[0]), plot.LineWidth(2))
	// First, middle and last day as tick labels.
	idx := []int{0}
	if len(points) > 2 {
		idx = append(idx, (len(points)-1)/2)
	}
	if len(points) > 1 {
		idx = append(idx, len(points)-1)
	}
	locs, labels := make([]float64, len(idx)), make([]string, len(idx))
	for i, j := range idx {
		locs[i], labels[i] = float64(j), clip(points[j].X, 16)
	}
	ax.SetXTickLabels(locs, labels, plot.PlainText())
	ax.SetYLabel(unit, plot.PlainText())
	out, ok := embed(f, 620, 240, 620, title, unit, "")
	if !ok {
		return noData(title, unit)
	}
	return out
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
