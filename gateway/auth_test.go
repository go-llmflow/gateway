package gateway

import (
	"context"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// countingStore counts key lookups that reach the store.
type countingStore struct {
	*store.Memory
	mu     sync.Mutex
	byHash map[[32]byte]int
	total  int
}

func (c *countingStore) KeyByHash(ctx context.Context, h [32]byte) (store.Key, error) {
	c.mu.Lock()
	c.byHash[h]++
	c.total++
	c.mu.Unlock()
	return c.Memory.KeyByHash(ctx, h)
}

func (c *countingStore) lookups(h [32]byte) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.byHash[h]
}

// A spray of random keys must not flush valid keys from the cache: the
// first version wiped the whole cache at 100 000 entries, after which every
// legitimate request went to PostgreSQL.
func TestValidKeysSurviveMissSpray(t *testing.T) {
	cs := &countingStore{Memory: store.NewMemory(), byHash: map[[32]byte]int{}}
	ctx := context.Background()
	if err := cs.CreateOrg(ctx, "etabA"); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateTeam(ctx, "etabA", "etabA"); err != nil {
		t.Fatal(err)
	}
	secret, id, h := store.GenerateKey()
	if err := cs.CreateKey(ctx, store.Key{ID: id, Hash: h, Team: "etabA"}); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig("http://127.0.0.1:1")
	srv, err := New(cfg, cs, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := srv.lookup(ctx, secret); !ok || err != nil {
		t.Fatalf("valid key: %v %v", ok, err)
	}
	for i := 0; i < 200_000; i++ {
		if _, ok, _ := srv.lookup(ctx, fmt.Sprintf("llmf-spray-%d", i)); ok {
			t.Fatal("random key accepted")
		}
	}
	if _, ok, err := srv.lookup(ctx, secret); !ok || err != nil {
		t.Fatalf("valid key after spray: %v %v", ok, err)
	}
	if n := cs.lookups(h); n != 1 {
		t.Fatalf("valid key looked up %d times in the store, want 1 (served from cache after the spray)", n)
	}
	srv.keys.mu.Lock()
	nm := len(srv.keys.missing)
	srv.keys.mu.Unlock()
	if nm > maxMissEntries {
		t.Fatalf("miss cache grew to %d", nm)
	}
}

func TestKeyCacheBounds(t *testing.T) {
	now := time.Unix(0, 0)
	c := newKeyCache(time.Minute, func() time.Time { return now })
	c.maxHit, c.maxMiss = 2, 2
	for i := byte(0); i < 5; i++ {
		c.putValid([32]byte{i}, store.Key{ID: fmt.Sprint(i)})
		c.putMissing([32]byte{100 + i})
	}
	if len(c.valid) > 2 || len(c.missing) > 2 {
		t.Fatalf("bounds: %d valid, %d missing", len(c.valid), len(c.missing))
	}
	// Expired misses make room.
	now = now.Add(time.Hour)
	c.putMissing([32]byte{200})
	if _, found, cached := c.get([32]byte{200}); found || !cached {
		t.Fatal("new miss not cached after expiry")
	}
	if _, _, cached := c.get([32]byte{4}); cached {
		t.Fatal("expired valid entry served")
	}
	c.putValid([32]byte{9}, store.Key{})
	c.putValid([32]byte{10}, store.Key{})
	c.putValid([32]byte{11}, store.Key{})
	if len(c.valid) > 2 {
		t.Fatalf("valid bound: %d", len(c.valid))
	}
}

func TestAuthFailureLimit(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.AuthFailuresPerMinute = 3
	e := newEnv(t, cfg)
	_, cached := e.newKey("etabA")
	_, uncached := e.newKey("etabA")
	if resp := e.do("GET", "/v1/models", cached, ""); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("warm cache: %d", resp.StatusCode)
	}
	for i := 0; i < 3; i++ {
		resp := e.do("GET", "/v1/models", fmt.Sprintf("llmf-wrong-%d", i), "")
		if readAll(t, resp); resp.StatusCode != 401 {
			t.Fatalf("failure %d: %d", i, resp.StatusCode)
		}
	}
	resp := e.do("GET", "/v1/models", "llmf-wrong-x", "")
	if readAll(t, resp); resp.StatusCode != 429 {
		t.Fatalf("after 3 failures: %d", resp.StatusCode)
	}
	// A valid key already in the cache still works from that address; an
	// uncached one waits (its lookup would hit the store).
	if resp := e.do("GET", "/v1/models", cached, ""); readAll(t, resp) == "" || resp.StatusCode != 200 {
		t.Fatalf("cached valid key: %d", resp.StatusCode)
	}
	if resp := e.do("GET", "/v1/models", uncached, ""); readAll(t, resp) == "" || resp.StatusCode != 429 {
		t.Fatalf("uncached key while blocked: %d", resp.StatusCode)
	}
	if resp := e.do("GET", "/admin/teams", master, ""); readAll(t, resp) == "" || resp.StatusCode != 429 {
		t.Fatalf("admin while blocked: %d", resp.StatusCode)
	}
}

func TestAdminFailuresCount(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.AuthFailuresPerMinute = 2
	e := newEnv(t, cfg)
	for i, want := range []int{401, 401, 429} {
		resp := e.do("GET", "/admin/teams", "guess", "")
		if readAll(t, resp); resp.StatusCode != want {
			t.Fatalf("attempt %d: %d, want %d", i, resp.StatusCode, want)
		}
	}
}

func TestClientNet(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	for addr, want := range map[string]string{
		"192.0.2.7:1234":             "192.0.2.7/32",
		"[2001:db8:1:2:3:4:5:6]:443": "2001:db8:1:2::/64",
		"[::ffff:192.0.2.9]:80":      "192.0.2.9/32",
		"198.51.100.1":               "198.51.100.1/32",
	} {
		r.RemoteAddr = addr
		p, ok := clientNet(r)
		if !ok || p.String() != want {
			t.Errorf("%s: %v %v, want %s", addr, p, ok, want)
		}
	}
	r.RemoteAddr = "garbage"
	if _, ok := clientNet(r); ok {
		t.Fatal("garbage parsed")
	}
	var nilLimiter *failLimiter
	nilLimiter.fail(netipPrefix(t, "192.0.2.1/32"))
	if nilLimiter.blocked(netipPrefix(t, "192.0.2.1/32")) {
		t.Fatal("nil limiter blocks")
	}
}

func TestFailLimiterBound(t *testing.T) {
	l := newFailLimiter(1, time.Now)
	for i := 0; i < maxTrackedNets+10; i++ {
		l.fail(netipPrefix(t, fmt.Sprintf("10.%d.%d.%d/32", i>>16&255, i>>8&255, i&255)))
	}
	if len(l.nets) > maxTrackedNets {
		t.Fatalf("tracked %d networks", len(l.nets))
	}
}

func netipPrefix(t *testing.T, s string) netip.Prefix {
	t.Helper()
	p, err := netip.ParsePrefix(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
