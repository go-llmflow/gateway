package gateway

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The admin UI: server-rendered HTML, no JavaScript, no third-party asset.
//
// It is a thin layer over the admin API: every page and every form calls
// the very handlers of /admin/… in-process (s.adminFuncs), with the
// signed-in principal, so authorisation, validation and the audit log are
// the API's and nothing is duplicated. It is reached with an SSO session
// only (a browser carries no admin token), on the admin route class.
//
// Hardening: a Content-Security-Policy that allows no script at all, no
// frame, forms posting to this origin only, and styles from /ui/style.css;
// every form carries the session's CSRF token (synchronizer token) and is
// refused cross-origin; pages are not cached (they show data, and a new
// key's secret once); request bodies are bounded.

const uiCSP = "default-src 'none'; style-src 'self'; img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'"

// maxUIForm bounds a posted form.
const maxUIForm = 16 << 10

func (s *Server) uiRoutes(mux *http.ServeMux) {
	wrap := func(f http.HandlerFunc) http.Handler { return s.withClientAddr(s.cfg.Routes.Admin, f) }
	mux.Handle("GET /ui/style.css", wrap(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = io.WriteString(w, uiCSS)
	}))
	page := func(f func(w http.ResponseWriter, r *http.Request, p *principal, csrf string)) http.Handler {
		return wrap(func(w http.ResponseWriter, r *http.Request) {
			p, csrf, ok := s.uiPrincipal(w, r)
			if ok {
				f(w, r, p, csrf)
			}
		})
	}
	mux.Handle("GET /ui/{$}", page(s.uiHome))
	mux.Handle("GET /ui/keys", page(s.uiKeys))
	mux.Handle("POST /ui/keys", page(s.uiCreateKey))
	mux.Handle("POST /ui/keys/{id}/rotate", page(s.uiRotateKey))
	mux.Handle("POST /ui/keys/{id}/revoke", page(s.uiRevokeKey))
	mux.Handle("GET /ui/budgets", page(s.uiBudgets))
	mux.Handle("POST /ui/budgets", page(s.uiSetBudget))
	mux.Handle("POST /ui/budgets/delete", page(s.uiDeleteBudget))
	mux.Handle("GET /ui/spend", page(s.uiSpend))
	mux.Handle("GET /ui/reports", page(s.uiReports))
	mux.Handle("GET /ui/fairshare", page(s.uiFairshare))
	mux.Handle("GET /ui/audit", page(s.uiAudit))
	mux.Handle("POST /ui/audit/verify", page(s.uiVerify))
	mux.Handle("POST /ui/logout", page(s.uiLogout))
}

// uiPrincipal authenticates a UI request by its SSO session. A POST must be
// a bounded url-encoded form whose "csrf" field is the session's token; the
// session check also refuses it cross-origin.
func (s *Server) uiPrincipal(w http.ResponseWriter, r *http.Request) (*principal, string, bool) {
	uiHeaders(w)
	if s.sso == nil {
		http.Error(w, "the admin UI needs an sso block (it is reached with an SSO session)", http.StatusNotFound)
		return nil, "", false
	}
	if r.Method == http.MethodPost {
		mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mt != "application/x-www-form-urlencoded" {
			http.Error(w, "a form is expected", http.StatusUnsupportedMediaType)
			return nil, "", false
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxUIForm)
		if err := r.ParseForm(); err != nil {
			http.Error(w, "form too large or malformed", http.StatusBadRequest)
			return nil, "", false
		}
		// The token travels in the form; the session check reads it from
		// the header the API uses.
		r.Header.Set(csrfHeader, r.PostForm.Get("csrf"))
	}
	p, csrf, handled := s.sessionPrincipal(w, r)
	if !handled {
		http.Redirect(w, r, "/admin/login?next=ui", http.StatusSeeOther)
		return nil, "", false
	}
	if p == nil {
		return nil, "", false
	}
	return p, csrf, true
}

func uiHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Security-Policy", uiCSP)
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
}

// uiRecorder collects an admin handler's answer.
type uiRecorder struct {
	h      http.Header
	status int
	body   bytes.Buffer
}

func (r *uiRecorder) Header() http.Header {
	if r.h == nil {
		r.h = http.Header{}
	}
	return r.h
}
func (r *uiRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(b)
}
func (r *uiRecorder) WriteHeader(s int) {
	if r.status == 0 {
		r.status = s
	}
}

// callAdmin runs an admin API handler in-process for p, as the API would
// for the same principal.
func (s *Server) callAdmin(r *http.Request, p *principal, pattern string, path string, query url.Values, body any, vals map[string]string) (int, []byte) {
	f := s.adminFuncs[pattern]
	if f == nil {
		return http.StatusNotFound, []byte(`{"error":{"message":"no such admin route"}}`)
	}
	method, _, _ := strings.Cut(pattern, " ")
	var rd io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return http.StatusBadRequest, nil
		}
		rd = bytes.NewReader(b)
	}
	u := path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(r.Context(), method, u, rd)
	if err != nil {
		return http.StatusBadRequest, nil
	}
	req.RemoteAddr = r.RemoteAddr
	req.Header.Set("Content-Type", "application/json")
	if id := r.Header.Get("X-Request-Id"); id != "" {
		req.Header.Set("X-Request-Id", id)
	}
	for k, v := range vals {
		req.SetPathValue(k, v)
	}
	rec := &uiRecorder{}
	f(rec, req, p)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return rec.status, rec.body.Bytes()
}

// apiMessage extracts an API error's message.
func apiMessage(b []byte) string {
	var e struct {
		Error struct{ Message string } `json:"error"`
	}
	if json.Unmarshal(b, &e) == nil && e.Error.Message != "" {
		return e.Error.Message
	}
	return "the request failed"
}

// table is a rendered table: headers and string cells.
type table struct {
	Cols []string
	Rows [][]string
	// IDs, when set, is a row's key id (for row actions).
	IDs []string
}

// objectsTable renders a JSON list of objects with the given columns.
func objectsTable(raw []byte, cols ...string) table {
	var list []map[string]any
	_ = json.Unmarshal(raw, &list)
	t := table{Cols: cols}
	for _, o := range list {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = cell(o[c])
		}
		t.Rows = append(t.Rows, row)
		id, _ := o["id"].(string)
		t.IDs = append(t.IDs, id)
	}
	return t
}

func cell(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'g', 8, 64)
	case bool:
		if x {
			return "yes"
		}
		return ""
	default:
		b, _ := json.Marshal(x)
		return string(b)
	}
}

// view is what a page template receives.
type view struct {
	Title   string
	User    string
	CSRF    string
	Message string
	Error   string
	Data    any
}

func (s *Server) render(w http.ResponseWriter, name string, v view) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := uiTemplates.ExecuteTemplate(w, name, v); err != nil {
		s.log.Error("ui: render", "page", name, "err", err)
	}
}

func (s *Server) uiHome(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	_, raw := s.callAdmin(r, p, "GET /admin/whoami", "/admin/whoami", nil, nil, nil)
	var who map[string]any
	_ = json.Unmarshal(raw, &who)
	s.render(w, "home", view{Title: "Administration", User: p.name, CSRF: csrf, Data: cell(who["roles"])})
}

func (s *Server) uiKeys(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	s.keysPage(w, r, p, csrf, "", "")
}

func (s *Server) keysPage(w http.ResponseWriter, r *http.Request, p *principal, csrf, msg, errMsg string) {
	q := url.Values{}
	if t := r.URL.Query().Get("team"); t != "" {
		q.Set("team", t)
	}
	st, raw := s.callAdmin(r, p, "GET /admin/keys", "/admin/keys", q, nil, nil)
	if st != http.StatusOK {
		errMsg = apiMessage(raw)
	}
	_, traw := s.callAdmin(r, p, "GET /admin/teams", "/admin/teams", nil, nil, nil)
	var teams []struct{ Name string }
	_ = json.Unmarshal(traw, &teams)
	names := []string{}
	for _, t := range teams {
		names = append(names, t.Name)
	}
	data := struct {
		Keys  table
		Teams []string
	}{objectsTable(raw, "id", "team", "project", "name", "owner", "rpm", "tpm", "max_inflight", "created_at", "rotated_at", "revoked"), names}
	s.render(w, "keys", view{Title: "Keys", User: p.name, CSRF: csrf, Message: msg, Error: errMsg, Data: data})
}

func formInt(r *http.Request, k string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(r.PostForm.Get(k)))
	return n
}

func (s *Server) uiCreateKey(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	body := map[string]any{
		"team": r.PostForm.Get("team"), "project": r.PostForm.Get("project"), "name": r.PostForm.Get("name"),
		"rpm": formInt(r, "rpm"), "tpm": formInt(r, "tpm"), "max_inflight": formInt(r, "max_inflight"),
	}
	st, raw := s.callAdmin(r, p, "POST /admin/keys", "/admin/keys", nil, body, nil)
	if st != http.StatusCreated {
		s.keysPage(w, r, p, csrf, "", apiMessage(raw))
		return
	}
	s.secretPage(w, p, csrf, raw, "key created")
}

// secretPage shows a new secret once (the page is not cached, the secret
// is not stored anywhere the UI can show it again).
func (s *Server) secretPage(w http.ResponseWriter, p *principal, csrf string, raw []byte, what string) {
	var out struct {
		ID  string `json:"id"`
		Key string `json:"key"`
	}
	_ = json.Unmarshal(raw, &out)
	s.render(w, "secret", view{Title: "New secret", User: p.name, CSRF: csrf, Message: what, Data: out})
}

func (s *Server) uiRotateKey(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	id := r.PathValue("id")
	body := map[string]any{}
	if g := strings.TrimSpace(r.PostForm.Get("grace_period")); g != "" {
		body["grace_period"] = g
	}
	st, raw := s.callAdmin(r, p, "POST /admin/keys/{id}/rotate", "/admin/keys/"+url.PathEscape(id)+"/rotate", nil, body, map[string]string{"id": id})
	if st != http.StatusOK && st != http.StatusCreated {
		s.keysPage(w, r, p, csrf, "", apiMessage(raw))
		return
	}
	s.secretPage(w, p, csrf, raw, "key rotated")
}

func (s *Server) uiRevokeKey(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	id := r.PathValue("id")
	st, raw := s.callAdmin(r, p, "DELETE /admin/keys/{id}", "/admin/keys/"+url.PathEscape(id), nil, nil, map[string]string{"id": id})
	if st >= 300 {
		s.keysPage(w, r, p, csrf, "", apiMessage(raw))
		return
	}
	s.keysPage(w, r, p, csrf, "key "+id+" revoked", "")
}

func (s *Server) uiBudgets(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	s.budgetsPage(w, r, p, csrf, "", "")
}

func (s *Server) budgetsPage(w http.ResponseWriter, r *http.Request, p *principal, csrf, msg, errMsg string) {
	st, raw := s.callAdmin(r, p, "GET /admin/budgets", "/admin/budgets", nil, nil, nil)
	if st != http.StatusOK {
		errMsg = apiMessage(raw)
	}
	t := objectsTable(raw, "kind", "name", "model", "period", "max_cost", "soft_limit", "spent", "limit")
	s.render(w, "budgets", view{Title: "Budgets", User: p.name, CSRF: csrf, Message: msg, Error: errMsg, Data: t})
}

func (s *Server) uiSetBudget(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	maxCost, err1 := strconv.ParseFloat(strings.TrimSpace(r.PostForm.Get("max_cost")), 64)
	soft := 0.0
	var err2 error
	if v := strings.TrimSpace(r.PostForm.Get("soft_limit")); v != "" {
		soft, err2 = strconv.ParseFloat(v, 64)
	}
	if err1 != nil || err2 != nil {
		s.budgetsPage(w, r, p, csrf, "", "max_cost and soft_limit must be numbers (GPU-hours)")
		return
	}
	body := map[string]any{"kind": r.PostForm.Get("kind"), "name": r.PostForm.Get("name"), "model": r.PostForm.Get("model"),
		"period": r.PostForm.Get("period"), "max_cost": maxCost, "soft_limit": soft}
	st, raw := s.callAdmin(r, p, "PUT /admin/budgets", "/admin/budgets", nil, body, nil)
	if st >= 300 {
		s.budgetsPage(w, r, p, csrf, "", apiMessage(raw))
		return
	}
	s.budgetsPage(w, r, p, csrf, "budget saved", "")
}

func (s *Server) uiDeleteBudget(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	q := url.Values{"kind": {r.PostForm.Get("kind")}, "name": {r.PostForm.Get("name")}}
	if m := r.PostForm.Get("model"); m != "" {
		q.Set("model", m)
	}
	st, raw := s.callAdmin(r, p, "DELETE /admin/budgets", "/admin/budgets", q, nil, nil)
	if st >= 300 {
		s.budgetsPage(w, r, p, csrf, "", apiMessage(raw))
		return
	}
	s.budgetsPage(w, r, p, csrf, "budget deleted", "")
}

var spendGroupings = []string{"org", "org,team", "org,team,project", "team,model", "org,pool", "tag"}

func (s *Server) uiSpend(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	q := r.URL.Query()
	now := s.now().UTC()
	from, to := q.Get("from"), q.Get("to")
	if from == "" {
		from = now.AddDate(0, 0, -30).Format("2006-01-02")
	}
	if to == "" {
		to = now.AddDate(0, 0, 1).Format("2006-01-02")
	}
	group := q.Get("group_by")
	if group == "" {
		group = "org,team"
	}
	st, raw := s.callAdmin(r, p, "GET /admin/spend", "/admin/spend",
		url.Values{"from": {from}, "to": {to}, "group_by": {group}, "format": {FormatCSV}}, nil, nil)
	var t table
	errMsg := ""
	if st != http.StatusOK {
		errMsg = apiMessage(raw)
	} else if recs, err := csv.NewReader(bytes.NewReader(raw)).ReadAll(); err == nil && len(recs) > 0 {
		t.Cols, t.Rows = recs[0], recs[1:]
	}
	data := struct {
		From, To, Group string
		Groups          []string
		Table           table
	}{from, to, group, spendGroupings, t}
	s.render(w, "spend", view{Title: "Spend (GPU-hours)", User: p.name, CSRF: csrf, Error: errMsg, Data: data})
}

func (s *Server) uiFairshare(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	_, raw := s.callAdmin(r, p, "GET /admin/fairshare", "/admin/fairshare", nil, nil, nil)
	var pools map[string]json.RawMessage
	_ = json.Unmarshal(raw, &pools)
	names := make([]string, 0, len(pools))
	for n := range pools {
		names = append(names, n)
	}
	sort.Strings(names)
	type poolView struct {
		Name  string
		Table table
	}
	var out []poolView
	for _, n := range names {
		out = append(out, poolView{n, objectsTable(pools[n], "org", "share", "reserved", "inflight", "borrowing", "usage", "level_fairshare", "rank")})
	}
	s.render(w, "fairshare", view{Title: "Fair share", User: p.name, CSRF: csrf, Data: out})
}

func (s *Server) uiAudit(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	s.auditPage(w, r, p, csrf, nil)
}

func (s *Server) auditPage(w http.ResponseWriter, r *http.Request, p *principal, csrf string, status *auditStatus) {
	from := r.URL.Query().Get("from")
	if from == "" {
		from = r.PostForm.Get("from")
	}
	q := url.Values{"limit": {"50"}}
	if from != "" {
		q.Set("from", from)
	}
	st, raw := s.callAdmin(r, p, "GET /admin/audit", "/admin/audit", q, nil, nil)
	errMsg := ""
	if st != http.StatusOK {
		errMsg = apiMessage(raw)
	}
	t := objectsTable(raw, "seq", "time_us", "kind", "actor", "action", "target", "status")
	for i := range t.Rows {
		if us, err := strconv.ParseInt(t.Rows[i][1], 10, 64); err == nil {
			t.Rows[i][1] = time.UnixMicro(us).UTC().Format(time.RFC3339)
		}
	}
	next := ""
	if n := len(t.Rows); n > 0 {
		if seq, err := strconv.ParseInt(t.Rows[n-1][0], 10, 64); err == nil {
			next = strconv.FormatInt(seq+1, 10)
		}
	}
	data := struct {
		Table  table
		Next   string
		Status *auditStatus
	}{t, next, status}
	s.render(w, "audit", view{Title: "Audit log", User: p.name, CSRF: csrf, Error: errMsg, Data: data})
}

func (s *Server) uiVerify(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	st, raw := s.callAdmin(r, p, "POST /admin/audit/verify", "/admin/audit/verify", nil, nil, nil)
	if st != http.StatusOK {
		s.render(w, "audit", view{Title: "Audit log", User: p.name, CSRF: csrf, Error: apiMessage(raw), Data: struct {
			Table  table
			Next   string
			Status *auditStatus
		}{}})
		return
	}
	var status auditStatus
	_ = json.Unmarshal(raw, &status)
	s.auditPage(w, r, p, csrf, &status)
}

func (s *Server) uiLogout(w http.ResponseWriter, r *http.Request, p *principal, csrf string) {
	rec := &uiRecorder{}
	s.logout(rec, r)
	for _, c := range rec.Header().Values("Set-Cookie") {
		w.Header().Add("Set-Cookie", c)
	}
	s.render(w, "loggedout", view{Title: "Signed out"})
}

var uiTemplates = template.Must(template.New("ui").Parse(uiHTML))
