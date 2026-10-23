package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// keyCache keeps recent key lookups so the request path does not query
// PostgreSQL every time. A revocation through this process's admin API is
// immediate; one made through another replica takes effect within the
// TTL.
//
// Hits and misses live in two separate bounded maps. Misses are cached
// (for a shorter time) so that a repeated wrong key costs one query, but a
// flood of random keys can only fill the miss map: it never evicts valid
// keys. (The first version wiped one shared map when it reached 100 000
// entries, so spraying random keys flushed every valid key and sent all
// legitimate traffic to PostgreSQL.) Neither map is ever cleared in bulk:
// when one is full, expired entries are dropped, then single entries.
type keyCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	valid   map[[32]byte]validEntry
	missing map[[32]byte]time.Time // expiry
	maxHit  int
	maxMiss int
	// nextMissPurge rate-limits the expiry scan of a full miss map.
	nextMissPurge time.Time
}

type validEntry struct {
	key     store.Key
	expires time.Time
}

const (
	maxValidEntries = 100_000
	maxMissEntries  = 10_000
)

func newKeyCache(ttl time.Duration, now func() time.Time) *keyCache {
	return &keyCache{
		ttl: ttl, now: now, valid: map[[32]byte]validEntry{}, missing: map[[32]byte]time.Time{},
		maxHit: maxValidEntries, maxMiss: maxMissEntries,
	}
}

// get returns (key, found, cached).
func (c *keyCache) get(h [32]byte) (store.Key, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if e, ok := c.valid[h]; ok && !now.After(e.expires) {
		return e.key, true, true
	}
	if exp, ok := c.missing[h]; ok && !now.After(exp) {
		return store.Key{}, false, true
	}
	return store.Key{}, false, false
}

func (c *keyCache) putValid(h [32]byte, k store.Key) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.valid) >= c.maxHit {
		now := c.now()
		for kh, e := range c.valid {
			if now.After(e.expires) {
				delete(c.valid, kh)
			}
		}
		for kh := range c.valid {
			if len(c.valid) < c.maxHit {
				break
			}
			delete(c.valid, kh)
		}
	}
	delete(c.missing, h)
	c.valid[h] = validEntry{key: k, expires: c.now().Add(c.ttl)}
}

func (c *keyCache) putMissing(h [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.missing) >= c.maxMiss {
		// Scanning on every miss of a spray would cost O(n) per request;
		// purge at most once per miss lifetime.
		if now.Before(c.nextMissPurge) {
			return // full of live misses: do not cache this one
		}
		c.nextMissPurge = now.Add(c.ttl / 5)
		for kh, exp := range c.missing {
			if now.After(exp) {
				delete(c.missing, kh)
			}
		}
		if len(c.missing) >= c.maxMiss {
			return
		}
	}
	c.missing[h] = now.Add(c.ttl / 5)
}

// forget drops cached valid entries matching a predicate.
func (c *keyCache) forget(match func(store.Key) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for h, e := range c.valid {
		if match(e.key) {
			delete(c.valid, h)
		}
	}
}

// failLimiter counts failed authentications per client network (IPv4
// address, IPv6 /64) over a sliding minute.
type failLimiter struct {
	mu    sync.Mutex
	limit int
	now   func() time.Time
	nets  map[netip.Prefix]*window
	// nextPurge rate-limits the scan of a full map.
	nextPurge time.Time
}

const maxTrackedNets = 100_000

func newFailLimiter(limit int, now func() time.Time) *failLimiter {
	return &failLimiter{limit: limit, now: now, nets: map[netip.Prefix]*window{}}
}

// clientNet keys a client by its network: the client address the network
// middleware established (see net.go), or the connection's address.
func clientNet(r *http.Request) (netip.Prefix, bool) {
	a, ok := clientAddrOf(r)
	if !ok {
		return netip.Prefix{}, false
	}
	bits := 32
	if a.Is6() {
		bits = 64
	}
	p, err := a.Prefix(bits)
	return p, err == nil
}

func (l *failLimiter) blocked(p netip.Prefix) bool {
	if l == nil || l.limit <= 0 {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	w := l.nets[p]
	return w != nil && w.value(l.now()) >= float64(l.limit)
}

func (l *failLimiter) fail(p netip.Prefix) {
	if l == nil || l.limit <= 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w := l.nets[p]
	if w == nil {
		if len(l.nets) >= maxTrackedNets {
			// At most one O(n) purge per 30 s, whatever the request rate.
			if now.Before(l.nextPurge) {
				return // cannot track more networks; fail open on tracking
			}
			l.nextPurge = now.Add(30 * time.Second)
			for k, v := range l.nets {
				if v.value(now) == 0 {
					delete(l.nets, k)
				}
			}
			if len(l.nets) >= maxTrackedNets {
				return
			}
		}
		w = &window{}
		l.nets[p] = w
	}
	w.roll(now)
	w.cur++
}

var errAuthUnavailable = errors.New("key store unreachable")

// lookup resolves a presented secret to a valid key.
func (s *Server) lookup(ctx context.Context, secret string) (store.Key, bool, error) {
	h := store.HashSecret(secret)
	k, found, cached := s.keys.get(h)
	if !cached {
		var err error
		k, err = s.st.KeyByHash(ctx, h)
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.keys.putMissing(h)
			return store.Key{}, false, nil
		case err != nil:
			return store.Key{}, false, errAuthUnavailable
		}
		s.keys.putValid(h, k)
		found = true
	}
	if !found || k.Revoked || !store.HashEqual(k.Hash, h) {
		return store.Key{}, false, nil
	}
	return k, true, nil
}

// authenticate checks the virtual key of a client request and writes the
// error response itself when it fails. A client network with too many
// failed authentications in the last minute gets 429 before its key is
// looked up, unless the key is a valid one already in the cache.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (store.Key, bool) {
	secret := bearer(r)
	cn, haveNet := clientNet(r)
	fail := func(status int, typ, msg string) (store.Key, bool) {
		if haveNet && status == http.StatusUnauthorized {
			s.authFails.fail(cn)
		}
		apiError(w, status, typ, msg)
		return store.Key{}, false
	}
	if secret == "" {
		return fail(http.StatusUnauthorized, "authentication_error", "missing bearer token")
	}
	if haveNet && s.authFails.blocked(cn) {
		if k, found, cached := s.keys.get(store.HashSecret(secret)); !(cached && found && !k.Revoked) {
			s.met.denied.add(1, "", "auth_failures")
			apiError(w, http.StatusTooManyRequests, "rate_limit_exceeded", "too many failed authentications from this address")
			return store.Key{}, false
		}
	}
	k, ok, err := s.lookup(r.Context(), secret)
	if err != nil {
		return fail(http.StatusServiceUnavailable, "service_unavailable", err.Error())
	}
	if !ok {
		return fail(http.StatusUnauthorized, "authentication_error", "invalid or revoked key")
	}
	if !s.addrAllowed(r, k) {
		s.met.denied.add(1, k.Team, "address")
		apiError(w, http.StatusForbidden, "permission_denied", "address not allowed")
		return store.Key{}, false
	}
	return k, true
}
