package gateway

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func quotaFixture(visible func(string) bool) quotaInputs {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	oct16 := time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)
	long := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	home := map[string][2]string{}
	var assigns []store.Assignment
	var events []store.WarnEventRow
	for _, u := range []string{"u1", "u2", "u3", "u4", "u5", "u6"} {
		home[u] = [2]string{"etabA", "genomics"}
		a := store.Assignment{User: u, Profile: "p1", From: long}
		if u == "u6" {
			a.From = oct16 // joined mid-month
		}
		assigns = append(assigns, a)
	}
	for _, u := range []string{"v1", "v2"} {
		home[u] = [2]string{"etabB", ""}
		assigns = append(assigns, store.Assignment{User: u, Profile: "p2", From: long})
		events = append(events, store.WarnEventRow{User: u, Window: "week", Threshold: 75, N: 4})
	}
	for _, u := range []string{"u1", "u2", "u3", "u4", "u5"} {
		events = append(events, store.WarnEventRow{User: u, Window: "week", Threshold: 75, N: 1})
	}
	events = append(events, store.WarnEventRow{User: "u1", Window: "session", Threshold: 95, N: 2}) // one person
	return quotaInputs{
		From: from, To: to, K: 5, Events: events,
		Refusals: []store.RefusalRow{
			{Org: "etabA", Team: "t1", Profile: "p1", Kind: store.RefusalProfileWeek, N: 7},
			{Org: "etabA", Team: "t1", Profile: "p1", Kind: store.RefusalProfileSession, N: 3}, // under the threshold
			{Org: "etabB", Team: "t2", Kind: store.RefusalBudget, N: 9},
		},
		Assignments: assigns,
		Used:        []store.ReportRow{{Org: "etabA", Lab: "genomics", Profile: "p1", Cost: 66}},
		Profiles:    config.Profiles{List: []config.Profile{{ID: "p1", Days: 5}, {ID: "p2", Days: 2}}},
		Workday:     3,
		Home:        func(u string) (string, string) { return home[u][0], home[u][1] },
		LabOfTeam: func(t string) string {
			if t == "t1" {
				return "genomics"
			}
			return ""
		},
		Capacity: func(string, time.Time) *float64 { c := 20.0; return &c },
		Visible:  visible,
	}
}

func near2(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Golden values, computed by hand: see the comments.
func TestQuotaReportGolden(t *testing.T) {
	q := buildQuotas(quotaFixture(func(string) bool { return true }))

	// Soft quotas: five people crossed 75 % in a week (shown, partner and lab
	// level); one person crossed 95 % in a session (suppressed); etabB's two
	// people are under the threshold (suppressed).
	var weekA, weekLab, sess, weekB *SoftRow
	for i := range q.Soft {
		r := &q.Soft[i]
		switch {
		case r.Org == "etabA" && r.Lab == "" && r.Window == "week":
			weekA = r
		case r.Org == "etabA" && r.Lab == "genomics" && r.Window == "week":
			weekLab = r
		case r.Org == "etabA" && r.Window == "session":
			sess = r
		case r.Org == "etabB" && r.Window == "week":
			weekB = r
		}
	}
	if weekA == nil || weekA.Suppressed || *weekA.Events != 5 || *weekA.People != 5 || weekLab == nil || *weekLab.Events != 5 {
		t.Fatalf("week: %+v %+v", weekA, weekLab)
	}
	if sess == nil || !sess.Suppressed || sess.Events != nil || sess.People != nil {
		t.Fatalf("a one-person cell is shown: %+v", sess)
	}
	if weekB == nil || !weekB.Suppressed || weekB.Events != nil {
		t.Fatalf("etabB's two people are shown: %+v", weekB)
	}

	// Refusals: 7 shown, 3 suppressed, etabB's 9 shown (a count, no person).
	got := map[string]*RefusalOut{}
	for i := range q.Refusals {
		r := &q.Refusals[i]
		got[r.Org+"|"+r.Lab+"|"+r.Kind] = r
	}
	if r := got["etabA||profile_week"]; r == nil || r.Suppressed || *r.Refusals != 7 || r.Profile != "p1" {
		t.Fatalf("%+v", r)
	}
	if r := got["etabA|genomics|profile_week"]; r == nil || *r.Refusals != 7 {
		t.Fatalf("lab level: %+v", r)
	}
	if r := got["etabA||profile_session"]; r == nil || !r.Suppressed || r.Refusals != nil {
		t.Fatalf("a 3-refusal cell is shown: %+v", r)
	}
	if r := got["etabB||budget"]; r == nil || r.Suppressed || *r.Refusals != 9 {
		t.Fatalf("%+v", r)
	}

	// Workload days. p1: 5 people for the 31 days (31 x 5/7 = 22.142857 each)
	// and u6 for 16 days (16 x 5/7 = 11.428571): 110.714286 + 11.428571 =
	// 122.142857. Used: 66 GPU-hours / 3 = 22 days; utilisation 22 / 122.142857.
	var p1, p2 *ProfileRow
	for i := range q.Profiles {
		r := &q.Profiles[i]
		if r.Org == "etabA" && r.Lab == "" && r.Profile == "p1" {
			p1 = r
		}
		if r.Org == "etabB" && r.Profile == "p2" {
			p2 = r
		}
	}
	if p1 == nil || p1.Suppressed || *p1.People != 6 || !near2(*p1.AssignedDays, 122.14) || !near2(*p1.UsedDays, 22) || !near2(*p1.Utilisation, 0.18) {
		t.Fatalf("p1: %+v", p1)
	}
	if p2 == nil || !p2.Suppressed || p2.AssignedDays != nil || p2.UsedDays != nil {
		t.Fatalf("p2 (2 people): %+v", p2)
	}

	// Oversubscription: 5 people x 5 days = 25 a week until Oct 15, 30 from
	// Oct 16 when u6 joins; capacity 20: ratios 1.25 then 1.5. etabB: 2 people.
	by := map[string]OversubRow{}
	n := 0
	for _, r := range q.Oversub {
		by[r.Org+" "+r.Day] = r
		if r.Org == "etabA" {
			n++
		}
	}
	if n != 31 {
		t.Fatalf("%d sampled days, want 31", n)
	}
	if r := by["etabA 2026-10-01"]; r.Ratio == nil || !near2(*r.Ratio, 1.25) || !near2(*r.AssignedDaysPerWeek, 25) || !near2(*r.CapacityDaysPerWeek, 20) {
		t.Fatalf("Oct 1: %+v", r)
	}
	if r := by["etabA 2026-10-16"]; r.Ratio == nil || !near2(*r.Ratio, 1.5) || *r.People != 6 {
		t.Fatalf("Oct 16: %+v", r)
	}
	if r := by["etabB 2026-10-10"]; !r.Suppressed || r.Ratio != nil || r.AssignedDaysPerWeek != nil {
		t.Fatalf("etabB's series is shown: %+v", r)
	}
	// No person anywhere in the output.
	b, _ := json.Marshal(q)
	for _, u := range []string{"u1", "u6", "v1", "v2"} {
		if strings.Contains(string(b), `"`+u+`"`) {
			t.Fatalf("person %s in the output: %s", u, b)
		}
	}
}

// An organisation's reader sees its own partner and nothing of another.
func TestQuotaReportVisibility(t *testing.T) {
	q := buildQuotas(quotaFixture(func(o string) bool { return o == "etabA" }))
	b, _ := json.Marshal(q)
	if strings.Contains(string(b), "etabB") || !strings.Contains(string(b), "etabA") {
		t.Fatalf("%s", b)
	}
}

// The threshold is a parameter: a cell of 5 is shown at k = 5 and not at k = 6,
// and the lab row follows the partner row.
func TestQuotaReportThreshold(t *testing.T) {
	in := quotaFixture(func(string) bool { return true })
	in.K = 6
	q := buildQuotas(in)
	for _, r := range q.Soft {
		if r.Org == "etabA" && r.Window == "week" && !r.Suppressed {
			t.Fatalf("5 people shown at k = 6: %+v", r)
		}
	}
	for _, r := range q.Profiles {
		if r.Org == "etabA" && r.Profile == "p1" && r.Suppressed {
			t.Fatalf("6 people hidden at k = 6: %+v", r)
		}
	}
}

// Any input: no shown cell is under the threshold, and a suppressed one has
// no number.
func FuzzBuildQuotas(f *testing.F) {
	f.Add(5, 3, int64(2), 1.5)
	f.Fuzz(func(t *testing.T, k, people int, n int64, workday float64) {
		if k < 2 || k > 50 || people < 0 || people > 40 || n < 0 || n > 1e6 || math.IsNaN(workday) || math.IsInf(workday, 0) {
			return
		}
		in := quotaFixture(func(string) bool { return true })
		in.K, in.Workday = k, workday
		in.Events = nil
		for i := 0; i < people; i++ {
			in.Events = append(in.Events, store.WarnEventRow{User: "x" + string(rune('a'+i%26)) + string(rune('a'+i/26)), Window: "week", Threshold: 75, N: n})
		}
		in.Refusals = []store.RefusalRow{{Org: "etabA", Team: "t1", Kind: store.RefusalBudget, N: n}}
		in.Home = func(u string) (string, string) { return "etabA", "genomics" }
		q := buildQuotas(in)
		for _, r := range q.Soft {
			if !r.Suppressed && (r.People == nil || *r.People < int64(k)) || r.Suppressed && (r.Events != nil || r.People != nil) {
				t.Fatalf("soft cell %+v at k=%d", r, k)
			}
		}
		for _, r := range q.Refusals {
			if !r.Suppressed && (r.Refusals == nil || *r.Refusals < int64(k)) || r.Suppressed && r.Refusals != nil {
				t.Fatalf("refusal cell %+v at k=%d", r, k)
			}
		}
		for _, r := range q.Profiles {
			if !r.Suppressed && (r.People == nil || *r.People < int64(k)) || r.Suppressed && (r.AssignedDays != nil || r.UsedDays != nil) {
				t.Fatalf("profile cell %+v", r)
			}
		}
		for _, r := range q.Oversub {
			if r.Suppressed && (r.Ratio != nil || r.AssignedDaysPerWeek != nil) {
				t.Fatalf("oversub cell %+v", r)
			}
		}
	})
}
