package gateway

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// Quotas and profiles in the decision report (#97, item 3). Per partner, lab
// and profile, for the period:
//
//   - soft-quota notifications by window (session, week, budget) and
//     threshold, from the event log (store.WarnEventsBetween);
//   - hard refusals (429: a budget or a profile exhausted) by profile and
//     kind, from the day counters (store.RefusalsBetween);
//   - workload days assigned against used per profile: assigned is, for every
//     assignment, the days of the period it was in force times the profile's
//     days per week over 7; used is the GPU-hours the people of the profile
//     consumed divided by the workday;
//   - oversubscription over time: per sampled day, the days a week assigned to
//     the partner's people against the days a week its share of the pools
//     serves (capacityOf, on the 7 days measured ending that day).
//
// Privacy, as for adoption: there is no row per person, and a cell is shown
// only when at least k distinct people stand behind it (k =
// adoption_min_users, default 5). The refusal counters hold no person: a cell
// is shown when it counts at least k refusals. A suppressed cell carries its
// place and no number.

// Bounds on the queries behind a quota report (#111).
const (
	maxQuotaEvents      = 100_000
	maxQuotaAssignments = 100_000
	maxQuotaRefusals    = 20_000
	maxQuotaSamples     = 70
)

// SoftRow counts the soft-quota notifications of a cell.
type SoftRow struct {
	Org, Lab, Window string
	Threshold        int
	Events, People   *int64
	Suppressed       bool
}

// RefusalOut counts the hard refusals of a cell.
type RefusalOut struct {
	Org, Lab, Profile, Kind string
	Refusals                *int64
	Suppressed              bool
}

// ProfileRow compares the workload days assigned and used in a cell.
type ProfileRow struct {
	Org, Lab, Profile string
	People            *int64
	AssignedDays      *float64
	UsedDays          *float64
	// Utilisation is UsedDays / AssignedDays (nil without assigned days).
	Utilisation *float64
	Suppressed  bool
}

// OversubRow is one sampled day of a partner.
type OversubRow struct {
	Org, Day            string
	People              *int64
	AssignedDaysPerWeek *float64
	CapacityDaysPerWeek *float64
	// Ratio is assigned over capacity; nil when capacity is unmeasured.
	Ratio      *float64
	Suppressed bool
}

// QuotaReport is the quota and profile section of the decision report.
type QuotaReport struct {
	MinCell     int
	Soft        []SoftRow
	Refusals    []RefusalOut
	Profiles    []ProfileRow
	Oversub     []OversubRow
	Notes       []string
	EventsSince string // the earliest event the log can hold, "" when unknown
}

// quotaInputs is what the section is computed from; the gateway assembles it
// from the store and the configuration.
type quotaInputs struct {
	From, To time.Time
	K        int
	Events   []store.WarnEventRow
	Refusals []store.RefusalRow
	// Assignments overlapping the period; Used is the GPU-hours report by org,
	// lab and profile.
	Assignments []store.Assignment
	Used        []store.ReportRow
	Profiles    config.Profiles
	// Workday is the GPU-hour workday (0: unknown, used days are not computed).
	Workday float64
	// Home is a person's partner and lab.
	Home func(user string) (org, lab string)
	// LabOfTeam is a team's lab.
	LabOfTeam func(team string) string
	// Capacity is a partner's days a week on a sampled day (nil: unmeasured).
	Capacity func(org string, day time.Time) *float64
	// Visible says whether the reader may see a partner ("" is unattributed).
	Visible func(org string) bool
}

func i64(v int64) *int64     { return &v }
func f64(v float64) *float64 { return &v }

func windowKind(w string) string {
	switch {
	case w == "session" || w == "week":
		return w
	case strings.HasPrefix(w, "budget:"):
		return "budget"
	}
	return w
}

type quotaCell struct{ org, lab, a, b string }

// buildQuotas computes the section.
func buildQuotas(in quotaInputs) *QuotaReport {
	q := &QuotaReport{MinCell: in.K}
	k := int64(in.K)
	label := func(org string) string {
		if org == "" {
			return Unallocated
		}
		return org
	}

	// Soft-quota notifications.
	type softAgg struct {
		events int64
		users  map[string]bool
		th     int
	}
	soft := map[quotaCell]*softAgg{}
	add := func(key quotaCell, th int, user string, n int64) {
		a := soft[key]
		if a == nil {
			a = &softAgg{users: map[string]bool{}, th: th}
			soft[key] = a
		}
		a.events += n
		a.users[user] = true
	}
	for _, e := range in.Events {
		org, lab := in.Home(e.User)
		if !in.Visible(org) {
			continue
		}
		wk := windowKind(e.Window)
		thKey := strconv.Itoa(e.Threshold)
		add(quotaCell{org, "", wk, thKey}, e.Threshold, e.User, e.N)
		if lab != "" {
			add(quotaCell{org, lab, wk, thKey}, e.Threshold, e.User, e.N)
		}
	}
	for key, a := range soft {
		r := SoftRow{Org: label(key.org), Lab: key.lab, Window: key.a, Threshold: a.th}
		if int64(len(a.users)) < k {
			r.Suppressed = true
		} else {
			r.Events, r.People = i64(a.events), i64(int64(len(a.users)))
		}
		q.Soft = append(q.Soft, r)
	}
	sort.Slice(q.Soft, func(i, j int) bool {
		a, b := q.Soft[i], q.Soft[j]
		if a.Org != b.Org {
			return a.Org < b.Org
		}
		if a.Lab != b.Lab {
			return a.Lab < b.Lab
		}
		if a.Window != b.Window {
			return a.Window < b.Window
		}
		return a.Threshold < b.Threshold
	})

	// Hard refusals.
	ref := map[quotaCell]int64{}
	for _, r := range in.Refusals {
		if !in.Visible(r.Org) {
			continue
		}
		lab := in.LabOfTeam(r.Team)
		ref[quotaCell{r.Org, "", r.Profile, r.Kind}] += r.N
		if lab != "" {
			ref[quotaCell{r.Org, lab, r.Profile, r.Kind}] += r.N
		}
	}
	for key, n := range ref {
		r := RefusalOut{Org: label(key.org), Lab: key.lab, Profile: key.a, Kind: key.b}
		if n < k {
			r.Suppressed = true
		} else {
			r.Refusals = i64(n)
		}
		q.Refusals = append(q.Refusals, r)
	}
	sort.Slice(q.Refusals, func(i, j int) bool {
		a, b := q.Refusals[i], q.Refusals[j]
		return a.Org+"\x00"+a.Lab+"\x00"+a.Profile+"\x00"+a.Kind < b.Org+"\x00"+b.Lab+"\x00"+b.Profile+"\x00"+b.Kind
	})

	// Workload days: assigned against used.
	type dayAgg struct {
		assigned float64
		used     float64
		people   map[string]bool
	}
	days := map[quotaCell]*dayAgg{}
	cell := func(key quotaCell) *dayAgg {
		a := days[key]
		if a == nil {
			a = &dayAgg{people: map[string]bool{}}
			days[key] = a
		}
		return a
	}
	for _, a := range in.Assignments {
		prof, ok := in.Profiles.Get(a.Profile)
		if !ok {
			continue
		}
		org, lab := in.Home(a.User)
		if !in.Visible(org) {
			continue
		}
		lo, hi := a.From, in.To
		if in.From.After(lo) {
			lo = in.From
		}
		if a.To != nil && a.To.Before(hi) {
			hi = *a.To
		}
		if !hi.After(lo) {
			continue
		}
		d := hi.Sub(lo).Hours() / 24 * prof.Days / 7
		for _, key := range []quotaCell{{org, "", a.Profile, ""}, {org, lab, a.Profile, ""}} {
			if key.lab == "" && key.org != org || key.lab != "" && lab == "" {
				continue
			}
			c := cell(key)
			c.assigned += d
			c.people[a.User] = true
		}
	}
	if in.Workday > 0 {
		for _, u := range in.Used {
			if u.Unit != "" || u.Profile == "" || !in.Visible(u.Org) {
				continue
			}
			used := u.Cost / in.Workday
			cell(quotaCell{u.Org, "", u.Profile, ""}).used += used
			if u.Lab != "" {
				cell(quotaCell{u.Org, u.Lab, u.Profile, ""}).used += used
			}
		}
	} else {
		q.Notes = append(q.Notes, "No GPU-hour workday is configured or calibrated: used workload days are not computed.")
	}
	for key, c := range days {
		r := ProfileRow{Org: label(key.org), Lab: key.lab, Profile: key.a}
		if int64(len(c.people)) < k {
			r.Suppressed = true
		} else {
			r.People = i64(int64(len(c.people)))
			r.AssignedDays = f64(round2(c.assigned))
			if in.Workday > 0 {
				r.UsedDays = f64(round2(c.used))
				if c.assigned > 0 {
					r.Utilisation = f64(round2(c.used / c.assigned))
				}
			}
		}
		q.Profiles = append(q.Profiles, r)
	}
	sort.Slice(q.Profiles, func(i, j int) bool {
		a, b := q.Profiles[i], q.Profiles[j]
		return a.Org+"\x00"+a.Lab+"\x00"+a.Profile < b.Org+"\x00"+b.Lab+"\x00"+b.Profile
	})

	// Oversubscription over time, per partner.
	orgs := map[string]bool{}
	for _, a := range in.Assignments {
		org, _ := in.Home(a.User)
		if in.Visible(org) && org != "" {
			orgs[org] = true
		}
	}
	names := make([]string, 0, len(orgs))
	for o := range orgs {
		names = append(names, o)
	}
	sort.Strings(names)
	span := in.To.Sub(in.From)
	step := 24 * time.Hour
	if span > maxQuotaSamples*24*time.Hour {
		step = 7 * 24 * time.Hour
	}
	for _, org := range names {
		n := 0
		for d := in.From; d.Before(in.To) && n < maxQuotaSamples; d = d.Add(step) {
			n++
			people := map[string]bool{}
			assigned := 0.0
			for _, a := range in.Assignments {
				if o, _ := in.Home(a.User); o != org || a.From.After(d) || a.To != nil && !a.To.After(d) {
					continue
				}
				if prof, ok := in.Profiles.Get(a.Profile); ok {
					assigned += prof.Days
					people[a.User] = true
				}
			}
			r := OversubRow{Org: org, Day: d.UTC().Format("2006-01-02")}
			if int64(len(people)) < k {
				r.Suppressed = true
			} else {
				r.People = i64(int64(len(people)))
				r.AssignedDaysPerWeek = f64(round2(assigned))
				if c := in.Capacity(org, d); c != nil {
					r.CapacityDaysPerWeek = c
					if *c > 0 {
						r.Ratio = f64(round2(assigned / *c))
					}
				}
			}
			q.Oversub = append(q.Oversub, r)
		}
	}
	return q
}

// ---- the gateway's part ----

// quotaSection assembles and computes the quota section for the readers p may
// see. It returns nil, nil when the store keeps none of it.
func (s *Server) quotaSection(ctx context.Context, p *principal, from, to time.Time) (*QuotaReport, error) {
	qs, ok := s.st.(store.QuotaReportStore)
	if !ok {
		return nil, nil
	}
	k := s.adoptionMin()
	events, err := qs.WarnEventsBetween(ctx, from, to, maxQuotaEvents)
	if err != nil {
		return nil, err
	}
	refusals, err := qs.RefusalsBetween(ctx, from, to, maxQuotaRefusals)
	if err != nil {
		return nil, err
	}
	var assigns []store.Assignment
	var used []store.ReportRow
	var profiles config.Profiles
	workday := 0.0
	if s.prof != nil {
		profiles = s.prof.cfg
		if assigns, err = qs.AssignmentsBetween(ctx, from, to, maxQuotaAssignments); err != nil {
			return nil, err
		}
		if used, err = s.st.Report(ctx, from, to, []string{"org", "lab", "profile"}); err != nil {
			return nil, err
		}
		workday, _ = s.prof.workdayOf("")
	}
	// A person's partner and lab: the team they belong to (the first, by
	// name), else the organisation they are bound to.
	teams, err := s.st.ListTeams(ctx, "")
	if err != nil {
		return nil, err
	}
	orgOfTeam := map[string]string{}
	for _, t := range teams {
		orgOfTeam[t.Name] = t.Org
	}
	binds, err := s.st.Bindings(ctx, "")
	if err != nil {
		return nil, err
	}
	homeTeam, homeOrg := map[string]string{}, map[string]string{}
	for _, b := range binds {
		switch {
		case b.Team != "":
			if cur, ok := homeTeam[b.User]; !ok || b.Team < cur {
				homeTeam[b.User] = b.Team
			}
		case b.Org != "":
			if cur, ok := homeOrg[b.User]; !ok || b.Org < cur {
				homeOrg[b.User] = b.Org
			}
		}
	}
	home := func(user string) (string, string) {
		if t, ok := homeTeam[user]; ok {
			return orgOfTeam[t], s.cfg.LabOf(t)
		}
		return homeOrg[user], ""
	}
	// Capacity: what a partner's share serves, on the 7 days ending the day.
	var wds map[string]workdayValue
	if s.prof != nil {
		wds = s.prof.workdays()
	}
	loadsOf := map[string][]store.PoolLoad{}
	capacity := func(org string, day time.Time) *float64 {
		key := day.Format("2006-01-02")
		loads, ok := loadsOf[key]
		if !ok {
			l, err := s.st.PoolLoad(ctx, day.AddDate(0, 0, -6), day.AddDate(0, 0, 1))
			if err != nil {
				return nil
			}
			loadsOf[key], loads = l, l
		}
		if c, ok := capacityOf(s.cfg, org, loads, map[string]workdayValue{"": wds[""]})[""]; ok {
			return c.days
		}
		return nil
	}
	q := buildQuotas(quotaInputs{
		From: from, To: to, K: k, Events: events, Refusals: refusals, Assignments: assigns, Used: used,
		Profiles: profiles, Workday: workday, Home: home, LabOfTeam: s.cfg.LabOf, Capacity: capacity,
		Visible: func(org string) bool { return p.reportView() || org != "" && p.orgView(org) },
	})
	q.Notes = append(q.Notes,
		"Cells with fewer than "+strconv.Itoa(k)+" people (refusals: "+strconv.Itoa(k)+" refusals) are suppressed; there is no row per person.",
		"Soft-quota events are counted from the event log, complete from this release on; earlier crossings are the marks the state held.",
		"Used workload days are the GPU-hours of the profile's people over the GPU-hour workday; assigned days are the days in force times days a week over 7.")
	return q, nil
}
