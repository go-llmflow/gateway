package gateway

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The admin UI of workload profiles (#90): each person's week as a bar in
// days, the assignment form, the organisations' capacity check and the
// workday calibration. Like every page, it calls the admin API in-process
// (authorisation, validation and audit are the API's).

func (s *Server) profilesUIRoutes(mux *http.ServeMux, page func(func(http.ResponseWriter, *http.Request, *principal, string)) http.Handler) {
	mux.Handle("GET /ui/profiles", page(s.uiProfiles))
	mux.Handle("POST /ui/profiles", page(s.uiAssignProfile))
	mux.Handle("GET /ui/profiles/capacity", page(s.uiProfileCapacity))
	mux.Handle("GET /ui/profiles/calibrate", page(s.uiCalibrate))
	mux.Handle("POST /ui/profiles/calibrate", page(s.uiAcceptCalibration))
}

// profileChoice is a profile in the assignment form.
type profileChoice struct{ ID, Label string }

// personRow is a person on the profiles page.
type personRow struct {
	person
	Bars []unitBar
}

// unitBar is a person's week and session in one unit, ready to show.
type unitBar struct {
	Unit                   string
	WeekUsed, WeekDays     float64
	WeekText, WeekResets   string
	SessionText            string
	SessionUsed, SessionOf float64
	OverflowText           string
}

func (s *Server) uiProfiles(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	s.profilesPage(w, r, p, csrf, "", "")
}

func (s *Server) profilesPage(w http.ResponseWriter, r *http.Request, p *principal, csrf, msg, errMsg string) {
	q := url.Values{}
	for _, k := range []string{"org", "team"} {
		if v := r.URL.Query().Get(k); v != "" {
			q.Set(k, v)
		}
	}
	st, raw := s.callAdmin(r, p, "GET /admin/profiles/people", "/admin/profiles/people", q, nil, nil)
	var people []person
	if st != http.StatusOK {
		if errMsg == "" {
			errMsg = apiMessage(raw)
		}
	} else {
		_ = json.Unmarshal(raw, &people)
	}
	rows := make([]personRow, 0, len(people))
	for _, pe := range people {
		row := personRow{person: pe}
		for _, u := range pe.Units {
			b := unitBar{Unit: u.Unit, WeekUsed: min(u.WeekUsedDays, u.WeekDays), WeekDays: u.WeekDays,
				WeekText:    strconv.FormatFloat(u.WeekUsedDays, 'f', -1, 64) + " / " + fmtDays(u.WeekDays),
				WeekResets:  u.WeekResets.UTC().Format("Monday 2 Jan 15:04 UTC"),
				SessionText: strconv.FormatFloat(u.SessionUsed, 'f', -1, 64) + " / " + fmtDays(u.SessionDays) + " in " + u.SessionWindow,
				SessionUsed: min(u.SessionUsed, u.SessionDays), SessionOf: u.SessionDays}
			if u.OverflowDays > 0 {
				b.OverflowText = strconv.FormatFloat(u.OverflowUsed, 'f', -1, 64) + " / " + fmtDays(u.OverflowDays) + " this month"
			}
			row.Bars = append(row.Bars, b)
		}
		rows = append(rows, row)
	}
	choices := []profileChoice{}
	for _, pr := range s.cfg.Profiles.List {
		choices = append(choices, profileChoice{pr.ID, pr.Label})
	}
	data := struct {
		People   []personRow
		Profiles []profileChoice
		Default  string
		Org      string
		Team     string
	}{rows, choices, s.cfg.Profiles.Default, q.Get("org"), q.Get("team")}
	s.render(w, "profiles", view{Title: "Workload profiles", User: p.name, CSRF: csrf, Message: msg, Error: errMsg, Data: data})
}

func (s *Server) uiAssignProfile(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	user := strings.TrimSpace(r.PostForm.Get("user"))
	profile := r.PostForm.Get("profile")
	overflow := r.PostForm.Get("overflow") == "on"
	if !validPerson(user) {
		s.profilesPage(w, r, p, csrf, "", "user name: want [A-Za-z0-9_.@+-]{1,128}")
		return
	}
	path := "/admin/users/" + url.PathEscape(user) + "/profile"
	var st int
	var raw []byte
	if profile == "" {
		st, raw = s.callAdmin(r, p, "DELETE /admin/users/{name}/profile", path, nil, nil, map[string]string{"name": user})
	} else {
		st, raw = s.callAdmin(r, p, "PUT /admin/users/{name}/profile", path, nil,
			map[string]any{"profile": profile, "overflow": overflow}, map[string]string{"name": user})
	}
	if st >= 300 {
		s.profilesPage(w, r, p, csrf, "", apiMessage(raw))
		return
	}
	what := "back to the default profile"
	if profile != "" {
		what = "profile " + profile
	}
	s.profilesPage(w, r, p, csrf, user+": "+what, "")
}

func (s *Server) uiProfileCapacity(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	st, raw := s.callAdmin(r, p, "GET /admin/profiles/capacity", "/admin/profiles/capacity", nil, nil, nil)
	var rows []orgCapacity
	errMsg := ""
	if st != http.StatusOK {
		errMsg = apiMessage(raw)
	} else {
		_ = json.Unmarshal(raw, &rows)
	}
	type line struct {
		orgCapacity
		Sentence string
	}
	out := []line{}
	for _, c := range rows {
		sentence := "assigned " + fmtDays(c.AssignedDays) + " a week to " + strconv.Itoa(c.People) + " people"
		switch {
		case c.Capacity == nil:
			sentence += "; capacity unknown: " + c.Note
		case c.Ratio == nil:
			sentence += " for a capacity of about " + fmtDays(*c.Capacity)
		default:
			sentence += " for a capacity of about " + fmtDays(*c.Capacity) + " → ×" + strconv.FormatFloat(*c.Ratio, 'f', -1, 64)
		}
		out = append(out, line{c, sentence})
	}
	s.render(w, "profilecapacity", view{Title: "Profiles: capacity", User: p.name, CSRF: csrf, Error: errMsg, Data: out})
}

func (s *Server) uiCalibrate(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	s.calibratePage(w, r, p, csrf, "", "")
}

func (s *Server) calibratePage(w http.ResponseWriter, r *http.Request, p *principal, csrf, msg, errMsg string) {
	q := url.Values{}
	if v := r.URL.Query().Get("weeks"); v != "" {
		q.Set("weeks", v)
	}
	st, raw := s.callAdmin(r, p, "GET /admin/profiles/calibrate", "/admin/profiles/calibrate", q, nil, nil)
	var cs []calibration
	if st != http.StatusOK {
		if errMsg == "" {
			errMsg = apiMessage(raw)
		}
	} else {
		_ = json.Unmarshal(raw, &cs)
	}
	type bin struct {
		Upper string
		Count int
		Max   int
	}
	type calView struct {
		calibration
		Bins     []bin
		Proposal string
	}
	out := []calView{}
	for _, c := range cs {
		v := calView{calibration: c, Proposal: strconv.FormatFloat(c.Proposal, 'g', 6, 64)}
		most := 0
		for _, n := range c.Counts {
			most = max(most, n)
		}
		for i, u := range c.Buckets {
			v.Bins = append(v.Bins, bin{strconv.FormatFloat(u, 'g', 4, 64), c.Counts[i], most})
		}
		out = append(out, v)
	}
	s.render(w, "calibrate", view{Title: "Profiles: workday calibration", User: p.name, CSRF: csrf, Message: msg, Error: errMsg,
		Data: struct {
			Units []calView
			Admin bool
		}{out, p.proxyAdmin()}})
}

func (s *Server) uiAcceptCalibration(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	amount, err := strconv.ParseFloat(strings.TrimSpace(r.PostForm.Get("amount")), 64)
	if err != nil {
		s.calibratePage(w, r, p, csrf, "", "amount: want a number")
		return
	}
	unit := r.PostForm.Get("unit")
	st, raw := s.callAdmin(r, p, "POST /admin/profiles/calibrate", "/admin/profiles/calibrate", nil,
		map[string]any{"unit": unit, "amount": amount}, nil)
	if st >= 300 {
		s.calibratePage(w, r, p, csrf, "", apiMessage(raw))
		return
	}
	s.calibratePage(w, r, p, csrf, "workday of "+unit+" set to "+strconv.FormatFloat(amount, 'g', 6, 64)+"; every profile follows", "")
}
