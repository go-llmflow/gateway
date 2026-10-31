package gateway

import (
	"strconv"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// labsFixture is one partner whose people are spread over labs: n[lab] people
// per lab ("" is a person with no lab), each with one soft-quota event, one
// refusal and a profile assignment.
func labsFixture(k int, n map[string]int) quotaInputs {
	from := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	home := map[string][2]string{}
	teamLab := map[string]string{}
	in := quotaInputs{From: from, To: from.AddDate(0, 1, 0), K: k, Workday: 3,
		Profiles: config.Profiles{List: []config.Profile{{ID: "p1", Days: 5}}},
		Capacity: func(string, time.Time) *float64 { return nil },
		Visible:  func(string) bool { return true },
	}
	for lab, c := range n {
		team := "team-" + lab
		teamLab[team] = lab
		for i := 0; i < c; i++ {
			u := lab + "-" + strconv.Itoa(i)
			home[u] = [2]string{"etabA", lab}
			in.Events = append(in.Events, store.WarnEventRow{User: u, Window: "week", Threshold: 75, N: 1})
			in.Refusals = append(in.Refusals, store.RefusalRow{Org: "etabA", Team: team, Profile: "p1", Kind: store.RefusalBudget, N: 1})
			in.RefusalUsers = append(in.RefusalUsers, store.RefusalUser{Org: "etabA", Team: team, Profile: "p1", Kind: store.RefusalBudget, User: store.UserHash(u)})
			in.Assignments = append(in.Assignments, store.Assignment{User: u, Profile: "p1", From: from.AddDate(0, -1, 0)})
		}
	}
	in.Home = func(u string) (string, string) { return home[u][0], home[u][1] }
	in.LabOfTeam = func(t string) string { return teamLab[t] }
	return in
}

// The partner has 6 people (shown), lab A 5 (shown) and lab B 1 (suppressed):
// partner minus lab A would give lab B's one person. Lab A is hidden too.
func TestQuotaSecondarySuppression(t *testing.T) {
	q := buildQuotas(labsFixture(5, map[string]int{"labA": 5, "labB": 1}))
	for _, r := range q.Soft {
		switch r.Lab {
		case "":
			if r.Suppressed || *r.People != 6 || *r.Events != 6 {
				t.Errorf("the partner row: %+v", r)
			}
		default:
			if !r.Suppressed || r.Events != nil || r.People != nil {
				t.Errorf("lab %s is published, so the partner row minus it gives the other: %+v", r.Lab, r)
			}
		}
	}
	for _, r := range q.Refusals {
		if r.Lab != "" && !r.Suppressed {
			t.Errorf("refusals: lab %s published: %+v", r.Lab, r)
		}
		if r.Lab == "" && (r.Suppressed || *r.Refusals != 6) {
			t.Errorf("refusals: the partner row: %+v", r)
		}
	}
	for _, r := range q.Profiles {
		if r.Lab != "" && !r.Suppressed {
			t.Errorf("profiles: lab %s published: %+v", r.Lab, r)
		}
		if r.Lab == "" && (r.Suppressed || *r.People != 6) {
			t.Errorf("profiles: the partner row: %+v", r)
		}
	}
	// Two labs of 5 and 6: all shown, nothing to recover.
	q = buildQuotas(labsFixture(5, map[string]int{"labA": 5, "labB": 6}))
	for _, r := range q.Soft {
		if r.Suppressed {
			t.Errorf("5 and 6 people: %+v", r)
		}
	}
	// A partner of 7 with labs of 5 and 2: the lab of 2 is hidden, so lab A is
	// hidden too (rule 2) and the partner stays (its 7 people are not a small
	// residual).
	q = buildQuotas(labsFixture(5, map[string]int{"labA": 5, "labB": 2}))
	for _, r := range q.Soft {
		if r.Lab != "" && !r.Suppressed {
			t.Errorf("5 and 2: %+v", r)
		}
		if r.Lab == "" && r.Suppressed {
			t.Errorf("the partner of 7 should stay: %+v", r)
		}
	}
	// Three labs of 5, 1 and 2: two are hidden, so the partner (8) minus lab A
	// would be 3 people (< 5): the partner row is hidden rather than leak them.
	q = buildQuotas(labsFixture(5, map[string]int{"labA": 5, "labB": 1, "labC": 2}))
	for _, r := range q.Soft {
		if r.Lab == "" && !r.Suppressed {
			t.Errorf("the partner row leaks the 3 hidden people: %+v", r)
		}
	}
	checkNoSmallResidual(t, q)
	// People with no lab are a residual too: 5 in lab A, 2 with no lab.
	q = buildQuotas(labsFixture(5, map[string]int{"labA": 5, "": 2}))
	checkNoSmallResidual(t, q)
}

// checkNoSmallResidual: in every group of the published rows, the partner's
// people minus the shown labs' people is zero or at least k.
func checkNoSmallResidual(t *testing.T, q *QuotaReport) {
	t.Helper()
	type g struct {
		org, window string
		th          int
	}
	parent := map[g]*SoftRow{}
	sum := map[g]int64{}
	for i := range q.Soft {
		r := &q.Soft[i]
		key := g{r.Org, r.Window, r.Threshold}
		if r.Lab == "" {
			parent[key] = r
		} else if !r.Suppressed {
			sum[key] += *r.People
		}
	}
	for key, p := range parent {
		if p.Suppressed {
			continue
		}
		if res := *p.People - sum[key]; res > 0 && res < int64(q.MinCell) {
			t.Errorf("%+v: partner %d minus shown labs %d leaves %d (< %d)", key, *p.People, sum[key], res, q.MinCell)
		}
	}
	pp := map[string]*ProfileRow{}
	psum := map[string]int64{}
	for i := range q.Profiles {
		r := &q.Profiles[i]
		if r.Lab == "" {
			pp[r.Org+r.Profile] = r
		} else if !r.Suppressed {
			psum[r.Org+r.Profile] += *r.People
		}
	}
	for key, p := range pp {
		if p.Suppressed {
			continue
		}
		if res := *p.People - psum[key]; res > 0 && res < int64(q.MinCell) {
			t.Errorf("profile %s: partner %d minus shown labs %d leaves %d", key, *p.People, psum[key], res)
		}
	}
}

// Fuzz: whatever the spread of people over labs, no published partner figure
// minus the published lab figures is a value from fewer than k people.
func FuzzQuotaSubtraction(f *testing.F) {
	f.Add(uint8(5), uint8(5), uint8(1), uint8(0), uint8(0))
	f.Add(uint8(5), uint8(5), uint8(2), uint8(1), uint8(2))
	f.Add(uint8(2), uint8(2), uint8(1), uint8(1), uint8(1))
	f.Fuzz(func(t *testing.T, k, a, b, c, none uint8) {
		kk := int(k%7) + 2
		in := labsFixture(kk, map[string]int{"labA": int(a % 14), "labB": int(b % 14), "labC": int(c % 14), "": int(none % 14)})
		q := buildQuotas(in)
		checkNoSmallResidual(t, q)
		for _, r := range q.Refusals {
			if r.Lab == "" && !r.Suppressed {
				var shown int64
				for _, l := range q.Refusals {
					if l.Lab != "" && !l.Suppressed {
						shown += *l.Refusals
					}
				}
				// One refusal per person here: the same residual rule in refusals.
				if res := *r.Refusals - shown; res > 0 && res < int64(kk) {
					t.Fatalf("refusals: partner %d minus labs %d leaves %d (< %d)", *r.Refusals, shown, res, kk)
				}
			}
		}
	})
}
