package gateway

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/go-llmflow/gateway/store"
)

// principal is an authenticated administrator: the master key, or a user
// with role bindings.
//
// Scopes nest like the Slurm account tree: a role on an organisation
// covers its teams, a role on a team covers its projects and keys. The
// permission order on one scope is admin > member > viewer, and the proxy
// roles cover every scope.
type principal struct {
	name     string
	master   bool
	bindings []store.Binding
	// session is the SHA-256 of the SSO session cookie, when the principal
	// comes from one.
	session [32]byte
}

func (p *principal) has(role store.Role, org, team string) bool {
	for _, b := range p.bindings {
		if b.Role == role && b.Org == org && b.Team == team {
			return true
		}
	}
	return false
}

func (p *principal) proxyAdmin() bool { return p.master || p.has(store.RoleProxyAdmin, "", "") }
func (p *principal) proxyView() bool  { return p.proxyAdmin() || p.has(store.RoleProxyViewer, "", "") }

// financeView is the finance_viewer role: reports across organisations.
func (p *principal) financeView() bool { return p.has(store.RoleFinanceViewer, "", "") }

// reportView reports whether p may read the reports of every organisation:
// the proxy roles and finance_viewer. It is deliberately not part of orgView,
// which also opens listings of teams, keys and users: a finance office reads
// money and energy, not people.
func (p *principal) reportView() bool { return p.proxyView() || p.financeView() }

func (p *principal) orgAdmin(org string) bool {
	return p.proxyAdmin() || org != "" && p.has(store.RoleOrgAdmin, org, "")
}

func (p *principal) orgView(org string) bool {
	return p.orgAdmin(org) || p.proxyView() || org != "" && p.has(store.RoleViewer, org, "")
}

func (p *principal) teamAdmin(t store.Team) bool {
	return p.orgAdmin(t.Org) || p.has(store.RoleTeamAdmin, "", t.Name)
}

func (p *principal) teamMember(t store.Team) bool {
	return p.teamAdmin(t) || p.has(store.RoleMember, "", t.Name)
}

func (p *principal) teamView(t store.Team) bool {
	return p.teamMember(t) || p.orgView(t.Org) || p.has(store.RoleViewer, "", t.Name)
}

// anyOrgAdmin reports whether the principal administers at least one
// organisation (or everything).
func (p *principal) anyOrgAdmin() bool {
	if p.proxyAdmin() {
		return true
	}
	for _, b := range p.bindings {
		if b.Role == store.RoleOrgAdmin {
			return true
		}
	}
	return false
}

func forbidden(w http.ResponseWriter) {
	apiError(w, http.StatusForbidden, "permission_denied", "not allowed for this principal")
}

var errBadAdminToken = errors.New("invalid admin token")

// adminPrincipal authenticates an admin request: the master key (compared
// in constant time after hashing both sides) or a user admin token
// (llma-…, stored hashed). It writes the error response itself.
func (s *Server) adminPrincipal(w http.ResponseWriter, r *http.Request) (*principal, bool) {
	cn, haveNet := clientNet(r)
	if haveNet && s.authFails.blocked(cn) {
		apiError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many failed authentications from this address")
		return nil, false
	}
	secret := bearer(r)
	if secret == "" {
		// A browser with an SSO session (CSRF checked there).
		if p, _, handled := s.sessionPrincipal(w, r); handled {
			return p, p != nil
		}
	}
	h := store.HashSecret(secret)
	if store.HashEqual(h, store.HashSecret(s.secrets.MasterKey)) {
		return &principal{name: "master", master: true}, true
	}
	if strings.HasPrefix(secret, store.AdminTokenPrefix) {
		p, err := s.userPrincipal(r.Context(), h)
		switch {
		case err == nil:
			return p, true
		case !errors.Is(err, errBadAdminToken):
			apiError(w, http.StatusServiceUnavailable, "service_unavailable", "user store unreachable")
			return nil, false
		}
	}
	if haveNet {
		s.authFails.fail(cn)
	}
	apiError(w, http.StatusUnauthorized, "authentication_error", "master key or admin token required")
	return nil, false
}

func (s *Server) userPrincipal(ctx context.Context, h [32]byte) (*principal, error) {
	u, tok, err := s.st.UserByTokenHash(ctx, h)
	if errors.Is(err, store.ErrNotFound) {
		return nil, errBadAdminToken
	}
	if err != nil {
		return nil, err
	}
	if tok.Revoked || u.Disabled || !store.HashEqual(tok.Hash, h) {
		return nil, errBadAdminToken
	}
	bs, err := s.st.Bindings(ctx, u.Name)
	if err != nil {
		return nil, err
	}
	return &principal{name: u.Name, bindings: bs}, nil
}

// grantProvenance decides who a grant is on behalf of, and whether p may make
// it to THIS user (#104). A proxy admin may grant to anybody (the binding is
// recorded as theirs). Anyone else may grant only to a user already
// provisioned into the organisation the binding is in: one that holds a team
// binding of that organisation created by provisioning (SCIM, an SSO claim),
// by a proxy admin, or before provenance was recorded. A binding-less user,
// or one of another organisation, is a proxy admin's to place; otherwise an
// organisation admin could pull anybody into their organisation by granting
// a team role, and then act on them.
func (s *Server) grantProvenance(ctx context.Context, p *principal, b store.Binding) (store.Provenance, bool, error) {
	if p.proxyAdmin() {
		return store.ProvProxyAdmin, true, nil
	}
	org := b.Org
	if b.Team != "" {
		t, err := s.st.GetTeam(ctx, b.Team)
		if err != nil {
			return "", false, err
		}
		org = t.Org
	}
	have, err := s.st.Bindings(ctx, b.User)
	if err != nil {
		return "", false, err
	}
	for _, h := range have {
		if h.Team == "" || !h.Provenance.Provisioned() {
			continue
		}
		t, err := s.st.GetTeam(ctx, h.Team)
		switch {
		case errors.Is(err, store.ErrNotFound):
		case err != nil:
			return "", false, err
		case t.Org == org:
			return store.ProvOrgAdmin, true, nil
		}
	}
	return "", false, nil
}

// canGrant reports whether p may grant or remove binding b. Proxy roles
// only by a proxy admin; organisation roles by an admin of that
// organisation; team_admin by an admin of the team's organisation; member
// and team viewer by an admin of the team. An organisation-level role is
// authority, not membership: it never makes the person the granter's to
// administer (personAccess, userBudgetAccess decide from team bindings).
func (s *Server) canGrant(ctx context.Context, p *principal, b store.Binding) (bool, error) {
	switch b.Role {
	case store.RoleProxyAdmin, store.RoleProxyViewer, store.RoleFinanceViewer:
		// Cross-organisation roles: a proxy admin grants them, nobody else, so
		// an organisation's admin can never open another organisation's reports.
		return p.proxyAdmin(), nil
	case store.RoleOrgAdmin:
		return p.orgAdmin(b.Org), nil
	case store.RoleViewer:
		if b.Org != "" {
			return p.orgAdmin(b.Org), nil
		}
	}
	t, err := s.st.GetTeam(ctx, b.Team)
	if err != nil {
		return false, err
	}
	if b.Role == store.RoleTeamAdmin {
		return p.orgAdmin(t.Org), nil
	}
	return p.teamAdmin(t), nil
}
