package ha

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/go-llmflow/gateway/fairshare"
	"github.com/go-llmflow/gateway/store"
)

// testPKI is a CA and a leaf it signed, in PEM.
type testPKI struct{ ca, cert, key []byte }

func newTestPKI(t testing.TB, name string) testPKI {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalECPrivateKey(key)
	return testPKI{
		ca:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		cert: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		key:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}),
	}
}

// startTLSMember is startMember with the internal channel under mutual TLS.
func startTLSMember(t testing.TB, st store.HAStore, inst string, p testPKI) *member {
	t.Helper()
	srvTLS, cliTLS, err := MutualTLS(p.cert, p.key, p.ca, "llmflow-ha")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(inst, ln.Addr().String())
	cfg.ClientTLS = cliTLS
	view := NewView(st)
	n, err := New(cfg, view)
	if err != nil {
		t.Fatal(err)
	}
	m := &member{t: t, n: n, ln: ln, view: view, srv: &http.Server{Handler: n.Handler(), ReadHeaderTimeout: time.Second}}
	go func() { _ = m.srv.Serve(tls.NewListener(ln, srvTLS)) }()
	n.Start(context.Background())
	t.Cleanup(m.kill)
	return m
}

// The internal channel under mutual TLS: forwarding works; a peer without
// a certificate, or with one another CA signed, or speaking plain HTTP,
// gets nothing although it holds the HMAC secret.
func TestMutualTLS(t *testing.T) {
	pki := newTestPKI(t, "llmflow-ha")
	st := NewMemStore(nil)
	m1 := startTLSMember(t, st, "r1", pki)
	m2 := startTLSMember(t, st, "r2", pki)
	g, ok, err := m2.n.Acquire("p", "a", fairshare.Interactive, 1, false)
	if !ok || err != nil || m2.n.Degraded() != 0 || g.Priority != 0 {
		t.Fatalf("forwarded over mTLS: ok %v err %v degraded %d priority %d", ok, err, m2.n.Degraded(), g.Priority)
	}
	g.Release(0)
	addr := m1.ln.Addr().String()
	var out acquireResp
	req := acquireReq{Pool: "p", ID: "x", Org: "a"}

	plain, _ := New(testConfig("plain", "127.0.0.1:9"), st)
	if err := plain.call(context.Background(), addr, pathAcquire, req, &out); err == nil {
		t.Fatal("a plain-HTTP peer holding the secret was answered")
	}
	rogue := newTestPKI(t, "llmflow-ha")
	_, rogueTLS, _ := MutualTLS(rogue.cert, rogue.key, pki.ca, "llmflow-ha") // trusts the CA, signed by another
	cfg := testConfig("rogue", "127.0.0.1:9")
	cfg.ClientTLS = rogueTLS
	r, _ := New(cfg, st)
	if err := r.call(context.Background(), addr, pathAcquire, req, &out); err == nil {
		t.Fatal("a peer with a certificate from another CA was answered")
	}
	// A server name the certificate does not carry is refused too.
	_, wrongName, _ := MutualTLS(pki.cert, pki.key, pki.ca, "other-name")
	cfg = testConfig("wrongname", "127.0.0.1:9")
	cfg.ClientTLS = wrongName
	w, _ := New(cfg, st)
	if err := w.call(context.Background(), addr, pathAcquire, req, &out); err == nil {
		t.Fatal("a server certificate without the configured name was accepted")
	}
	if _, _, err := MutualTLS(pki.cert, pki.key, []byte("not pem"), "llmflow-ha"); err == nil {
		t.Fatal("a CA file without a certificate was accepted")
	}
}
