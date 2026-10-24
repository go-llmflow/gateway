package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/audit"
	"github.com/go-llmflow/gateway/config"
	"github.com/go-llmflow/gateway/store"
)

func TestAuditKeygenPubkeyVerify(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "audit.key")
	out, err := run(t, "audit", "keygen", "--out", keyPath)
	if err != nil || !strings.Contains(out, "public key ") {
		t.Fatalf("keygen: %q %v", out, err)
	}
	if _, err := run(t, "audit", "keygen", "--out", keyPath); err == nil {
		t.Fatal("keygen overwrote an existing key")
	}
	seed := mustRead(t, keyPath)
	if strings.Contains(out, strings.TrimSpace(seed)) {
		t.Fatal("the private key was printed")
	}
	mk := writeFile(t, dir, "master", "m-secret\n")
	vk := writeFile(t, dir, "vllm", "v-secret\n")
	sk := writeFile(t, dir, "salt", "0123456789abcdef0123456789abcdef-salt\n")
	cfg := writeFile(t, dir, "c.hcl", `
master_key_file = "`+mk+`"
cache_salt_key_file = "`+sk+`"
audit { signing_key_file = "`+keyPath+`" }
pool "p" { capacity = 4 }
model "m" {
  pool     = "p"
  key_file = "`+vk+`"
  upstream "u" { base_url = "http://127.0.0.1:1" }
}
`)
	pub, err := run(t, "audit", "pubkey", "-c", cfg)
	if err != nil || strings.TrimSpace(pub) != strings.TrimSpace(out) {
		t.Fatalf("pubkey %q, keygen said %q (%v)", pub, out, err)
	}
	if _, err := run(t, "audit", "verify", "-c", cfg); err == nil || !strings.Contains(err.Error(), "PostgreSQL") {
		t.Fatalf("verify on the memory store: %v", err)
	}
	if _, err := run(t, "audit", "verify", "-c", cfg, "--public-key", "nope"); err == nil {
		t.Fatal("a bad public key accepted")
	}

	// The report, and the failure on a tampered row.
	k, _ := audit.ParsePrivateKey(seed)
	st := store.NewMemory()
	l := audit.New(st, k, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := l.Append(ctx, store.AuditEntry{Time: time.Now(), Kind: "admin", Actor: "a", Roles: "[]", Action: "PUT /admin/x", Before: "null", After: "{}", Status: 200}); err != nil {
			t.Fatal(err)
		}
	}
	l.Checkpoint(ctx)
	var w bytes.Buffer
	keys := []ed25519.PublicKey{k.Public().(ed25519.PublicKey)}
	if err := verifyLog(ctx, st, keys, nil, &w); err != nil || !strings.Contains(w.String(), "entries 1 to 3 (3), 1 checkpoints verified") ||
		!strings.Contains(w.String(), "WARNING: no --anchor") {
		t.Fatalf("%q %v", w.String(), err)
	}
	// An anchor file, as exported: the intact log passes, an emptied one
	// passes without the anchor and fails with it.
	cps, _ := st.AuditCheckpoints(ctx)
	anchorPath := writeFile(t, dir, "cp.json", string(audit.MarshalCheckpoint(cps[0])))
	a, err := loadAnchor(ctx, nil, "file:"+anchorPath)
	if err != nil {
		t.Fatal(err)
	}
	w.Reset()
	if err := verifyLog(ctx, st, keys, &a, &w); err != nil || !strings.Contains(w.String(), "anchored at checkpoint 3") {
		t.Fatalf("anchored: %q %v", w.String(), err)
	}
	w.Reset()
	if err := verifyLog(ctx, store.NewMemory(), keys, nil, &w); err != nil || !strings.Contains(w.String(), "no entries") {
		t.Fatalf("empty log: %q %v", w.String(), err)
	}
	w.Reset()
	if err := verifyLog(ctx, store.NewMemory(), keys, &a, &w); err == nil {
		t.Fatalf("an emptied log passed with the anchor: %q", w.String())
	}
	for _, spec := range []string{"file:/nonexistent", "s3://x", "exportfoo", "export:none"} {
		if _, err := loadAnchor(ctx, &config.Config{}, spec); err == nil {
			t.Errorf("anchor %q accepted", spec)
		}
	}
	st.TamperAudit(2, func(e *store.AuditEntry) { e.Actor = "mallory" })
	w.Reset()
	if err := verifyLog(ctx, st, keys, nil, &w); err == nil || !strings.Contains(w.String(), "PROBLEM: entry 2") {
		t.Fatalf("tampered log: %q %v", w.String(), err)
	}
}
