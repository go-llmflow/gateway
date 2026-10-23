package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The secrets of a configuration can live in OpenBao/Vault KV v2.
func TestCheckConfigWithVault(t *testing.T) {
	bao := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "bao-token" || r.URL.Path != "/v1/secret/data/llmflow" {
			http.Error(w, "{}", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte(`{"data":{"data":{"master":"m-secret","salt":"0123456789abcdef0123456789abcdef-salt","vllm":"v-secret"}}}`))
	}))
	defer bao.Close()
	dir := t.TempDir()
	tok := writeFile(t, dir, "bao.token", "bao-token\n")
	cfg := writeFile(t, dir, "c.hcl", `
master_key_file     = "vault:secret/llmflow#master"
cache_salt_key_file = "vault:secret/llmflow#salt"
vault {
  address    = "`+bao.URL+`"
  token_file = "`+tok+`"
}
org "o" {
  share = 1
  team "a" {}
}
pool "p" { capacity = 4 }
model "m" {
  pool     = "p"
  key_file = "vault:secret/llmflow#vllm"
  upstream "u" { base_url = "http://127.0.0.1:1" }
}
`)
	out, err := run(t, "check-config", "-c", cfg)
	if err != nil || !strings.Contains(out, "ok:") {
		t.Fatalf("%q %v", out, err)
	}
	bad := writeFile(t, dir, "bad.hcl", strings.Replace(mustRead(t, cfg), "#vllm", "#missing", 1))
	if _, err := run(t, "check-config", "-c", bad); err == nil || !strings.Contains(err.Error(), "no such field") {
		t.Fatalf("missing field: %v", err)
	}
	writeFile(t, dir, "bao.token", "wrong\n")
	if _, err := run(t, "check-config", "-c", cfg); err == nil || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("bad token: %v", err)
	}
}
