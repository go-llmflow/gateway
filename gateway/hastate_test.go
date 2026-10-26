package gateway

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
	"github.com/jackc/pgx/v5"
)

// A revocation on one replica reaches the other's key cache at once
// (NOTIFY), although key_cache_ttl is an hour in this configuration.
func TestChaosHARevocationNotify(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	au, err := url.Parse(pgAdminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	px := newChaosProxy(t, au.Host)
	direct, viaProxy := freshDB(t, px.addr())
	a := haEnv(t, cfg, direct, "a")
	b := haEnv(t, cfg, viaProxy, "b")

	// A notification missed: b is cut from PostgreSQL while a revokes a
	// key; when b's listener reconnects it drops its cached keys, so the
	// revoked one is looked up again and refused.
	id, k := a.newKey("etabA")
	if st, _ := chatPriority(b, k); st != 200 {
		t.Fatalf("chat on b: %d", st)
	}
	px.set("refuse")
	a.check(nil, []call{{"master", "DELETE", "/admin/keys/" + id, "", 204}})
	time.Sleep(200 * time.Millisecond)
	px.set("")
	eventually(t, 10*time.Second, "a revocation missed during an outage is applied after the reconnect", func() bool {
		st, _ := chatPriority(b, k)
		return st == 401
	})

	for _, kind := range []string{"key", "team"} {
		id, k := a.newKey("etabA")
		if st, _ := chatPriority(b, k); st != 200 { // cached on b
			t.Fatalf("%s: chat on b: %d", kind, st)
		}
		t0 := time.Now()
		if kind == "key" {
			a.check(nil, []call{{"master", "DELETE", "/admin/keys/" + id, "", 204}})
		} else {
			a.check(nil, []call{{"master", "DELETE", "/admin/teams/etabA", "", 204}})
		}
		var took time.Duration
		for {
			st, _ := chatPriority(b, k)
			if st == 401 || st == 403 {
				took = time.Since(t0)
				break
			}
			if time.Since(t0) > time.Second {
				t.Fatalf("%s revoked on a still accepted on b after 1 s (status %d)", kind, st)
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Logf("%s revoked on a, refused on b after %v (key_cache_ttl %v)", kind, took.Round(time.Millisecond), cfg.KeyCacheTTL)
	}
}

// An SSO login started on one replica finishes on another, and the
// session outlives the replica that created it. What PostgreSQL holds is
// sealed: neither the cookie nor the user name appears in it.
func TestChaosHASSOSessionSurvives(t *testing.T) {
	idp := newFakeIdP(t)
	cfg := testConfig("http://127.0.0.1:1")
	cfg.AuthFailuresPerMinute = 0
	cfg.SSO = &config.SSO{
		Issuer: idp.srv.URL, ClientID: "llmflow-admin", RedirectURL: gwOrigin + "/admin/callback",
		Scopes: []string{"openid", "profile"}, SessionTTL: time.Hour,
		Roles: []config.SSORole{{Claim: "groups", Value: "llm-admins", Role: "proxy_admin"}},
	}
	direct, _ := freshDB(t, "")
	a := &ssoEnv{env: haEnv(t, cfg, direct, "a"), idp: idp, clock: time.Now()}
	b := &ssoEnv{env: haEnv(t, cfg, direct, "b"), idp: idp, clock: time.Now()}

	authURL, ck := a.start() // on a
	state, code := idp.issue(t, authURL, a.claims("alice", "llm-admins"))
	resp := b.callback(state, code, ck) // on b
	body := readAll(t, resp)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("callback on another replica: %d %s", resp.StatusCode, body)
	}
	sess, ok := cookieOf(resp, sessionCookie)
	if !ok {
		t.Fatal("no session cookie")
	}
	// The state was used once, on b: a replay on a is refused.
	if r := a.callback(state, code, ck); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("replayed callback on a: %d", r.StatusCode)
	}

	// b dies without a goodbye; a new replica c serves the session.
	b.gw.CloseClientConnections()
	b.gw.Close()
	c := &ssoEnv{env: haEnv(t, cfg, direct, "c"), idp: idp, clock: time.Now()}
	r := c.req("GET", "/admin/session", map[string]string{sessionCookie: sess}, "")
	info := readAll(t, r)
	if r.StatusCode != 200 || !bytes.Contains([]byte(info), []byte(`"alice"`)) {
		t.Fatalf("session on a replica started after its creator died: %d %s", r.StatusCode, info)
	}

	// At rest: hashed id, sealed body.
	conn, err := pgx.Connect(context.Background(), direct)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var id, sealed []byte
	if err := conn.QueryRow(context.Background(), `SELECT id, sealed FROM llmflow_sso WHERE kind = 'sso_session'`).Scan(&id, &sealed); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte("alice")) || bytes.Contains(sealed, []byte(sess)) || bytes.Equal(id, []byte(sess)) {
		t.Fatal("the stored session is not sealed")
	}
	// A sealed value moved to another row does not open.
	if _, err := conn.Exec(context.Background(), `UPDATE llmflow_sso SET id = $1 WHERE kind = 'sso_session'`, bytes.Repeat([]byte{1}, 32)); err != nil {
		t.Fatal(err)
	}
	var h [32]byte
	copy(h[:], bytes.Repeat([]byte{1}, 32))
	if _, _, err := c.srv.getSession(context.Background(), h); err == nil {
		t.Fatal("a session moved to another id opened")
	}
}

// Soft-budget alerts in the outbox: queued by two replicas for the same
// budget and period, delivered once per channel, retried after a failure
// by whichever replica claims it, and kept across a restart.
func TestChaosHAAlertOutbox(t *testing.T) {
	var hits, fails atomic.Int32
	var mu sync.Mutex
	keys := map[string]int{}
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fails.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // the first try fails
			return
		}
		hits.Add(1)
		mu.Lock()
		keys[r.Header.Get("Idempotency-Key")]++
		mu.Unlock()
	}))
	defer hook.Close()
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.Alerts.WebhookURL = hook.URL
	direct, _ := freshDB(t, "")
	a := haEnv(t, cfg, direct, "a")
	b := haEnv(t, cfg, direct, "b")
	for _, e := range []*env{a, b} {
		e.srv.alerts.backoff = func(int) time.Duration { return 100 * time.Millisecond }
	}
	al := softAlert{b: store.Budget{BudgetKey: store.BudgetKey{Kind: store.ScopeTeam, Name: "etabA"}, Period: store.Day, MaxCost: 10, SoftLimit: 8},
		spent: 8.5, start: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	a.srv.enqueueAlert(al)
	b.srv.enqueueAlert(al) // the same alert from the other replica
	deadline := time.Now().Add(10 * time.Second)
	for hits.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	time.Sleep(1500 * time.Millisecond) // a second delivery would have happened by now
	mu.Lock()
	defer mu.Unlock()
	if hits.Load() != 1 || len(keys) != 1 || fails.Load() != 2 {
		t.Fatalf("deliveries %d (keys %v), attempts %d: want one delivery after one failed attempt", hits.Load(), keys, fails.Load())
	}
}
