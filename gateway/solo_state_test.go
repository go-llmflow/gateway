package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// soloEnv is a gateway on PostgreSQL with no ha block: one node.
func soloEnv(t *testing.T, base *config.Config, dsn string) (*env, func()) {
	t.Helper()
	c := *base
	c.Store.Kind, c.Store.SpoolDir = "postgres", t.TempDir()
	c.HA = nil
	st, err := store.OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(&c, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(&c), CacheSaltKey: testSaltKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	stop := func() {
		gw.CloseClientConnections()
		gw.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Close(ctx)
		st.Close()
	}
	t.Cleanup(stop)
	return &env{t: t, srv: srv, gw: gw, cfg: &c}, stop
}

// With one gateway on PostgreSQL and no ha block, an SSO session outlives a
// restart (it was lost with the process: everyone logged in again).
func TestSoloSSOSessionSurvivesARestart(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := testConfig("http://127.0.0.1:1")
	cfg.AuthFailuresPerMinute = 0
	cfg.SSO = &config.SSO{
		Domain: "example.org", Issuer: idp.srv.URL, ClientID: "llmflow-admin", RedirectURL: gwOrigin + "/admin/callback",
		Scopes: []string{"openid", "profile"}, SessionTTL: time.Hour,
		Roles: []config.SSORole{{Claim: "groups", Value: "llm-admins", Role: "proxy_admin"}},
	}
	direct, _ := freshDB(t, "")
	e1, stop1 := soloEnv(t, cfg, direct)
	if e1.srv.shared == nil {
		t.Fatal("a gateway on PostgreSQL keeps no shared state")
	}
	a := &ssoEnv{env: e1, idp: idp, clock: time.Now()}
	authURL, ck := a.start()
	state, code := idp.issue(t, authURL, a.claims("alice", "llm-admins"))
	resp := a.callback(state, code, ck)
	if body := readAll(t, resp); resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback: %d %s", resp.StatusCode, body)
	}
	sess, ok := cookieOf(resp, sessionCookie)
	if !ok {
		t.Fatal("no session cookie")
	}
	stop1() // the restart

	e2, _ := soloEnv(t, cfg, direct)
	b := &ssoEnv{env: e2, idp: idp, clock: time.Now()}
	r := b.req("GET", "/admin/session", map[string]string{sessionCookie: sess}, "")
	if info := readAll(t, r); r.StatusCode != 200 || !bytes.Contains([]byte(info), []byte(`"alice@example.org"`)) {
		t.Fatalf("session after a restart: %d %s", r.StatusCode, info)
	}
}

// A soft-budget alert whose delivery failed is not lost with the process: the
// restarted gateway sends it, once.
func TestSoloAlertRetrySurvivesARestart(t *testing.T) {
	var up, hits atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if up.Load() == 0 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		hits.Add(1)
	}))
	defer hook.Close()
	fake := newFakeVLLM(t)
	cfg := testConfig(fake.URL())
	cfg.Alerts.WebhookURL = hook.URL
	direct, _ := freshDB(t, "")
	e1, stop1 := soloEnv(t, cfg, direct)
	e1.srv.alerts.backoff = func(int) time.Duration { return 100 * time.Millisecond }
	// What onSoftLimit does once the store says this alert is the first of its
	// period, with the outbox a gateway on PostgreSQL now has.
	e1.srv.enqueueAlert(softAlert{b: store.Budget{BudgetKey: store.BudgetKey{Kind: store.ScopeTeam, Name: "etabA"}, Period: store.Day, MaxCost: 10, SoftLimit: 8},
		spent: 8.5, start: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)})
	// The delivery fails while the hook is down; the gateway is stopped mid-retry.
	time.Sleep(1500 * time.Millisecond)
	if hits.Load() != 0 {
		t.Fatal("delivered while the hook was down")
	}
	stop1()

	up.Store(1)
	e2, _ := soloEnv(t, cfg, direct)
	e2.srv.alerts.backoff = func(int) time.Duration { return 100 * time.Millisecond }
	eventually(t, 15*time.Second, "the alert is delivered by the restarted gateway", func() bool { return hits.Load() >= 1 })
	time.Sleep(1500 * time.Millisecond)
	if hits.Load() != 1 {
		t.Fatalf("%d deliveries, want one", hits.Load())
	}
}
