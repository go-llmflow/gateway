package gateway

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Server-rendered charts for the Reports page (#68): inline SVG, no script,
// and no style attribute (the UI's CSP allows styles from /ui/style.css
// only), so every colour is a presentation attribute. Every text is escaped.
//
// The charts sit behind chartRenderer so that go-plot/plot can draw them
// once its pie chart is released (go-plot/plot v0.1.1 has none); svgCharts
// is the gateway's own small renderer until then.

type chartSlice struct {
	Label string
	Value float64
}

type chartBar struct {
	Label             string
	Current, Previous float64
}

type chartPoint struct {
	X string
	Y float64
}

type chartRenderer interface {
	Pie(title, unit string, slices []chartSlice) template.HTML
	Bars(title, unit string, bars []chartBar) template.HTML
	Series(title, unit string, points []chartPoint) template.HTML
}

// palette: colour-blind-safe (Okabe–Ito), then grey for "other".
var palette = []string{"#0072B2", "#E69F00", "#009E73", "#CC79A7", "#56B4E9", "#D55E00", "#F0E442", "#000000"}

const otherColour = "#999999"

type svgCharts struct{}

func esc(s string) string { return html.EscapeString(s) }

func fmtNum(v float64) string {
	switch {
	case v == 0:
		return "0"
	case math.Abs(v) >= 100:
		return strconv.FormatFloat(v, 'f', 0, 64)
	case math.Abs(v) >= 1:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
	return strconv.FormatFloat(v, 'g', 3, 64)
}

// topSlices keeps the 7 largest slices and folds the rest into "other".
func topSlices(in []chartSlice) []chartSlice {
	s := make([]chartSlice, 0, len(in))
	for _, x := range in {
		if x.Value > 0 && !math.IsInf(x.Value, 0) && !math.IsNaN(x.Value) {
			s = append(s, x)
		}
	}
	sort.SliceStable(s, func(i, j int) bool { return s[i].Value > s[j].Value })
	if len(s) <= len(palette)-1 {
		return s
	}
	other := 0.0
	for _, x := range s[len(palette)-1:] {
		other += x.Value
	}
	return append(s[:len(palette)-1:len(palette)-1], chartSlice{Label: "other", Value: other})
}

func (svgCharts) Pie(title, unit string, in []chartSlice) template.HTML {
	slices := topSlices(in)
	var b strings.Builder
	const w, h, cx, cy, r = 520, 240, 120, 120, 100
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img"><title>%s (%s)</title>`,
		w, h, w, h, esc(title), esc(unit))
	total := 0.0
	for _, s := range slices {
		total += s.Value
	}
	if total <= 0 {
		b.WriteString(`<text x="10" y="20" fill="#1d2330">no data</text></svg>`)
		return template.HTML(b.String())
	}
	angle := -math.Pi / 2
	for i, s := range slices {
		colour := palette[i%len(palette)]
		if s.Label == "other" {
			colour = otherColour
		}
		frac := s.Value / total
		if frac >= 0.9999 {
			fmt.Fprintf(&b, `<circle cx="%d" cy="%d" r="%d" fill="%s"/>`, cx, cy, r, colour)
		} else {
			end := angle + 2*math.Pi*frac
			x1, y1 := cx+r*math.Cos(angle), cy+r*math.Sin(angle)
			x2, y2 := cx+r*math.Cos(end), cy+r*math.Sin(end)
			large := 0
			if frac > 0.5 {
				large = 1
			}
			fmt.Fprintf(&b, `<path d="M%d,%d L%.2f,%.2f A%d,%d 0 %d 1 %.2f,%.2f Z" fill="%s" stroke="#ffffff" stroke-width="1"/>`,
				cx, cy, x1, y1, r, r, large, x2, y2, colour)
			angle = end
		}
		y := 24 + i*24
		fmt.Fprintf(&b, `<rect x="250" y="%d" width="12" height="12" fill="%s"/><text x="268" y="%d" font-size="12" fill="#1d2330">%s — %s %s (%.1f %%)</text>`,
			y-10, colour, y, esc(labelOr(s.Label)), fmtNum(s.Value), esc(unit), 100*frac)
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String()) // every text above was escaped
}

func (svgCharts) Bars(title, unit string, bars []chartBar) template.HTML {
	if len(bars) > 20 {
		bars = bars[:20]
	}
	max := 0.0
	for _, x := range bars {
		max = math.Max(max, math.Max(x.Current, x.Previous))
	}
	const left, barW = 180, 300
	h := 30 + 26*len(bars)
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="620" height="%d" viewBox="0 0 620 %d" role="img"><title>%s (%s)</title>`,
		h, h, esc(title), esc(unit))
	fmt.Fprintf(&b, `<rect x="%d" y="4" width="10" height="8" fill="%s"/><text x="%d" y="12" font-size="11" fill="#1d2330">this period</text>`, left, palette[0], left+14)
	fmt.Fprintf(&b, `<rect x="%d" y="4" width="10" height="8" fill="%s"/><text x="%d" y="12" font-size="11" fill="#1d2330">previous period</text>`, left+100, otherColour, left+114)
	for i, x := range bars {
		y := 24 + 26*i
		cw, pw := 0.0, 0.0
		if max > 0 {
			cw, pw = barW*x.Current/max, barW*x.Previous/max
		}
		fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="12" text-anchor="end" fill="#1d2330">%s</text>`, left-6, y+11, esc(clip(labelOr(x.Label), 28)))
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%.1f" height="10" fill="%s"/>`, left, y, cw, palette[0])
		fmt.Fprintf(&b, `<rect x="%d" y="%d" width="%.1f" height="6" fill="%s"/>`, left, y+11, pw, otherColour)
		fmt.Fprintf(&b, `<text x="%.1f" y="%d" font-size="11" fill="#1d2330">%s</text>`, float64(left)+cw+4, y+9, fmtNum(x.Current))
	}
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func (svgCharts) Series(title, unit string, points []chartPoint) template.HTML {
	const w, h, left, top, pw, ph = 620, 220, 60, 16, 540, 160
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img"><title>%s (%s)</title>`,
		w, h, w, h, esc(title), esc(unit))
	if len(points) == 0 {
		b.WriteString(`<text x="10" y="20" fill="#1d2330">no data</text></svg>`)
		return template.HTML(b.String())
	}
	max := 0.0
	for _, p := range points {
		max = math.Max(max, p.Y)
	}
	if max <= 0 {
		max = 1
	}
	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#888888"/>`, left, top+ph, left+pw, top+ph)
	fmt.Fprintf(&b, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#888888"/>`, left, top, left, top+ph)
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" text-anchor="end" fill="#1d2330">%s</text>`, left-4, top+8, fmtNum(max))
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" text-anchor="end" fill="#1d2330">0</text>`, left-4, top+ph)
	var pts []string
	step := 0.0
	if len(points) > 1 {
		step = float64(pw) / float64(len(points)-1)
	}
	for i, p := range points {
		x := float64(left) + step*float64(i)
		y := float64(top+ph) - float64(ph)*p.Y/max
		pts = append(pts, fmt.Sprintf("%.1f,%.1f", x, y))
	}
	fmt.Fprintf(&b, `<polyline points="%s" fill="none" stroke="%s" stroke-width="2"/>`, strings.Join(pts, " "), palette[0])
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" fill="#1d2330">%s</text>`, left, top+ph+16, esc(points[0].X))
	fmt.Fprintf(&b, `<text x="%d" y="%d" font-size="11" text-anchor="end" fill="#1d2330">%s</text>`, left+pw, top+ph+16, esc(points[len(points)-1].X))
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}

func labelOr(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
