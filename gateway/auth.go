package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/go-llmflow/gateway/store"
)

// keyCache keeps recent key lookups so the request path does not query
// PostgreSQL every time. A revocation through this process's admin API is
// immediate; one made through another replica takes effect within the
// TTL. Misses are cached too (for a shorter time) so random keys cannot
// turn into one database query each.
type keyCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[[32]byte]cacheEntry
}

type cacheEntry struct {
	key     store.Key
	found   bool
	expires time.Time
}

const maxCacheEntries = 100_000

func newKeyCache(ttl time.Duration, now func() time.Time) *keyCache {
	return &keyCache{ttl: ttl, now: now, entries: map[[32]byte]cacheEntry{}}
}

func (c *keyCache) get(h [32]byte) (cacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[h]
	if !ok || c.now().After(e.expires) {
		return cacheEntry{}, false
	}
	return e, true
}

func (c *keyCache) put(h [32]byte, k store.Key, found bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.entries) >= maxCacheEntries {
		c.entries = map[[32]byte]cacheEntry{}
	}
	ttl := c.ttl
	if !found {
		ttl = c.ttl / 5
	}
	c.entries[h] = cacheEntry{key: k, found: found, expires: c.now().Add(ttl)}
}

// forget drops cached entries matching a predicate.
func (c *keyCache) forget(match func(store.Key) bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for h, e := range c.entries {
		if e.found && match(e.key) {
			delete(c.entries, h)
		}
	}
}

var errAuthUnavailable = errors.New("key store unreachable")

// lookup resolves a presented secret to a valid key.
func (s *Server) lookup(ctx context.Context, secret string) (store.Key, bool, error) {
	h := store.HashSecret(secret)
	e, ok := s.keys.get(h)
	if !ok {
		k, err := s.st.KeyByHash(ctx, h)
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.keys.put(h, store.Key{}, false)
			return store.Key{}, false, nil
		case err != nil:
			return store.Key{}, false, errAuthUnavailable
		}
		s.keys.put(h, k, true)
		e = cacheEntry{key: k, found: true}
	}
	if !e.found || e.key.Revoked || !store.HashEqual(e.key.Hash, h) {
		return store.Key{}, false, nil
	}
	return e.key, true, nil
}

// authenticate checks the virtual key of a client request and writes the
// error response itself when it fails.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (store.Key, bool) {
	secret := bearer(r)
	if secret == "" {
		apiError(w, http.StatusUnauthorized, "authentication_error", "missing bearer token")
		return store.Key{}, false
	}
	k, ok, err := s.lookup(r.Context(), secret)
	if err != nil {
		apiError(w, http.StatusServiceUnavailable, "service_unavailable", err.Error())
		return store.Key{}, false
	}
	if !ok {
		apiError(w, http.StatusUnauthorized, "authentication_error", "invalid or revoked key")
		return store.Key{}, false
	}
	return k, true
}

// adminAuth checks the master key in constant time (both sides hashed to a
// fixed length first, so the comparison does not leak the key's length).
func (s *Server) adminAuth(w http.ResponseWriter, r *http.Request) bool {
	if !store.HashEqual(store.HashSecret(bearer(r)), store.HashSecret(s.secrets.MasterKey)) {
		apiError(w, http.StatusUnauthorized, "authentication_error", "master key required")
		return false
	}
	return true
}
