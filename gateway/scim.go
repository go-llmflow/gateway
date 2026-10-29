package gateway

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/go-authn/scim"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// SCIM 2.0 provisioning. The protocol (filters, PATCH, paging, versions,
// errors) is github.com/go-authn/scim; this file is its Backend: the
// provisioned Users and Groups are kept as documents in the store, and
// after every change the gateway's own state is brought in line with them:
//
//   - a provisioned user is a gateway user of the same name, disabled while
//     the user is not active (which revokes their admin tokens) and when the
//     user is deleted;
//   - the members of a group named in a scim { group "…" } block get that
//     block's role on its team; leaving the group, being disabled or deleted
//     removes it. Only those (role, team) pairs of provisioned users are
//     touched: roles granted by hand, and users nobody provisioned, are left
//     alone.
//
// The reconciliation reads the whole provisioned state and is idempotent,
// so a failure half-way is repaired by the next change or the next start.

const minSCIMToken = 32

type scimBackend struct {
	s  *Server
	mu sync.Mutex // serialises reconciliations in this process
}

func toDoc(kind, id, name, version string, v any) (store.SCIMDoc, error) {
	b, err := json.Marshal(v)
	return store.SCIMDoc{Kind: kind, ID: id, Name: name, Data: b, Version: version}, err
}

func storeToSCIM(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return scim.ErrNotFound
	case errors.Is(err, store.ErrExists):
		return scim.ErrConflict
	case errors.Is(err, store.ErrConflict):
		return scim.ErrPrecondition
	}
	return err
}

func validSCIMUser(u scim.User) error {
	if !config.ValidUserName(u.UserName) {
		return scim.InvalidValue("userName must be [A-Za-z0-9_.@+-]{1,128} to become a gateway user")
	}
	return nil
}

func (b *scimBackend) CreateUser(ctx context.Context, u scim.User) error {
	if err := validSCIMUser(u); err != nil {
		return err
	}
	d, err := toDoc("User", u.ID, u.UserName, u.Meta.Version, u)
	if err != nil {
		return err
	}
	if err := b.s.st.SCIMCreate(ctx, d); err != nil {
		return storeToSCIM(err)
	}
	return b.reconcile(ctx)
}

func (b *scimBackend) user(ctx context.Context, id string) (scim.User, error) {
	d, err := b.s.st.SCIMGet(ctx, "User", id)
	if err != nil {
		return scim.User{}, storeToSCIM(err)
	}
	var u scim.User
	return u, json.Unmarshal(d.Data, &u)
}

func (b *scimBackend) groups(ctx context.Context, name string) ([]scim.Group, error) {
	ds, err := b.s.st.SCIMList(ctx, "Group", name)
	if err != nil {
		return nil, err
	}
	out := make([]scim.Group, 0, len(ds))
	for _, d := range ds {
		var g scim.Group
		if err := json.Unmarshal(d.Data, &g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func (b *scimBackend) users(ctx context.Context, name string) ([]scim.User, error) {
	ds, err := b.s.st.SCIMList(ctx, "User", name)
	if err != nil {
		return nil, err
	}
	out := make([]scim.User, 0, len(ds))
	for _, d := range ds {
		var u scim.User
		if err := json.Unmarshal(d.Data, &u); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, nil
}

// withGroups fills a user's direct groups.
func withGroups(u scim.User, gs []scim.Group) scim.User {
	u.Groups = nil
	for _, g := range gs {
		if slices.ContainsFunc(g.Members, func(m scim.Ref) bool { return m.Value == u.ID }) {
			u.Groups = append(u.Groups, scim.Ref{Value: g.ID, Display: g.DisplayName, Type: "direct"})
		}
	}
	return u
}

func (b *scimBackend) GetUser(ctx context.Context, id string) (scim.User, error) {
	u, err := b.user(ctx, id)
	if err != nil {
		return u, err
	}
	gs, err := b.groups(ctx, "")
	if err != nil {
		return u, err
	}
	return withGroups(u, gs), nil
}

func (b *scimBackend) ReplaceUser(ctx context.Context, u scim.User, ifVersion string) error {
	if err := validSCIMUser(u); err != nil {
		return err
	}
	old, err := b.user(ctx, u.ID)
	if err != nil {
		return err
	}
	d, err := toDoc("User", u.ID, u.UserName, u.Meta.Version, u)
	if err != nil {
		return err
	}
	if err := b.s.st.SCIMReplace(ctx, d, ifVersion); err != nil {
		return storeToSCIM(err)
	}
	if old.UserName != u.UserName {
		// A renamed person: the old gateway user leaves with its managed
		// roles, the new one gets them from the reconciliation.
		if err := b.retire(ctx, old.UserName); err != nil {
			return err
		}
	}
	return b.reconcile(ctx)
}

func (b *scimBackend) DeleteUser(ctx context.Context, id string) error {
	u, err := b.user(ctx, id)
	if err != nil {
		return err
	}
	if err := b.s.st.SCIMDelete(ctx, "User", id); err != nil {
		return storeToSCIM(err)
	}
	// Out of every group, as a deleted resource must be (RFC 7644 §3.6).
	gs, err := b.groups(ctx, "")
	if err != nil {
		return err
	}
	for _, g := range gs {
		if !slices.ContainsFunc(g.Members, func(m scim.Ref) bool { return m.Value == id }) {
			continue
		}
		v := g.Meta.Version
		g.Members = slices.DeleteFunc(g.Members, func(m scim.Ref) bool { return m.Value == id })
		g.Meta.Version = v + "-" + id // any new value: the version is opaque
		d, err := toDoc("Group", g.ID, g.DisplayName, g.Meta.Version, g)
		if err != nil {
			return err
		}
		if err := b.s.st.SCIMReplace(ctx, d, v); err != nil {
			return storeToSCIM(err)
		}
	}
	if err := b.retire(ctx, u.UserName); err != nil {
		return err
	}
	return b.reconcile(ctx)
}

// retire disables a gateway user that provisioning no longer names and
// removes the roles provisioning gave it.
func (b *scimBackend) retire(ctx context.Context, name string) error {
	bs, err := b.s.st.Bindings(ctx, name)
	if err != nil {
		return err
	}
	for _, bd := range bs {
		if b.managed(bd) {
			if err := b.s.st.Ungrant(ctx, bd); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
		}
	}
	if err := b.s.st.DisableUser(ctx, name); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	b.s.audit("scim.user_retired", "user", name)
	return nil
}

func (b *scimBackend) ListUsers(ctx context.Context, f *scim.Filter) ([]scim.User, error) {
	name := ""
	if f != nil {
		if a, v, ok := f.Equality(); ok && a == "username" {
			name = v
		}
	}
	us, err := b.users(ctx, name)
	if err != nil {
		return nil, err
	}
	gs, err := b.groups(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range us {
		us[i] = withGroups(us[i], gs)
	}
	return us, nil
}

// members checks that every member is a provisioned user or group.
func (b *scimBackend) members(ctx context.Context, g *scim.Group) error {
	for i, m := range g.Members {
		if _, err := b.s.st.SCIMGet(ctx, "User", m.Value); err == nil {
			g.Members[i].Type = "User"
			continue
		} else if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if _, err := b.s.st.SCIMGet(ctx, "Group", m.Value); err == nil && m.Value != g.ID {
			g.Members[i].Type = "Group"
			continue
		} else if err != nil && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		return scim.InvalidValue("member " + m.Value + " does not exist")
	}
	return nil
}

func (b *scimBackend) CreateGroup(ctx context.Context, g scim.Group) error {
	if err := b.members(ctx, &g); err != nil {
		return err
	}
	d, err := toDoc("Group", g.ID, g.DisplayName, g.Meta.Version, g)
	if err != nil {
		return err
	}
	if err := b.s.st.SCIMCreate(ctx, d); err != nil {
		return storeToSCIM(err)
	}
	return b.reconcile(ctx)
}

func (b *scimBackend) GetGroup(ctx context.Context, id string) (scim.Group, error) {
	d, err := b.s.st.SCIMGet(ctx, "Group", id)
	if err != nil {
		return scim.Group{}, storeToSCIM(err)
	}
	var g scim.Group
	return g, json.Unmarshal(d.Data, &g)
}

func (b *scimBackend) ReplaceGroup(ctx context.Context, g scim.Group, ifVersion string) error {
	if err := b.members(ctx, &g); err != nil {
		return err
	}
	d, err := toDoc("Group", g.ID, g.DisplayName, g.Meta.Version, g)
	if err != nil {
		return err
	}
	if err := b.s.st.SCIMReplace(ctx, d, ifVersion); err != nil {
		return storeToSCIM(err)
	}
	return b.reconcile(ctx)
}

func (b *scimBackend) DeleteGroup(ctx context.Context, id string) error {
	if err := b.s.st.SCIMDelete(ctx, "Group", id); err != nil {
		return storeToSCIM(err)
	}
	return b.reconcile(ctx)
}

func (b *scimBackend) ListGroups(ctx context.Context, f *scim.Filter) ([]scim.Group, error) {
	name := ""
	if f != nil {
		if a, v, ok := f.Equality(); ok && a == "displayname" {
			name = v
		}
	}
	return b.groups(ctx, name)
}

// mapping finds the scim group block for a display name.
func (b *scimBackend) mapping(displayName string) (config.SCIMGroup, bool) {
	for _, g := range b.s.cfg.SCIM.Groups {
		if strings.EqualFold(g.Name, displayName) {
			return g, true
		}
	}
	return config.SCIMGroup{}, false
}

// managed reports whether provisioning owns a binding's (role, team).
func (b *scimBackend) managed(bd store.Binding) bool {
	if bd.Org != "" {
		return false
	}
	for _, g := range b.s.cfg.SCIM.Groups {
		if g.Team == bd.Team && string(bd.Role) == g.Role {
			return true
		}
	}
	return false
}

// reconcile brings gateway users and managed bindings in line with the
// provisioned state.
func (b *scimBackend) reconcile(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	us, err := b.users(ctx, "")
	if err != nil {
		return err
	}
	gs, err := b.groups(ctx, "")
	if err != nil {
		return err
	}
	byID := map[string]scim.User{}
	provisioned := map[string]bool{}
	for _, u := range us {
		byID[u.ID] = u
		provisioned[u.UserName] = true
		gu, err := b.s.st.GetUser(ctx, u.UserName)
		switch {
		case errors.Is(err, store.ErrNotFound) || err == nil && gu.Disabled:
			if u.Enabled() {
				if err := b.s.st.CreateUser(ctx, u.UserName); err != nil && !errors.Is(err, store.ErrExists) {
					return err
				}
				b.s.audit("scim.user_enabled", "user", u.UserName)
			} else if errors.Is(err, store.ErrNotFound) {
				// Known, but inactive: a disabled gateway user.
				if err := b.s.st.CreateUser(ctx, u.UserName); err != nil && !errors.Is(err, store.ErrExists) {
					return err
				}
				if err := b.s.st.DisableUser(ctx, u.UserName); err != nil {
					return err
				}
			}
		case err != nil:
			return err
		case !u.Enabled():
			if err := b.s.st.DisableUser(ctx, u.UserName); err != nil {
				return err
			}
			b.s.audit("scim.user_disabled", "user", u.UserName)
		}
	}
	want := map[store.Binding]bool{}
	for _, g := range gs {
		m, ok := b.mapping(g.DisplayName)
		if !ok {
			continue
		}
		for _, mem := range g.Members {
			if u, ok := byID[mem.Value]; ok && u.Enabled() {
				want[store.Binding{User: u.UserName, Role: store.Role(m.Role), Team: m.Team}] = true
			}
		}
	}
	have, err := b.s.st.Bindings(ctx, "")
	if err != nil {
		return err
	}
	for _, bd := range have {
		if provisioned[bd.User] && b.managed(bd) {
			if want[bd.Key()] && bd.Provenance != store.ProvOrgAdmin {
				delete(want, bd.Key())
				continue
			}
			// A binding an administrator granted that provisioning also
			// wants is re-created as the provisioning's (#104).
			if want[bd.Key()] {
				if err := b.s.st.Ungrant(ctx, bd); err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				continue
			}
			if err := b.s.st.Ungrant(ctx, bd); err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			b.s.audit("scim.role_removed", "user", bd.User, "role", string(bd.Role), "team", bd.Team)
		}
	}
	for bd := range want {
		bd.Provenance = store.ProvSCIM
		switch err := b.s.st.Grant(ctx, bd); {
		case err == nil:
			b.s.audit("scim.role_granted", "user", bd.User, "role", string(bd.Role), "team", bd.Team)
		case errors.Is(err, store.ErrExists):
		case errors.Is(err, store.ErrNotFound):
			b.s.log.Warn("scim: cannot grant a role on a team that does not exist", "team", bd.Team, "user", bd.User)
		default:
			return err
		}
	}
	return nil
}

// scimHandler serves /scim/v2/: the bearer token compared in constant time,
// failures counted in the per-network limiter, then go-authn/scim.
func (s *Server) scimHandler() http.Handler {
	b := s.scimBackend
	h := http.StripPrefix("/scim/v2", &scim.Server{
		Backend: b, BaseURL: s.cfg.SCIM.BaseURL, Now: s.now,
		ErrorLog: func(err error) { s.log.Error("scim: backend error", "err", err) },
	})
	want := sha256.Sum256([]byte("Bearer " + s.secrets.SCIMToken))
	return s.withClientAddr(s.cfg.Routes.Admin, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn, haveNet := clientNet(r)
		if haveNet && s.authFails.blocked(cn) {
			apiError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many failed authentications from this address")
			return
		}
		got := sha256.Sum256([]byte(r.Header.Get("Authorization")))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			if haveNet {
				s.authFails.fail(cn)
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="scim"`)
			w.Header().Set("Content-Type", scim.MediaType)
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"schemas":["` + scim.ErrorSchema + `"],"status":"401","detail":"a valid bearer token is required"}`))
			return
		}
		h.ServeHTTP(w, r)
	}))
}
