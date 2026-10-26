package gateway

import (
	"math"
	"net/url"
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
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

func TestCharts(t *testing.T) {
	var c chartRenderer = svgCharts{}
	var many []chartSlice
	for i := 0; i < 30; i++ {
		many = append(many, chartSlice{Label: "<&\"'>" + string(rune('a'+i%26)), Value: float64(i)})
	}
	many = append(many, chartSlice{Label: "nan", Value: math.NaN()}, chartSlice{Label: "inf", Value: math.Inf(1)})
	out := []string{
		string(c.Pie("t<", "GPU-hours", many)),
		string(c.Pie("one", "USD", []chartSlice{{Label: "x", Value: 1}})),
		string(c.Pie("none", "USD", nil)),
		string(c.Bars("b", "GPU-hours", []chartBar{{Label: "<x>", Current: 1, Previous: 2}})),
		string(c.Series("s", "GPU-hours", []chartPoint{{X: "<d>", Y: 1}, {X: "d2", Y: 0}})),
		string(c.Series("s", "GPU-hours", nil)),
	}
	for _, s := range out {
		if strings.Contains(s, "<&") || strings.Contains(s, "<x>") || strings.Contains(s, "<d>") || strings.Contains(s, "style=") ||
			strings.Contains(s, "NaN") || !strings.HasPrefix(s, "<svg") || !strings.HasSuffix(s, "</svg>") {
			t.Errorf("bad svg: %s", s)
		}
	}
	if !strings.Contains(out[0], ">other —") {
		t.Error("small slices not folded into other")
	}
	if !strings.Contains(out[1], "<circle") {
		t.Error("a single slice is a full circle")
	}
}
