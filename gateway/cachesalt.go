package gateway

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"

	"github.com/go-llmflow/gateway/openai"
	"github.com/go-llmflow/gateway/store"
)

// minCacheSaltKey is the smallest accepted HMAC key: 256 bits.
const minCacheSaltKey = 32

// cacheScope names the tenant whose requests may share vLLM's prefix cache.
func (s *Server) cacheScope(k store.Key) string {
	switch s.cfg.CacheIsolation {
	case "key":
		return "key:" + k.ID
	case "team":
		return "team:" + k.Team
	default:
		return "org:" + k.Org
	}
}

// derive is base64url(HMAC-SHA256(key, label ‖ 0 ‖ parts…)), 43 characters
// (256 bits), as the vLLM security guide recommends for cache salts:
// unpredictable without the key, and stable for a given input so prefix
// caching keeps working inside the scope. (A salt drawn at random per
// request would disable prefix caching altogether.)
func (s *Server) derive(label string, parts ...string) string {
	m := hmac.New(sha256.New, []byte(s.secrets.CacheSaltKey))
	m.Write([]byte(label))
	for _, p := range parts {
		m.Write([]byte{0})
		m.Write([]byte(p))
	}
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// isolation returns the cache scoping of a request made with key k: the
// tenant's cache_salt, and multimodal uuids mapped into the tenant's own
// namespace (scoped, not stripped: within a tenant a client can still
// refer to media it sent before, but the same uuid presented by two
// tenants names two different cache entries). With cache_isolation none,
// client salts are still removed and uuids pass unchanged.
func (s *Server) isolation(k store.Key) openai.Isolation {
	if s.cfg.CacheIsolation == "none" {
		return openai.Isolation{}
	}
	scope := s.cacheScope(k)
	return openai.Isolation{
		CacheSalt: s.derive("cache_salt", scope),
		ScopeUUID: func(u string) string { return "llmf-" + s.derive("mm_uuid", scope, u) },
	}
}
