package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
