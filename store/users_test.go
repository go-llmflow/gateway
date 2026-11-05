package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
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
	contractIdP(t, s)
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

// v0.27.0: a user is linked once to the identity provider and subject that
// vouched for them; the first link wins.
func TestUserIdPLink(t *testing.T) { contractIdP(t, NewMemory()) }

func contractIdP(t *testing.T, m Store) {
	ctx := context.Background()
	if _, err := m.UserIdP(ctx, "idp-user"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("no link yet: %v", err)
	}
	a := IdPLink{Provider: "a", Subject: "s1"}
	if got, err := m.LinkUserIdP(ctx, "idp-user", a); err != nil || got != a {
		t.Fatalf("first link: %+v %v", got, err)
	}
	if got, err := m.LinkUserIdP(ctx, "idp-user", IdPLink{Provider: "b", Subject: "s1"}); err != nil || got != a {
		t.Fatalf("the first link wins: %+v %v", got, err)
	}
	if got, err := m.UserIdP(ctx, "idp-user"); err != nil || got != a {
		t.Fatalf("read back: %+v %v", got, err)
	}
}

// RenameUsers: a legacy user, with everything keyed by their name, is renamed in
// one go; a second run does nothing; a name already in use refuses the whole
// batch and changes nothing.
func contractRenameUsers(t *testing.T, s Store) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(s.CreateUser(ctx, "mig-alice"))
	must(s.CreateUser(ctx, "mig-bob"))
	must(s.Grant(ctx, Binding{User: "mig-alice", Role: RoleTeamAdmin, Team: "etabA", Provenance: ProvProxyAdmin}))
	must(s.CreateUserToken(ctx, UserToken{ID: "t_mig", Hash: HashSecret("llma-mig"), User: "mig-alice"}))
	_, err := s.LinkUserIdP(ctx, "mig-alice", IdPLink{Provider: "p", Subject: "sub-1"})
	must(err)
	must(s.CreateKey(ctx, Key{ID: "k_mig", Hash: HashSecret("k-mig"), Team: "etabA", Owner: "mig-alice"}))
	must(s.SetBudget(ctx, Budget{BudgetKey: BudgetKey{Kind: ScopeUser, Name: "mig-alice"}, Period: Month, MaxCost: 5}))
	at := time.Now().UTC()
	must(s.InsertRecords(ctx, []Record{{ID: "mig-r1", Time: at, Org: "orgA", Team: "etabA", KeyID: "k_mig", User: "mig-alice", Model: "m", Pool: "p", PromptTokens: 10, Cost: 1}}))
	must(s.AssignProfile(ctx, Assignment{User: "mig-alice", Profile: "mig-prof", SetBy: "test", From: at}))

	// A collision: mig-bob@x.org exists already, so the batch is refused whole.
	must(s.CreateUser(ctx, "mig-bob@x.org"))
	renames := []UserRename{{From: "mig-alice", To: "mig-alice@x.org"}, {From: "mig-bob", To: "mig-bob@x.org"}}
	coll, err := s.RenameUsers(ctx, renames)
	if err != nil || len(coll) != 1 || coll[0].From != "mig-bob" {
		t.Fatalf("collision: %v %v", coll, err)
	}
	if _, err := s.GetUser(ctx, "mig-alice"); err != nil {
		t.Fatalf("a refused batch changed something: %v", err)
	}
	if _, err := s.GetUser(ctx, "mig-alice@x.org"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a refused batch created a user: %v", err)
	}

	// Without the collision it is made.
	coll, err = s.RenameUsers(ctx, renames[:1])
	if err != nil || len(coll) != 0 {
		t.Fatalf("rename: %v %v", coll, err)
	}
	if _, err := s.GetUser(ctx, "mig-alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old name remains: %v", err)
	}
	if u, err := s.GetUser(ctx, "mig-alice@x.org"); err != nil || u.Name != "mig-alice@x.org" {
		t.Fatalf("the new user: %+v %v", u, err)
	}
	if bs, _ := s.Bindings(ctx, "mig-alice@x.org"); len(bs) != 1 || bs[0].Role != RoleTeamAdmin {
		t.Errorf("roles: %+v", bs)
	}
	if ts, _ := s.ListUserTokens(ctx, "mig-alice@x.org"); len(ts) != 1 {
		t.Errorf("tokens: %+v", ts)
	}
	if l, err := s.UserIdP(ctx, "mig-alice@x.org"); err != nil || l.Subject != "sub-1" {
		t.Errorf("link: %+v %v", l, err)
	}
	if k, _ := s.GetKey(ctx, "k_mig"); k.Owner != "mig-alice@x.org" {
		t.Errorf("key owner: %q", k.Owner)
	}
	found := false
	bsd, _ := s.Budgets(ctx)
	for _, b := range bsd {
		if b.Kind == ScopeUser && b.Name == "mig-alice@x.org" && b.MaxCost == 5 {
			found = true
		}
		if b.Kind == ScopeUser && b.Name == "mig-alice" {
			t.Errorf("a budget kept the old name")
		}
	}
	if !found {
		t.Errorf("the budget did not follow")
	}
	rows, err := s.Report(ctx, at.Add(-time.Hour), at.Add(time.Hour), []string{"user"})
	must(err)
	spent := 0.0
	for _, r := range rows {
		if r.User == "mig-alice" {
			t.Errorf("spend history kept the old name")
		}
		if r.User == "mig-alice@x.org" {
			spent += r.Cost
		}
	}
	if spent != 1 {
		t.Errorf("spend history under the new name: %v", spent)
	}
	as, _ := s.Assignments(ctx)
	okA := false
	for _, a := range as {
		okA = okA || a.User == "mig-alice@x.org"
		if a.User == "mig-alice" {
			t.Errorf("assignment kept the old name")
		}
	}
	if !okA {
		t.Errorf("the profile assignment did not follow")
	}

	// A second run is a no-op.
	coll, err = s.RenameUsers(ctx, renames[:1])
	if err != nil || len(coll) != 0 {
		t.Fatalf("second run: %v %v", coll, err)
	}
	if u, err := s.GetUser(ctx, "mig-alice@x.org"); err != nil || u.Name != "mig-alice@x.org" {
		t.Fatalf("second run changed the user: %+v %v", u, err)
	}
}

// RecordOnlyUsers and the record-only renames, on a fixture with many records:
// people who were never stored users (JWT-only callers) have only spend records
// under a bare name. The plan is one grouped query; stored users and scoped
// names are not in it; a collision refuses the batch whole; a second run is a
// no-op.
func contractRecordOnly(t *testing.T, s Store) {
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const people, perPerson = 300, 7
	at := time.Now().UTC()
	var recs []Record
	for i := 0; i < people; i++ {
		for j := 0; j < perPerson; j++ {
			recs = append(recs, Record{ID: fmt.Sprintf("ro-%d-%d", i, j), Time: at, Org: "orgA", Team: "etabA", User: fmt.Sprintf("jwt%03d", i), Model: "m", Pool: "p", PromptTokens: 1, Cost: 1})
		}
	}
	must(s.CreateUser(ctx, "ro-stored"))
	must(s.CreateUser(ctx, "jwt007@x.org")) // a stored user holding a name a record-only rename would take
	recs = append(recs,
		Record{ID: "ro-stored-1", Time: at, Org: "orgA", Team: "etabA", User: "ro-stored", Model: "m", Pool: "p", Cost: 1},
		Record{ID: "ro-scoped-1", Time: at, Org: "orgA", Team: "etabA", User: "someone@y.org", Model: "m", Pool: "p", Cost: 1},
		Record{ID: "ro-empty-1", Time: at, Org: "orgA", Team: "etabA", User: "", Model: "m", Pool: "p", Cost: 1})
	must(s.InsertRecords(ctx, recs))

	names, err := s.RecordOnlyUsers(ctx)
	must(err)
	got := map[string]bool{}
	for _, n := range names {
		got[n] = true
	}
	if len(names) != people || got["ro-stored"] || got["someone@y.org"] || got[""] || !got["jwt000"] {
		t.Fatalf("RecordOnlyUsers: %d names (want %d), stored=%v scoped=%v", len(names), people, got["ro-stored"], got["someone@y.org"])
	}
	var plan []UserRename
	for _, n := range names {
		plan = append(plan, UserRename{From: n, To: n + "@x.org", RecordsOnly: true})
	}
	// jwt007 -> jwt007@x.org is taken by a stored user: the batch is refused whole.
	coll, err := s.RenameUsers(ctx, plan)
	if err != nil || len(coll) != 1 || coll[0].From != "jwt007" {
		t.Fatalf("collision: %v %v", coll, err)
	}
	if after, _ := s.RecordOnlyUsers(ctx); len(after) != people {
		t.Fatalf("a refused batch renamed records: %d names left", len(after))
	}
	// Without the colliding rename it is made, in one go.
	var ok []UserRename
	for _, r := range plan {
		if r.From != "jwt007" {
			ok = append(ok, r)
		}
	}
	if coll, err = s.RenameUsers(ctx, ok); err != nil || len(coll) != 0 {
		t.Fatalf("rename: %v %v", coll, err)
	}
	rows, err := s.Report(ctx, at.Add(-time.Hour), at.Add(time.Hour), []string{"user"})
	must(err)
	spent := map[string]float64{}
	for _, r := range rows {
		spent[r.User] += r.Cost
	}
	if spent["jwt000@x.org"] != perPerson || spent["jwt299@x.org"] != perPerson || spent["jwt000"] != 0 || spent["jwt007"] != perPerson {
		t.Errorf("spend by user: jwt000@=%v jwt299@=%v jwt000=%v jwt007=%v", spent["jwt000@x.org"], spent["jwt299@x.org"], spent["jwt000"], spent["jwt007"])
	}
	if spent["ro-stored"] != 1 || spent["someone@y.org"] != 1 {
		t.Errorf("a stored or scoped user's records changed: %v %v", spent["ro-stored"], spent["someone@y.org"])
	}
	// A second run is a no-op, and the plan is empty now but for the refused one.
	if coll, err = s.RenameUsers(ctx, ok); err != nil || len(coll) != 0 {
		t.Fatalf("second run: %v %v", coll, err)
	}
	if left, _ := s.RecordOnlyUsers(ctx); len(left) != 1 || left[0] != "jwt007" {
		t.Fatalf("left after the run: %v", left)
	}
}

func TestRenameUsersMemory(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if err := m.CreateOrg(ctx, "orgA"); err != nil {
		t.Fatal(err)
	}
	if err := m.CreateTeam(ctx, "etabA", "orgA"); err != nil {
		t.Fatal(err)
	}
	contractRenameUsers(t, m)
	contractRecordOnly(t, m)
}

func TestRenameUsersPostgres(t *testing.T) {
	dsn := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	p, err := OpenPostgres(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	dropAll(t, p)
	p.Close()
	if p, err = OpenPostgres(ctx, dsn); err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if err := p.CreateOrg(ctx, "orgA"); err != nil {
		t.Fatal(err)
	}
	if err := p.CreateTeam(ctx, "etabA", "orgA"); err != nil {
		t.Fatal(err)
	}
	contractRenameUsers(t, p)
	contractRecordOnly(t, p)
}
