package gateway

import (
	"errors"
	"net/http"

	"github.com/go-llmflow/gateway/store"
)

// allowAccess says who reads and writes an IP allowlist: an organisation's
// admins for the organisation, a team's admins for the team and its keys.
func (s *Server) allowAccess(r *http.Request, p *principal, k store.AllowKey) (read, write bool, err error) {
	switch k.Kind {
	case store.ScopeOrg:
		orgs, err := s.st.ListOrgs(r.Context())
		if err != nil {
			return false, false, err
		}
		for _, o := range orgs {
			if o.Name == k.Name {
				return p.orgView(k.Name), p.orgAdmin(k.Name), nil
			}
		}
	case store.ScopeTeam:
		t, err := s.st.GetTeam(r.Context(), k.Name)
		if err != nil {
			return false, false, err
		}
		return p.teamView(t), p.teamAdmin(t), nil
	case store.ScopeKey:
		key, err := s.st.GetKey(r.Context(), k.Name)
		if err != nil {
			return false, false, err
		}
		t, err := s.st.GetTeam(r.Context(), key.Team)
		if err != nil {
			return false, false, err
		}
		return keyVisible(p, t, key), p.teamAdmin(t), nil
	}
	return false, false, store.ErrNotFound
}

func (s *Server) listAllowlists(w http.ResponseWriter, r *http.Request, p *principal) {
	ls, err := s.st.Allowlists(r.Context())
	if err != nil {
		storeError(w, err)
		return
	}
	out := []store.Allowlist{}
	for _, l := range ls {
		read, _, err := s.allowAccess(r, p, l.AllowKey)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			storeError(w, err)
			return
		}
		if read || p.proxyView() {
			out = append(out, l)
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// setAllowlist replaces an allowlist; {"cidrs": []} removes it.
func (s *Server) setAllowlist(w http.ResponseWriter, r *http.Request, p *principal) {
	var in store.Allowlist
	if !readJSON(w, r, &in) {
		return
	}
	a, err := store.ValidateAllowlist(in)
	if err != nil {
		apiError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	_, write, err := s.allowAccess(r, p, a.AllowKey)
	if err != nil {
		storeError(w, err)
		return
	}
	if !write {
		forbidden(w)
		return
	}
	if err := s.st.SetAllowlist(r.Context(), a); err != nil {
		storeError(w, err)
		return
	}
	_ = s.budget.refresh(r.Context())
	s.log.Info("allowlist set", "kind", a.Kind, "name", a.Name, "prefixes", len(a.CIDRs), "by", p.name)
	writeJSON(w, http.StatusOK, a)
}
