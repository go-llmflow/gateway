package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/internal/testpki"
	"github.com/jackc/pgx/v5"
)

// Mutual TLS between gateway processes, with real PEM files as an
// operator writes them (v0.16.0 read them with the one-line secret reader
// and refused every real certificate): leaves signed by an intermediate
// CA, one ECDSA and one Ed25519 key, a CA file bundling two roots. A
// replica whose certificate another CA signed is not answered; an expired
// certificate is refused by check-config.
func TestHAMutualTLSProcesses(t *testing.T) {
	admin := os.Getenv("LLMFLOW_TEST_PG_DSN")
	if admin == "" {
		t.Skip("LLMFLOW_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	var b [6]byte
	_, _ = rand.Read(b[:])
	name := "hatls_" + hex.EncodeToString(b[:])
	if _, err := c.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	c.Close(ctx)
	t.Cleanup(func() {
		if c, err := pgx.Connect(context.Background(), admin); err == nil {
			_, _ = c.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			c.Close(context.Background())
		}
	})
	u, _ := url.Parse(admin)
	u.Path = "/" + name

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`)
	}))
	defer up.Close()

	root, otherRoot, rogueRoot := testpki.NewRoot("site root"), testpki.NewRoot("other root"), testpki.NewRoot("rogue root")
	inter := root.Intermediate("ha intermediate")
	dir := t.TempDir()
	pemFile := func(name string, data []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, data, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ca := pemFile("ha-ca.pem", append(otherRoot.PEM(), root.PEM()...), 0o644) // a bundle of two roots
	mk := writeFile(t, dir, "master", "m-secret-0123456789\n")
	vk := writeFile(t, dir, "vllm", "v-secret\n")
	sk := writeFile(t, dir, "salt", "0123456789abcdef0123456789abcdef-salt\n")
	hk := writeFile(t, dir, "ha", "ha-shared-secret-0123456789abcdef0123456789\n")
	dsn := writeFile(t, dir, "dsn", u.String()+"\n")
	conf := func(id string, cert, key []byte) string {
		cf := pemFile(id+"-cert.pem", cert, 0o644)
		kf := pemFile(id+"-key.pem", key, 0o600)
		return writeFile(t, dir, id+".hcl", `
listen              = "127.0.0.1:0"
master_key_file     = "`+mk+`"
cache_salt_key_file = "`+sk+`"
budget_refresh      = "1h"
key_cache_ttl       = "1h"
org "a" {
  share = 1
  team "ta" {}
}
pool "p" { capacity = 4 }
model "m" {
  pool = "p"
  upstream "u" {
    base_url = "`+up.URL+`"
    key_file = "`+vk+`"
  }
}
store {
  kind      = "postgres"
  dsn_file  = "`+dsn+`"
  spool_dir = "`+filepath.Join(dir, "spool-"+id)+`"
}
ha {
  replica_id      = "`+id+`"
  listen          = "127.0.0.1:`+freePort(t)+`"
  peer_networks   = ["127.0.0.0/8"]
  secret_file     = "`+hk+`"
  lease_ttl       = "1500ms"
  heartbeat       = "200ms"
  forward_timeout = "200ms"
  usage_persist   = "300ms"
  tls_cert_file   = "`+cf+`"
  tls_key_file    = "`+kf+`"
  tls_ca_file     = "`+ca+`"
}
`)
	}
	certA, keyA := inter.ValidLeaf("llmflow-ha", "ecdsa")
	certB, keyB := inter.ValidLeaf("llmflow-ha", "ed25519")
	certR, keyR := rogueRoot.ValidLeaf("llmflow-ha", "ecdsa")
	expC, expK := inter.Leaf("llmflow-ha", "ecdsa", time.Now().Add(-48*time.Hour), time.Now().Add(-24*time.Hour))
	cfgA, cfgB, cfgR := conf("A", certA, keyA), conf("B", certB, keyB), conf("R", certR, keyR)

	for _, cf := range []string{cfgA, cfgB} {
		if out, err := run(t, "check-config", "-c", cf); err != nil {
			t.Fatalf("check-config with real PEM files: %v %s", err, out)
		}
	}
	if _, err := run(t, "check-config", "-c", conf("X", expC, expK)); err == nil || !strings.Contains(err.Error(), "valid from") {
		t.Fatalf("an expired certificate: %v", err)
	}
	// The key file's mode is checked.
	open := conf("O", certA, keyA)
	if err := os.Chmod(filepath.Join(dir, "O-key.pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := run(t, "check-config", "-c", open); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("a key readable by others: %v", err)
	}

	bin := filepath.Join(t.TempDir(), "llmflow-gateway")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	procA, addrA := startGateway(t, bin, cfgA)
	defer func() { _ = procA.Process.Kill(); _ = procA.Wait() }()
	waitFor(t, 5*time.Second, "A owns the pool", func() bool { return metricL(t, addrA, `llmflow_ha_owner{pool="p"}`) == 1 })
	procB, addrB := startGateway(t, bin, cfgB)
	defer func() { _ = procB.Process.Kill(); _ = procB.Wait() }()
	procR, addrR := startGateway(t, bin, cfgR)
	defer func() { _ = procR.Process.Kill(); _ = procR.Wait() }()
	key := createKeyFor(t, addrA, "ta")
	waitFor(t, 5*time.Second, "B and R read the lease", func() bool {
		return metricL(t, addrB, `llmflow_ha_epoch{pool="p"}`) >= 1 && metricL(t, addrR, `llmflow_ha_epoch{pool="p"}`) >= 1
	})

	chat := func(addr string) int {
		req, _ := http.NewRequest("POST", "http://"+addr+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("chat on %s: %d", addr, resp.StatusCode)
		}
		p, _ := strconv.Atoi(resp.Header.Get("X-Llmflow-Priority"))
		return p
	}
	for range 5 {
		if p := chat(addrB); p != 0 {
			t.Fatalf("B (Ed25519 leaf, intermediate CA) forwarding over mTLS: priority %d", p)
		}
	}
	if n := metric(t, addrB, "llmflow_ha_unowned_total"); n != 0 {
		t.Fatalf("B admitted %v requests without the owner: the mTLS handshake failed", n)
	}
	chat(addrR)
	if n := metric(t, addrR, "llmflow_ha_unowned_total"); n < 1 {
		t.Fatalf("R, whose certificate another CA signed, reached the owner (unowned %v)", n)
	}
	t.Logf("mTLS between processes: B forwarded 5 of 5; R (rogue CA) refused, admitted without owner")
}
