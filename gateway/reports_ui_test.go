package gateway

import (
	"bytes"
	"math"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
	"github.com/go-plot/plot"
	"github.com/go-plot/plot/text"
)

func TestUIReports(t *testing.T) {
	e := newSSOEnv(t, "")
	expect(t, "user", e.do("POST", "/admin/users", master, `{"name":"carol"}`).StatusCode, 201)
	expect(t, "grant", e.do("PUT", "/admin/users/carol/roles", master, `{"role":"org_admin","org":"etabB"}`).StatusCode, 200, 201, 204)
	now := e.srv.now().UTC()
	from, to := now.AddDate(0, 0, -10), now.AddDate(0, 0, 1)
	_ = e.st.InsertRecords(t.Context(), []store.Record{
		{Org: "etabA", Lab: "physics", Team: "etabA", Project: "p1", User: "<script>alert(1)</script>", Model: "llama", Pool: "b300", Cost: 2, Time: now},
		{Org: "etabA", Lab: "physics", Team: "etabA", Project: "p1", User: "bob", Model: "llama", Pool: "b300", Cost: 1, Time: from.AddDate(0, 0, -3)},
		{Org: "etabA", Team: "etabA", Model: "claude", Cost: 3, Unit: "USD", Time: now},
		{Org: "etabB", Team: "etabB", Model: "llama", Pool: "b300", Cost: 7, Time: now},
	})
	q := url.Values{"from": {from.Format("2006-01-02")}, "to": {to.Format("2006-01-02")}}
	sess, _ := e.login(e.claims("alice", "llm-admins"))
	code, body, csp := e.uiGet("/ui/reports?"+q.Encode()+"&group=org", sess)
	if code != 200 || csp != uiCSP {
		t.Fatalf("%d %q", code, csp)
	}
	// No script, no inline style (the CSP would block it), and the charts.
	for _, bad := range []string{"<script", "style=", "javascript:"} {
		if strings.Contains(body, bad) {
			t.Fatalf("page contains %q", bad)
		}
	}
	for _, want := range []string{"<svg", "<h2>GPU-hours</h2>", "<h2>USD</h2>", "Total <strong>9.00 GPU-hours</strong>",
		"Total <strong>3.00 USD</strong>", q.Get("from"), q.Get("to"), "group=lab", "new"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q", want)
		}
	}
	// Drill-down: org etabA → lab physics → … → user; the previous period
	// is compared (2 against 1: +100 %).
	_, body, _ = e.uiGet("/ui/reports?"+q.Encode()+"&group=project&org=etabA&lab=physics", sess)
	if !strings.Contains(body, "&#43;100.0 %") || !strings.Contains(body, "group=user") || strings.Contains(body, "<h2>USD</h2>") {
		t.Fatalf("drill-down: %s", body)
	}
	_, body, _ = e.uiGet("/ui/reports?"+q.Encode()+"&group=user", sess)
	if strings.Contains(body, "<script>alert") || !strings.Contains(body, "&lt;script&gt;") {
		t.Fatal("a user name came back unescaped")
	}
	// Bounds and bad input.
	for _, bad := range []string{"from=2020-01-01&to=2026-01-01", "from=x", "group=planet", "from=2026-02-01&to=2026-01-01"} {
		if c, b, _ := e.uiGet("/ui/reports?"+bad, sess); c != 200 || !strings.Contains(b, `class="err"`) {
			t.Errorf("%s: %d", bad, c)
		}
	}
	// An org_admin sees their organisation only.
	sess, _ = e.login(e.claims("carol"))
	_, body, _ = e.uiGet("/ui/reports?"+q.Encode()+"&group=team", sess)
	if strings.Contains(body, "etabA") || !strings.Contains(body, "etabB") {
		t.Fatalf("org admin sees another org")
	}
}

// FuzzReportsQuery sends arbitrary queries to the Reports page: no 5xx,
// the CSP unchanged, and nothing the query said comes back unescaped.
func FuzzReportsQuery(f *testing.F) {
	f.Add("from=2026-01-01&to=2026-02-01&group=org")
	f.Add("group=user&org=%3Cscript%3E&lab=")
	f.Add("from=0001-01-01&to=9999-12-31")
	f.Add("group=day&day=x&tag=%22%3E%3Csvg")
	e := newSSOEnv(f, "")
	sess, _ := e.login(e.claims("alice", "llm-admins"))
	_ = e.st.InsertRecords(f.Context(), []store.Record{{Org: "etabA", Team: "etabA", Model: "llama", Cost: 1, Time: e.srv.now()}})
	f.Fuzz(func(t *testing.T, q string) {
		e.t = t // iterations run one after the other in a fuzz process
		vals, err := url.ParseQuery(q)
		if err != nil {
			return
		}
		resp := e.req("GET", "/ui/reports?"+vals.Encode(), map[string]string{sessionCookie: sess}, "")
		body := readAll(t, resp)
		if resp.StatusCode >= 500 || resp.Header.Get("Content-Security-Policy") != uiCSP {
			t.Fatalf("%d %q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
		}
		if strings.Contains(body, "<script") || strings.Contains(body, "style=") {
			t.Fatalf("unescaped: %s", body)
		}
	})
}

// TestPlotPie: go-plot draws the wedges, no data string reaches it, the
// legend escapes the names, and two pies on one page share no id.
func TestPlotPie(t *testing.T) {
	var c chartRenderer = plotCharts{}
	in := []chartSlice{{Label: `$\input{/etc/passwd}$`, Value: 3}, {Label: "<script>alert(1)</script>", Value: 2}, {Label: "$5 and $6", Value: 1}}
	start := time.Now()
	a, b := string(c.Pie("one", "GPU-hours", in)), string(c.Pie("two", "USD", in[:2]))
	t.Logf("two pies in %v", time.Since(start))
	page := a + b
	if p := os.Getenv("LLMFLOW_PIE_SAMPLE"); p != "" {
		_ = os.WriteFile(p, []byte(a), 0o600) // for a person to look at
		// The two pies on one page under the UI's policy, and a control page
		// with a planted inline style the policy must refuse.
		head := `<!doctype html><html><head><meta http-equiv="Content-Security-Policy" content="` + uiCSP + `"></head><body>`
		bars := c.Bars("bars", "GPU-hours", []chartBar{{Label: in[0].Label, Current: 3, Previous: 2}, {Label: in[1].Label, Current: 2, Previous: 4}})
		series := c.Series("series", "GPU-hours", []chartPoint{{X: "2026-10-01", Y: 1}, {X: "2026-10-02", Y: 3}, {X: "2026-10-03", Y: 2}})
		_ = os.WriteFile(p+".html", []byte(head+a+b+string(bars)+string(series)+"</body></html>"), 0o600)
		_ = os.WriteFile(p+".control.html", []byte(head+`<p style="color:red">x</p>`+a+"</body></html>"), 0o600)
	}
	if !strings.Contains(a, "xmlns:xlink") || strings.Count(a, "<svg") != 2 {
		t.Fatal("go-plot was not used (fallback)")
	}
	for _, bad := range []string{"<script", "style=", " id=", "<foreignObject", "href=", "<image", "root:"} {
		if strings.Contains(page, bad) {
			t.Fatalf("page contains %q", bad)
		}
	}
	for _, want := range []string{`$\input{/etc/passwd}$ —`, "&lt;script&gt;alert(1)&lt;/script&gt; —", "$5 and $6 —"} {
		if !strings.Contains(a, want) {
			t.Errorf("legend misses %q", want)
		}
	}
	// Refused sizes fall back to the gateway's renderer, never to an error.
	if s := string(c.Pie("z", "GPU-hours", []chartSlice{{Label: "x", Value: 0}})); !strings.Contains(s, "no data") {
		t.Fatalf("zero: %s", s)
	}
}

func TestCharts(t *testing.T) {
	var c chartRenderer = plotCharts{}
	var many []chartSlice
	for i := 0; i < 30; i++ {
		many = append(many, chartSlice{Label: "<&\"'>" + string(rune('a'+i%26)), Value: float64(i)})
	}
	many = append(many, chartSlice{Label: "nan", Value: math.NaN()}, chartSlice{Label: "inf", Value: math.Inf(1)})
	out := []string{
		string(c.Pie("t<", "GPU-hours", many)),
		string(c.Pie("one", "USD", []chartSlice{{Label: "x", Value: 1}})),
		string(c.Pie("none", "USD", nil)),
		string(c.Bars("b", "GPU-hours", []chartBar{{Label: "<x>", Current: 1, Previous: 2}, {Label: "y", Current: math.NaN()}})),
		string(c.Bars("b", "GPU-hours", nil)),
		string(c.Series("s", "GPU-hours", []chartPoint{{X: "<d>", Y: 1}, {X: "d2", Y: math.Inf(1)}, {X: "d3", Y: 0}})),
		string(c.Series("s", "GPU-hours", []chartPoint{{X: "d", Y: 1}})),
		string(c.Series("s", "GPU-hours", nil)),
	}
	for i, s := range out {
		if strings.Contains(s, "<&") || strings.Contains(s, "<x>") || strings.Contains(s, "<d>") || strings.Contains(s, "style=") ||
			strings.Contains(s, "<script") || strings.Contains(s, "NaN") || !strings.HasPrefix(s, "<svg") || !strings.HasSuffix(s, "</svg>") {
			t.Errorf("bad svg %d: %.300s", i, s)
		}
	}
	if !strings.Contains(out[0], ">other —") {
		t.Error("small slices not folded into other")
	}
	for _, i := range []int{3, 5, 6} {
		if !strings.Contains(out[i], "xmlns:xlink") {
			t.Errorf("chart %d not drawn by go-plot", i)
		}
	}
	for _, i := range []int{2, 4, 7} {
		if !strings.Contains(out[i], "no data") {
			t.Errorf("chart %d: want no data", i)
		}
	}
}

// texSpy records which strings go-plot laid out as TeX and which as plain
// text.
type texSpy struct {
	*text.Default
	mu          sync.Mutex
	math, plain []string
}

func (s *texSpy) Layout(str string, px float64) text.Layout {
	s.mu.Lock()
	s.math = append(s.math, str)
	s.mu.Unlock()
	return s.Default.Layout(str, px)
}

func (s *texSpy) LayoutPlain(str string, px float64) text.Layout {
	s.mu.Lock()
	s.plain = append(s.plain, str)
	s.mu.Unlock()
	return s.Default.LayoutPlain(str, px)
}

// TestChartsNoTeX: no string from the data reaches go-plot's TeX layout,
// on any chart; the control proves the spy sees the TeX path.
func TestChartsNoTeX(t *testing.T) {
	hostile := []string{`$\input{/etc/passwd}$`, "<script>alert(1)</script>", "$5 and $6", `100$`}
	spy := &texSpy{Default: text.NewDefault()}
	figureHook = func(f *plot.Figure) { f.Text = spy }
	defer func() { figureHook = nil }()
	var c chartRenderer = plotCharts{}
	var slices []chartSlice
	var bars []chartBar
	var points []chartPoint
	for i, h := range hostile {
		slices = append(slices, chartSlice{Label: h, Value: float64(i + 1)})
		bars = append(bars, chartBar{Label: h, Current: float64(i + 1), Previous: 1})
		points = append(points, chartPoint{X: h, Y: float64(i)})
	}
	unit := `$\alpha$`
	_ = c.Pie(hostile[0], unit, slices)
	_ = c.Bars(hostile[0], unit, bars)
	_ = c.Series(hostile[0], unit, points)
	// Only go-plot's own numbers (tick values, percentages) may take the
	// TeX path; nothing with a dollar or a backslash, nothing of ours.
	for _, m := range spy.math {
		if strings.ContainsAny(m, `$\<`) {
			t.Fatalf("%q was laid out as TeX", m)
		}
	}
	seen := strings.Join(spy.plain, "\n")
	for _, want := range []string{`$\input{/etc/passwd}$`, "$5 and $6", unit} {
		if !strings.Contains(seen, want) {
			t.Errorf("%q not laid out as plain text (plain: %q)", want, spy.plain)
		}
	}
	// Control: the same tick label without PlainText, on a figure that
	// parses math, reaches the TeX layout, and the spy sees it.
	ctl := &texSpy{Default: text.NewDefault()}
	f := plot.NewFigure()
	f.Text = ctl
	ax := f.AddAxes(0.1, 0.1, 0.8, 0.8)
	ax.Barh([]float64{0}, []float64{1})
	ax.SetYTickLabels([]float64{0}, []string{unit})
	var b bytes.Buffer
	_ = f.SaveSVG(&b)
	if !strings.Contains(strings.Join(ctl.math, "\n"), unit) {
		t.Fatalf("the control did not reach TeX: math %q plain %q", ctl.math, ctl.plain)
	}
}
