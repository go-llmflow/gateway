package gateway

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/fairshare"
	"github.com/go-llmflow/gateway/store"
)

// Chaos for high availability (#74), on a real PostgreSQL: replicas of
// one gateway share pool b300 through a lease.

const haTestSecret = "ha-test-secret-0123456789abcdef0123456789"

// haEnv runs one replica with an ha block on PostgreSQL at dsn.
func haEnv(t *testing.T, base *config.Config, dsn, id string, opts ...Option) *env {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := *base
	c.Store.Kind, c.Store.SpoolDir = "postgres", t.TempDir()
	c.HA = &config.HA{
		ReplicaID: id, Listen: ln.Addr().String(), Advertise: ln.Addr().String(),
		PeerNetworks: []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")},
		LeaseTTL:     900 * time.Millisecond, Heartbeat: 100 * time.Millisecond,
		ForwardTimeout: 100 * time.Millisecond, UsagePersist: 200 * time.Millisecond,
		OnOwnerUnreachable: "lowest_priority",
	}
	st, err := store.OpenPostgres(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := New(&c, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(&c), CacheSaltKey: testSaltKey, HASecret: haTestSecret},
		append([]Option{WithHAListener(ln)}, opts...)...)
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
	return &env{t: t, srv: srv, gw: gw, cfg: &c}
}

func haOwner(e *env) bool { o, _ := e.srv.ha.Status("b300"); return o }

// chatPriority sends a chat and returns its status and priority.
func chatPriority(e *env, key string) (int, int) {
	req, _ := http.NewRequest("POST", e.gw.URL+"/v1/chat/completions", strings.NewReader(chat))
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, -1
	}
	readAll(e.t, resp)
	p, _ := strconv.Atoi(resp.Header.Get("X-Llmflow-Priority"))
	return resp.StatusCode, p
}

// The owner is partitioned from PostgreSQL (a blackhole: nothing answers,
// nothing fails fast). It must stop deciding before the lease can go to
// the other replica, and never decide again under its old epoch: at no
// moment do both replicas decide. Requests on either keep being served.
func TestChaosHAPartitionedOwner(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	au, err := url.Parse(pgAdminDSN(t))
	if err != nil {
		t.Fatal(err)
	}
	px := newChaosProxy(t, au.Host)
	direct, viaProxy := freshDB(t, px.addr())
	a := haEnv(t, cfg, viaProxy, "a")
	b := haEnv(t, cfg, direct, "b")
	if !haOwner(a) || haOwner(b) {
		t.Fatal("a, started first, should own the pool")
	}
	_, k := a.newKey("etabA")
	if st, p := chatPriority(b, k); st != 200 || p != 0 {
		t.Fatalf("forwarded chat: %d priority %d", st, p)
	}
	if st, _ := chatPriority(a, k); st != 200 { // a caches the key
		t.Fatalf("chat on the owner: %d", st)
	}
	_, e1 := b.srv.ha.Status("b300")

	var both, sampled int
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			sampled++
			if haOwner(a) && haOwner(b) {
				both++
			}
		}
	}()
	px.set("blackhole")
	cut := time.Now()
	eventually(t, 2*time.Second, "the partitioned owner stops deciding", func() bool { return !haOwner(a) })
	stopped := time.Since(cut)
	eventually(t, 3*time.Second, "the other replica takes over", func() bool { return haOwner(b) })
	took := time.Since(cut)
	_, e2 := b.srv.ha.Status("b300")
	if e2 <= e1 {
		t.Fatalf("epoch %d after the takeover, was %d", e2, e1)
	}
	// Served on both sides meanwhile: on b by its own decision, on a at the
	// lowest priority (a cannot read the lease, so it does not know b).
	if st, _ := chatPriority(b, k); st != 200 {
		t.Fatalf("chat on the new owner: %d", st)
	}
	if st, p := chatPriority(a, k); st != 200 || p < fairshare.BorrowBase {
		t.Fatalf("chat on the partitioned replica: %d priority %d (it must not decide)", st, p)
	}
	px.set("")
	time.Sleep(time.Second)
	if haOwner(a) {
		t.Fatal("the healed replica took the pool back from a live owner")
	}
	if st, p := chatPriority(a, k); st != 200 || p != 0 {
		t.Fatalf("after the heal, a forwards to b: %d priority %d", st, p)
	}
	close(stop)
	wg.Wait()
	t.Logf("stopped deciding %v after the cut, takeover after %v, epoch %d -> %d; %d samples, %d with two owners",
		stopped.Round(time.Millisecond), took.Round(time.Millisecond), e1, e2, sampled, both)
	if both != 0 {
		t.Fatalf("both replicas decided at once in %d of %d samples", both, sampled)
	}
	if stopped > 900*time.Millisecond {
		t.Fatalf("the partitioned owner decided for %v, longer than its lease", stopped)
	}
}

// Clock skew of +-2 minutes on the second replica: the lease is timed by
// PostgreSQL, its validity by the monotonic clock, the internal channel
// accepts 5 minutes; ownership does not move and forwarding works.
func TestChaosHAClockSkew(t *testing.T) {
	for _, skew := range []time.Duration{2 * time.Minute, -2 * time.Minute} {
		up := newFakeVLLM(t)
		cfg := testConfig(up.URL())
		direct, _ := freshDB(t, "")
		a := haEnv(t, cfg, direct, "a")
		b := haEnv(t, cfg, direct, "b", WithClock(func() time.Time { return time.Now().Add(skew) }))
		_, k := a.newKey("etabA")
		time.Sleep(2 * time.Second) // more than two leases
		if !haOwner(a) || haOwner(b) {
			t.Fatalf("skew %v: ownership moved", skew)
		}
		if st, p := chatPriority(b, k); st != 200 || p != 0 {
			t.Fatalf("skew %v: forwarded chat %d priority %d", skew, st, p)
		}
		if n := b.srv.ha.Degraded(); n != 0 {
			t.Fatalf("skew %v: %d requests admitted without an owner", skew, n)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		a.gw.Close()
		a.srv.Close(ctx) // a graceful stop hands the lease over at once
		cancel()
		eventually(t, 2*time.Second, "the skewed replica takes over", func() bool { return haOwner(b) })
	}
}

// Two replicas started at the same moment on an empty database race for
// the lease: exactly one owns the pool.
func TestChaosHAStartRace(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	direct, _ := freshDB(t, "")
	envs := make([]*env, 2)
	var wg sync.WaitGroup
	for i := range envs {
		wg.Add(1)
		go func() { defer wg.Done(); envs[i] = haEnv(t, cfg, direct, "r"+strconv.Itoa(i)) }()
	}
	wg.Wait()
	owners := 0
	for _, e := range envs {
		if haOwner(e) {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("%d owners after a concurrent start", owners)
	}
}
