package secret

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/internal/testpki"
)

// Real PEM, over several lines, from files and from OpenBao/Vault: a leaf
// with its intermediate, a key, a bundle of two CAs. Resolve (one line)
// still refuses them; ResolvePEM reads them, and refuses the wrong kind,
// two keys, and a key readable by others.
func TestResolvePEM(t *testing.T) {
	root, other := testpki.NewRoot("root"), testpki.NewRoot("other")
	inter := root.Intermediate("intermediate")
	cert, key := inter.ValidLeaf("llmflow-ha", "ed25519")
	bundle := append(root.PEM(), other.PEM()...)
	dir := t.TempDir()
	w := func(name string, b []byte, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, mode); err != nil {
			t.Fatal(err)
		}
		return p
	}
	certF, keyF, caF := w("cert.pem", cert, 0o644), w("key.pem", key, 0o600), w("ca.pem", bundle, 0o644)
	r, _ := New(nil)
	ctx := context.Background()
	if _, err := r.Resolve(ctx, certF); err == nil || !strings.Contains(err.Error(), "more than one line") {
		t.Fatalf("the one-line reader took a PEM file: %v", err)
	}
	for _, c := range []struct {
		ref, kind string
		want      []byte
	}{{certF, PEMCertificates, cert}, {keyF, PEMPrivateKey, key}, {caF, PEMCertificates, bundle}} {
		got, err := r.ResolvePEM(ctx, c.ref, c.kind)
		if err != nil || !bytes.Equal(got, bytes.TrimSpace(c.want)) {
			t.Fatalf("%s: %v", c.ref, err)
		}
	}
	if b, _ := r.ResolvePEM(ctx, caF, PEMCertificates); bytes.Count(b, []byte("BEGIN CERTIFICATE")) != 2 {
		t.Fatal("the CA bundle lost a certificate")
	}
	// openssl pkcs12 writes "Bag Attributes" lines before each block.
	bag := w("bag.pem", append([]byte("Bag Attributes\n    friendlyName: llmflow-ha\nsubject=CN = llmflow-ha\n"), cert...), 0o644)
	if _, err := r.ResolvePEM(ctx, bag, PEMCertificates); err != nil {
		t.Fatalf("PEM with openssl's attribute lines: %v", err)
	}
	for name, c := range map[string]struct {
		ref, kind, want string
	}{
		"key readable by others": {w("open.pem", key, 0o644), PEMPrivateKey, "chmod 600"},
		"two keys":               {w("two.pem", append(append([]byte{}, key...), key...), 0o600), PEMPrivateKey, "2 private keys"},
		"key as certificate":     {keyF, PEMCertificates, "where certificates were expected"},
		"certificate as key":     {w("c600.pem", cert, 0o600), PEMPrivateKey, "where a private key was expected"},
		"text after the blocks":  {w("junk.pem", append(append([]byte{}, cert...), "trailing junk\n"...), 0o644), PEMCertificates, "not PEM"},
		"empty":                  {w("empty.pem", []byte("\n"), 0o644), PEMCertificates, "empty"},
		"broken certificate":     {w("broken.pem", []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), 0o644), PEMCertificates, "does not parse"},
	} {
		if _, err := r.ResolvePEM(ctx, c.ref, c.kind); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		} else if strings.Contains(err.Error(), "BEGIN") {
			t.Errorf("%s: the error quotes the material", name)
		}
	}

	// The same from OpenBao/Vault: KV v2 fields hold the PEM with its
	// newlines (JSON strings).
	body, _ := json.Marshal(map[string]any{"data": map[string]any{"data": map[string]string{
		"cert": string(cert), "key": string(key), "ca": string(bundle)}, "metadata": map[string]any{"version": 1}}})
	bao := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != "tok" || r.URL.Path != "/v1/secret/data/llmflow/ha" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	}))
	defer bao.Close()
	vr, err := New(&VaultConfig{Address: bao.URL, Token: "tok", CacheTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		field, kind string
		want        []byte
	}{{"cert", PEMCertificates, cert}, {"key", PEMPrivateKey, key}, {"ca", PEMCertificates, bundle}} {
		got, err := vr.ResolvePEM(ctx, "vault:secret/llmflow/ha#"+c.field, c.kind)
		if err != nil || !bytes.Equal(got, bytes.TrimSpace(c.want)) {
			t.Fatalf("vault %s: %v", c.field, err)
		}
	}
}
