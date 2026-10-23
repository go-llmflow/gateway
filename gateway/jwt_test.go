package gateway

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

// fakeIdP is an OpenID provider: discovery and a key set (RFC 7517) with
// one RSA key, and a counter of key set fetches.
type fakeIdP struct {
	srv     *httptest.Server
	key     *rsa.PrivateKey
	fetches atomic.Int64

	// The token endpoint (RFC 6749 §4.1.3, RFC 7636 §4.6): codes issued by
	// the test with the claims of the ID token they buy.
	mu     sync.Mutex
	grants map[string]grant
	secret string // client secret expected with client_secret_basic
}

type grant struct {
	challenge string
	claims    map[string]any
}

// issue registers a code for a login whose authorization request was
// authURL, as the provider would after the person authenticated.
func (p *fakeIdP) issue(t testing.TB, authURL string, claims map[string]any) (state, code string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" {
		t.Fatalf("authorization request %s: no PKCE S256 code flow", authURL)
	}
	if _, set := claims["nonce"]; !set {
		claims["nonce"] = q.Get("nonce")
	}
	code = randomToken()
	p.mu.Lock()
	p.grants[code] = grant{challenge: q.Get("code_challenge"), claims: claims}
	p.mu.Unlock()
	return q.Get("state"), code
}

func (p *fakeIdP) token(t testing.TB, w http.ResponseWriter, r *http.Request) {
	if p.secret != "" {
		id, sec, ok := r.BasicAuth()
		if !ok || id != "llmflow-admin" || sec != p.secret {
			w.WriteHeader(http.StatusUnauthorized)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client"})
			return
		}
	}
	code := r.PostFormValue("code")
	p.mu.Lock()
	g, ok := p.grants[code]
	delete(p.grants, code)
	p.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostFormValue("code_verifier")))
	if !ok || r.PostFormValue("grant_type") != "authorization_code" || b64u(sum[:]) != g.challenge {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at-" + code, "token_type": "Bearer", "expires_in": 300,
		"id_token": p.sign(t, map[string]any{"alg": "RS256", "kid": "k1"}, g.claims),
	})
}

func newFakeIdP(t testing.TB) *fakeIdP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	p := &fakeIdP{key: k, grants: map[string]grant{}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			json.NewEncoder(w).Encode(map[string]any{
				"issuer": p.srv.URL, "jwks_uri": p.srv.URL + "/keys",
				"authorization_endpoint": p.srv.URL + "/authorize", "token_endpoint": p.srv.URL + "/token",
				"end_session_endpoint": p.srv.URL + "/logout", "code_challenge_methods_supported": []string{"S256"},
			})
		case "/token":
			p.token(t, w, r)
		case "/keys":
			p.fetches.Add(1)
			json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
				"kty": "RSA", "kid": "k1", "use": "sig", "alg": "RS256",
				"n": b64u(k.N.Bytes()), "e": b64u(big.NewInt(int64(k.E)).Bytes()),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func jsonB64(t testing.TB, v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b64u(b)
}

// sign makes an RS256 JWS compact serialization (RFC 7515 §7.1).
func (p *fakeIdP) sign(t testing.TB, hdr, claims map[string]any) string {
	t.Helper()
	in := jsonB64(t, hdr) + "." + jsonB64(t, claims)
	sum := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, p.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + b64u(sig)
}

func (p *fakeIdP) claims(now time.Time, extra map[string]any) map[string]any {
	c := map[string]any{
		"iss": p.srv.URL, "aud": "llmflow", "sub": "u-123", "preferred_username": "alice",
		"groups": []string{"lab-a"}, "iat": now.Unix(), "exp": now.Add(10 * time.Minute).Unix(),
	}
	for k, v := range extra {
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
	}
	return c
}

func jwtEnv(t *testing.T, idp *fakeIdP, clock func() time.Time, typ string) *env {
	up := newFakeVLLM(t)
	cfg := testConfig(up.URL())
	cfg.AuthFailuresPerMinute = 0
	cfg.JWT = &config.JWT{
		Issuer: idp.srv.URL, Audience: "llmflow", Type: typ, ClockSkew: time.Minute, MinRefresh: time.Hour,
		Maps: []config.JWTMap{
			{Claim: "groups", Value: "lab-a", Team: "etabA"},
			{Claim: "department", Value: "physics", Team: "etabB", Project: "p1"},
		},
	}
	return newEnv(t, cfg, WithClock(clock))
}

func TestJWTInference(t *testing.T) {
	idp := newFakeIdP(t)
	var mu sync.Mutex
	now := time.Now()
	clock := func() time.Time { mu.Lock(); defer mu.Unlock(); return now }
	e := jwtEnv(t, idp, clock, "")
	rs := map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}

	good := idp.sign(t, rs, idp.claims(now, nil))
	expect(t, "valid token", e.chatWith(good, chat), 200)
	expect(t, "valid token, cached", e.chatWith(good, chat), 200)

	// A claim other than groups, as a string, mapped to a team and project;
	// the project must exist.
	dept := idp.sign(t, rs, idp.claims(now, map[string]any{"groups": nil, "department": "physics", "sub": "u-456"}))
	expect(t, "department map, no project yet", e.chatWith(dept, chat), 401)
	e.check(nil, []call{{"master", "POST", "/admin/teams/etabB/projects", `{"name":"p1"}`, 201}})
	expect(t, "department map", e.chatWith(dept, chat), 200)

	pub, err := x509.MarshalPKIXPublicKey(&idp.key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pub})
	hs := func(c map[string]any) string {
		in := jsonB64(t, map[string]any{"alg": "HS256", "kid": "k1"}) + "." + jsonB64(t, c)
		m := hmac.New(sha256.New, pubPEM)
		m.Write([]byte(in))
		return in + "." + b64u(m.Sum(nil))
	}
	refused := map[string]string{
		"alg none":                  jsonB64(t, map[string]any{"alg": "none"}) + "." + jsonB64(t, idp.claims(now, nil)) + ".",
		"HS256 with the public key": hs(idp.claims(now, nil)),
		"expired":                   idp.sign(t, rs, idp.claims(now, map[string]any{"exp": now.Add(-time.Hour).Unix()})),
		"wrong audience":            idp.sign(t, rs, idp.claims(now, map[string]any{"aud": "other-service"})),
		"wrong issuer":              idp.sign(t, rs, idp.claims(now, map[string]any{"iss": idp.srv.URL + ".evil.test"})),
		"bad signature":             good[:len(good)-4] + "AAAA",
		"unmapped group":            idp.sign(t, rs, idp.claims(now, map[string]any{"groups": []string{"lab-z"}})),
		"no aud":                    idp.sign(t, rs, idp.claims(now, map[string]any{"aud": nil})),
	}
	var body string
	for name, tok := range refused {
		resp := e.do("POST", "/v1/chat/completions", tok, chat)
		b := readAll(t, resp)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d %s, want 401", name, resp.StatusCode, b)
		}
		// One refusal whatever the check: the body says nothing about which.
		if body == "" {
			body = b
		} else if b != body {
			t.Errorf("%s: refusal %q differs from %q", name, b, body)
		}
	}

	// A flood of unknown kids makes at most one extra key set fetch per
	// MinRefresh (one hour here).
	before := idp.fetches.Load()
	for i := 0; i < 50; i++ {
		tok := idp.sign(t, map[string]any{"alg": "RS256", "kid": fmt.Sprintf("kid-%d", i)}, idp.claims(now, nil))
		expect(t, "unknown kid", e.chatWith(tok, chat), 401)
	}
	if n := idp.fetches.Load() - before; n > 1 {
		t.Errorf("unknown kid flood: %d key set fetches, want at most 1", n)
	}

	// The cache never outlives the token.
	mu.Lock()
	now = now.Add(15 * time.Minute)
	mu.Unlock()
	expect(t, "expired after caching", e.chatWith(good, chat), 401)

	// Requests are accounted to one stable key per (issuer, subject).
	e.srv.Close(context.Background()) // drains the accounting queue
	ids := map[string]bool{}
	for _, r := range e.st.Records() {
		ids[r.KeyID] = true
		if !strings.HasPrefix(r.KeyID, "jwt-") {
			t.Errorf("record key %q, want a jwt- key", r.KeyID)
		}
	}
	if len(ids) != 2 {
		t.Errorf("records under %d keys, want 2 (two subjects)", len(ids))
	}
}

// A resource server configured for access tokens refuses an ID token
// (RFC 9068 §4).
func TestJWTType(t *testing.T) {
	idp := newFakeIdP(t)
	now := time.Now()
	e := jwtEnv(t, idp, func() time.Time { return now }, "at+jwt")
	expect(t, "id token", e.chatWith(idp.sign(t, map[string]any{"alg": "RS256", "kid": "k1", "typ": "JWT"}, idp.claims(now, nil)), chat), 401)
	expect(t, "access token", e.chatWith(idp.sign(t, map[string]any{"alg": "RS256", "kid": "k1", "typ": "at+jwt"}, idp.claims(now, nil)), chat), 200)
}

// A team the store does not know is a refusal, not a 500; virtual keys
// still work beside tokens.
func TestJWTUnknownTeamAndKeys(t *testing.T) {
	idp := newFakeIdP(t)
	now := time.Now()
	e := jwtEnv(t, idp, func() time.Time { return now }, "")
	e.srv.cfg.JWT.Maps[0].Team = "nope"
	expect(t, "unknown team", e.chatWith(idp.sign(t, map[string]any{"alg": "RS256", "kid": "k1"}, idp.claims(now, nil)), chat), 401)
	expect(t, "virtual key", e.chatWith(e.createKey(`{"team":"etabA"}`), chat), 200)
}

// The gateway refuses to start when its issuer does not answer.
func TestJWTDiscoveryFailureStopsStart(t *testing.T) {
	dead := httptest.NewServer(http.NotFoundHandler())
	dead.Close()
	cfg := testConfig("http://127.0.0.1:1")
	cfg.JWT = &config.JWT{Issuer: dead.URL, Audience: "x", Maps: []config.JWTMap{{Claim: "groups", Value: "a", Team: "etabA"}}}
	srv, err := New(cfg, store.NewMemory(), Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Start(context.Background()); err == nil {
		srv.Close(context.Background())
		t.Fatal("Start succeeded with an unreachable issuer")
	}
}

// FuzzJWTBearer sends arbitrary bearer tokens through authenticate with
// go-authn/oidc in the loop: nothing panics, and only the one token the
// fake provider signed is accepted.
func FuzzJWTBearer(f *testing.F) {
	idp := newFakeIdP(f)
	now := time.Now()
	cfg := testConfig("http://127.0.0.1:1")
	cfg.AuthFailuresPerMinute = 0
	cfg.JWT = &config.JWT{Issuer: idp.srv.URL, Audience: "llmflow", MinRefresh: time.Hour,
		Maps: []config.JWTMap{{Claim: "groups", Value: "lab-a", Team: "etabA"}}}
	srv, err := New(cfg, store.NewMemory(), Secrets{MasterKey: master, UpstreamKeys: testKeys(cfg), CacheSaltKey: testSaltKey},
		WithClock(func() time.Time { return now }))
	if err != nil {
		f.Fatal(err)
	}
	if err := srv.Start(context.Background()); err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { srv.Close(context.Background()) })
	good := idp.sign(f, map[string]any{"alg": "RS256", "kid": "k1"}, idp.claims(now, nil))
	f.Add(good)
	f.Add(good[:len(good)-1])
	f.Add(jsonB64(f, map[string]any{"alg": "none"}) + "." + jsonB64(f, idp.claims(now, nil)) + ".")
	f.Add(jsonB64(f, map[string]any{"alg": "RS256", "kid": "k1", "crit": []string{"x"}}) + ".e30.AAAA")
	f.Add("eyJhbGciOiJSUzI1NiJ9.eyJleHAiOjF9.")
	f.Fuzz(func(t *testing.T, tok string) {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		_, ok := srv.authenticate(w, r)
		if ok && tok != good {
			t.Fatalf("accepted %q", tok)
		}
		if !ok && w.Code != http.StatusUnauthorized {
			t.Fatalf("status %d for %q", w.Code, tok)
		}
	})
}

func TestLooksLikeJWT(t *testing.T) {
	for s, want := range map[string]bool{
		"llmf-abcdefghijklmnopqrstuvwxyz":  false,
		"aaaaaa.bbbbbbbbb.cccccccc":        true,
		"a.b.c":                            false, // too short
		"aaaaaaaa.bbbbbbbb.cccc.dddddd":    false,
		strings.Repeat("a", 17<<10) + "..": false,
	} {
		if got := looksLikeJWT(s); got != want {
			t.Errorf("looksLikeJWT(%.20q) = %v", s, got)
		}
	}
}
