package gateway

import (
	"bytes"
	"fmt"
	"html/template"
	"regexp"
	"strings"

	"github.com/go-plot/plot"
)

// plotCharts draws the pie with go-plot/plot (v0.2.0 has Pie) and keeps
// svgCharts for the bars and the series: go-plot's titles and tick labels
// take no PlainText option, so a data label there would be typeset as TeX.
//
// No string from the data reaches go-plot at all: it draws the wedges and
// their percentages (formatted by us, PlainText), and the legend, which
// carries the names, is the gateway's own escaped SVG text. go-plot writes
// presentation attributes only and text as outlines; its ids are removed
// when nothing refers to them, so two pies on one page do not repeat one.
type plotCharts struct{ svgCharts }

var (
	xmlProlog = regexp.MustCompile(`^<\?xml[^>]*\?>\s*`)
	svgIDAttr = regexp.MustCompile(` id="[^"]*"`)
	// The root element's width and height (the first ones in the document).
	svgRootSize = regexp.MustCompile(`^<svg([^>]*?) width="[0-9.]+pt" height="[0-9.]+pt"`)
)

func (c plotCharts) Pie(title, unit string, in []chartSlice) template.HTML {
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
		return c.svgCharts.Pie(title, unit, in)
	}
	f := plot.NewFigure(plot.FigSize(2.5, 2.5), plot.DPI(96))
	ax := f.AddAxes(0, 0, 1, 1)
	err := ax.Pie(vals, plot.Colors(cols...), plot.StartAngle(90), plot.CounterClock(false), plot.PlainText(),
		plot.AutoPctFunc(func(pct float64) string {
			if pct < 5 {
				return ""
			}
			return fmt.Sprintf("%.0f%%", pct)
		}), plot.TextColor("#ffffff"), plot.EdgeColor("#ffffff"), plot.LineWidth(1))
	var b bytes.Buffer
	if err == nil {
		err = f.SaveSVG(&b)
	}
	if err != nil {
		return c.svgCharts.Pie(title, unit, in)
	}
	inner := xmlProlog.ReplaceAllString(b.String(), "")
	if !strings.Contains(inner, "url(#") && !strings.Contains(inner, `href="#`) {
		inner = svgIDAttr.ReplaceAllString(inner, "")
	}
	// go-plot's root is sized in points with its own viewBox; it becomes a
	// 240 × 240 box at the left of the chart, the legend on its right.
	if !svgRootSize.MatchString(inner) {
		return c.svgCharts.Pie(title, unit, in)
	}
	inner = svgRootSize.ReplaceAllString(inner, `<svg$1 x="0" y="0" width="240" height="240"`)
	var out strings.Builder
	fmt.Fprintf(&out, `<svg xmlns="http://www.w3.org/2000/svg" width="620" height="240" viewBox="0 0 620 240" role="img"><title>%s (%s)</title>`, esc(title), esc(unit))
	out.WriteString(inner)
	for i, s := range slices {
		y := 24 + i*24
		fmt.Fprintf(&out, `<rect x="250" y="%d" width="12" height="12" fill="%s"/><text x="268" y="%d" font-size="12" fill="#1d2330">%s — %s %s (%.1f %%)</text>`,
			y-10, cols[i], y, esc(labelOr(s.Label)), fmtNum(s.Value), esc(unit), 100*s.Value/total)
	}
	out.WriteString(`</svg>`)
	return template.HTML(out.String()) // go-plot's SVG holds no data string; every legend text is escaped
}
