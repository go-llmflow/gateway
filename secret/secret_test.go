package secret

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	good := map[string]Ref{
		"/etc/k":                             {File: "/etc/k"},
		"file:/etc/k":                        {File: "/etc/k"},
		"vault:secret/llmflow/master#key":    {Mount: "secret", Path: "llmflow/master", Field: "key"},
		"vault:kv/a#b?version=3":             {Mount: "kv", Path: "a", Field: "b", Version: 3},
		"vault:kv/team-a/x.y/z_1#field.name": {Mount: "kv", Path: "team-a/x.y/z_1", Field: "field.name"},
	}
	for in, want := range good {
		got, err := Parse(in)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", in, got, err)
		}
		if got.String() != strings.TrimPrefix(in, "file:") {
			t.Errorf("%q round trip %q", in, got.String())
		}
	}
	for _, bad := range []string{
		"", "file:", "vault:secret/x", "vault:secret#k", "vault:secret/../x#k", "vault:secret/x#",
		"vault:secret/x#k?version=0", "vault:secret/x#k?v=1", "vault:secret/x y#k", "vault:secret/x#k/l",
		"vault:/x#k", "vault:secret/x?y#k", "a\nb", "vault:secret/x%2f..#k",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func FuzzRef(f *testing.F) {
	for _, s := range []string{"/etc/k", "vault:secret/a/b#c?version=2", "vault:a/../b#c", "file:x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		r, err := Parse(s)
		if err != nil || !r.IsVault() {
			return
		}
		// What is accepted cannot leave the mount or inject URL syntax.
		for _, seg := range append(strings.Split(r.Path, "/"), r.Mount, r.Field) {
			if !validSegment(seg) {
				t.Fatalf("accepted segment %q in %q", seg, s)
			}
		}
		if strings.ContainsAny(r.Path+r.Mount+r.Field, "?#%\\ ") {
			t.Fatalf("URL syntax in %q", s)
		}
	})
}

func FuzzParseKV2(f *testing.F) {
	f.Add([]byte(`{"data":{"data":{"k":"v","n":1},"metadata":{"version":2}}}`))
	f.Add([]byte(`{"data":null}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		m, err := parseKV2(b)
		if err == nil && m == nil {
			t.Fatal("nil map without error")
		}
	})
}

func writeFile(t *testing.T, content string) string {
	p := filepath.Join(t.TempDir(), "s")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFile(t *testing.T) {
	r, _ := New(nil)
	p := writeFile(t, "  s3cret\n")
	for _, ref := range []string{p, "file:" + p} {
		if v, err := r.Resolve(context.Background(), ref); err != nil || v != "s3cret" {
			t.Fatalf("%q %v", v, err)
		}
	}
	for _, content := range []string{"\n", "a\nb"} {
		if _, err := r.Resolve(context.Background(), writeFile(t, content)); err == nil || strings.Contains(err.Error(), "b") && content == "a\nb" && strings.Contains(err.Error(), "a\nb") {
			t.Fatalf("content %q: %v", content, err)
		}
	}
	if _, err := r.Resolve(context.Background(), t.TempDir()); err == nil {
		t.Fatal("directory accepted")
	}
	if _, err := r.Resolve(context.Background(), "vault:kv/a#b"); err == nil {
		t.Fatal("vault reference without vault block accepted")
	}
}

// fakeBao answers KV v2 reads like OpenBao.
func fakeBao(t *testing.T, hits *atomic.Int64) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("X-Vault-Token") != "tok" {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/v1/secret/data/llmflow/master":
			v := "first"
			if r.URL.Query().Get("version") == "1" {
				v = "old"
			}
			_, _ = w.Write([]byte(`{"data":{"data":{"key":"` + v + `","n":1},"metadata":{"version":2}}}`))
		case "/v1/secret/data/redirect":
			http.Redirect(w, r, "http://evil.example/", http.StatusFound)
		case "/v1/secret/data/deleted":
			_, _ = w.Write([]byte(`{"data":null}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func TestVault(t *testing.T) {
	var hits atomic.Int64
	srv := fakeBao(t, &hits)
	r, err := New(&VaultConfig{Address: srv.URL, Token: "tok", CacheTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if v, err := r.Resolve(ctx, "vault:secret/llmflow/master#key"); err != nil || v != "first" {
		t.Fatalf("%q %v", v, err)
	}
	if v, err := r.Resolve(ctx, "vault:secret/llmflow/master#key?version=1"); err != nil || v != "old" {
		t.Fatalf("version: %q %v", v, err)
	}
	n := hits.Load()
	if _, err := r.Resolve(ctx, "vault:secret/llmflow/master#key"); err != nil || hits.Load() != n {
		t.Fatalf("not cached: %v (%d → %d)", err, n, hits.Load())
	}
	for ref, why := range map[string]string{
		"vault:secret/llmflow/master#n":       "non-string field",
		"vault:secret/llmflow/master#missing": "missing field",
		"vault:secret/nope#k":                 "404",
		"vault:secret/redirect#k":             "redirect not followed",
		"vault:secret/deleted#k":              "deleted version",
	} {
		if _, err := r.Resolve(ctx, ref); err == nil {
			t.Errorf("%s: accepted", why)
		}
	}
	bad, _ := New(&VaultConfig{Address: srv.URL, Token: "wrong"})
	if _, err := bad.Resolve(ctx, "vault:secret/llmflow/master#key"); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("bad token: %v", err)
	}
	// Cache expiry refetches.
	now := time.Now()
	r.now = func() time.Time { return now.Add(2 * time.Hour) }
	n = hits.Load()
	if _, err := r.Resolve(ctx, "vault:secret/llmflow/master#key"); err != nil || hits.Load() == n {
		t.Fatalf("expired entry not refetched: %v", err)
	}
}

func TestVaultConfig(t *testing.T) {
	for _, a := range []string{"http://bao.example:8200", "ftp://x", "https://u:p@x", "https://x?y", ""} {
		if _, err := New(&VaultConfig{Address: a, Token: "t"}); err == nil {
			t.Errorf("address %q accepted", a)
		}
	}
	for _, a := range []string{"https://bao.example:8200", "http://127.0.0.1:8200", "http://[::1]:1", "http://localhost:1"} {
		if err := ValidateAddress(a); err != nil {
			t.Errorf("%q: %v", a, err)
		}
	}
	if _, err := New(&VaultConfig{Address: "https://x", Token: ""}); err == nil {
		t.Fatal("empty token accepted")
	}
	if _, err := New(&VaultConfig{Address: "https://x", Token: "t", CAFile: writeFile(t, "not pem")}); err == nil {
		t.Fatal("bad CA accepted")
	}
	if _, err := New(&VaultConfig{Address: "https://x", Token: "t", CAFile: "/nonexistent"}); err == nil {
		t.Fatal("missing CA accepted")
	}
}
