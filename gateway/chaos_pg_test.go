package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
	"github.com/jackc/pgx/v5"
)

// Chaos against a real PostgreSQL (security audit 2026-10-26). Run when
// LLMFLOW_TEST_PG_DSN names a server (CI runs a service container); each
// test works in a database of its own, reached through a chaosProxy that
// can refuse (the server is down or restarting) or blackhole (a network
// partition) the gateway's connections.

func pgAdminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	return dsn
}

// freshDB creates a database for one test and returns its DSN, with the
// host replaced by via when via is not empty.
func freshDB(t *testing.T, via string) (direct, proxied string) {
	t.Helper()
	admin := pgAdminDSN(t)
	ctx := context.Background()
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "chaos_" + hex.EncodeToString(b[:])
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		c, err := pgx.Connect(context.Background(), admin)
		if err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	direct = u.String()
	if via != "" {
		u.Host = via
	}
	return direct, u.String()
}

// pgEnv runs a gateway on PostgreSQL at dsn.
func pgEnv(t *testing.T, cfg *config.Config, dsn string, opts ...Option) (*env, *store.Postgres) {
	t.Helper()
	st, err := store.OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey}, opts...)
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		gw.CloseClientConnections()
		gw.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Close(ctx)
		st.Close()
	})
	return &env{t: t, srv: srv, gw: gw, cfg: cfg}, st
}

// timedChat sends a chat request with a client deadline and returns the
// status (0 when the client gave up) and how long it took.
func timedChat(e *env, key string, limit time.Duration) (int, time.Duration) {
	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/chat/completions", strings.NewReader(chat))
	req.Header.Set("Authorization", "Bearer "+key)
	c := &http.Client{Timeout: limit}
	t0 := time.Now()
	resp, err := c.Do(req)
	if err != nil {
		return 0, time.Since(t0)
	}
	readAll(e.t, resp)
	return resp.StatusCode, time.Since(t0)
}

// eventually polls f until it holds, or fails after limit.
func eventually(t *testing.T, limit time.Duration, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, limit)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// PostgreSQL down, then back: an unknown key is not mistaken for an invalid
// one (503, not 401, and not cached as missing), a cached key is served
// until the budgets go stale, then everything fails closed; all of it
// recovers when the server is back.
func TestChaosPostgresDown(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.BudgetRefresh = 200 * time.Millisecond // stale after 600 ms
	direct, _ := freshDB(t, "")
	host, _ := url.Parse(direct)
	proxy := newChaosProxy(t, host.Host)
	_, dsn := freshDBAt(t, direct, proxy.addr())
	e, _ := pgEnv(t, cfg, dsn)
	_, warm := e.newKey("etabA")
	_, cold := e.newKey("etabA")
	expect(t, "warm key", e.chatWith(warm, chat), 200)

	proxy.set("refuse")
	if st, d := timedChat(e, cold, 20*time.Second); st != http.StatusServiceUnavailable {
		t.Errorf("unknown key while the store is down: %d after %s, want 503", st, d)
	}
	time.Sleep(time.Second)
	if st, _ := timedChat(e, warm, 20*time.Second); st != http.StatusServiceUnavailable {
		t.Errorf("cached key with stale budgets: %d, want 503 (fail-closed)", st)
	}
	resp := e.do("GET", "/admin/keys", master, "")
	if readAll(t, resp); resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("admin API while the store is down: %d", resp.StatusCode)
	}

	proxy.set("")
	eventually(t, 10*time.Second, "warm key after recovery", func() bool { st, _ := timedChat(e, warm, 5*time.Second); return st == 200 })
	if st, _ := timedChat(e, cold, 5*time.Second); st != 200 {
		t.Errorf("the key unknown during the outage after recovery: %d (cached as invalid?)", st)
	}
}

// freshDBAt rewrites direct's host to via.
func freshDBAt(t *testing.T, direct, via string) (string, string) {
	t.Helper()
	u, err := url.Parse(direct)
	if err != nil {
		t.Fatal(err)
	}
	u.Host = via
	return direct, u.String()
}

// A network partition between the gateway and PostgreSQL: nothing is
// refused, nothing answers. Requests must not hang on the store: an
// unknown key gets 503 within a bounded time, and the budgets fail closed
// on time even though the refresh never returns.
func TestChaosPostgresPartition(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.BudgetRefresh = 200 * time.Millisecond
	direct, _ := freshDB(t, "")
	host, _ := url.Parse(direct)
	proxy := newChaosProxy(t, host.Host)
	_, dsn := freshDBAt(t, direct, proxy.addr())
	e, _ := pgEnv(t, cfg, dsn)
	_, warm := e.newKey("etabA")
	_, cold := e.newKey("etabA")
	expect(t, "warm key", e.chatWith(warm, chat), 200)

	proxy.set("blackhole")
	st, d := timedChat(e, cold, 20*time.Second)
	t.Logf("unknown key during the partition: status %d after %s", st, d.Round(time.Millisecond))
	if st != http.StatusServiceUnavailable || d > 10*time.Second {
		t.Errorf("unknown key during a partition: %d after %s, want 503 within 10 s", st, d)
	}
	time.Sleep(time.Second)
	st, d = timedChat(e, warm, 20*time.Second)
	if st != http.StatusServiceUnavailable {
		t.Errorf("cached key during a partition: %d after %s, want 503 (fail-closed)", st, d)
	}

	proxy.set("")
	eventually(t, 15*time.Second, "service after the partition heals", func() bool { st, _ := timedChat(e, warm, 5*time.Second); return st == 200 })
}

// PostgreSQL restarted: every pooled connection is cut at once. A call that
// draws a dead pooled connection fails safe (503, never a 401 or a 500),
// and the pool recovers by itself within a few calls.
func TestChaosPostgresRestart(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	direct, _ := freshDB(t, "")
	host, _ := url.Parse(direct)
	proxy := newChaosProxy(t, host.Host)
	_, dsn := freshDBAt(t, direct, proxy.addr())
	e, _ := pgEnv(t, cfg, dsn)
	_, k := e.newKey("etabA")
	expect(t, "before", e.chatWith(k, chat), 200)
	proxy.set("refuse")
	proxy.set("")
	failed := 0
	eventually(t, 5*time.Second, "admin write after a restart", func() bool {
		resp := e.do("POST", "/admin/keys", master, `{"team":"etabA"}`)
		readAll(t, resp)
		switch resp.StatusCode {
		case http.StatusCreated:
			return true
		case http.StatusServiceUnavailable:
			failed++
			return false
		}
		t.Fatalf("after a restart: %d, want 201 or 503", resp.StatusCode)
		return false
	})
	t.Logf("admin calls answered 503 after the restart before the pool recovered: %d", failed)
}

// Usage records of requests served while PostgreSQL is down: the recorder
// tries a batch twice, then counts it failed (README open defect 6). This
// measures what an outage loses.
func TestChaosAccountingOutage(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	direct, _ := freshDB(t, "")
	host, _ := url.Parse(direct)
	proxy := newChaosProxy(t, host.Host)
	_, dsn := freshDBAt(t, direct, proxy.addr())
	e, st := pgEnv(t, cfg, dsn)
	_, k := e.newKey("etabA")
	expect(t, "warm", e.chatWith(k, chat), 200)
	proxy.set("refuse")
	served := 0
	for i := 0; i < 20; i++ {
		if s, _ := timedChat(e, k, 10*time.Second); s == 200 {
			served++
		}
	}
	time.Sleep(1500 * time.Millisecond) // the recorder's flush, twice
	proxy.set("")
	time.Sleep(1500 * time.Millisecond)
	rows, err := st.Report(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour), []string{"team"})
	if err != nil {
		t.Fatal(err)
	}
	var stored int64
	for _, r := range rows {
		stored += r.Requests
	}
	t.Logf("served during the outage: %d; records stored: %d of %d; failed: %d", served, stored, served+1, e.srv.rec.failed.Load())
	if stored+e.srv.rec.failed.Load() != int64(served+1) {
		t.Errorf("records neither stored nor counted as failed: stored %d, failed %d, served %d", stored, e.srv.rec.failed.Load(), served+1)
	}
}

// Two gateways on one database (the README says one replica per pool):
// what is shared works (keys created on one are served by the other, the
// audit chain stays one chain under concurrent writers), what is not is
// what the README says (a revocation reaches the other replica's cache
// only after key_cache_ttl).
func TestChaosTwoReplicas(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	direct, _ := freshDB(t, "")
	// Both start at once on an empty database: the schema is created
	// concurrently.
	var wg sync.WaitGroup
	envs := make([]*env, 2)
	stores := make([]*store.Postgres, 2)
	errs := make([]error, 2)
	for i := range envs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st, err := store.OpenPostgres(context.Background(), direct)
			if err != nil {
				errs[i] = err
				return
			}
			stores[i] = st
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("replica %d could not open the empty database while the other did: %v", i, err)
		}
		stores[i].Close()
	}
	a, _ := pgEnv(t, cfg, direct)
	b, stB := pgEnv(t, cfg, direct)

	id, k := a.newKey("etabA")
	expect(t, "key from A on B", b.chatWith(k, chat), 200)
	a.check(nil, []call{{"master", "DELETE", "/admin/keys/" + id, "", 204}})
	expect(t, "revoked, on A", a.chatWith(k, chat), 401)
	t.Logf("revoked on A, on B within key_cache_ttl: %d (README: takes effect within the TTL)", b.chatWith(k, chat))

	// Concurrent admin writes on both replicas: one audit chain.
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			readAll(t, a.do("POST", "/admin/teams/etabA/projects", master, fmt.Sprintf(`{"name":"a%d"}`, i)))
		}()
		go func() {
			defer wg.Done()
			readAll(t, b.do("POST", "/admin/teams/etabB/projects", master, fmt.Sprintf(`{"name":"b%d"}`, i)))
		}()
	}
	wg.Wait()
	rep, err := audit.Verify(context.Background(), stB, nil)
	if err != nil || !rep.OK() || rep.Entries < 40 {
		t.Fatalf("audit chain with two writers: %+v %v", rep, err)
	}
}
