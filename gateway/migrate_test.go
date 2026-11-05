package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// legacyEnv is a gateway over a store that already holds users from before
// scoped names (bare names), with the sso block of the institution that
// carries the domain. start is false when the gateway is expected to refuse.
func legacyEnv(t *testing.T, st *store.Memory, legacyNames bool) (*ssoEnv, error) {
	t.Helper()
	idp := newFakeIdP(t)
	cfg := testConfig("http://127.0.0.1:1")
	cfg.AuthFailuresPerMinute = 0
	cfg.LegacyNames = legacyNames
	cfg.SSO = &config.SSO{Domain: "example.org", Issuer: idp.srv.URL, ClientID: "llmflow-admin", RedirectURL: gwOrigin + "/admin/callback",
		Scopes: []string{"openid", "profile"}, SessionTTL: time.Hour,
		Roles: []config.SSORole{{Claim: "groups", Value: "llm-admins", Role: "proxy_admin"}}}
	e := &ssoEnv{idp: idp, clock: time.Now()}
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		return nil, err
	}
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { gw.Close(); srv.Close(context.Background()) })
	e.env = &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	return e, nil
}

func legacyStore(t *testing.T) *store.Memory {
	t.Helper()
	st := store.NewMemory()
	ctx := context.Background()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.CreateOrg(ctx, "etabA"))
	must(st.CreateTeam(ctx, "etabA", "etabA"))
	must(st.CreateUser(ctx, "alice"))
	must(st.Grant(ctx, store.Binding{User: "alice", Role: store.RoleTeamAdmin, Team: "etabA", Provenance: store.ProvProxyAdmin}))
	must(st.SetBudget(ctx, store.Budget{BudgetKey: store.BudgetKey{Kind: store.ScopeUser, Name: "alice"}, Period: store.Month, MaxCost: 5}))
	must(st.InsertRecords(ctx, []store.Record{{ID: "r1", Time: time.Now().UTC(), Org: "etabA", Team: "etabA", User: "alice", Model: "llama", Pool: "b300", PromptTokens: 10, Cost: 2}}))
	return st
}

func spendByUser(t *testing.T, e *ssoEnv) map[string]float64 {
	t.Helper()
	from, to := time.Now().AddDate(0, 0, -1).Format("2006-01-02"), time.Now().AddDate(0, 0, 2).Format("2006-01-02")
	rep := readAll(t, e.do("GET", "/admin/spend?from="+from+"&to="+to+"&group_by=user", master, ""))
	out := map[string]float64{}
	for _, name := range []string{"alice", "alice@example.org"} {
		if strings.Contains(rep, `"user":"`+name+`"`) {
			out[name] = 1
		}
	}
	return out
}

// A legacy database, migrated: a login gives the same person, with their roles,
// and the spend history is under the new name.
func TestMigrateUsersKeepsThePersonAndTheirHistory(t *testing.T) {
	st := legacyStore(t)
	// Not migrated: the gateway refuses to start, and says what to do.
	if _, err := legacyEnv(t, st, false); err == nil || !strings.Contains(err.Error(), "migrate-users") {
		t.Fatalf("started over unscoped users: %v", err)
	}
	plan, coll, err := MigrateUsers(context.Background(), st, "example.org", false)
	if err != nil || len(plan) != 1 || len(coll) != 0 || plan[0] != (store.UserRename{From: "alice", To: "alice@example.org"}) {
		t.Fatalf("dry run: %v %v %v", plan, coll, err)
	}
	if _, err := st.GetUser(context.Background(), "alice"); err != nil {
		t.Fatal("a dry run renamed a user")
	}
	if _, coll, err := MigrateUsers(context.Background(), st, "example.org", true); err != nil || len(coll) != 0 {
		t.Fatalf("migrate: %v %v", coll, err)
	}
	// A second run is a no-op.
	if plan, coll, err := MigrateUsers(context.Background(), st, "example.org", true); err != nil || len(plan) != 0 || len(coll) != 0 {
		t.Fatalf("second run: %v %v %v", plan, coll, err)
	}
	e, err := legacyEnv(t, st, false)
	if err != nil {
		t.Fatal(err)
	}
	// The same person logs in: alice (the IdP's bare name) is alice@example.org, who
	// holds the stored role.
	sess, _ := e.login(e.claims("alice"))
	if got := sessionName(t, e, sess); got != "alice@example.org" {
		t.Errorf("the person after migration: %q", got)
	}
	resp := e.req("GET", "/admin/session", map[string]string{sessionCookie: sess}, "")
	if body := readAll(t, resp); !strings.Contains(body, `"team_admin"`) {
		t.Errorf("the stored role did not follow: %s", body)
	}
	// The spend history is under the new name, and none under the old.
	if got := spendByUser(t, e); got["alice@example.org"] != 1 || got["alice"] != 0 {
		t.Errorf("spend history: %v", got)
	}
}

// A new name already in use refuses the migration whole.
func TestMigrateUsersRefusesCollisions(t *testing.T) {
	st := legacyStore(t)
	ctx := context.Background()
	if err := st.CreateUser(ctx, "alice@example.org"); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(ctx, "bob"); err != nil {
		t.Fatal(err)
	}
	plan, coll, err := MigrateUsers(ctx, st, "example.org", true)
	if err != nil || len(coll) != 1 || coll[0].From != "alice" || len(plan) != 2 {
		t.Fatalf("collision: %v %v %v", plan, coll, err)
	}
	for _, n := range []string{"alice", "bob"} {
		if _, err := st.GetUser(ctx, n); err != nil {
			t.Errorf("%s was renamed by a refused migration: %v", n, err)
		}
	}
	if _, err := st.GetUser(ctx, "bob@example.org"); err == nil {
		t.Error("bob was renamed by a refused migration")
	}
	// A dry run names the collision too.
	if _, coll, _ := MigrateUsers(ctx, st, "example.org", false); len(coll) != 1 {
		t.Errorf("dry run collisions: %v", coll)
	}
}

// legacy_unscoped_names keeps the read-only alias while the users wait: the
// gateway starts, and a bare name is the stored user.
func TestLegacyNamesAliasIsOptIn(t *testing.T) {
	st := legacyStore(t)
	e, err := legacyEnv(t, st, true)
	if err != nil {
		t.Fatal(err)
	}
	sess, _ := e.login(e.claims("alice"))
	if got := sessionName(t, e, sess); got != "alice" {
		t.Errorf("the alias: %q", got)
	}
	_ = http.StatusOK
}

// A person who was never a stored user (a JWT-only caller) has spend records,
// and maybe a budget, under the bare name of before v0.29.0. migrate-users
// brings them under the scoped name, so /v1/me/usage and the per-user report
// show one person, not two; a second run is a no-op; the audit chain is not
// touched.
func TestMigrateUsersRenamesJWTOnlyHistory(t *testing.T) {
	a := newFakeIdP(t)
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.AuthFailuresPerMinute = 0
	cfg.JWTs = []*config.JWT{{Name: "a", Issuer: a.srv.URL, Audience: "llmflow", ClockSkew: time.Minute, MinRefresh: time.Hour, Orgs: []string{"etabA"},
		Domain: "univ-a.fr", Maps: []config.JWTMap{{Claim: "groups", Value: "lab", Team: "etabA"}}}}
	st := store.NewMemory()
	ctx := context.Background()
	if err := st.SetBudget(ctx, store.Budget{BudgetKey: store.BudgetKey{Kind: store.ScopeUser, Name: "carol"}, Period: store.Month, MaxCost: 5}); err != nil {
		t.Fatal(err)
	}
	var recs []store.Record
	for i := 0; i < 50; i++ {
		recs = append(recs, store.Record{ID: "c" + string(rune('A'+i/26)) + string(rune('a'+i%26)), Time: time.Now().UTC(), Org: "etabA", Team: "etabA", User: "carol", Model: "llama", Pool: "b300", PromptTokens: 10, Cost: 0.02})
	}
	if err := st.InsertRecords(ctx, recs); err != nil {
		t.Fatal(err)
	}
	log := audit.New(st, nil, nil)
	if _, err := log.Append(ctx, store.AuditEntry{Kind: "system", Actor: "t", Roles: "[]", Action: "before", Target: "x", Before: "null", After: "null"}); err != nil {
		t.Fatal(err)
	}

	plan, coll, err := MigrateUsers(ctx, st, "univ-a.fr", false)
	if err != nil || len(coll) != 0 || len(plan) != 1 || plan[0] != (store.UserRename{From: "carol", To: "carol@univ-a.fr", RecordsOnly: true}) {
		t.Fatalf("dry run: %v %v %v", plan, coll, err)
	}
	if n, _ := st.RecordOnlyUsers(ctx); len(n) != 1 {
		t.Fatalf("a dry run changed records: %v", n)
	}
	if _, coll, err := MigrateUsers(ctx, st, "univ-a.fr", true); err != nil || len(coll) != 0 {
		t.Fatalf("migrate: %v %v", coll, err)
	}
	if plan, coll, err := MigrateUsers(ctx, st, "univ-a.fr", true); err != nil || len(plan) != 0 || len(coll) != 0 {
		t.Fatalf("second run: %v %v %v", plan, coll, err)
	}
	if _, err := log.Append(ctx, store.AuditEntry{Kind: "system", Actor: "t", Roles: "[]", Action: "after", Target: "x", Before: "null", After: "null"}); err != nil {
		t.Fatal(err)
	}
	if rep, err := audit.Verify(ctx, st, nil); err != nil || !rep.OK() {
		t.Fatalf("the audit chain after the migration: %v %+v", err, rep.Problems)
	}

	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { gw.Close(); srv.Close(ctx) })
	e := &env{t: t, srv: srv, gw: gw, st: st, cfg: cfg}
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}
	jwt := a.sign(t, rs, a.claims(time.Now(), map[string]any{"sub": "c-1", "preferred_username": "carol", "groups": []string{"lab"}}))
	// /v1/me/usage: the caller is carol@univ-a.fr, and the budget of her history is hers.
	me := readAll(t, e.do("GET", "/v1/me/usage", jwt, ""))
	if !strings.Contains(me, `"user":"carol@univ-a.fr"`) || !strings.Contains(me, `"budgets":[{`) {
		t.Errorf("/v1/me/usage after the migration: %s", me)
	}
	// The per-user report: one person.
	from, to := time.Now().AddDate(0, 0, -1).Format("2006-01-02"), time.Now().AddDate(0, 0, 2).Format("2006-01-02")
	rep := readAll(t, e.do("GET", "/admin/spend?from="+from+"&to="+to+"&group_by=user", master, ""))
	if !strings.Contains(rep, `"user":"carol@univ-a.fr"`) || strings.Contains(rep, `"user":"carol"`) {
		t.Errorf("the per-user report: %s", rep)
	}
}

// A record-only rename onto a name already in use is refused and listed, and
// nothing is changed.
func TestMigrateUsersRefusesRecordOnlyCollisions(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	if err := st.CreateUser(ctx, "dave@univ-a.fr"); err != nil {
		t.Fatal(err)
	}
	recs := []store.Record{
		{ID: "d1", Time: time.Now().UTC(), Org: "etabA", Team: "etabA", User: "dave", Model: "llama", Pool: "b300", Cost: 1},
		{ID: "e1", Time: time.Now().UTC(), Org: "etabA", Team: "etabA", User: "erin", Model: "llama", Pool: "b300", Cost: 1},
	}
	if err := st.InsertRecords(ctx, recs); err != nil {
		t.Fatal(err)
	}
	plan, coll, err := MigrateUsers(ctx, st, "univ-a.fr", true)
	if err != nil || len(plan) != 2 || len(coll) != 1 || coll[0].From != "dave" || !coll[0].RecordsOnly {
		t.Fatalf("collision: %v %v %v", plan, coll, err)
	}
	for _, r := range st.Records() {
		if strings.Contains(r.User, "@") {
			t.Errorf("a refused migration renamed %q", r.User)
		}
	}
	if _, coll, _ := MigrateUsers(ctx, st, "univ-a.fr", false); len(coll) != 1 {
		t.Errorf("a dry run names the collision too: %v", coll)
	}
}
