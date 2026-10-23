package gateway

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func (s *Server) adminRoutes(mux *http.ServeMux) {
	h := func(pattern string, f http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			if !s.adminAuth(w, r) {
				return
			}
			f(w, r)
		})
	}
	h("GET /admin/teams", s.listTeams)
	h("POST /admin/teams", s.createTeam)
	h("DELETE /admin/teams/{name}", s.disableTeam)
	h("GET /admin/keys", s.listKeys)
	h("POST /admin/keys", s.createKey)
	h("DELETE /admin/keys/{id}", s.revokeKey)
	h("GET /admin/budgets", s.listBudgets)
	h("PUT /admin/budgets/{team}", s.setBudget)
	h("DELETE /admin/budgets/{team}", s.deleteBudget)
	h("GET /admin/usage.csv", s.usageCSV)
	h("GET /admin/fairshare", s.fairshareState)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

func storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		apiError(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, store.ErrExists):
		apiError(w, http.StatusConflict, "conflict", "already exists")
	default:
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", "store error")
	}
}

func (s *Server) listTeams(w http.ResponseWriter, r *http.Request) {
	ts, err := s.st.ListTeams(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	type team struct {
		store.Team
		Share float64 `json:"share"`
	}
	out := make([]team, 0, len(ts))
	for _, t := range ts {
		out = append(out, team{t, s.cfg.TeamShare(t.Name)})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) createTeam(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if !config.ValidName(in.Name) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "team name: want [A-Za-z0-9_.-]{1,64}")
		return
	}
	if err := s.st.CreateTeam(r.Context(), in.Name); err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"name": in.Name, "share": s.cfg.TeamShare(in.Name)})
}

func (s *Server) disableTeam(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.st.DisableTeam(r.Context(), name); err != nil {
		storeError(w, err)
		return
	}
	s.keys.forget(func(k store.Key) bool { return k.Team == name })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	ks, err := s.st.ListKeys(r.Context(), r.URL.Query().Get("team"))
	if err != nil {
		storeError(w, err)
		return
	}
	if ks == nil {
		ks = []store.Key{}
	}
	writeJSON(w, http.StatusOK, ks)
}

func (s *Server) createKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Team        string `json:"team"`
		Name        string `json:"name"`
		RPM         int    `json:"rpm"`
		TPM         int    `json:"tpm"`
		MaxInFlight int    `json:"max_inflight"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if in.RPM < 0 || in.TPM < 0 || in.MaxInFlight < 0 || len(in.Name) > 128 {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "limits must be non-negative and the name at most 128 bytes")
		return
	}
	secret, id, hash := store.GenerateKey()
	k := store.Key{ID: id, Hash: hash, Team: in.Team, Name: in.Name, RPM: in.RPM, TPM: in.TPM, MaxInFlight: in.MaxInFlight}
	if err := s.st.CreateKey(r.Context(), k); err != nil {
		storeError(w, err)
		return
	}
	s.log.Info("key created", "key", id, "team", in.Team)
	// The secret is returned once and never logged or stored.
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "team": in.Team, "key": secret})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.st.RevokeKey(r.Context(), id); err != nil {
		storeError(w, err)
		return
	}
	s.keys.forget(func(k store.Key) bool { return k.ID == id })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listBudgets(w http.ResponseWriter, r *http.Request) {
	bs, err := s.st.Budgets(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, bs)
}

func (s *Server) setBudget(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Period  store.Period `json:"period"`
		MaxCost float64      `json:"max_cost"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	b := store.Budget{Team: r.PathValue("team"), Period: in.Period, MaxCost: in.MaxCost}
	if err := store.ValidateBudget(b); err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	if err := s.st.SetBudget(r.Context(), b); err != nil {
		storeError(w, err)
		return
	}
	_ = s.budget.refresh(r.Context())
	writeJSON(w, http.StatusOK, b)
}

func (s *Server) deleteBudget(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteBudget(r.Context(), r.PathValue("team")); err != nil {
		storeError(w, err)
		return
	}
	_ = s.budget.refresh(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

// ParseDay accepts RFC 3339 or YYYY-MM-DD (UTC midnight).
func ParseDay(v string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", v)
}

func (s *Server) usageCSV(w http.ResponseWriter, r *http.Request) {
	from, err1 := ParseDay(r.URL.Query().Get("from"))
	to, err2 := ParseDay(r.URL.Query().Get("to"))
	if err1 != nil || err2 != nil || !from.Before(to) {
		apiError(w, http.StatusBadRequest, "invalid_request_error", "from and to are required (RFC 3339 or YYYY-MM-DD), from < to")
		return
	}
	us, err := s.st.Usage(r.Context(), from, to)
	if err != nil {
		storeError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	_ = WriteUsageCSV(w, from, to, us)
}

// WriteUsageCSV writes the per-team GPU-hour export. Fields are written by
// encoding/csv; a team name cannot start a spreadsheet formula because
// names are restricted to [A-Za-z0-9_.-].
func WriteUsageCSV(w io.Writer, from, to time.Time, us []store.TeamUsage) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"from", "to", "team", "pool", "requests", "prompt_tokens", "completion_tokens", "cached_tokens", "gpu_hours"})
	for _, u := range us {
		_ = cw.Write([]string{
			from.UTC().Format(time.RFC3339), to.UTC().Format(time.RFC3339), u.Team, u.Pool,
			strconv.FormatInt(u.Requests, 10), strconv.FormatInt(u.PromptTokens, 10),
			strconv.FormatInt(u.CompletionTokens, 10), strconv.FormatInt(u.CachedTokens, 10),
			strconv.FormatFloat(u.GPUHours, 'f', 6, 64),
		})
	}
	cw.Flush()
	return cw.Error()
}

func (s *Server) fairshareState(w http.ResponseWriter, _ *http.Request) {
	type teamOut struct {
		Team           string  `json:"team"`
		Share          float64 `json:"share"`
		Reserved       int     `json:"reserved"`
		InFlight       int     `json:"inflight"`
		Borrowing      int     `json:"borrowing"`
		InFlightTokens int64   `json:"inflight_tokens"`
		Usage          float64 `json:"usage"`
		LevelFairshare string  `json:"level_fairshare"`
		Rank           int     `json:"rank"`
	}
	out := map[string][]teamOut{}
	for _, p := range s.porder {
		for _, t := range s.pools[p].Snapshot() {
			out[p] = append(out[p], teamOut{t.Team, t.Share, t.Reserved, t.InFlight, t.Borrowing, t.InFlightTokens,
				t.Usage, formatFloat(t.LevelFairshare), t.Rank})
		}
	}
	writeJSON(w, http.StatusOK, out)
}
