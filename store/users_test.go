package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// contractUsers runs after contract (orgA, etabA exist).
func contractUsers(t *testing.T, s Store) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateUser(ctx, "alice"))
	must(s.CreateUser(ctx, "bob"))
	if err := s.CreateUser(ctx, "alice"); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate user: %v", err)
	}
	us, err := s.ListUsers(ctx)
	must(err)
	if len(us) != 2 || us[0].Name != "alice" {
		t.Fatalf("users %+v", us)
	}
	if _, err := s.GetUser(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetUser unknown: %v", err)
	}

	secret, id, h := GenerateAdminToken()
	if !strings.HasPrefix(secret, AdminTokenPrefix) || !HashEqual(h, HashSecret(secret)) {
		t.Fatalf("token %q", secret)
	}
	must(s.CreateUserToken(ctx, UserToken{ID: id, Hash: h, User: "alice"}))
	if err := s.CreateUserToken(ctx, UserToken{ID: "t_x", Hash: h, User: "alice"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate token hash: %v", err)
	}
	if err := s.CreateUserToken(ctx, UserToken{ID: "t_y", Hash: HashSecret("y"), User: "nobody"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token of unknown user: %v", err)
	}
	u, tok, err := s.UserByTokenHash(ctx, h)
	must(err)
	if u.Name != "alice" || tok.ID != id || tok.Revoked || tok.Hash != h {
		t.Fatalf("by hash %+v %+v", u, tok)
	}
	if _, _, err := s.UserByTokenHash(ctx, HashSecret("nope")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown token: %v", err)
	}
	ts, err := s.ListUserTokens(ctx, "alice")
	must(err)
	if len(ts) != 1 {
		t.Fatalf("tokens %+v", ts)
	}
	if err := s.RevokeUserToken(ctx, "bob", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking another user's token: %v", err)
	}
	must(s.RevokeUserToken(ctx, "alice", id))
	if _, tok, _ := s.UserByTokenHash(ctx, h); !tok.Revoked {
		t.Fatal("revoke token")
	}

	for _, b := range []Binding{
		{User: "alice", Role: RoleOrgAdmin, Org: "orgA"},
		{User: "bob", Role: RoleMember, Team: "etabA"},
		{User: "bob", Role: RoleViewer, Org: "orgA"},
		{User: "alice", Role: RoleProxyViewer},
	} {
		must(s.Grant(ctx, b))
	}
	if err := s.Grant(ctx, Binding{User: "alice", Role: RoleOrgAdmin, Org: "orgA"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate grant: %v", err)
	}
	for _, b := range []Binding{
		{User: "nobody", Role: RoleMember, Team: "etabA"},
		{User: "alice", Role: RoleOrgAdmin, Org: "nope"},
		{User: "alice", Role: RoleTeamAdmin, Team: "nope"},
	} {
		if err := s.Grant(ctx, b); !errors.Is(err, ErrNotFound) {
			t.Fatalf("grant %+v: %v", b, err)
		}
	}
	if err := s.Grant(ctx, Binding{User: "alice", Role: RoleOrgAdmin, Team: "etabA"}); err == nil {
		t.Fatal("org_admin on a team accepted")
	}
	bs, err := s.Bindings(ctx, "bob")
	must(err)
	if len(bs) != 2 || bs[0].Role != RoleMember || bs[1].Role != RoleViewer {
		t.Fatalf("bindings %+v", bs)
	}
	all, err := s.Bindings(ctx, "")
	must(err)
	if len(all) != 4 {
		t.Fatalf("all bindings %+v", all)
	}
	// Provenance (#104): recorded, not part of the identity, validated;
	// none recorded reads back as unknown.
	if bs[0].Provenance != ProvUnknown {
		t.Fatalf("a grant without provenance reads back %q", bs[0].Provenance)
	}
	must(s.Grant(ctx, Binding{User: "alice", Role: RoleMember, Team: "etabA", Provenance: ProvSCIM}))
	if err := s.Grant(ctx, Binding{User: "alice", Role: RoleMember, Team: "etabA", Provenance: ProvOrgAdmin}); !errors.Is(err, ErrExists) {
		t.Fatalf("same binding, another provenance: %v", err)
	}
	if err := s.Grant(ctx, Binding{User: "alice", Role: RoleViewer, Team: "etabA", Provenance: "root"}); err == nil {
		t.Fatal("an unknown provenance accepted")
	}
	ab, err := s.Bindings(ctx, "alice")
	must(err)
	found := false
	for _, b := range ab {
		if b.Role == RoleMember && b.Provenance == ProvSCIM {
			found = true
		}
	}
	if !found {
		t.Fatalf("provenance not stored: %+v", ab)
	}
	// Ungrant by identity, whatever the provenance.
	must(s.Ungrant(ctx, Binding{User: "alice", Role: RoleMember, Team: "etabA"}))
	if !ProvUnknown.Provisioned() || !ProvSCIM.Provisioned() || !ProvSSOClaim.Provisioned() || !ProvProxyAdmin.Provisioned() || ProvOrgAdmin.Provisioned() || Provenance("x").Provisioned() {
		t.Fatal("Provisioned")
	}
	must(s.Ungrant(ctx, Binding{User: "bob", Role: RoleViewer, Org: "orgA"}))
	if err := s.Ungrant(ctx, Binding{User: "bob", Role: RoleViewer, Org: "orgA"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungrant twice: %v", err)
	}

	// Key owner.
	_, kid, kh := GenerateKey()
	must(s.CreateKey(ctx, Key{ID: kid, Hash: kh, Team: "etabA", Owner: "bob"}))
	if k, err := s.GetKey(ctx, kid); err != nil || k.Owner != "bob" {
		t.Fatalf("owner %+v %v", k, err)
	}

	_, id2, h2 := GenerateAdminToken()
	must(s.CreateUserToken(ctx, UserToken{ID: id2, Hash: h2, User: "bob"}))
	must(s.DisableUser(ctx, "bob"))
	if u, tok, _ := s.UserByTokenHash(ctx, h2); !u.Disabled || !tok.Revoked {
		t.Fatalf("disable user: %+v %+v", u, tok)
	}
	if err := s.DisableUser(ctx, "nobody"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("disable unknown user: %v", err)
	}
	if err := s.CreateUserToken(ctx, UserToken{ID: "t_z", Hash: HashSecret("z"), User: "bob"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("token for disabled user: %v", err)
	}
	must(s.CreateUser(ctx, "bob")) // re-enable
}

func TestBindingValidate(t *testing.T) {
	good := []Binding{
		{Role: RoleProxyAdmin}, {Role: RoleProxyViewer}, {Role: RoleOrgAdmin, Org: "o"},
		{Role: RoleTeamAdmin, Team: "t"}, {Role: RoleMember, Team: "t"}, {Role: RoleViewer, Org: "o"}, {Role: RoleViewer, Team: "t"},
	}
	for _, b := range good {
		if err := b.Validate(); err != nil {
			t.Errorf("%+v: %v", b, err)
		}
	}
	bad := []Binding{
		{Role: RoleProxyAdmin, Org: "o"}, {Role: RoleOrgAdmin}, {Role: RoleOrgAdmin, Org: "o", Team: "t"},
		{Role: RoleTeamAdmin, Org: "o"}, {Role: RoleMember}, {Role: RoleViewer}, {Role: RoleViewer, Org: "o", Team: "t"},
		{Role: "root"},
	}
	for _, b := range bad {
		if b.Validate() == nil {
			t.Errorf("%+v accepted", b)
		}
	}
}

func TestMemoryUsersFaults(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	boom := errors.New("boom")
	m.SetFail(boom)
	_, e1 := m.GetUser(ctx, "a")
	_, e2 := m.ListUsers(ctx)
	_, e3 := m.ListUserTokens(ctx, "a")
	_, _, e4 := m.UserByTokenHash(ctx, [32]byte{})
	_, e5 := m.Bindings(ctx, "")
	for i, err := range []error{
		m.CreateUser(ctx, "a"), m.DisableUser(ctx, "a"), m.CreateUserToken(ctx, UserToken{}), m.RevokeUserToken(ctx, "a", "b"),
		m.Grant(ctx, Binding{Role: RoleProxyAdmin}), m.Ungrant(ctx, Binding{}), e1, e2, e3, e4, e5,
	} {
		if !errors.Is(err, boom) {
			t.Fatalf("call %d: %v", i, err)
		}
	}
}
