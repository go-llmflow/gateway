package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// BenchmarkJWTLookup measures the inference JWT path with one issuer:
// Hit is the cache (the steady state), Miss verifies the signature, routes
// the token to its issuer and maps it to a team. It compiles unchanged before
// and after the several-issuer change (v0.27.0), so the two can be compared
// with abbench.
func BenchmarkJWTLookup(b *testing.B) {
	idp := newFakeIdP(b)
	cfg := testConfig("http://127.0.0.1:1")
	cfg.JWT = &config.JWT{Issuer: idp.srv.URL, Audience: "llmflow", ClockSkew: time.Minute, MinRefresh: time.Hour,
		Maps: []config.JWTMap{{Claim: "groups", Value: "lab-a", Team: "etabA"}}}
	srv, err := New(cfg, store.NewMemory(), Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		b.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer srv.Close(context.Background())
	tok := idp.sign(b, map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}, idp.claims(time.Now(), nil))
	ctx := context.Background()
	if _, err := srv.lookupJWT(ctx, tok); err != nil {
		b.Fatal(err)
	}
	b.Run("Hit", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := srv.lookupJWT(ctx, tok); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Miss", func(b *testing.B) {
		b.ReportAllocs()
		all := func(store.Key) bool { return true }
		for i := 0; i < b.N; i++ {
			srv.jwtCache.forget(all)
			if _, err := srv.lookupJWT(ctx, tok); err != nil {
				b.Fatal(err)
			}
		}
	})
}
