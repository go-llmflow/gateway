package gateway

import (
	"strings"
	"testing"

	"github.com/go-llmflow/gateway/store"
)

// saltOf sends a chat request with key and returns the cache_salt the
// recording upstream received.
func saltOf(t *testing.T, e *env, up *fakeVLLM, key, body string) string {
	t.Helper()
	expect(t, "request", e.chatWith(key, body), 200)
	s, _ := up.last().Body["cache_salt"].(string)
	return s
}

const mmChat = `{"model":"llama","cache_salt":"client-chosen","messages":[{"role":"user","content":[` +
	`{"type":"text","text":"what is this?"},{"type":"image_url","image_url":{"url":"http://img/1"},"uuid":"img-1"}]}]}`

func TestCacheSaltPerOrganisation(t *testing.T) {
	up := newFakeVLLM(t)
	e := newEnv(t, testConfig(up.URL()))
	e.check(nil, []call{{"master", "POST", "/admin/teams", `{"name":"etabA2","org":"etabA"}`, 201}})
	a1 := e.createKey(`{"team":"etabA"}`)
	a2 := e.createKey(`{"team":"etabA2"}`) // same organisation, other team
	b := e.createKey(`{"team":"etabB"}`)

	sa1 := saltOf(t, e, up, a1, mmChat)
	uuidA1, _ := up.last().Body["messages"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)["uuid"].(string)
	sa2 := saltOf(t, e, up, a2, mmChat)
	sb := saltOf(t, e, up, b, mmChat)
	uuidB, _ := up.last().Body["messages"].([]any)[0].(map[string]any)["content"].([]any)[1].(map[string]any)["uuid"].(string)

	if len(sa1) != 43 || strings.ContainsAny(sa1, "+/=") {
		t.Fatalf("salt %q: want 43 base64url characters", sa1)
	}
	if sa1 != sa2 {
		t.Fatalf("same organisation, different salts: %q %q", sa1, sa2)
	}
	if sa1 == sb {
		t.Fatal("two organisations share a salt")
	}
	if sa1 == "client-chosen" || sb == "client-chosen" {
		t.Fatal("client salt reached vLLM")
	}
	// Stable: the same tenant gets the same salt on the next request.
	if again := saltOf(t, e, up, a1, chat); again != sa1 {
		t.Fatalf("salt not stable: %q then %q", sa1, again)
	}
	// Multimodal uuids are scoped per tenant, not passed through.
	if uuidA1 == "img-1" || uuidB == "img-1" || uuidA1 == uuidB || !strings.HasPrefix(uuidA1, "llmf-") {
		t.Fatalf("uuids A=%q B=%q", uuidA1, uuidB)
	}
}

func TestCacheSaltScopes(t *testing.T) {
	for _, tc := range []struct {
		iso      string
		sameTeam bool // two keys of one team share a salt
		sameOrg  bool // two teams of one organisation share a salt
	}{
		{"team", true, false},
		{"key", false, false},
		{"org", true, true},
	} {
		t.Run(tc.iso, func(t *testing.T) {
			up := newFakeVLLM(t)
			cfg := testConfig(up.URL())
			cfg.CacheIsolation = tc.iso
			e := newEnv(t, cfg)
			e.check(nil, []call{{"master", "POST", "/admin/teams", `{"name":"etabA2","org":"etabA"}`, 201}})
			k1 := e.createKey(`{"team":"etabA"}`)
			k2 := e.createKey(`{"team":"etabA"}`)
			k3 := e.createKey(`{"team":"etabA2"}`)
			s1, s2, s3 := saltOf(t, e, up, k1, chat), saltOf(t, e, up, k2, chat), saltOf(t, e, up, k3, chat)
			if (s1 == s2) != tc.sameTeam || (s1 == s3) != tc.sameOrg {
				t.Fatalf("%s: same team %v, same org %v", tc.iso, s1 == s2, s1 == s3)
			}
		})
	}
}

func TestCacheSaltNoneAndKeyRequirements(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.CacheIsolation = "none"
	st := store.NewMemory()
	// none: no key needed, client salt still removed, uuids unchanged.
	srv, err := New(cfg, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	if iso := srv.isolation(store.Key{Org: "o"}); iso.CacheSalt != "" || iso.ScopeUUID != nil {
		t.Fatalf("none: %+v", iso)
	}
	// Isolation on: the key is required and must be 256 bits at least.
	cfg2 := testConfig(up.URL())
	for _, k := range []string{"", "too-short"} {
		if _, err := New(cfg2, st, Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg2), CacheSaltKey: k}); err == nil {
			t.Fatalf("cache salt key %q accepted", k)
		}
	}
}

// Rotating the key changes every salt (and so starts every tenant with a
// cold cache, by design).
func TestCacheSaltKeyRotation(t *testing.T) {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	mk := func(key string) *Server {
		s, err := New(cfg, store.NewMemory(), Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: key})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	k := store.Key{Org: "etabA", Team: "etabA", ID: "k_1"}
	a, b := mk(testSaltKey), mk(testSaltKey+"-rotated")
	if a.isolation(k).CacheSalt == b.isolation(k).CacheSalt {
		t.Fatal("rotation kept the salt")
	}
	if a.isolation(k).ScopeUUID("u") == b.isolation(k).ScopeUUID("u") {
		t.Fatal("rotation kept the uuid mapping")
	}
	if a.isolation(k).CacheSalt != mk(testSaltKey).isolation(k).CacheSalt {
		t.Fatal("same key, different salt")
	}
	// The salt and the uuid namespace never coincide (domain separation).
	if a.isolation(k).CacheSalt == strings.TrimPrefix(a.isolation(k).ScopeUUID(""), "llmf-") {
		t.Fatal("salt and uuid derivations collide")
	}
}
