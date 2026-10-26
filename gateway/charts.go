package gateway

import (
	"fmt"
	"html"
	"html/template"
	"math"
	"sort"
	"strconv"
)

// Charts of the Reports page (#68) and of the PDF summary: the data types,
// the palette and the helpers both use. The page's charts are drawn by
// go-plot/plot (plot_charts.go); the PDF draws its own with go-pdfkit.

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

// noData is the chart of an empty period.
func noData(title, unit string) template.HTML {
	return template.HTML(fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="320" height="30" viewBox="0 0 320 30" role="img"><title>%s (%s)</title><text x="10" y="20" fill="#1d2330">no data</text></svg>`,
		esc(title), esc(unit)))
}

func labelOr(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
